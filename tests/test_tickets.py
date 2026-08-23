"""Tickets, claims and the attempt log.

Comments are append-only with server-assigned ids and timestamps, which is what
makes them usable as both an audit trail and an attempt counter. These tests pin
the two rules that are easy to get subtly wrong: which claim wins a race, and
which claims still count against a ticket's budget.
"""

from datetime import datetime, timedelta, timezone

import pytest

from blacksmith import workflow as wf
from blacksmith.tickets import (
    CLAIM_MARKER,
    RETURNED_MARKER,
    STALE_CLAIM_AFTER,
    ClaimToken,
    Comment,
    Dependency,
    Ticket,
    attempts,
    claimed_by,
    format_claim,
    oldest_claim,
    parse_claim,
)

NOW = datetime(2026, 8, 21, 12, 0, tzinfo=timezone.utc)


def comment(body: str, *, cid: str = "c1", at: datetime | None = None) -> Comment:
    return Comment(comment_id=cid, ticket_id="T-1", author_id="dev-agent", body=body, created_at=at or NOW)


def claim(role: str, host: str = "box-a", *, cid: str = "c1", at: datetime | None = None) -> Comment:
    return comment(format_claim(ClaimToken(host=host, role=role, run_id="r1")), cid=cid, at=at)


def ticket(*comments: Comment, status: str = wf.COL_IN_DEV, **kw) -> Ticket:
    return Ticket(ticket_id="T-1", title="t", status=status, comments=list(comments), **kw)


# --- the claim token ------------------------------------------------------


def test_a_claim_round_trips():
    tok = ClaimToken(host="box-a", role=wf.ROLE_DEV, run_id="T-1@box-a")
    assert parse_claim(format_claim(tok)) == tok


def test_a_claim_is_an_html_comment():
    """It has to be matched exactly by code and it should not be something a
    person reads on the board."""
    body = format_claim(ClaimToken(host="box-a", role=wf.ROLE_DEV, run_id="r1"))
    assert body.startswith(CLAIM_MARKER) and body.endswith("-->")


def test_ordinary_prose_is_not_a_claim():
    for body in ("hello", CLAIM_MARKER + "not json -->", CLAIM_MARKER + '{"host":"a"}'):
        assert parse_claim(body) is None


# --- arbitration ----------------------------------------------------------


def test_the_oldest_claim_for_a_role_wins():
    """Every host appends and reads back; both apply the same deterministic rule
    to the same server-ordered list, so they agree even when one reads first."""
    late = claim(wf.ROLE_DEV, "box-b", cid="c2", at=NOW + timedelta(seconds=5))
    early = claim(wf.ROLE_DEV, "box-a", cid="c1", at=NOW)
    winner = oldest_claim([late, early], role=wf.ROLE_DEV)
    assert winner is not None and winner.comment_id == "c1"


def test_a_tie_on_time_breaks_on_comment_id():
    """Two hosts can land on the same timestamp. Without a total order they can
    reach different answers and both proceed."""
    a = claim(wf.ROLE_DEV, "box-a", cid="c2", at=NOW)
    b = claim(wf.ROLE_DEV, "box-b", cid="c1", at=NOW)
    winner = oldest_claim([a, b], role=wf.ROLE_DEV)
    assert winner is not None and winner.comment_id == "c1"


def test_arbitration_ignores_other_roles():
    """Comparing against every claim means losing to the stage before: the
    product manager's claim is older than the developer's and always wins, so
    the developer never gets a ticket the PM has scoped."""
    pm = claim(wf.ROLE_SCOPING, "box-a", cid="c1", at=NOW)
    dev = claim(wf.ROLE_DEV, "box-b", cid="c2", at=NOW + timedelta(minutes=5))
    winner = oldest_claim([pm, dev], role=wf.ROLE_DEV)
    assert winner is not None and winner.comment_id == "c2"


def test_no_claim_for_the_role_is_no_winner():
    assert oldest_claim([claim(wf.ROLE_SCOPING)], role=wf.ROLE_DEV) is None


# --- who holds a ticket ---------------------------------------------------


