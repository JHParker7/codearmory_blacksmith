"""Recording what a run cost, so a change can be shown to have helped.

A number printed to a terminal is not a baseline. To compare a harness change
against the one before it the run has to be WRITTEN DOWN, with enough context to
know what produced it: which model, which task, and which version of the harness.

That last one is the part people leave out and the part that matters. A score
with no harness version attached cannot be compared to anything, because you
cannot tell whether the model got better or the rig changed underneath it.
"""

import json

import pytest

from blacksmith.agent import Run
from blacksmith.metrics import BenchRecord, compare, load_history, record_run
from blacksmith.tools import SuiteResult


def a_run(**kw) -> Run:
    defaults = dict(
        finished=True,
        verified=False,
        result=SuiteResult(passed=17, failed=20),
        turns=12,
        stop_reason="budget",
        prompt_tokens=5000,
        completion_tokens=900,
        model_ms=14000,
        tool_ms=6000,
        wall_ms=21000,
    )
    return Run(**{**defaults, **kw})


# --- what a record carries ------------------------------------------------


def test_a_record_names_the_model_the_task_and_the_harness():
    """All three, or the number cannot be attributed to anything."""
    record = BenchRecord.of(a_run(), model="qwen3.8", endpoint="http://x/v1", task="ticketing")
    assert record.model == "qwen3.8"
    assert record.task == "ticketing"
    assert record.harness, "no harness version recorded"


def test_a_record_carries_the_graded_score():
    record = BenchRecord.of(a_run(), model="m", endpoint="e", task="t")
    assert (record.tests_passed, record.tests_total) == (17, 37)
    assert 0.45 < record.score < 0.47


def test_a_record_carries_the_cost_split():
    """Model time and tool time separately: a change that cuts turns while
    doubling time in pytest is not an improvement."""
    record = BenchRecord.of(a_run(), model="m", endpoint="e", task="t")
    assert (record.model_ms, record.tool_ms, record.wall_ms) == (14000, 6000, 21000)


def test_a_record_carries_the_generation_rate():
    record = BenchRecord.of(a_run(), model="m", endpoint="e", task="t")
    assert 60 < record.tokens_per_second < 70


def test_a_record_is_stamped_with_a_time():
    assert BenchRecord.of(a_run(), model="m", endpoint="e", task="t").at


def test_a_record_carries_the_behavioural_counts():
    """turns, refusals and spins are how two runs with the same score are told
    apart — one of them got there cleanly."""
    record = BenchRecord.of(a_run(), model="m", endpoint="e", task="t")
    assert record.turns == 12
    assert record.stop_reason == "budget"


# --- persistence ----------------------------------------------------------


def test_a_run_is_appended_to_the_history(tmp_path):
    path = tmp_path / "history.jsonl"
    record_run(a_run(), model="m", endpoint="e", task="t", path=path)
    record_run(a_run(), model="m", endpoint="e", task="t", path=path)
    assert len(load_history(path)) == 2


def test_history_survives_a_restart(tmp_path):
    """Appended, never rewritten. A baseline you can lose is not a baseline."""
    path = tmp_path / "history.jsonl"
    record_run(a_run(turns=1), model="m", endpoint="e", task="t", path=path)
    first = path.read_text()
    record_run(a_run(turns=2), model="m", endpoint="e", task="t", path=path)
    assert path.read_text().startswith(first)


def test_each_record_is_one_json_line(tmp_path):
    path = tmp_path / "history.jsonl"
    record_run(a_run(), model="m", endpoint="e", task="t", path=path)
    line = path.read_text().strip()
    assert json.loads(line)["task"] == "t"


def test_history_from_a_missing_file_is_empty(tmp_path):
    assert load_history(tmp_path / "nope.jsonl") == []


def test_a_corrupt_line_does_not_lose_the_rest(tmp_path):
    """A half-written line from a killed process must not cost every earlier
    measurement."""
    path = tmp_path / "history.jsonl"
    record_run(a_run(), model="m", endpoint="e", task="t", path=path)
    with path.open("a") as fh:
        fh.write("{not json\n")
    record_run(a_run(), model="m", endpoint="e", task="t", path=path)
    assert len(load_history(path)) == 2


def test_the_history_directory_is_created(tmp_path):
    path = tmp_path / "nested" / "history.jsonl"
    record_run(a_run(), model="m", endpoint="e", task="t", path=path)
    assert path.is_file()


# --- comparing ------------------------------------------------------------


