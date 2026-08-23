"""The pull loop.

blacksmith is a client, so it PULLS work — conductor cannot reach an intermittent
workstation behind NAT. The loop is edge-triggered on completion for latency and
level-triggered on a timer for correctness. The timer is not a stopgap to remove
once completion notification works: it is what makes the loop self-healing when a
task dies without reporting, or when a ticket arrives while the queue is empty.
"""

import asyncio
from datetime import timedelta

import pytest

from blacksmith import workflow as wf
from blacksmith.dispatch import Dispatcher, DispatcherOpts, plan_dispatch
from blacksmith.errors import Conflict, Transient
from blacksmith.outcomes import Outcome
from blacksmith.tickets import RETURNED_MARKER, ClaimToken, Comment, Ticket, format_claim
from blacksmith.transcript import Recorder
from blacksmith.wake import Wake
from blacksmith.workflow import Dependency
from tests.conftest import settle
from tests.fake_platform import EPOCH, FakePlatform

MAX = 3


class FakeHandler:
    """Records what it was given and answers with whatever the test set."""

    def __init__(self, role: str = wf.ROLE_DEV, outcome: Outcome = Outcome.SUCCESS) -> None:
        self.role = role
        self.outcome = outcome
        self.detail = ""
        self.seen: list[Ticket] = []
        self.wanted: bool | None = None
        self.raises: Exception | None = None
        self.gate: asyncio.Event | None = None

    def wants(self, ticket: Ticket) -> bool:
        return True if self.wanted is None else self.wanted

    async def handle(self, ticket: Ticket, transcript) -> tuple[Outcome, str]:
        self.seen.append(ticket)
        if self.gate is not None:
            await self.gate.wait()
        if self.raises is not None:
            raise self.raises
        return self.outcome, self.detail


def dispatcher(api, handler=None, **opts) -> tuple[Dispatcher, FakeHandler]:
    handler = handler or FakeHandler()
    defaults = dict(host="box-a", board_id="B-1", concurrency=1, max_attempts=MAX)
    return (
        Dispatcher(
            api,
            Recorder(None, host="box-a"),
            handler,
            wf.routing(),
            DispatcherOpts(**{**defaults, **opts}),
        ),
        handler,
    )


def queued(
    ticket_id: str = "T-1",
    status: str = wf.COL_READY_FOR_DEV,
    *,
    priority: str = "normal",
    created_by: str = "a-person",
    comments: list[Comment] | None = None,
    depends_on: list[Dependency] | None = None,
    created_at=None,
) -> Ticket:
    return Ticket(
        ticket_id=ticket_id,
        title=ticket_id,
        status=status,
        priority=priority,
        created_by=created_by,
        board_id="B-1",
        comments=comments or [],
        depends_on=depends_on or [],
        created_at=created_at or EPOCH,
    )


def claim_comment(role: str, host: str = "box-a", *, cid: str = "c1", at=None) -> Comment:
    return Comment(
        comment_id=cid,
        ticket_id="T-1",
        author_id=role,
        body=format_claim(ClaimToken(host=host, role=role, run_id="r")),
        created_at=at or EPOCH,
    )


# --- what a poll asks for -------------------------------------------------


async def test_a_poll_asks_the_server_for_this_stages_column_only():
    """The column replaces the marker predicates routing used to need, and it is
    what makes the read below cheap: a handful of tickets waiting on one stage
    rather than every open ticket on the board."""
    api = FakePlatform()
    d, _ = dispatcher(api)
    await d.fill()
    assert ("list_tickets", wf.COL_READY_FOR_DEV) in api.calls


async def test_a_candidate_is_read_in_full_before_it_is_judged():
    """A listing carries no comments, and two things that decide eligibility are
    comments: the attempt ceiling and the refinements some stages make within
    their column. Judged from a listing those evaluate against nothing —
    silently, and in the worst direction, since a predicate requiring a marker's
    ABSENCE is satisfied by every ticket and re-claims the column every poll."""
    api = FakePlatform()
    api.add(queued())
    d, handler = dispatcher(api)
    await d.fill()
    await settle(5)
    assert handler.seen and handler.seen[0].comments, "the handler was given a listing, not a ticket"


