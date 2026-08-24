"""Taking exclusive ownership of a ticket.

THE COLUMN IS THE CLAIM. A ticket in a stage's Ready column is unheld by
definition, and taking it means moving it out — so the conditional write IS the
arbitration, not a lock around it. Two hosts that both read the ticket in Ready
both try to move it, and the version check means only one succeeds.

The fallback exists for instances with no If-Match, where a conditional write
degrades to last-write-wins and both hosts would believe they won.
"""

import asyncio

import pytest

from blacksmith import workflow as wf
from blacksmith.errors import Conflict, NotFound, Transient
from blacksmith.platform import claim, move_to
from blacksmith.tickets import ClaimToken, Ticket, claimed_by, parse_claim
from tests.fake_platform import FakePlatform

DEV = wf.routing().stage_for(wf.ROLE_DEV)


def token(host: str = "box-a") -> ClaimToken:
    return ClaimToken(host=host, role=wf.ROLE_DEV, run_id=f"T-1@{host}")


def queued(status: str = wf.COL_READY_FOR_DEV) -> Ticket:
    return Ticket(ticket_id="T-1", title="add rate limiting", status=status)


# --- the conditional-write path -------------------------------------------


async def test_a_won_claim_moves_the_ticket_out_of_its_queue():
    api = FakePlatform()
    api.add(queued())
    await claim(api, "T-1", DEV, token())
    assert api.status_of("T-1") == wf.COL_IN_DEV


async def test_a_won_claim_names_the_holder():
    """The agent account alone cannot express 'dev-agent on box-a', and one role
    on three machines is otherwise one identity for three actors."""
    api = FakePlatform()
    api.add(queued())
    await claim(api, "T-1", DEV, token())
    assert api.tickets["T-1"].assignee_id == "dev-agent@box-a"


async def test_a_won_claim_leaves_an_attempt_record():
    """The comment is the audit trail AND the attempt counter."""
    api = FakePlatform()
    api.add(queued())
    await claim(api, "T-1", DEV, token())
    claims = [parse_claim(b) for b in api.bodies_on("T-1")]
    assert [c for c in claims if c] == [token()]


async def test_a_ticket_not_in_the_queue_is_already_someone_elses():
    api = FakePlatform()
    api.add(queued(wf.COL_IN_DEV))
    with pytest.raises(Conflict):
        await claim(api, "T-1", DEV, token())


async def test_the_conflict_says_which_column_it_actually_found():
    """A message that names the symptom and not the cause sends a correct agent
    to the wrong place."""
    api = FakePlatform()
    api.add(queued(wf.COL_BLOCKED))
    with pytest.raises(Conflict) as raised:
        await claim(api, "T-1", DEV, token())
    assert wf.COL_BLOCKED in str(raised.value) and wf.COL_READY_FOR_DEV in str(raised.value)


async def test_two_hosts_racing_produce_exactly_one_winner():
    """Both read the ticket in Ready; the version is in the WHERE clause of the
    update, so there is no window between checking and writing."""
    api = FakePlatform()
    api.add(queued())
    results = await asyncio.gather(
        claim(api, "T-1", DEV, token("box-a")),
        claim(api, "T-1", DEV, token("box-b")),
        return_exceptions=True,
    )
    won = [r for r in results if not isinstance(r, Exception)]
    lost = [r for r in results if isinstance(r, Conflict)]
    assert len(won) == 1 and len(lost) == 1


async def test_a_lost_version_check_is_a_conflict_not_a_failure():
    """Expected, not exceptional. Treating a lost race as an error would make the
    dispatch loop noisy and eventually stall it."""
    api = FakePlatform()
    api.add(queued())

    async def steal(_target: str) -> None:
        api.before.pop("update_ticket", None)
        await move_to(api, "T-1", wf.COL_IN_DEV)

    api.before["update_ticket"] = steal
    with pytest.raises(Conflict):
        await claim(api, "T-1", DEV, token())


async def test_a_claim_on_a_ticket_that_is_gone_is_not_a_conflict():
    """404 and 403 are the same outcome for scheduling — the tickets service
    returns 404 for a ticket the caller cannot see, so existence does not leak —
    but neither is a race that retrying will win."""
    with pytest.raises(NotFound):
        await claim(FakePlatform(), "T-1", DEV, token())


