"""The cross-stage edge trigger.

Dispatchers hand work to each other through a database column, and each has only
its own poll timer to discover that a hand-off happened. A ticket finishing one
stage then waits up to a full poll interval before the next stage notices —
several times per ticket, since the pipeline has several hand-offs, which at a
15s interval is around 22 seconds of idle GPU per ticket doing nothing but
waiting.

Deliberately NOT a message broker: the thing being coordinated is two tasks in
one address space, and a broker would put a network hop and a second ordering
authority between them when the ticket store is already the only arbiter of who
owns a ticket. Also not a webhook, which would need an inbound listener on the
agent host and give up the pull-only egress posture.
"""

import asyncio

from blacksmith.wake import Wake
from tests.conftest import settle


async def test_a_signal_wakes_a_subscriber():
    wake = Wake()
    woken = wake.subscribe()
    task = asyncio.create_task(woken.wait())
    await settle()
    assert not task.done()
    wake.signal()
    await asyncio.wait_for(task, timeout=1)


async def test_every_subscriber_is_woken():
    """One stage moving a ticket may make work for any of the others."""
    wake = Wake()
    subs = [wake.subscribe() for _ in range(3)]
    wake.signal()
    for sub in subs:
        await asyncio.wait_for(sub.wait(), timeout=1)


async def test_a_signal_that_arrives_first_is_not_lost():
    """The hand-off happens while the next stage is still working the previous
    ticket. A wake that only reaches a subscriber already parked at the door
    would be dropped exactly when the pipeline is busy."""
    wake = Wake()
    woken = wake.subscribe()
    wake.signal()
    await asyncio.wait_for(woken.wait(), timeout=1)


async def test_waiting_consumes_the_signal():
    wake = Wake()
    woken = wake.subscribe()
    wake.signal()
    await woken.wait()
    task = asyncio.create_task(woken.wait())
    await settle()
    assert not task.done(), "the same wake fired twice"
    task.cancel()


async def test_two_signals_before_a_wait_are_one_wake():
    """A wake already pending is as good as two: the subscriber's response is to
    poll, and it will see everything either signal was about."""
    wake = Wake()
    woken = wake.subscribe()
    wake.signal()
    wake.signal()
    await woken.wait()
    task = asyncio.create_task(woken.wait())
    await settle()
    assert not task.done()
    task.cancel()


async def test_signalling_never_blocks_on_a_slow_subscriber():
    """The signaller is a stage finishing a ticket. Making that wait on a
    subscriber would trade the latency this removes for a worse one."""
    wake = Wake()
    wake.subscribe()  # never waited on
    for _ in range(1000):
        wake.signal()  # returns, rather than filling a buffer and stalling


async def test_a_subscriber_that_never_waits_costs_nothing():
    wake = Wake()
    wake.subscribe()
    other = wake.subscribe()
    wake.signal()
    await asyncio.wait_for(other.wait(), timeout=1)


def test_signalling_with_no_subscribers_is_a_no_op():
    """A single-stage host has nobody to tell."""
    Wake().signal()