def test_the_column_decides_whether_a_ticket_is_held():
    """Nothing deletes comments, so a ticket carries every claim ever made
    against it. Reading 'claimed' off that history locks each stage out of the
    work the one before it just handed over."""
    r = wf.routing()
    held = ticket(claim(wf.ROLE_DEV), status=wf.COL_IN_DEV)
    handed_on = ticket(claim(wf.ROLE_DEV), status=wf.COL_READY_FOR_REVIEW)
    assert claimed_by(held, r) is not None
    assert claimed_by(handed_on, r) is None


def test_a_working_column_with_no_claim_comment_is_unattributed():
    assert claimed_by(ticket(status=wf.COL_IN_DEV), wf.routing()) is None


def test_the_holder_is_named_by_host_and_role():
    tok = claimed_by(ticket(claim(wf.ROLE_DEV, "box-a")), wf.routing())
    assert tok is not None and (tok.role, tok.host) == (wf.ROLE_DEV, "box-a")


# --- the attempt log ------------------------------------------------------


def test_attempts_counts_claims_by_this_role_only():
    t = ticket(
        claim(wf.ROLE_SCOPING, cid="c1"),
        claim(wf.ROLE_DEV, cid="c2"),
        claim(wf.ROLE_DEV, cid="c3"),
    )
    assert attempts(t, wf.ROLE_DEV, now=NOW) == 2
    assert attempts(t, wf.ROLE_SCOPING, now=NOW) == 1


def test_a_return_restarts_the_count():
    """The work waiting in the queue after a send-back is not the work already
    tried: it is a fix for a finding that did not exist when those attempts were
    spent. Counting across the return lets a ticket be sent back and then refused
    by the very stage asked to fix it."""
    t = ticket(
        claim(wf.ROLE_DEV, cid="c1", at=NOW),
        claim(wf.ROLE_DEV, cid="c2", at=NOW + timedelta(minutes=1)),
        comment("rejected " + RETURNED_MARKER, cid="c3", at=NOW + timedelta(minutes=2)),
        claim(wf.ROLE_DEV, cid="c4", at=NOW + timedelta(minutes=3)),
    )
    assert attempts(t, wf.ROLE_DEV, now=NOW + timedelta(minutes=4)) == 1


def test_a_return_restarts_the_count_for_every_role():
    """Measured on r68: a hand-back loop charged one dev attempt AND one spec
    attempt per round trip, and the dispatcher then refused to give the ticket to
    either — sitting in ready_for_dev with nothing in any counter to say why."""
    t = ticket(
        claim(wf.ROLE_SPEC, cid="c1", at=NOW),
        comment(RETURNED_MARKER, cid="c2", at=NOW + timedelta(minutes=1)),
        claim(wf.ROLE_SPEC, cid="c3", at=NOW + timedelta(minutes=2)),
    )
    assert attempts(t, wf.ROLE_SPEC, now=NOW + timedelta(minutes=3)) == 1


def test_comments_are_sorted_before_counting():
    """The whole meaning of the count depends on which side of the marker a claim
    falls, which is too much to rest on an undocumented ordering guarantee."""
    t = ticket(
        claim(wf.ROLE_DEV, cid="c4", at=NOW + timedelta(minutes=3)),
        claim(wf.ROLE_DEV, cid="c1", at=NOW),
        comment(RETURNED_MARKER, cid="c3", at=NOW + timedelta(minutes=2)),
    )
    assert attempts(t, wf.ROLE_DEV, now=NOW + timedelta(minutes=4)) == 1


def test_a_claim_older_than_the_stale_window_is_forgiven():
    """A claim is written when work starts, so a killed process leaves one behind
    that counts against the ceiling. Past this window its process is gone."""
    old = claim(wf.ROLE_DEV, cid="c1", at=NOW - STALE_CLAIM_AFTER - timedelta(minutes=1))
    fresh = claim(wf.ROLE_DEV, cid="c2", at=NOW)
    assert attempts(ticket(old, fresh), wf.ROLE_DEV, now=NOW) == 1


def test_a_claim_inside_the_stale_window_still_counts():
    recent = claim(wf.ROLE_DEV, cid="c1", at=NOW - STALE_CLAIM_AFTER + timedelta(minutes=1))
    assert attempts(ticket(recent), wf.ROLE_DEV, now=NOW) == 1


