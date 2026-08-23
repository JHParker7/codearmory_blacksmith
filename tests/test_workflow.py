"""The routing table: which column a stage takes from, and where work goes next.

These tests are the specification for `blacksmith.workflow`. They are written
against the BOARD as the workflow — a stage is a function from column to column
— because that is the property the whole pipeline rests on: a ticket that leaves
one stage must land in a column some other stage is polling, or it stops moving
and nothing says why.
"""

import pytest

from blacksmith import workflow as wf


# --- the board ------------------------------------------------------------


def test_every_column_a_stage_names_is_on_the_board():
    """A stage that routes to a column the board does not have is a ticket that
    vanishes: it lands somewhere no person sees and no dispatcher polls."""
    on_board = {c.value for c in wf.WORKFLOW_COLUMNS}
    for stage in wf.routing().stages():
        for edge, column in stage.destinations().items():
            assert column in on_board, f"{stage.role}.{edge} -> {column!r} is not a column"


def test_column_values_are_unique():
    values = [c.value for c in wf.WORKFLOW_COLUMNS]
    assert len(values) == len(set(values))


def test_board_order_is_the_list_order():
    """Position is the index, so reordering the list reorders the board."""
    assert [c.value for c in wf.WORKFLOW_COLUMNS][:2] == [wf.COL_INBOX, wf.COL_DESIGNING]


# --- the pipeline shape ---------------------------------------------------


def test_no_two_stages_take_from_the_same_column():
    """Two stages polling one column race for every ticket in it, and the claim
    makes that race destructive: whichever host wins, the other never sees the
    ticket again."""
    seen: dict[str, str] = {}
    for stage in wf.routing().stages():
        assert stage.ready not in seen, (
            f"{stage.role} and {seen.get(stage.ready)} both take from {stage.ready}"
        )
        seen[stage.ready] = stage.role


def test_success_edges_terminate_from_every_stage():
    """Follow Success from any stage and you arrive at a column where work rests.
    A cycle here is a ticket that circulates forever looking busy."""
    r = wf.routing()
    for start in r.stages():
        seen, column, hops = set(), start.success, 0
        while column not in wf.TERMINAL_COLUMNS:
            assert column not in seen, f"success loop from {start.role} at {column}"
            seen.add(column)
            stage = r.stage_owning_queue(column)
            assert stage is not None, f"{column} has no stage to move it on"
            column = stage.success
            hops += 1
            assert hops < 20, f"success chain from {start.role} does not settle"


def test_the_test_author_runs_before_the_developer():
    """The load-bearing order. An agent that writes both the change and its test
    writes the test its change already passes."""
    r = wf.routing()
    assert r.stage_for(wf.ROLE_TEST).success == wf.COL_READY_FOR_DEV
    assert r.stage_for(wf.ROLE_DEV).ready == wf.COL_READY_FOR_DEV


def test_the_developer_can_hand_a_broken_spec_back():
    """The developer may not edit tests, so a specification that does not compile
    is the one failure it can neither fix nor route around."""
    assert wf.routing().stage_for(wf.ROLE_DEV).returns == wf.COL_READY_FOR_SPEC


def test_scoping_success_is_tracking_not_ready_for_dev():
    """The parent request is not work; its children are. Pushing the parent
    forward puts a developer on 'add rate limiting', the whole request."""
    assert wf.routing().stage_for(wf.ROLE_SCOPING).success == wf.COL_TRACKING


def test_stages_that_start_work_wait_for_dependencies():
    r = wf.routing()
    for role in (wf.ROLE_TEST, wf.ROLE_SPEC, wf.ROLE_SPEC_MERGE, wf.ROLE_DEV):
        assert r.stage_for(role).requires_dependencies, role


def test_review_and_integration_do_not_wait_for_dependencies():
    """By the time work reaches review its prerequisites were already gated at
    development; re-gating only strands work behind a blocker it does not use."""
    r = wf.routing()
    for role in (wf.ROLE_REVIEW, wf.ROLE_INTEGRATE, wf.ROLE_RESOLVE):
        assert not r.stage_for(role).requires_dependencies, role


