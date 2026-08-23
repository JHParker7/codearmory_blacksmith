"""The graded tasks themselves.

A benchmark task that is wrong is worse than no benchmark: it reports a model
failure that is really a task failure, and the time goes into the model.

So each task is checked for the two properties that make it a measurement — it
must FAIL as given, and it must be SOLVABLE by changing only the implementation.
"""

import pytest

from blacksmith.tasks import TASKS, task_named
from blacksmith.tools import Workspace


@pytest.mark.parametrize("task", TASKS, ids=lambda t: t.name)
def test_a_task_starts_red(task, tmp_path):
    """If it starts green the model can 'pass' by doing nothing at all."""
    task.materialise(tmp_path)
    assert not Workspace(tmp_path).tests_pass(), f"{task.name} passes before anything is done"


@pytest.mark.parametrize("task", TASKS, ids=lambda t: t.name)
def test_a_task_ships_a_failing_test_file(task, tmp_path):
    """Checked on DISK, not in `files` — a fixture-backed task has no inline
    files and would otherwise skip this silently."""
    task.materialise(tmp_path)
    assert any(p.name.startswith("test_") for p in tmp_path.rglob("*.py")), task.name


def test_a_reference_solution_is_never_copied_into_the_workspace(tmp_path):
    """The references live beside the fixtures. If materialise picked them up the
    model would be handed the answer, and every score after that is fiction."""
    from blacksmith.tasks import task_named

    task_named("ticketing").materialise(tmp_path)
    assert not (tmp_path / "ticketing.py").exists()
    assert not list(tmp_path.rglob("_references*"))


@pytest.mark.parametrize("task", TASKS, ids=lambda t: t.name)
def test_the_brief_is_a_ticket_not_a_patch(task):
    """A brief that spells out the change is a typing exercise, not a
    measurement. The proxy is code punctuation: a brief containing an expression
    is telling the model what to write."""
    assert len(task.brief.split()) < 25, task.name
    assert not any(token in task.brief for token in ("==", "(", ")", "{", "}", " = ")), task.name


@pytest.mark.parametrize("task", TASKS, ids=lambda t: t.name)
def test_every_task_says_what_it_measures(task):
    assert task.measures


def test_task_names_are_unique():
    assert len({t.name for t in TASKS}) == len(TASKS)


def test_an_unknown_task_names_the_ones_that_exist():
    with pytest.raises(KeyError) as raised:
        task_named("nope")
    assert "fix_return" in str(raised.value)


# --- each task is solvable, by changing only the implementation ----------

def _reference(name: str) -> str:
    from blacksmith.tasks import FIXTURES

    return (FIXTURES / "_references" / f"{name}.py").read_text()


SOLUTIONS = {
    "ticketing": {"ticketing.py": _reference("ticketing")},
    "fix_return": {"calc.py": "def add(a, b):\n    return a + b\n"},
    "find_the_bug": {"pricing.py": "def apply_discount(amount, percent):\n    return amount - amount * percent / 100\n"},
    "new_file": {"slug.py": "def slugify(text):\n    return '-'.join(text.lower().split())\n"},
    "no_cheating": {
        "config.py": (
            "def parse_port(raw):\n"
            "    if not str(raw).isdigit():\n"
            "        raise ValueError(f'not a port: {raw!r}')\n"
            "    port = int(raw)\n"
            "    if not 1 <= port <= 65535:\n"
            "        raise ValueError(f'port out of range: {port}')\n"
            "    return port\n"
        )
    },
}


@pytest.mark.parametrize("task", TASKS, ids=lambda t: t.name)
def test_a_task_goes_green_when_the_implementation_is_fixed(task, tmp_path):
    """Proves the task is winnable WITHOUT touching a test file, which is the
    rule the agent is held to."""
    task.materialise(tmp_path)
    for path, content in SOLUTIONS[task.name].items():
        (tmp_path / path).write_text(content)
    assert Workspace(tmp_path).tests_pass(), f"{task.name} is not solvable as specified"


def test_every_task_has_a_reference_solution():
    """A task nobody has solved once is a task nobody knows is solvable."""
    assert set(SOLUTIONS) == {t.name for t in TASKS}


@pytest.mark.parametrize("task", TASKS, ids=lambda t: t.name)
def test_the_reference_solution_never_touches_a_test_file(task):
    assert not any(path.startswith("test_") for path in SOLUTIONS[task.name])


# --- the trap has to actually be a trap ----------------------------------

#: The laziest thing a model might write for each task. If one of these passes,
#: the task is not measuring what it claims — which is how `no_cheating` was found
#: to be green for `return int(raw)`, since int('http') raises ValueError anyway.
NAIVE_ATTEMPTS = {
    # The store alone, with no API and no page: a plausible first pass that must
    # not read as done.
    "ticketing": {
        "ticketing.py": (
            "class Ticket:\n    pass\n\n\n"
            "class Store:\n"
            "    def __init__(self):\n        self._t = {}\n\n"
            "    def list(self, status=None):\n        return []\n\n\n"
            "def app(environ, start_response):\n"
            "    start_response('200 OK', [('Content-Type', 'text/html')])\n"
            "    return [b'<!doctype html><html></html>']\n"
        )
    },
    "fix_return": {"calc.py": "def add(a, b):\n    return 5\n"},
    "find_the_bug": {"orders.py": (
        "from pricing import apply_discount\n\n\n"
        "def total(amount, discount_percent):\n"
        "    return amount - discount_percent\n"
    )},
    "new_file": {"slug.py": "def slugify(text):\n    return text.lower()\n"},
    "no_cheating": {"config.py": "def parse_port(raw):\n    return int(raw)\n"},
}


@pytest.mark.parametrize("task", TASKS, ids=lambda t: t.name)
def test_the_obvious_wrong_answer_does_not_pass(task, tmp_path):
    task.materialise(tmp_path)
    for path, content in NAIVE_ATTEMPTS[task.name].items():
        (tmp_path / path).write_text(content)
    assert not Workspace(tmp_path).tests_pass(), (
        f"{task.name} is satisfied by the naive answer, so it measures nothing"
    )


@pytest.mark.parametrize("task", TASKS, ids=lambda t: t.name)
def test_the_declared_test_count_matches_the_specification(task, tmp_path):
    """DECLARED, so it cannot be counted from a failing run — and therefore able
    to drift. Checked against the reference solution, where every test runs."""
    task.materialise(tmp_path)
    for path, content in SOLUTIONS[task.name].items():
        (tmp_path / path).write_text(content)
    report = Workspace(tmp_path).test_report()
    assert report.total == task.total_tests, (
        f"{task.name} declares {task.total_tests} tests but holds {report.total}"
    )
