"""The department: several stages on one host, sharing one board.

One dispatcher works one column. The department is what makes them a pipeline —
one wake between them, one shutdown, and the rule that a stage this host is not
running does not get a dispatcher polling a column nobody fills.
"""

import asyncio

import pytest

from blacksmith import workflow as wf
from blacksmith.config import load_config
from blacksmith.department import Department
from blacksmith.errors import Transient
from blacksmith.outcomes import Outcome
from blacksmith.tickets import Ticket
from blacksmith.transcript import Recorder
from tests.fake_platform import EPOCH, FakePlatform


class FakeHandler:
    def __init__(self, role: str, outcome: Outcome = Outcome.SUCCESS) -> None:
        self.role = role
        self.outcome = outcome
        self.seen: list[Ticket] = []

    def wants(self, ticket: Ticket) -> bool:
        return True

    async def handle(self, ticket, transcript):
        self.seen.append(ticket)
        return self.outcome, ""


def department(api, handlers, **env) -> Department:
    cfg = load_config(env={"AGENTS_HOST": "box-a", "AGENTS_POLL_SECONDS": "0.005", **env})
    return Department(cfg, api, Recorder(None, host="box-a"), handlers)


def queued(ticket_id: str, status: str) -> Ticket:
    return Ticket(ticket_id=ticket_id, title=ticket_id, status=status, created_at=EPOCH)


# --- what gets a dispatcher ----------------------------------------------


def test_one_dispatcher_per_handler():
    d = department(FakePlatform(), [FakeHandler(wf.ROLE_DEV), FakeHandler(wf.ROLE_REVIEW)])
    assert {disp.stage.role for disp in d.dispatchers} == {wf.ROLE_DEV, wf.ROLE_REVIEW}


def test_a_handler_for_a_stage_this_host_does_not_run_is_left_out():
    """Not an error — a host that turned coverage off should still start. But a
    dispatcher polling a column no stage fills would poll forever and report
    nothing, which reads as a broken stage rather than an absent one."""
    d = department(
        FakePlatform(), [FakeHandler(wf.ROLE_DEV), FakeHandler(wf.ROLE_COVERAGE)], AGENTS_COVERAGE="0"
    )
    assert {disp.stage.role for disp in d.dispatchers} == {wf.ROLE_DEV}
    assert wf.ROLE_COVERAGE in d.skipped


def test_a_handler_for_a_role_no_pipeline_has_is_a_configuration_error():
    """A typo in a role name is otherwise a stage that silently never runs."""
    with pytest.raises(KeyError):
        department(FakePlatform(), [FakeHandler("dev-agnet")])


def test_the_columns_the_host_will_work_are_reportable_before_it_starts():
    d = department(FakePlatform(), [FakeHandler(wf.ROLE_DEV), FakeHandler(wf.ROLE_REVIEW)])
    assert d.columns() == [wf.COL_READY_FOR_DEV, wf.COL_READY_FOR_REVIEW]


# --- running them together ------------------------------------------------


async def test_every_stage_works_its_own_column():
    api = FakePlatform()
    api.add(queued("T-dev", wf.COL_READY_FOR_DEV))
    api.add(queued("T-rev", wf.COL_READY_FOR_REVIEW))
    dev, review = FakeHandler(wf.ROLE_DEV), FakeHandler(wf.ROLE_REVIEW)
    d = department(api, [dev, review])

    task = asyncio.create_task(d.run())
    await asyncio.sleep(0.05)
    await d.aclose()
    await task

    assert [t.ticket_id for t in dev.seen] == ["T-dev"]
    assert [t.ticket_id for t in review.seen] == ["T-rev"]


async def test_a_hand_off_reaches_the_next_stage_without_waiting_for_a_timer():
    """The stages share one wake, which is the whole reason the department is a
    single process rather than several."""
    api = FakePlatform()
    api.add(queued("T-1", wf.COL_READY_FOR_DEV))
    dev, review = FakeHandler(wf.ROLE_DEV), FakeHandler(wf.ROLE_REVIEW)
    d = department(api, [dev, review], AGENTS_COVERAGE="0", AGENTS_POLL_SECONDS="30")

    task = asyncio.create_task(d.run())
    await asyncio.sleep(0.05)
    await d.aclose()
    await task

    assert [t.ticket_id for t in review.seen] == ["T-1"], "the hand-off waited for the poll timer"


async def test_one_failing_stage_does_not_take_the_others_down():
    """A department where one broken column stops the board is worse than one
    where a single stage is quiet."""
    api = FakePlatform()
    api.add(queued("T-rev", wf.COL_READY_FOR_REVIEW))

    class Exploding(FakeHandler):
        async def handle(self, ticket, transcript):
            raise Transient("the model went away")

    review = FakeHandler(wf.ROLE_REVIEW)
    d = department(api, [Exploding(wf.ROLE_DEV), review])
    task = asyncio.create_task(d.run())
    await asyncio.sleep(0.05)
    await d.aclose()
    await task
    assert [t.ticket_id for t in review.seen] == ["T-rev"]


async def test_closing_stops_every_stage():
    api = FakePlatform()
    d = department(api, [FakeHandler(wf.ROLE_DEV), FakeHandler(wf.ROLE_REVIEW)])
    task = asyncio.create_task(d.run())
    await asyncio.sleep(0.02)
    await d.aclose()
    await asyncio.wait_for(task, timeout=1)
    assert all(disp.in_flight == 0 for disp in d.dispatchers)


async def test_a_department_with_no_handlers_starts_and_stops():
    """A host may be configured for a stage it has no model for yet."""
    d = department(FakePlatform(), [])
    task = asyncio.create_task(d.run())
    await asyncio.sleep(0.01)
    await d.aclose()
    await asyncio.wait_for(task, timeout=1)