def test_the_only_columns_nothing_collects_from_are_the_terminal_ones():
    """Every other column has a stage that polls it. A column nothing polls is
    where tickets go to be forgotten — and a stalled queue reads exactly like a
    busy one, so this is the failure that costs the most to notice.

    Three columns are terminal, and each for a different reason: done is merged,
    blocked means a person is needed, and tracking is a request whose work now
    lives in its children — the parent stays visible but is not itself work.
    """
    r = wf.routing()
    polled = {s.ready for s in r.stages()}
    forward = {c for s in r.stages() for c in (s.success, s.exhausted, s.returns) if c}
    assert wf.TERMINAL_COLUMNS == {wf.COL_DONE, wf.COL_BLOCKED, wf.COL_TRACKING}
    for column in forward - polled:
        assert column in wf.TERMINAL_COLUMNS, f"{column} has no stage polling it"


def test_a_stage_never_parks_work_in_another_stages_queue():
    """The working column is the claim. If it were some stage's Ready column,
    that stage would claim work already being held."""
    r = wf.routing()
    queues = {s.ready for s in r.stages()}
    for stage in r.stages():
        if stage.role == wf.ROLE_RESOLVE:
            continue  # shares the integrator's hold; see test below
        assert stage.working not in queues, f"{stage.role} parks work in {stage.working}"


def test_the_resolver_takes_from_conflicted_and_holds_in_integrating():
    """Conflicted is separate from blocked because it is actionable by an agent;
    one column for both would make the resolver take work it cannot do."""
    resolve = wf.routing().stage_for(wf.ROLE_RESOLVE)
    assert resolve.ready == wf.COL_CONFLICTED
    assert resolve.working == wf.COL_INTEGRATING
    assert resolve.returns == wf.COL_READY_FOR_INTEGRATION


# --- the settings that rewire it -----------------------------------------


def test_coverage_disabled_sends_the_developer_straight_to_review():
    """A host not running the coverage stage must not hand work to a column it
    is not polling."""
    r = wf.routing(coverage=False)
    assert r.stage_for(wf.ROLE_DEV).success == wf.COL_READY_FOR_REVIEW
    assert wf.ROLE_COVERAGE not in {s.role for s in r.stages()}


def test_coverage_enabled_inserts_the_stage_and_the_hand_off():
    r = wf.routing(coverage=True)
    assert r.stage_for(wf.ROLE_DEV).success == wf.COL_READY_FOR_COVERAGE
    assert r.stage_for(wf.ROLE_COVERAGE).ready == wf.COL_READY_FOR_COVERAGE


def test_coverage_exhaustion_moves_forward_rather_than_blocking():
    """Coverage is a quality goal, not a correctness gate. Blocking here would
    blackhole finished, working code over a percentage."""
    cov = wf.routing(coverage=True).stage_for(wf.ROLE_COVERAGE)
    assert cov.exhausted == wf.COL_READY_FOR_REVIEW
    assert cov.returns == wf.COL_READY_FOR_DEV


def test_no_architect_puts_the_product_manager_on_the_inbox():
    """A triage-only host has no repository to commit design docs into, so if the
    PM's queue were hard-wired behind the architect every request would sit in
    inbox with nothing coming to collect it."""
    r = wf.routing(architect=False)
    assert r.stage_for(wf.ROLE_SCOPING).ready == wf.COL_INBOX
    assert wf.ROLE_ARCHITECT not in {s.role for s in r.stages()}


def test_an_architect_moves_the_product_manager_behind_it():
    r = wf.routing(architect=True)
    assert r.stage_for(wf.ROLE_ARCHITECT).ready == wf.COL_INBOX
    assert r.stage_for(wf.ROLE_SCOPING).ready == wf.COL_READY_FOR_SCOPING


