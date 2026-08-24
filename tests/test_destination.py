"""Where a ticket goes when a stage finishes with it.

THE ONLY PLACE THAT DECIDES. Every stage reports WHAT HAPPENED and this reads the
routing table, so adding a column is one edit and no agent has to be taught about
it. An agent naming its own destination would be a second routing table, and the
two would disagree the day either changed.
"""

import pytest

from blacksmith import workflow as wf
from blacksmith.outcomes import Outcome

MAX = 3


def where(role: str, outcome: Outcome, *, spent: int = 0, max_attempts: int = MAX, **kw) -> str | None:
    return wf.routing(**kw).destination(role, outcome, attempts_spent=spent, max_attempts=max_attempts)


def test_success_advances_to_the_next_queue():
    assert where(wf.ROLE_REVIEW, Outcome.SUCCESS) == wf.COL_READY_FOR_INTEGRATION


def test_blocked_goes_straight_to_a_person():
    """No agent can carry this further, so spending the remaining attempts on a
    retry that cannot succeed only delays the person who has to look."""
    assert where(wf.ROLE_DEV, Outcome.BLOCKED, spent=0) == wf.COL_BLOCKED


def test_conflicted_goes_to_the_resolver_whichever_stage_reports_it():
    """Finished work that does not merge is the resolver's queue, not a failure of
    the stage reporting it."""
    assert where(wf.ROLE_INTEGRATE, Outcome.CONFLICTED) == wf.COL_CONFLICTED


def test_returned_goes_to_the_stage_that_has_to_look_again():
    assert where(wf.ROLE_REVIEW, Outcome.RETURNED) == wf.COL_READY_FOR_DEV
    assert where(wf.ROLE_DEV, Outcome.RETURNED) == wf.COL_READY_FOR_SPEC


def test_a_stage_that_cannot_return_work_sends_it_to_a_person():
    """Falling through to 'do not move it' would strand the ticket in the working
    column with nothing coming to collect it, so it goes where every other dead
    end goes."""
    assert where(wf.ROLE_TEST, Outcome.RETURNED) == wf.COL_BLOCKED


def test_handled_means_the_stage_placed_the_ticket_itself():
    """Rare and deliberate: the product manager sends a request that needs no
    breakdown straight to the developers, which is neither its success
    destination nor a failure. Without this the dispatcher's move would silently
    undo the handler's."""
    assert where(wf.ROLE_SCOPING, Outcome.HANDLED) is None


def test_abandoned_returns_the_work_to_its_queue_without_charging_the_ticket():
    """The host went away mid-task. That is not the agent getting it wrong."""
    assert where(wf.ROLE_DEV, Outcome.ABANDONED) == wf.COL_READY_FOR_DEV


def test_a_failure_with_attempts_left_goes_back_to_the_queue():
    assert where(wf.ROLE_DEV, Outcome.FAILED, spent=0) == wf.COL_READY_FOR_DEV


def test_the_attempt_just_spent_counts_against_the_ceiling():
    """The claim just made is not yet in the copy of the ticket read before the
    work, so the count seen here is one behind. Off by one and the last attempt is
    either never taken or taken twice."""
    assert where(wf.ROLE_DEV, Outcome.FAILED, spent=MAX - 2) == wf.COL_READY_FOR_DEV
    assert where(wf.ROLE_DEV, Outcome.FAILED, spent=MAX - 1) == wf.COL_BLOCKED


def test_an_exhausted_failure_stops_rather_than_cycling():
    """A ticket cycling through a queue it cannot leave looks like progress and is
    not."""
    assert where(wf.ROLE_DEV, Outcome.FAILED, spent=MAX) == wf.COL_BLOCKED


def test_exhaustion_moves_coverage_forward_not_into_the_blocked_column():
    r = wf.routing(coverage=True)
    assert r.destination(wf.ROLE_COVERAGE, Outcome.FAILED, attempts_spent=MAX, max_attempts=MAX) == (
        wf.COL_READY_FOR_REVIEW
    )
    assert r.destination(wf.ROLE_COVERAGE, Outcome.BLOCKED, attempts_spent=0, max_attempts=MAX) == (
        wf.COL_READY_FOR_REVIEW
    )


def test_exhaustion_moves_a_failed_design_forward_to_scoping():
    r = wf.routing(architect=True)
    assert r.destination(wf.ROLE_ARCHITECT, Outcome.FAILED, attempts_spent=MAX, max_attempts=MAX) == (
        wf.COL_READY_FOR_SCOPING
    )


def test_every_outcome_has_a_destination():
    """A new outcome that falls through the routing decision is a ticket left in a
    working column, which is the one failure nothing sweeps."""
    r = wf.routing()
    for outcome in Outcome:
        for stage in r.stages():
            got = r.destination(stage.role, outcome, attempts_spent=0, max_attempts=MAX)
            assert got is not None or outcome is Outcome.HANDLED, f"{stage.role}/{outcome}"


def test_a_destination_is_always_a_real_column():
    r = wf.routing(coverage=True, architect=True)
    on_board = {c.value for c in wf.WORKFLOW_COLUMNS}
    for outcome in Outcome:
        for stage in r.stages():
            for spent in (0, MAX):
                got = r.destination(stage.role, outcome, attempts_spent=spent, max_attempts=MAX)
                assert got is None or got in on_board


def test_an_unknown_role_raises_rather_than_guessing():
    with pytest.raises(KeyError):
        where("no-such-agent", Outcome.SUCCESS)


def test_outcomes_are_a_closed_set():
    """These become a metric label. Free text may never be one — an unbounded
    label is a cardinality bug."""
    assert {str(o) for o in Outcome} == {
        "success",
        "failed",
        "abandoned",
        "conflicted",
        "blocked",
        "returned",
        "handled",
    }
