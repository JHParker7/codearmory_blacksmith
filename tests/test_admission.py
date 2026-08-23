"""Admission control in front of one model class's serving slots.

The measurement this exists to exploit: a serving backend reaches its aggregate
throughput only while every slot is busy (~317 tok/s across 4 slots against ~160
for one), so the queue's job is to keep `slots` requests in flight and no more.
Oversubscribing is measurably worse than matching.
"""

import asyncio

import pytest

from blacksmith.admission import AdmissionQueue, Priority, QueueFull, parse_priority
from tests.conftest import settle


# --- priorities -----------------------------------------------------------


def test_priorities_order_low_to_critical():
    assert Priority.LOW < Priority.NORMAL < Priority.HIGH < Priority.CRITICAL


def test_a_forgotten_priority_sorts_low():
    """The zero value must not be able to jump the queue."""
    assert Priority(0) is Priority.LOW


def test_ticket_labels_map_onto_admission_priorities():
    assert parse_priority("critical") is Priority.CRITICAL
    assert parse_priority("urgent") is Priority.CRITICAL
    assert parse_priority("p0") is Priority.CRITICAL
    assert parse_priority("high") is Priority.HIGH
    assert parse_priority("p1") is Priority.HIGH
    assert parse_priority("normal") is Priority.NORMAL
    assert parse_priority("medium") is Priority.NORMAL
    assert parse_priority("p2") is Priority.NORMAL
    assert parse_priority("low") is Priority.LOW


def test_an_unrecognised_label_sorts_low():
    """An unknown label must never be able to starve known work."""
    assert parse_priority("") is Priority.LOW
    assert parse_priority("blocker!!") is Priority.LOW


def test_priority_labels_round_trip():
    for p in Priority:
        assert parse_priority(str(p)) is p


# --- rule 3: fill the slots, and no more ---------------------------------


async def test_it_admits_up_to_the_slot_count():
    q = AdmissionQueue(slots=2, max_queue=8)
    async with q.slot(Priority.NORMAL), q.slot(Priority.NORMAL):
        assert q.in_flight == 2


async def test_the_slot_after_the_last_one_waits():
    q = AdmissionQueue(slots=1, max_queue=8)
    async with q.slot(Priority.NORMAL):
        waiter = asyncio.create_task(_hold(q, Priority.NORMAL))
        await settle()
        assert q.in_flight == 1 and q.waiting == 1
        assert not waiter.done()
    await settle()
    assert waiter.done()
    await waiter


async def test_releasing_admits_the_next_one():
    q = AdmissionQueue(slots=1, max_queue=8)
    order: list[str] = []
    async with q.slot(Priority.NORMAL):
        order.append("first")
        task = asyncio.create_task(_record(q, Priority.NORMAL, "second", order))
        await settle()
        assert order == ["first"]
    await task
    assert order == ["first", "second"]


async def test_highest_priority_goes_first():
    q = AdmissionQueue(slots=1, max_queue=8)
    order: list[str] = []
    async with q.slot(Priority.NORMAL):
        low = asyncio.create_task(_record(q, Priority.LOW, "low", order))
        high = asyncio.create_task(_record(q, Priority.HIGH, "high", order))
        await settle()
    await asyncio.gather(low, high)
    assert order == ["high", "low"]


async def test_within_a_priority_the_older_request_wins():
    """FIFO inside a band, so nothing starves behind a stream of equals."""
    q = AdmissionQueue(slots=1, max_queue=8)
    order: list[str] = []
    async with q.slot(Priority.NORMAL):
        first = asyncio.create_task(_record(q, Priority.NORMAL, "first", order))
        await settle()
        second = asyncio.create_task(_record(q, Priority.NORMAL, "second", order))
        await settle()
    await asyncio.gather(first, second)
    assert order == ["first", "second"]


# --- rules 1 and 2: a critical request takes the box ---------------------


async def test_nothing_else_is_admitted_while_a_critical_request_runs():
    """It gets the box. That is what critical means, and the point of it is that
    one stream is FASTER than a share of four."""
    q = AdmissionQueue(slots=4, max_queue=8)
    async with q.slot(Priority.CRITICAL):
        blocked = asyncio.create_task(_hold(q, Priority.HIGH))
        await settle()
        assert q.in_flight == 1 and not blocked.done()
    await blocked


async def test_a_waiting_critical_holds_the_door_shut():
    """In-flight work drains naturally and the critical one starts once the box is
    empty. New non-critical work must not refill the slots behind it or the
    critical request never starts."""
    q = AdmissionQueue(slots=2, max_queue=8)
    running = await q.acquire(Priority.NORMAL)
    critical = asyncio.create_task(_hold(q, Priority.CRITICAL))
    await settle()
    assert not critical.done()

    latecomer = asyncio.create_task(_hold(q, Priority.NORMAL))
    await settle()
    assert q.in_flight == 1, "a latecomer refilled a slot the critical request was draining for"

    running.release()
    await settle()
    assert critical.done(), "the box drained but the critical request did not start"
    await critical
    await latecomer