def test_comparing_finds_the_previous_run_of_the_same_model_and_task(tmp_path):
    path = tmp_path / "h.jsonl"
    record_run(a_run(result=SuiteResult(passed=10, failed=27)), model="m", endpoint="e", task="t", path=path)
    record_run(a_run(result=SuiteResult(passed=20, failed=17)), model="m", endpoint="e", task="t", path=path)
    history = load_history(path)
    delta = compare(history[-1], history)
    assert delta is not None and delta.tests_passed == 10


def test_comparing_ignores_a_different_task(tmp_path):
    """Otherwise the ticketing baseline is compared against fix_return and every
    delta is noise."""
    path = tmp_path / "h.jsonl"
    record_run(a_run(), model="m", endpoint="e", task="other", path=path)
    record_run(a_run(), model="m", endpoint="e", task="t", path=path)
    history = load_history(path)
    assert compare(history[-1], history) is None


def test_comparing_ignores_a_different_model(tmp_path):
    path = tmp_path / "h.jsonl"
    record_run(a_run(), model="other", endpoint="e", task="t", path=path)
    record_run(a_run(), model="m", endpoint="e", task="t", path=path)
    history = load_history(path)
    assert compare(history[-1], history) is None


def test_the_first_ever_run_has_nothing_to_compare_against(tmp_path):
    path = tmp_path / "h.jsonl"
    record_run(a_run(), model="m", endpoint="e", task="t", path=path)
    history = load_history(path)
    assert compare(history[-1], history) is None


def test_a_delta_reports_the_direction_of_each_number(tmp_path):
    path = tmp_path / "h.jsonl"
    record_run(a_run(result=SuiteResult(passed=10, failed=27), turns=20), model="m", endpoint="e", task="t", path=path)
    record_run(a_run(result=SuiteResult(passed=20, failed=17), turns=12), model="m", endpoint="e", task="t", path=path)
    history = load_history(path)
    delta = compare(history[-1], history)
    assert delta.tests_passed == 10  # ten more tests satisfied
    assert delta.turns == -8  # in eight fewer turns
    assert delta.better is True


def test_a_delta_knows_when_a_change_made_things_worse(tmp_path):
    """The whole point. A change that drops the score has to be visible as a
    regression, not just as a different number."""
    path = tmp_path / "h.jsonl"
    record_run(a_run(result=SuiteResult(passed=30, failed=7)), model="m", endpoint="e", task="t", path=path)
    record_run(a_run(result=SuiteResult(passed=12, failed=25)), model="m", endpoint="e", task="t", path=path)
    history = load_history(path)
    assert compare(history[-1], history).better is False


def test_an_unchanged_score_is_neither_better_nor_worse(tmp_path):
    path = tmp_path / "h.jsonl"
    for _ in range(2):
        record_run(a_run(), model="m", endpoint="e", task="t", path=path)
    history = load_history(path)
    assert compare(history[-1], history).better is None


# --- sampling is part of the identity of a measurement --------------------
#
# A run at temperature 0.3 is not comparable with a run at 0, and filing them
# together makes every delta afterwards a lie: the next comparison reports a
# regression that is really a settings change. So the temperature is recorded,
# and `compare` is scoped by it the same way it is scoped by model and task.


def test_a_record_carries_the_temperature(tmp_path):
    record = BenchRecord.of(
        a_run(), model="m", endpoint="e", task="t", temperature=0.3
    )
    assert record.temperature == 0.3


def test_the_temperature_defaults_to_zero(tmp_path):
    assert BenchRecord.of(a_run(), model="m", endpoint="e", task="t").temperature == 0.0


def test_comparing_ignores_a_run_at_a_different_temperature(tmp_path):
    """The one that matters. Without this, switching to 0.3 and scoring worse
    reads as 'the harness got worse' rather than 'the sampler changed'."""
    path = tmp_path / "h.jsonl"
    record_run(a_run(), model="m", endpoint="e", task="t", temperature=0.0, path=path)
    record_run(a_run(), model="m", endpoint="e", task="t", temperature=0.3, path=path)
    history = load_history(path)
    assert compare(history[-1], history) is None


def test_comparing_finds_the_previous_run_at_the_same_temperature(tmp_path):
    path = tmp_path / "h.jsonl"
    record_run(
        a_run(result=SuiteResult(passed=10, failed=27)),
        model="m", endpoint="e", task="t", temperature=0.3, path=path,
    )
    record_run(a_run(), model="m", endpoint="e", task="t", temperature=0.0, path=path)
    record_run(
        a_run(result=SuiteResult(passed=20, failed=17)),
        model="m", endpoint="e", task="t", temperature=0.3, path=path,
    )
    history = load_history(path)
    delta = compare(history[-1], history)
    assert delta is not None and delta.tests_passed == 10