async def test_a_ticket_rejected_from_the_listing_costs_no_read():
    """Dependencies and authorship travel ON the listing, so a ticket that is not
    ready costs nothing to reject."""
    api = FakePlatform()
    api.add(queued(depends_on=[Dependency(ticket_id="T-0", status=wf.COL_IN_DEV)]))
    d, _ = dispatcher(api)
    await d.fill()
    assert not [c for c in api.calls if c[0] == "get_ticket"]


async def test_a_poll_stops_reading_at_the_cap():
    """The cap keeps a large board from turning one poll into hundreds of
    requests: past it, the poll works with what it has and picks the rest up next
    time.

    The bound is the cap plus one re-read per ticket actually STARTED — a
    different read, bounded by concurrency rather than by board size, and the one
    that gets the comments the work itself needs.
    """
    api = FakePlatform()
    concurrency = 2
    for i in range(Dispatcher.max_hydrate_per_poll + 10):
        api.add(queued(f"T-{i}"))
    d, _ = dispatcher(api, concurrency=concurrency)
    await d.fill()
    reads = len([c for c in api.calls if c[0] == "get_ticket"])
    assert reads <= Dispatcher.max_hydrate_per_poll + concurrency
    assert reads > concurrency, "the cap stopped the poll before it hydrated anything"
    await d.drain()


async def test_one_unreadable_ticket_does_not_cost_the_whole_poll():
    api = FakePlatform()
    api.add(queued("T-1"))
    api.add(queued("T-2"))
    api.fail_once["get_ticket"] = Transient("502")
    d, handler = dispatcher(api, concurrency=2)
    await d.fill()
    await settle(5)
    assert len(handler.seen) == 1


# --- who is eligible ------------------------------------------------------


async def test_a_ticket_in_another_column_is_not_this_stages_work():
    api = FakePlatform()
    api.add(queued(status=wf.COL_IN_REVIEW))
    d, handler = dispatcher(api)
    await d.fill()
    await settle(5)
    assert handler.seen == []


async def test_a_ticket_the_department_wrote_is_not_taken_back_off_the_inbox():
    """It exists so the department does not feed its own output back to itself."""
    api = FakePlatform()
    api.add(queued(status=wf.COL_INBOX, created_by="pm-agent"))
    d, handler = dispatcher(
        api, FakeHandler(role=wf.ROLE_SCOPING), agent_authors=["pm-agent", "dev-agent"]
    )
    await d.fill()
    await settle(5)
    assert handler.seen == []


async def test_the_authorship_guard_applies_to_the_inbox_stage_only():
    """Under column routing the feedback loop is structurally impossible, and
    applying the guard everywhere would do real harm: the child tickets the
    product manager creates are precisely the work the developer exists to pick
    up."""
    api = FakePlatform()
    api.add(queued(created_by="pm-agent"))
    d, handler = dispatcher(api, agent_authors=["pm-agent", "dev-agent"])
    await d.fill()
    await settle(5)
    assert len(handler.seen) == 1


async def test_a_ticket_at_its_attempt_ceiling_is_left_alone():
    api = FakePlatform()
    api.add(queued(comments=[claim_comment(wf.ROLE_DEV, cid=f"c{i}") for i in range(MAX)]))
    d, handler = dispatcher(api)
    await d.fill()
    await settle(5)
    assert handler.seen == []


async def test_a_send_back_gives_the_ticket_a_fresh_budget():
    """The work waiting after a send-back is a fix for a finding that did not
    exist when those attempts were spent."""
    api = FakePlatform()
    api.add(
        queued(
            comments=[
                *[claim_comment(wf.ROLE_DEV, cid=f"c{i}") for i in range(MAX)],
                Comment(
                    comment_id="c9",
                    body=RETURNED_MARKER,
                    created_at=EPOCH + timedelta(minutes=1),
                ),
            ]
        )
    )
    d, handler = dispatcher(api)
    await d.fill()
    await settle(5)
    assert len(handler.seen) == 1


async def test_work_waiting_on_an_unfinished_prerequisite_is_not_due():
    """Writing code against something that does not exist yet produces a branch
    nobody can usefully review."""
    api = FakePlatform()
    api.add(queued(depends_on=[Dependency(ticket_id="T-0", status=wf.COL_IN_DEV)]))
    d, handler = dispatcher(api)
    await d.fill()
    await settle(5)
    assert handler.seen == []


async def test_work_whose_prerequisites_are_done_runs():
    api = FakePlatform()
    api.add(queued(depends_on=[Dependency(ticket_id="T-0", status=wf.COL_DONE)]))
    d, handler = dispatcher(api)
    await d.fill()
    await settle(5)
    assert len(handler.seen) == 1


