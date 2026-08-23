"""Test support.

`async def` tests run without a plugin. The harness itself has no runtime
dependencies and the test tier should not quietly acquire one either — a suite
that only runs where someone remembered to install an extra is a suite that stops
being run.
"""

import asyncio
import inspect

import pytest


@pytest.hookimpl(tryfirst=True)
def pytest_pyfunc_call(pyfuncitem):
    """Run a coroutine test on a fresh event loop."""
    test = pyfuncitem.obj
    if not inspect.iscoroutinefunction(test):
        return None
    kwargs = {name: pyfuncitem.funcargs[name] for name in pyfuncitem._fixtureinfo.argnames}
    asyncio.run(_with_cleanup(test(**kwargs)))
    return True


async def _with_cleanup(test):
    """Run the test, then stop whatever it left behind.

    A test that deliberately checks mid-flight state leaves a task running. Left
    to the loop's own teardown that surfaces as "Task was destroyed but it is
    pending" on some later, unrelated test — the noise that trains people to
    ignore the output.
    """
    try:
        await test
    finally:
        leftovers = [t for t in asyncio.all_tasks() if t is not asyncio.current_task()]
        for task in leftovers:
            task.cancel()
        await asyncio.gather(*leftovers, return_exceptions=True)


async def settle(times: int = 3) -> None:
    """Let every task that is ready to run, run.

    The alternative is sleeping, which turns a scheduling assertion into a timing
    one: it passes on a fast machine, fails under load, and gets "fixed" by
    raising the sleep until the test no longer checks anything.
    """
    for _ in range(times):
        await asyncio.sleep(0)
