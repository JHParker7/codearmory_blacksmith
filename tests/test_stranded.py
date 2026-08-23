"""Work waiting on a prerequisite that can never finish.

`dependencies_met` requires every blocker to reach done, and blocked is terminal
WITHOUT being done — so a ticket behind a blocked one is not waiting, it is
stranded. Observed on a real batch: a chain of six tickets lost its second, and
the remaining four sat in ready_for_dev indefinitely, looking queued and
consuming nothing. Nothing swept them and nothing said why.

TWO WRONG ANSWERS WERE TRIED FIRST, and naming them is the point of this file.
Stranding the dependents turned one real failure into a batch that never ran —
measured at one failure and six untouched tickets. Releasing the dependency and
letting them run anyway was worse in a subtler way: a prerequisite is BY
DEFINITION needed, so the dependents then fail for a reason that is not theirs,
and the run produces six misleading failures instead of one honest one.

The thing that actually wants retrying is the PREREQUISITE.
"""

import pytest

from blacksmith import workflow as wf
from blacksmith.dispatch import (
    MAX_REVIVALS,
    REVIVED_MARKER,
    STRANDED_MARKER,
    Dispatcher,
    DispatcherOpts,
)
from blacksmith.errors import Transient
from blacksmith.outcomes import Outcome
from blacksmith.tickets import RETURNED_MARKER, Comment, Ticket
from blacksmith.transcript import Recorder
from blacksmith.workflow import Dependency
from tests.conftest import settle
from tests.fake_platform import EPOCH, FakePlatform


class FakeHandler:
    def __init__(self, role: str = wf.ROLE_DEV) -> None:
        self.role = role
        self.seen: list[Ticket] = []

    def wants(self, ticket: Ticket) -> bool:
        return True

    async def handle(self, ticket, transcript):
        self.seen.append(ticket)
        return Outcome.SUCCESS, ""


def dispatcher(api, role: str = wf.ROLE_DEV):
    handler = FakeHandler(role)
    return (
        Dispatcher(
            api,
            Recorder(None, host="box-a"),
            handler,
            wf.routing(),
            DispatcherOpts(host="box-a", board_id="B-1", concurrency=1, max_attempts=3),
        ),
        handler,
    )


def ticket(ticket_id, status, *, depends_on=None, comments=None) -> Ticket:
    return Ticket(
        ticket_id=ticket_id,
        title=ticket_id,
        status=status,
        board_id="B-1",
        created_at=EPOCH,
        depends_on=depends_on or [],
        comments=comments or [],
    )


def claim_by(role: str, cid: str = "k1") -> Comment:
    from blacksmith.tickets import ClaimToken, format_claim

    return Comment(
        comment_id=cid,
        body=format_claim(ClaimToken(host="box-a", role=role, run_id="r")),
        created_at=EPOCH,
    )


def board_with_dead_prerequisite(
    api, *, blocker_status=wf.COL_BLOCKED, revivals=0, worked_by=wf.ROLE_DEV
):
    comments = [
        Comment(comment_id=f"r{i}", body=REVIVED_MARKER, created_at=EPOCH) for i in range(revivals)
    ]
    if worked_by:
        comments.append(claim_by(worked_by))
    api.add(ticket("T-0", blocker_status, comments=comments))
    api.add(
        ticket(
            "T-1",
            wf.COL_READY_FOR_DEV,
            depends_on=[Dependency(ticket_id="T-0", title="the foundation", status=blocker_status)],
        )
    )
    return api


# --- reviving the prerequisite -------------------------------------------


async def test_a_blocked_prerequisite_is_sent_back_because_work_depends_on_it():
    """Its failure is more consequential than an ordinary ticket's precisely
    because other work rests on it, and it is the only ticket whose success
    unblocks the rest."""
    api = board_with_dead_prerequisite(FakePlatform())
    d, _ = dispatcher(api)
    await d.fill()
    assert api.status_of("T-0") == wf.COL_READY_FOR_DEV