async def test_a_stage_that_does_not_gate_on_dependencies_takes_the_work():
    api = FakePlatform()
    api.add(
        queued(
            status=wf.COL_READY_FOR_REVIEW,
            depends_on=[Dependency(ticket_id="T-0", status=wf.COL_IN_DEV)],
        )
    )
    d, handler = dispatcher(api, FakeHandler(role=wf.ROLE_REVIEW))
    await d.fill()
    await settle(5)
    assert len(handler.seen) == 1


async def test_the_handler_gets_the_final_say_and_the_full_ticket():
    """Most stages accept everything in their column — the column IS the predicate
    now — but one that wants a subset still says so, with the full ticket."""
    api = FakePlatform()
    api.add(queued())
    handler = FakeHandler()
    handler.wanted = False
    d, _ = dispatcher(api, handler)
    await d.fill()
    await settle(5)
    assert handler.seen == []
    assert api.status_of("T-1") == wf.COL_READY_FOR_DEV, "a refused ticket was left claimed"


async def test_a_ticket_already_in_flight_is_not_claimed_twice():
    api = FakePlatform()
    api.add(queued())
    handler = FakeHandler()
    handler.gate = asyncio.Event()
    d, _ = dispatcher(api, handler, concurrency=2)
    await d.fill()
    await settle(5)
    api.tickets["T-1"] = api.tickets["T-1"].with_(status=wf.COL_READY_FOR_DEV)
    await d.fill()
    await settle(5)
    assert len(handler.seen) == 1
    handler.gate.set()
    await d.drain()


# --- claiming -------------------------------------------------------------


async def test_a_lost_race_is_quiet_and_does_not_stop_the_poll():
    """A peer won. Expected, not an error worth logging loudly — and the rest of
    the poll must still happen."""
    api = FakePlatform()
    api.add(queued("T-1"))
    api.add(queued("T-2"))
    api.fail_once["update_ticket"] = Conflict("already claimed")
    d, handler = dispatcher(api, concurrency=2)
    await d.fill()
    await settle(5)
    assert [t.ticket_id for t in handler.seen] == ["T-2"]


async def test_a_ticket_unreadable_after_the_claim_is_put_back():
    """Unverifiable. Yield rather than work from data known to be partial."""
    api = FakePlatform()
    api.add(queued())
    d, handler = dispatcher(api)
    api.before["update_ticket"] = lambda _t: api.fail_once.setdefault(
        "get_ticket", Transient("502")
    )
    await d.fill()
    await settle(5)
    assert handler.seen == []
    assert api.status_of("T-1") == wf.COL_READY_FOR_DEV


async def test_claiming_moves_the_ticket_into_the_working_column():
    api = FakePlatform()
    api.add(queued())
    handler = FakeHandler()
    handler.gate = asyncio.Event()
    d, _ = dispatcher(api, handler)
    await d.fill()
    await settle(5)
    assert api.status_of("T-1") == wf.COL_IN_DEV
    handler.gate.set()
    await d.drain()


# --- ordering -------------------------------------------------------------


def test_the_next_ticket_is_the_highest_priority_available_one():
    picked = plan_dispatch(
        [queued("T-low", priority="low"), queued("T-crit", priority="critical")],
        in_flight=0,
        limit=1,
    )
    assert [t.ticket_id for t in picked] == ["T-crit"]


def test_within_a_priority_the_older_ticket_goes_first():
    old = queued("T-old", created_at=EPOCH)
    new = queued("T-new", created_at=EPOCH + timedelta(hours=1))
    assert [t.ticket_id for t in plan_dispatch([new, old], in_flight=0, limit=2)] == ["T-old", "T-new"]


def test_an_unknown_priority_does_not_jump_the_queue():
    picked = plan_dispatch(
        [queued("T-odd", priority="blocker!!"), queued("T-normal", priority="normal")],
        in_flight=0,
        limit=1,
    )
    assert [t.ticket_id for t in picked] == ["T-normal"]


def test_nothing_starts_when_the_stage_is_full():
    """How many run at once is this stage's concurrency and nothing else."""
    assert plan_dispatch([queued("T-1", priority="critical")], in_flight=2, limit=2) == []