async def test_running_work_is_never_preempted():
    """The tokens already spent on in-flight work would be thrown away, and a
    half-written branch is worse than a late one."""
    q = AdmissionQueue(slots=2, max_queue=8)
    async with q.slot(Priority.NORMAL):
        critical = asyncio.create_task(_hold(q, Priority.CRITICAL))
        await settle()
        assert q.in_flight == 1 and not critical.done()
    await critical


async def test_the_box_reopens_once_the_critical_request_finishes():
    q = AdmissionQueue(slots=2, max_queue=8)
    async with q.slot(Priority.CRITICAL):
        pass
    async with q.slot(Priority.NORMAL), q.slot(Priority.NORMAL):
        assert q.in_flight == 2


# --- load shedding --------------------------------------------------------


async def test_a_full_queue_sheds_rather_than_waiting():
    """Load shedding, not failure: the caller leaves the ticket unclaimed so
    another host — or this one, later — picks it up."""
    q = AdmissionQueue(slots=1, max_queue=1)
    async with q.slot(Priority.NORMAL):
        queued = asyncio.create_task(_hold(q, Priority.NORMAL))
        await settle()
        with pytest.raises(QueueFull):
            await q.acquire(Priority.NORMAL)
    await queued


async def test_a_shed_request_does_not_stay_on_the_waiting_list():
    q = AdmissionQueue(slots=1, max_queue=1)
    async with q.slot(Priority.NORMAL):
        queued = asyncio.create_task(_hold(q, Priority.NORMAL))
        await settle()
        with pytest.raises(QueueFull):
            await q.acquire(Priority.NORMAL)
        assert q.waiting == 1
    await queued


# --- giving up ------------------------------------------------------------


async def test_a_cancelled_waiter_leaves_no_slot_behind():
    """A caller that goes away while queued never took a slot. Leaking one here
    shrinks the box by one for the life of the process."""
    q = AdmissionQueue(slots=1, max_queue=8)
    async with q.slot(Priority.NORMAL):
        waiter = asyncio.create_task(_hold(q, Priority.NORMAL))
        await settle()
        waiter.cancel()
        with pytest.raises(asyncio.CancelledError):
            await waiter
        assert q.waiting == 0
    async with q.slot(Priority.NORMAL):
        assert q.in_flight == 1


async def test_a_waiter_cancelled_as_it_is_admitted_gives_the_slot_back():
    """The race that leaks: cancellation and admission land together, and the
    caller holds a slot it will never use."""
    q = AdmissionQueue(slots=1, max_queue=8)
    holder = await q.acquire(Priority.NORMAL)
    waiter = asyncio.create_task(_hold(q, Priority.NORMAL))
    await settle()
    holder.release()  # admits the waiter...
    waiter.cancel()  # ...which is cancelled in the same tick
    with pytest.raises(asyncio.CancelledError):
        await waiter
    await settle()
    assert q.in_flight == 0, "a cancelled waiter kept the slot it was admitted to"
    async with q.slot(Priority.NORMAL):
        assert q.in_flight == 1


async def test_leaving_the_block_releases_even_when_the_body_raises():
    q = AdmissionQueue(slots=1, max_queue=8)
    with pytest.raises(RuntimeError):
        async with q.slot(Priority.NORMAL):
            raise RuntimeError("the model call failed")
    assert q.in_flight == 0


async def test_releasing_twice_is_a_no_op():
    """The common caller is an error path that also releases. Double release must
    not hand out a slot that does not exist."""
    q = AdmissionQueue(slots=1, max_queue=8)
    held = await q.acquire(Priority.NORMAL)
    held.release()
    held.release()
    assert q.in_flight == 0


# --- construction ---------------------------------------------------------


def test_a_zero_slot_queue_is_clamped_rather_than_deadlocking():
    """Deadlocking every caller is the worse of the two failures: it looks like a
    slow model rather than a misconfiguration."""
    q = AdmissionQueue(slots=0, max_queue=0)
    assert q.slots >= 1 and q.max_queue >= 1


async def test_slots_are_counted_down_and_back_up():
    q = AdmissionQueue(slots=2, max_queue=8)
    assert (q.in_flight, q.waiting) == (0, 0)
    async with q.slot(Priority.NORMAL):
        assert q.in_flight == 1
    assert q.in_flight == 0


# --- helpers --------------------------------------------------------------


async def _hold(q: AdmissionQueue, priority: Priority) -> None:
    async with q.slot(priority):
        await asyncio.sleep(0)


async def _record(q: AdmissionQueue, priority: Priority, name: str, into: list[str]) -> None:
    async with q.slot(priority):
        into.append(name)
        await asyncio.sleep(0)