async def test_a_claim_that_wins_but_cannot_record_attribution_still_reports():
    """The comment is attribution, not arbitration, so failing to write it does
    not lose the claim — but it does cost the attempt counter an increment."""
    api = FakePlatform()
    api.add(queued())
    api.fail_always["add_comment"] = Transient("502")
    with pytest.raises(Exception) as raised:
        await claim(api, "T-1", DEV, token())
    assert "attribution" in str(raised.value)
    assert api.status_of("T-1") == wf.COL_IN_DEV, "the claim was lost along with the comment"


# --- the append fallback --------------------------------------------------


async def test_without_a_conditional_write_the_oldest_claim_wins():
    """Comments are append-only with server-assigned ordering: every host
    appends, reads back, and yields unless the oldest claim is its own."""
    api = FakePlatform(supports_if_match=False)
    api.add(queued())
    await claim(api, "T-1", DEV, token("box-a"))
    with pytest.raises(Conflict):
        await claim(api, "T-1", DEV, token("box-b"))


async def test_the_loser_of_an_append_race_does_not_take_the_ticket():
    api = FakePlatform(supports_if_match=False)
    api.add(queued())
    await claim(api, "T-1", DEV, token("box-a"))
    api.tickets["T-1"] = api.tickets["T-1"].with_(status=wf.COL_READY_FOR_DEV)  # as if released
    with pytest.raises(Conflict):
        await claim(api, "T-1", DEV, token("box-b"))
    assert api.tickets["T-1"].assignee_id == "dev-agent@box-a"


async def test_the_append_race_arbitrates_within_one_role_only():
    """Comparing against every claim means losing to the stage before: the
    product manager's claim is older than the developer's and always wins, so the
    developer never gets a ticket the product manager has scoped."""
    api = FakePlatform(supports_if_match=False)
    api.add(queued())
    await api.add_comment(
        "T-1", _claim_body(ClaimToken(host="box-a", role=wf.ROLE_SCOPING, run_id="r0"))
    )
    await claim(api, "T-1", DEV, token("box-b"))
    assert api.status_of("T-1") == wf.COL_IN_DEV


async def test_a_claim_that_cannot_be_verified_yields():
    """The claim is written but unverifiable. A ticket nobody picks up is retried
    next poll, whereas two hosts both proceeding is unrecoverable."""
    api = FakePlatform(supports_if_match=False)
    api.add(queued())
    api.fail_always["get_ticket"] = Transient("502")
    with pytest.raises(Exception):
        await claim(api, "T-1", DEV, token())
    assert api.status_of("T-1") == wf.COL_READY_FOR_DEV


async def test_a_claim_missing_from_the_read_back_yields():
    """Replica lag or a deletion. Same reasoning: yield."""
    api = FakePlatform(supports_if_match=False)
    api.add(queued())

    async def swallow(ticket_id: str) -> None:
        api.tickets[ticket_id] = api.tickets[ticket_id].with_(comments=[])

    api.before["get_ticket"] = swallow
    with pytest.raises(Conflict):
        await claim(api, "T-1", DEV, token())
    assert api.status_of("T-1") == wf.COL_READY_FOR_DEV


async def test_the_fallback_is_chosen_by_the_absence_of_an_etag():
    """Not by configuration. An instance is discovered, not declared, so a host
    pointed at an older one degrades on its own."""
    api = FakePlatform(supports_if_match=False)
    api.add(queued())
    await claim(api, "T-1", DEV, token())
    names = [name for name, _ in api.calls]
    assert "add_comment" in names and names.index("add_comment") < names.index("update_ticket")


# --- handing a ticket on -------------------------------------------------


async def test_moving_a_ticket_on_clears_the_assignee():
    """A ticket that moves on still carrying the last agent's name reads, on the
    board, as though that agent is still working it — and the next stage's claim
    would then be the second thing to write the field rather than the first."""
    api = FakePlatform()
    api.add(queued())
    await claim(api, "T-1", DEV, token())
    await move_to(api, "T-1", wf.COL_READY_FOR_REVIEW)
    assert api.tickets["T-1"].assignee_id is None
    assert api.status_of("T-1") == wf.COL_READY_FOR_REVIEW


async def test_a_moved_on_ticket_is_no_longer_held():
    api = FakePlatform()
    api.add(queued())
    await claim(api, "T-1", DEV, token())
    r = wf.routing()
    assert claimed_by(api.tickets["T-1"], r) is not None
    await move_to(api, "T-1", wf.COL_READY_FOR_REVIEW)
    assert claimed_by(api.tickets["T-1"], r) is None


def _claim_body(tok: ClaimToken) -> str:
    from blacksmith.tickets import format_claim

    return format_claim(tok)