async def test_a_revived_prerequisite_gets_a_fresh_budget():
    """Using the same mechanism a review rejection uses: a return marker restarts
    the count."""
    api = board_with_dead_prerequisite(FakePlatform())
    d, _ = dispatcher(api)
    await d.fill()
    explained = "\n".join(api.bodies_on("T-0"))
    assert RETURNED_MARKER in explained


async def test_the_revival_says_which_ticket_is_waiting_and_why():
    """Every refusal must be actionable. A message that names the symptom and not
    the cause sends a correct reader to the wrong place."""
    api = board_with_dead_prerequisite(FakePlatform())
    d, _ = dispatcher(api)
    await d.fill()
    explained = "\n".join(api.bodies_on("T-0"))
    assert "T-1" in explained and REVIVED_MARKER in explained


async def test_a_prerequisite_goes_back_to_its_own_queue_not_the_revivers():
    """Measured: a blocked TASK was revived by a SECTION dispatcher into
    ready_for_spec, where the section author claimed it, read a hundred times and
    was killed. A task belongs to the stage that develops it; a section to the
    stage that writes it. Its own claim history is what says which."""
    api = FakePlatform()
    api.add(ticket("T-0", wf.COL_BLOCKED, comments=[claim_by(wf.ROLE_DEV)]))
    api.add(
        ticket(
            "T-9",
            wf.COL_READY_FOR_SPEC,
            depends_on=[Dependency(ticket_id="T-0", status=wf.COL_BLOCKED)],
        )
    )
    d, _ = dispatcher(api, role=wf.ROLE_SPEC)
    await d.fill()
    assert api.status_of("T-0") == wf.COL_READY_FOR_DEV


async def test_the_most_recent_stage_to_hold_it_is_the_one_it_goes_back_to():
    """A ticket that has been through several stages belongs to the last one that
    had it, not the first."""
    api = FakePlatform()
    api.add(
        ticket(
            "T-0",
            wf.COL_BLOCKED,
            comments=[claim_by(wf.ROLE_SPEC, "k1"), claim_by(wf.ROLE_DEV, "k2")],
        )
    )
    api.add(
        ticket(
            "T-1",
            wf.COL_READY_FOR_DEV,
            depends_on=[Dependency(ticket_id="T-0", status=wf.COL_BLOCKED)],
        )
    )
    d, _ = dispatcher(api)
    await d.fill()
    assert api.status_of("T-0") == wf.COL_READY_FOR_DEV


async def test_a_prerequisite_with_no_history_is_not_sent_somewhere_invented():
    """Nothing on the ticket says which stage it belongs to, and the reviver's own
    queue is a guess — the one that sent a task to a section author. Better to
    leave it where a person will see it and say plainly what is waiting."""
    api = board_with_dead_prerequisite(FakePlatform(), worked_by="")
    d, _ = dispatcher(api, role=wf.ROLE_SPEC)
    api.tickets["T-1"] = api.tickets["T-1"].with_(status=wf.COL_READY_FOR_SPEC)
    await d.fill()
    assert api.status_of("T-0") == wf.COL_BLOCKED
    assert api.status_of("T-1") == wf.COL_BLOCKED
    assert STRANDED_MARKER in "\n".join(api.bodies_on("T-1"))


async def test_a_prerequisite_that_moved_on_its_own_is_left_alone():
    """The listing is a snapshot. Reviving a ticket that is already running would
    take it away from the host working it."""
    api = FakePlatform()
    api.add(ticket("T-0", wf.COL_IN_DEV))
    api.add(
        ticket(
            "T-1",
            wf.COL_READY_FOR_DEV,
            depends_on=[Dependency(ticket_id="T-0", status=wf.COL_BLOCKED)],
        )
    )
    d, _ = dispatcher(api)
    await d.fill()
    assert api.status_of("T-0") == wf.COL_IN_DEV


async def test_a_prerequisite_that_cannot_be_read_is_not_guessed_at():
    api = board_with_dead_prerequisite(FakePlatform())
    api.fail_always["get_ticket"] = Transient("502")
    d, _ = dispatcher(api)
    await d.fill()
    assert api.status_of("T-0") == wf.COL_BLOCKED


# --- and when reviving does not work -------------------------------------