def test_a_critical_ticket_does_not_pin_the_stage():
    """Critical exclusivity belongs to the model, not to this stage. A dispatcher
    is per stage, so pinning here could only stop its OWN queue while the other
    stages went on hitting the same server — the critical ticket never got an
    exclusive stream, it merely lost its siblings."""
    picked = plan_dispatch(
        [queued("T-crit", priority="critical"), queued("T-2"), queued("T-3")],
        in_flight=0,
        limit=3,
    )
    assert len(picked) == 3 and picked[0].ticket_id == "T-crit"


def test_only_the_free_slots_are_filled():
    picked = plan_dispatch([queued(f"T-{i}") for i in range(5)], in_flight=1, limit=3)
    assert len(picked) == 2


def test_planning_with_nothing_available_is_empty():
    assert plan_dispatch([], in_flight=0, limit=4) == []


# --- finishing ------------------------------------------------------------


async def test_success_advances_the_ticket_to_the_next_stages_queue():
    """Finishing a stage moves the work to the next column, which is both the
    handover and the thing a person sees on the board."""
    api = FakePlatform()
    api.add(queued())
    d, _ = dispatcher(api)
    await d.fill()
    await d.drain()
    assert api.status_of("T-1") == wf.COL_READY_FOR_COVERAGE


async def test_a_handler_that_places_the_ticket_itself_is_not_overruled():
    api = FakePlatform()
    api.add(queued())
    d, _ = dispatcher(api, FakeHandler(outcome=Outcome.HANDLED))
    await d.fill()
    await d.drain()
    assert api.status_of("T-1") == wf.COL_IN_DEV, "routing undid the handler's own placement"


async def test_a_handler_that_raises_is_a_failed_attempt_not_a_crash():
    api = FakePlatform()
    api.add(queued())
    handler = FakeHandler()
    handler.raises = RuntimeError("the model went away")
    d, _ = dispatcher(api, handler)
    await d.fill()
    await d.drain()
    assert api.status_of("T-1") == wf.COL_READY_FOR_DEV


async def test_the_last_failure_stops_rather_than_cycling():
    api = FakePlatform()
    api.add(queued(comments=[claim_comment(wf.ROLE_DEV, cid=f"c{i}") for i in range(MAX - 1)]))
    d, _ = dispatcher(api, FakeHandler(outcome=Outcome.FAILED))
    await d.fill()
    await d.drain()
    assert api.status_of("T-1") == wf.COL_BLOCKED


async def test_a_hand_back_lands_in_the_earlier_stages_queue():
    api = FakePlatform()
    api.add(queued(status=wf.COL_READY_FOR_REVIEW))
    d, _ = dispatcher(api, FakeHandler(role=wf.ROLE_REVIEW, outcome=Outcome.RETURNED))
    await d.fill()
    await d.drain()
    assert api.status_of("T-1") == wf.COL_READY_FOR_DEV


async def test_finishing_a_ticket_wakes_the_other_stages():
    """The ticket is now in another stage's queue. Tell everyone rather than
    leaving it to be discovered a poll interval later."""
    wake = Wake()
    woken = wake.subscribe()
    api = FakePlatform()
    api.add(queued())
    d, _ = dispatcher(api, wake=wake)
    await d.fill()
    await d.drain()
    await asyncio.wait_for(woken.wait(), timeout=1)


async def test_a_ticket_that_could_not_be_advanced_is_left_for_the_next_reconcile():
    api = FakePlatform()
    api.add(queued())
    d, _ = dispatcher(api)
    await d.fill()
    await settle(2)
    api.fail_always["update_ticket"] = Transient("502")
    await d.drain()  # must not raise


async def test_a_slot_is_freed_when_the_ticket_finishes():
    api = FakePlatform()
    api.add(queued())
    d, _ = dispatcher(api)
    await d.fill()
    await d.drain()
    assert d.in_flight == 0


# --- being shut down ------------------------------------------------------


async def test_work_cut_short_by_a_shutdown_goes_back_to_its_queue():
    """The host went away mid-task. That is not the agent getting it wrong, so the
    work goes back to be picked up again rather than counting against the
    ticket."""
    api = FakePlatform()
    api.add(queued())
    handler = FakeHandler()
    handler.gate = asyncio.Event()
    d, _ = dispatcher(api, handler)
    await d.fill()
    await settle(5)
    await d.aclose()
    assert api.status_of("T-1") == wf.COL_READY_FOR_DEV