def test_a_stage_that_is_not_running_is_not_in_the_table_at_all():
    """An inert entry still claiming the inbox gives 'who works this column' two
    answers, and which one you get depends on iteration order."""
    r = wf.routing(architect=False)
    assert r.stage_working(wf.COL_DESIGNING) is None
    assert r.stage_owning_queue(wf.COL_INBOX).role == wf.ROLE_SCOPING


def test_a_failed_design_still_gets_scoped():
    """Documentation is context, not a gate. Blocking here would make a weak or
    unreachable architect fatal to work that never needed one."""
    arch = wf.routing(architect=True).stage_for(wf.ROLE_ARCHITECT)
    assert arch.exhausted == wf.COL_READY_FOR_SCOPING


def test_routing_is_immutable():
    """The table used to be a global map mutated at startup. A table that changed
    under a running pipeline would move tickets to a column nothing polls."""
    r = wf.routing()
    with pytest.raises((AttributeError, TypeError)):
        r.stage_for(wf.ROLE_DEV).success = wf.COL_BLOCKED  # type: ignore[misc]


def test_stage_for_an_unknown_role_raises_rather_than_returning_a_default():
    """A handler with no routing entry would poll a column that does not exist
    and silently never work."""
    with pytest.raises(KeyError):
        wf.routing().stage_for("no-such-agent")


def test_working_columns_are_derived_from_the_table():
    """Listed separately, a stage added to the table gets forgotten here."""
    r = wf.routing(coverage=True, architect=True)
    assert r.is_working_column(wf.COL_IN_DEV)
    assert r.is_working_column(wf.COL_DESIGNING)
    assert not r.is_working_column(wf.COL_READY_FOR_DEV)
    assert not r.is_working_column(wf.COL_DONE)


# --- dependency gating ----------------------------------------------------


def dep(status: str, ticket_id: str = "T-1", title: str = "") -> wf.Dependency:
    return wf.Dependency(ticket_id=ticket_id, title=title, status=status)


def test_dependencies_met_when_every_blocker_is_done():
    assert wf.dependencies_met([dep(wf.COL_DONE), dep(wf.COL_DONE)])


def test_no_dependencies_is_met():
    assert wf.dependencies_met([])


def test_a_blocker_short_of_done_is_not_met():
    assert not wf.dependencies_met([dep(wf.COL_DONE), dep(wf.COL_IN_REVIEW)])


def test_an_invisible_blocker_counts_as_unmet():
    """The tickets service returns an empty status for a blocker this account
    cannot see. Waiting means a person has to look; proceeding means a branch
    built on something that may not exist."""
    assert not wf.dependencies_met([dep("")])


def test_a_blocked_blocker_is_a_deadlock_not_a_wait():
    """ColBlocked is terminal without being done, so a ticket behind one is not
    queued, it is stranded — and a stalled queue reads exactly like a busy one."""
    blocker = dep(wf.COL_BLOCKED, "T-9", "the foundation")
    assert wf.dependency_deadlocked([dep(wf.COL_DONE), blocker]) == blocker


def test_a_conflicted_blocker_is_not_a_deadlock():
    """That is the resolver's queue, so it is still on its way to done."""
    assert wf.dependency_deadlocked([dep(wf.COL_CONFLICTED)]) is None


def test_deadlock_returns_the_dependency_not_a_label():
    """An earlier version returned the blocker's title, which reads well in a
    comment and is useless to a caller that has to act on the blocker."""
    found = wf.dependency_deadlocked([dep(wf.COL_BLOCKED, "T-9", "the foundation")])
    assert found is not None and found.ticket_id == "T-9"


def test_blocked_by_lists_only_the_unfinished():
    unmet = wf.blocked_by([dep(wf.COL_DONE, "T-1"), dep(wf.COL_IN_DEV, "T-2")])
    assert [d.ticket_id for d in unmet] == ["T-2"]


def test_a_blocker_is_named_by_title_and_falls_back_to_its_id():
    assert wf.blocker_name(dep(wf.COL_BLOCKED, "T-9", "the foundation")) == "the foundation"
    assert wf.blocker_name(dep(wf.COL_BLOCKED, "T-9", "")) == "T-9"