async def test_revival_is_bounded():
    """A prerequisite that cannot be built is a real answer too. Each revival
    costs a full attempt at every stage, and a foundation that has failed this
    many rounds is telling you something another will not."""
    api = board_with_dead_prerequisite(FakePlatform(), revivals=MAX_REVIVALS)
    d, _ = dispatcher(api)
    await d.fill()
    assert api.status_of("T-0") == wf.COL_BLOCKED


async def test_at_the_ceiling_the_waiting_work_is_put_in_front_of_a_person():
    """This is the original failure: candidates skipped every poll forever,
    looking queued while nothing was coming for them. A stalled queue reads
    exactly like a busy one."""
    api = board_with_dead_prerequisite(FakePlatform(), revivals=MAX_REVIVALS)
    d, _ = dispatcher(api)
    await d.fill()
    assert api.status_of("T-1") == wf.COL_BLOCKED


async def test_stranded_work_says_what_it_is_waiting_for():
    api = board_with_dead_prerequisite(FakePlatform(), revivals=MAX_REVIVALS)
    d, _ = dispatcher(api)
    await d.fill()
    explained = "\n".join(api.bodies_on("T-1"))
    assert STRANDED_MARKER in explained and "the foundation" in explained


async def test_a_blocker_with_no_title_is_named_by_its_id():
    api = FakePlatform()
    api.add(
        ticket(
            "T-0",
            wf.COL_BLOCKED,
            comments=[
                *[
                    Comment(comment_id=f"r{i}", body=REVIVED_MARKER, created_at=EPOCH)
                    for i in range(MAX_REVIVALS)
                ],
                claim_by(wf.ROLE_DEV),
            ],
        )
    )
    api.add(
        ticket(
            "T-1",
            wf.COL_READY_FOR_DEV,
            depends_on=[Dependency(ticket_id="T-0", status=wf.COL_BLOCKED)],
        )
    )
    d, _ = dispatcher(api)
    await d.fill()
    assert "T-0" in "\n".join(api.bodies_on("T-1"))


# --- what it must not do -------------------------------------------------


async def test_the_dependent_is_never_run_without_its_prerequisite():
    """A prerequisite is by definition needed. Letting the work that rests on it
    run anyway produces failures that are not its fault — six misleading ones
    instead of one honest one."""
    api = board_with_dead_prerequisite(FakePlatform())
    d, handler = dispatcher(api)
    await d.fill()
    await settle(5)
    assert handler.seen == []


async def test_a_conflicted_prerequisite_is_still_on_its_way_to_done():
    """That is the resolver's queue, not a dead end."""
    api = board_with_dead_prerequisite(FakePlatform(), blocker_status=wf.COL_CONFLICTED)
    d, _ = dispatcher(api)
    await d.fill()
    assert api.status_of("T-0") == wf.COL_CONFLICTED
    assert api.status_of("T-1") == wf.COL_READY_FOR_DEV


async def test_a_stage_that_does_not_gate_on_dependencies_does_not_sweep():
    """Only the stage that would otherwise have worked the ticket both sees it and
    knows its dependencies."""
    api = FakePlatform()
    api.add(ticket("T-0", wf.COL_BLOCKED))
    api.add(
        ticket(
            "T-1",
            wf.COL_READY_FOR_REVIEW,
            depends_on=[Dependency(ticket_id="T-0", status=wf.COL_BLOCKED)],
        )
    )
    d, handler = dispatcher(api, role=wf.ROLE_REVIEW)
    await d.fill()
    await settle(5)
    assert api.status_of("T-0") == wf.COL_BLOCKED
    assert [t.ticket_id for t in handler.seen] == ["T-1"]


async def test_healthy_work_is_untouched_by_the_sweep():
    api = FakePlatform()
    api.add(
        ticket(
            "T-1",
            wf.COL_READY_FOR_DEV,
            depends_on=[Dependency(ticket_id="T-0", status=wf.COL_DONE)],
        )
    )
    d, handler = dispatcher(api)
    await d.fill()
    await settle(5)
    assert [t.ticket_id for t in handler.seen] == ["T-1"]