async def test_work_returned_by_a_shutdown_is_picked_up_again():
    """The ticket must be claimable by the host that restarts, or a shutdown
    quietly costs a ticket its budget."""
    api = FakePlatform()
    api.add(queued())
    first = FakeHandler()
    first.gate = asyncio.Event()
    d, _ = dispatcher(api, first)
    await d.fill()
    await settle(5)
    await d.aclose()

    restarted, second = dispatcher(api)
    await restarted.reconcile()
    await restarted.fill()
    await restarted.drain()
    assert [t.ticket_id for t in second.seen] == ["T-1"]


# --- picking up after a restart ------------------------------------------


async def test_reconcile_releases_work_this_host_was_killed_in_the_middle_of():
    api = FakePlatform()
    api.add(queued(status=wf.COL_IN_DEV, comments=[claim_comment(wf.ROLE_DEV, "box-a")]))
    d, _ = dispatcher(api)
    await d.reconcile()
    assert api.status_of("T-1") == wf.COL_READY_FOR_DEV


async def test_reconcile_leaves_another_hosts_work_alone():
    """Another host's in-flight work is indistinguishable from a stranded claim
    when viewed from here, so an unscoped sweep would steal live work from a
    peer."""
    api = FakePlatform()
    api.add(queued(status=wf.COL_IN_DEV, comments=[claim_comment(wf.ROLE_DEV, "box-b")]))
    d, _ = dispatcher(api)
    await d.reconcile()
    assert api.status_of("T-1") == wf.COL_IN_DEV


async def test_reconcile_leaves_work_this_host_is_actually_doing():
    api = FakePlatform()
    api.add(queued())
    handler = FakeHandler()
    handler.gate = asyncio.Event()
    d, _ = dispatcher(api, handler)
    await d.fill()
    await settle(5)
    await d.reconcile()
    assert api.status_of("T-1") == wf.COL_IN_DEV
    handler.gate.set()
    await d.drain()


async def test_reconciled_work_gets_a_fresh_budget():
    """A claim is written when work STARTS, so a killed process leaves one behind
    that counts against maxAttempts."""
    api = FakePlatform()
    api.add(
        queued(
            status=wf.COL_IN_DEV,
            comments=[claim_comment(wf.ROLE_DEV, "box-a", cid=f"c{i}") for i in range(MAX)],
        )
    )
    d, handler = dispatcher(api)
    await d.reconcile()
    await d.fill()
    await settle(5)
    assert len(handler.seen) == 1, "an interrupted attempt was charged to the ticket"


async def test_a_failed_reconcile_does_not_stop_the_host_starting():
    """It leaves some tickets stranded until the next restart, which is better
    than refusing to start at all."""
    api = FakePlatform()
    api.fail_always["list_tickets"] = Transient("502")
    d, _ = dispatcher(api)
    await d.reconcile()  # must not raise


# --- the loop itself ------------------------------------------------------


async def test_the_loop_polls_until_it_is_stopped():
    api = FakePlatform()
    d, _ = dispatcher(api, poll=0.001)
    task = asyncio.create_task(d.run())
    await asyncio.sleep(0.02)
    await d.aclose()
    await task
    assert len([c for c in api.calls if c[0] == "list_tickets"]) >= 2


async def test_a_wake_polls_without_waiting_for_the_timer():
    """Without it a hand-off waits for the timer, several times per ticket."""
    wake = Wake()
    api = FakePlatform()
    d, _ = dispatcher(api, poll=30.0, wake=wake)
    task = asyncio.create_task(d.run())
    await asyncio.sleep(0.01)
    before = len([c for c in api.calls if c[0] == "list_tickets"])
    wake.signal()
    await asyncio.sleep(0.01)
    assert len([c for c in api.calls if c[0] == "list_tickets"]) > before
    await d.aclose()
    await task


async def test_a_poll_that_fails_is_retried_on_the_next_tick():
    api = FakePlatform()
    api.fail_once["list_tickets"] = Transient("502")
    d, _ = dispatcher(api, poll=0.001)
    task = asyncio.create_task(d.run())
    await asyncio.sleep(0.02)
    await d.aclose()
    await task
    assert len([c for c in api.calls if c[0] == "list_tickets"]) >= 2


async def test_a_handler_with_no_routing_entry_fails_where_it_is_configured():
    """It would otherwise poll a column that does not exist and silently never
    work."""
    with pytest.raises(KeyError):
        dispatcher(FakePlatform(), FakeHandler(role="no-such-agent"))