def test_a_timestamp_that_is_not_a_real_time_is_not_treated_as_old():
    """A zero or epoch value means the field was never populated. Reading that as
    'very old' would forgive every claim ever made."""
    unset = claim(wf.ROLE_DEV, cid="c1", at=datetime(1, 1, 1, tzinfo=timezone.utc))
    assert attempts(ticket(unset), wf.ROLE_DEV, now=NOW) == 1


def test_pruning_a_stale_claim_does_not_prune_the_return_marker():
    """Forgiving an abandoned claim must not also erase the reset that a hand-back
    recorded, or the count silently jumps back up."""
    t = ticket(
        claim(wf.ROLE_DEV, cid="c1", at=NOW - STALE_CLAIM_AFTER * 2),
        comment(RETURNED_MARKER, cid="c2", at=NOW - STALE_CLAIM_AFTER * 2 + timedelta(seconds=1)),
        claim(wf.ROLE_DEV, cid="c3", at=NOW),
    )
    assert attempts(t, wf.ROLE_DEV, now=NOW) == 1


def test_attempts_defaults_now_to_the_clock():
    """Callers that forget the window are the reason it is folded in here rather
    than left to a separate prune step at five call sites."""
    assert attempts(ticket(claim(wf.ROLE_DEV, at=datetime.now(timezone.utc))), wf.ROLE_DEV) == 1


# --- decoding what the platform sends ------------------------------------


def test_a_ticket_decodes_from_the_api_payload():
    t = Ticket.from_api(
        {
            "ticket_id": "T-1",
            "title": "add rate limiting",
            "description": "d",
            "status": "ready_for_dev",
            "priority": "high",
            "created_by": "someone",
            "version": 7,
            "depends_on": [{"ticket_id": "T-0", "title": "foundation", "status": "done"}],
            "comments": [
                {
                    "comment_id": "c1",
                    "ticket_id": "T-1",
                    "author_id": "dev-agent",
                    "body": "hi",
                    "created_at": "2026-08-21T12:00:00Z",
                }
            ],
            "created_at": "2026-08-21T11:00:00Z",
            "updated_at": "2026-08-21T12:00:00Z",
        }
    )
    assert t.ticket_id == "T-1"
    assert t.version == 7
    assert t.depends_on == [Dependency(ticket_id="T-0", title="foundation", status="done")]
    assert t.comments[0].created_at == NOW
    assert t.created_at is not None and t.created_at.tzinfo is not None


def test_missing_fields_decode_to_empty_rather_than_raising():
    """A tickets service that adds or drops an optional field must not take the
    department down."""
    t = Ticket.from_api({"ticket_id": "T-1"})
    assert (t.title, t.status, t.comments, t.depends_on) == ("", "", [], [])


def test_unknown_fields_are_ignored():
    assert Ticket.from_api({"ticket_id": "T-1", "invented_by_the_server": 1}).ticket_id == "T-1"


def test_naive_timestamps_are_read_as_utc():
    """Mixing naive and aware datetimes raises on comparison, which would take out
    the attempt count rather than the parse."""
    t = Ticket.from_api(
        {"ticket_id": "T-1", "comments": [{"comment_id": "c1", "created_at": "2026-08-21T12:00:00"}]}
    )
    assert t.comments[0].created_at == NOW


def test_a_ticket_is_immutable():
    """Handlers are handed the ticket they were dispatched for. One that mutated
    it would change what the dispatcher then reads to decide routing."""
    with pytest.raises((AttributeError, TypeError)):
        ticket().status = wf.COL_DONE  # type: ignore[misc]


def test_the_fixture_clock_is_never_inside_the_stale_window():
    """A guard against the bug this file's own window created elsewhere.

    A fixture timestamp fixed to a literal date drifts past STALE_CLAIM_AFTER as
    real time passes, and every claim built on it stops counting — silently, and
    in tests that never mention time. Caught once, in the afternoon, on unrelated
    work.
    """
    from tests.fake_platform import EPOCH

    age = datetime.now(timezone.utc) - EPOCH
    assert age < STALE_CLAIM_AFTER, (
        f"the fixture clock is {age} old, past the {STALE_CLAIM_AFTER} stale window — "
        "fixture claims are being forgiven and attempt-ceiling tests are meaningless"
    )
