"""The agent loop.

This is the thing that answers "can this model act as an agent". It is
deliberately small: a system prompt, a tool schema, and a loop that executes what
the model asks for and hands back the result.

Everything interesting is in what the loop does when the model gets it WRONG,
because with a local model that is most turns. The rule throughout: a mistake
costs one turn and produces a message the model can act on. Nothing a model can
emit should end the run except running out of budget.

And the one rule that is not about recovery — THE MODEL'S CLAIM TO HAVE FINISHED
IS NOT EVIDENCE. The tests decide.
"""

import pytest

from blacksmith.agent import SYSTEM_PROMPT, Run, solve
from blacksmith.model import Reply, ToolCall
from blacksmith.tools import Workspace


class ScriptedModel:
    """A model that says exactly what the test tells it to."""

    def __init__(self, *replies: Reply) -> None:
        self.script = list(replies)
        self.seen: list[list[dict]] = []
        self.tools_offered: list[dict] | None = None
        self.model = "scripted"
        self.max_tokens = 2048

    async def chat(self, messages, *, tools=None, max_tokens=None) -> Reply:
        self.seen.append([dict(m) for m in messages])
        self.tools_offered = tools
        if not self.script:
            # Never stall the loop on an exhausted script: that would look like a
            # budget failure and hide the real one.
            return Reply(content="I have run out of things to say.")
        return self.script.pop(0)


def calls(name, **arguments) -> Reply:
    return Reply(tool_calls=(ToolCall(id=f"c_{name}", name=name, arguments=arguments),), finish_reason="tool_calls")


def broken_call(name, error="the arguments you sent were not valid JSON") -> Reply:
    return Reply(tool_calls=(ToolCall(id="c_bad", name=name, error=error),), finish_reason="tool_calls")


@pytest.fixture
def ws(tmp_path):
    (tmp_path / "calc.py").write_text("def add(a, b):\n    return 0\n")
    (tmp_path / "test_calc.py").write_text("from calc import add\n\ndef test_add():\n    assert add(1, 2) == 3\n")
    return Workspace(tmp_path)


def fix(ws) -> Reply:
    return calls("edit_file", path="calc.py", old="return 0", new="return a + b")


TASK = "Make the failing test pass."


# --- the shape of the conversation ---------------------------------------


async def test_the_model_is_given_the_task_and_the_rules(ws):
    model = ScriptedModel(calls("finish", summary="done"))
    await solve(TASK, ws, model, max_turns=2)
    first = model.seen[0]
    assert first[0]["role"] == "system" and SYSTEM_PROMPT in first[0]["content"]
    assert TASK in first[-1]["content"]


async def test_the_tools_are_offered_every_turn(ws):
    model = ScriptedModel(calls("run_tests"), calls("finish", summary="done"))
    await solve(TASK, ws, model, max_turns=3)
    names = {t["function"]["name"] for t in model.tools_offered}
    assert {"read_file", "edit_file", "run_tests", "finish"} <= names


async def test_a_tool_result_is_returned_against_its_call_id(ws):
    """The id is how the result is matched to the request. Mismatched, the model
    sees an answer to a question it did not ask."""
    model = ScriptedModel(calls("read_file", path="calc.py"), calls("finish", summary="done"))
    await solve(TASK, ws, model, max_turns=3)
    tool_messages = [m for m in model.seen[-1] if m["role"] == "tool"]
    assert tool_messages and tool_messages[0]["tool_call_id"] == "c_read_file"
    assert "def add" in tool_messages[0]["content"]


async def test_the_assistants_own_call_is_in_the_history_before_its_result(ws):
    """Without it the conversation has a result for a request that was never
    made, and most servers reject the next turn outright."""
    model = ScriptedModel(calls("run_tests"), calls("finish", summary="done"))
    await solve(TASK, ws, model, max_turns=3)
    roles = [m["role"] for m in model.seen[-1]]
    assert roles.index("assistant") < roles.index("tool")


async def test_several_calls_in_one_reply_are_all_executed(ws):
    reply = Reply(
        tool_calls=(
            ToolCall(id="c1", name="read_file", arguments={"path": "calc.py"}),
            ToolCall(id="c2", name="run_tests"),
        ),
        finish_reason="tool_calls",
    )
    model = ScriptedModel(reply, calls("finish", summary="done"))
    await solve(TASK, ws, model, max_turns=3)
    # The SECOND turn's history: what the first reply produced, before finish.
    answered = {m["tool_call_id"] for m in model.seen[1] if m["role"] == "tool"}
    assert answered == {"c1", "c2"}


# --- finishing ------------------------------------------------------------


async def test_a_finished_run_is_verified_against_the_tests_not_the_claim(ws):
    """The single most important rule here. A model that says it is done and is
    not is the failure mode that makes an agent harness worthless."""
    model = ScriptedModel(fix(ws), calls("run_tests"), calls("finish", summary="fixed add"))
    run = await solve(TASK, ws, model, max_turns=5)
    assert run.finished and run.verified
    assert run.summary == "fixed add"


async def test_finishing_before_the_tests_pass_is_refused_and_the_run_continues(ws):
    """Observed constantly with small models: they declare victory after an edit
    without ever running anything."""
    model = ScriptedModel(
        calls("finish", summary="I fixed it"),
        fix(ws),
        calls("finish", summary="actually fixed it"),
    )
    run = await solve(TASK, ws, model, max_turns=6)
    assert run.verified
    assert any(step.kind == "refusal" and "finish" in step.tool for step in run.steps)


async def test_the_refusal_of_a_premature_finish_says_what_is_still_failing(ws):
    """'Not yet' is not actionable. The failing assertion is."""
    model = ScriptedModel(calls("finish", summary="done"))
    run = await solve(TASK, ws, model, max_turns=2)
    refusal = next(s for s in run.steps if s.kind == "refusal")
    assert "test_add" in refusal.detail


async def test_a_run_that_never_finishes_is_still_verified(ws):
    """A model can fix the code and then wander off. That is a success with bad
    manners, not a failure, and scoring it as a failure hides real progress."""
    model = ScriptedModel(fix(ws), Reply(content="I think that is right."), Reply(content="Yes."))
    run = await solve(TASK, ws, model, max_turns=3)
    assert not run.finished and run.verified


# --- when the model gets it wrong ----------------------------------------


async def test_a_malformed_tool_call_costs_a_turn_not_the_run(ws):
    model = ScriptedModel(broken_call("edit_file"), fix(ws), calls("finish", summary="done"))
    run = await solve(TASK, ws, model, max_turns=5)
    assert run.verified
    assert any(step.kind == "refusal" for step in run.steps)


async def test_the_model_is_told_what_was_wrong_with_its_call(ws):
    model = ScriptedModel(broken_call("edit_file", "not valid JSON: {\"old\": }"), calls("finish", summary="x"))
    await solve(TASK, ws, model, max_turns=3)
    tool_messages = [m for m in model.seen[1] if m["role"] == "tool"]
    assert "not valid JSON" in tool_messages[0]["content"]


async def test_a_refused_tool_is_reported_back_as_that_tools_result(ws):
    """Not as a user message. A refusal that arrives out of band breaks the
    call/result pairing and the next turn is rejected by the server."""
    model = ScriptedModel(
        calls("edit_file", path="calc.py", old="return None", new="x"),
        calls("finish", summary="x"),
    )
    await solve(TASK, ws, model, max_turns=3)
    tool_messages = [m for m in model.seen[1] if m["role"] == "tool"]
    assert tool_messages and "did not match" in tool_messages[0]["content"]


async def test_an_unknown_tool_is_refused_and_the_run_continues(ws):
    model = ScriptedModel(calls("delete_repo"), fix(ws), calls("finish", summary="done"))
    run = await solve(TASK, ws, model, max_turns=5)
    assert run.verified


async def test_a_reply_with_no_tool_call_is_nudged_once(ws):
    """A model answering in prose has not acted. It is told so and given another
    turn, because the nudge usually works."""
    model = ScriptedModel(Reply(content="I would edit calc.py."), fix(ws), calls("finish", summary="done"))
    run = await solve(TASK, ws, model, max_turns=5)
    assert run.verified
    assert any(s.kind == "nudge" for s in run.steps)


async def test_a_model_that_will_not_act_is_stopped_rather_than_burning_the_budget(ws):
    """Twenty turns of a model narrating what it would do costs real time on a
    workstation and tells you nothing the third turn did not."""
    model = ScriptedModel(*[Reply(content="Let me think about it.") for _ in range(10)])
    run = await solve(TASK, ws, model, max_turns=10)
    assert run.stop_reason == "no_progress"
    assert run.turns < 10


async def test_a_truncated_reply_tells_the_model_it_was_cut_off(ws):
    """`length` looks exactly like a model that stopped early, and the fix is the
    opposite: it needs to say LESS, not be prompted harder."""
    model = ScriptedModel(Reply(content="I will edit", finish_reason="length"), fix(ws), calls("finish", summary="d"))
    run = await solve(TASK, ws, model, max_turns=5)
    assert any("cut off" in s.detail for s in run.steps if s.kind == "nudge")


async def test_repeating_an_identical_failing_call_is_named_as_a_repeat(ws):
    """A small model that gets a refusal will very often send exactly the same
    call again. Telling it the result is the same teaches nothing; telling it
    that it repeated itself is at least new information."""
    bad = calls("edit_file", path="calc.py", old="return None", new="x")
    model = ScriptedModel(bad, calls("edit_file", path="calc.py", old="return None", new="x"), fix(ws), calls("finish", summary="d"))
    run = await solve(TASK, ws, model, max_turns=6)
    assert any("already tried" in s.detail for s in run.steps)


# --- the budget -----------------------------------------------------------


async def test_the_run_stops_at_the_turn_budget(ws):
    """Distinct calls each turn, so this measures the budget and not the
    no-progress stop — a model doing genuinely new things simply runs out."""
    model = ScriptedModel(*[calls("write_file", path=f"f{i}.py", content="x") for i in range(20)])
    run = await solve(TASK, ws, model, max_turns=4)
    assert run.turns == 4 and run.stop_reason == "budget"
    assert not run.verified



async def test_the_run_reports_what_it_cost(ws):
    """The point of the rig is comparing models, and cost is half the comparison."""
    model = ScriptedModel(
        Reply(tool_calls=(ToolCall(id="c1", name="run_tests"),), prompt_tokens=100, completion_tokens=20),
        Reply(content="done", prompt_tokens=150, completion_tokens=10),
    )
    run = await solve(TASK, ws, model, max_turns=2)
    assert run.prompt_tokens == 250 and run.completion_tokens == 30


async def test_every_step_is_recorded_in_order(ws):
    model = ScriptedModel(calls("run_tests"), fix(ws), calls("finish", summary="done"))
    run = await solve(TASK, ws, model, max_turns=5)
    assert [s.tool for s in run.steps if s.kind == "call"] == ["run_tests", "edit_file", "finish"]


async def test_a_run_is_a_plain_result_object(ws):
    model = ScriptedModel(calls("finish", summary="x"))
    assert isinstance(await solve(TASK, ws, model, max_turns=1), Run)


# --- the transcript -------------------------------------------------------


async def test_the_run_is_written_to_a_transcript_when_one_is_given(ws, tmp_path):
    from blacksmith.transcript import JSONLSink, Recorder

    sink = JSONLSink(tmp_path / "t")
    recorder = Recorder(sink, host="box-a")
    transcript = recorder.start("run-1", task_id="smoke", role="dev-agent")
    model = ScriptedModel(fix(ws), calls("finish", summary="done"))
    await solve(TASK, ws, model, max_turns=4, transcript=transcript)
    sink.close()

    written = (tmp_path / "t").glob("*.jsonl")
    lines = next(written).read_text().splitlines()
    kinds = [line.split('"kind":"')[1].split('"')[0] for line in lines]
    assert "turn" in kinds and "action" in kinds


async def test_a_refusal_is_recorded_as_a_refusal(ws, tmp_path):
    """What the agent attempted and why it was refused is the most useful thing
    to know about a failed run, and the easiest to forget to write down."""
    from blacksmith.transcript import JSONLSink, Recorder

    sink = JSONLSink(tmp_path / "t")
    transcript = Recorder(sink, host="box-a").start("run-1", task_id="smoke", role="dev-agent")
    model = ScriptedModel(broken_call("edit_file"), Reply(content="giving up"))
    await solve(TASK, ws, model, max_turns=3, transcript=transcript)
    sink.close()
    body = next((tmp_path / "t").glob("*.jsonl")).read_text()
    assert '"kind":"refusal"' in body


# --- spinning -------------------------------------------------------------
#
# A call that SUCCEEDS and returns exactly what it returned last time. Measured
# on the web-GUI task against the real codebase: 8 of 15 turns were run_tests
# against an unchanged failure, because the model had not written the file yet.
#
# IT IS COUNTED AND NOT CORRECTED, and that is a finding rather than laziness.
# Two interventions were tried and both made the same task measurably worse:
#
#   baseline (no intervention)          PASS in 15 turns
#   a "you already ran this" note       FAIL at the 25-turn budget
#   spinning counts toward no-progress  FAIL after 6 turns
#
# The note grew the context with near-identical text every turn and pushed the
# model further into the loop. The early stop killed the run on a legitimate
# RE-READ — going back to check a file before writing is what a careful agent
# does, and this test cannot tell it apart from spinning.
#
# So the wasted turns are left alone and merely counted, because a model that
# passes in 4 turns and one that passes in 15 with 8 spins are not the same
# result and a score should say so.


async def test_a_repeated_call_with_an_unchanged_result_is_counted(ws):
    model = ScriptedModel(calls("run_tests"), calls("run_tests"), Reply(content="ok"))
    run = await solve(TASK, ws, model, max_turns=3)
    assert run.spins == 1


async def test_the_result_is_handed_back_untouched(ws):
    """No note, no preamble. Both attempts at coaching the model here made the
    outcome worse, so the loop says nothing and gets out of the way."""
    model = ScriptedModel(calls("run_tests"), calls("run_tests"), Reply(content="ok"))
    await solve(TASK, ws, model, max_turns=3)
    first, second = [
        m["content"] for m in model.seen[2] if m["role"] == "tool"
    ][-2:]
    assert first == second, "the second result was rewritten rather than repeated"


async def test_spinning_does_not_end_the_run(ws):
    """It burns turns, and burning turns is the honest cost. Ending the run here
    cost a task that would otherwise have passed."""
    model = ScriptedModel(*[calls("run_tests") for _ in range(20)])
    run = await solve(TASK, ws, model, max_turns=6)
    assert run.stop_reason == "budget" and run.turns == 6


async def test_a_call_whose_result_did_change_is_not_a_spin(ws):
    """run_tests going from FAIL to PASS is exactly the progress being looked
    for, and counting it as spinning would make the metric meaningless."""
    model = ScriptedModel(calls("run_tests"), fix(ws), calls("run_tests"), calls("finish", summary="d"))
    run = await solve(TASK, ws, model, max_turns=6)
    assert run.spins == 0 and run.verified


async def test_a_result_that_only_differs_by_timing_still_counts_as_a_spin(ws):
    """pytest prints its own duration, so byte equality flaps between identical
    runs. Before this was normalised the detector fired every other turn, which
    is worse than not firing: the same situation got an inconsistent answer."""
    model = ScriptedModel(calls("run_tests"), calls("run_tests"), Reply(content="ok"))
    run = await solve(TASK, ws, model, max_turns=3)
    assert run.spins == 1


async def test_a_different_call_is_not_a_spin(ws):
    model = ScriptedModel(
        calls("run_tests"), calls("read_file", path="calc.py"), calls("finish", summary="x")
    )
    run = await solve(TASK, ws, model, max_turns=4)
    assert run.spins == 0


# --- what a run cost ------------------------------------------------------
#
# For comparing harness changes the SPLIT matters more than the total. A change
# that cuts turns but doubles the time in tools is not an improvement, and a
# single wall-clock number cannot tell you which happened.


async def test_a_run_reports_wall_clock(ws):
    model = ScriptedModel(fix(ws), calls("run_tests"), calls("finish", summary="d"))
    run = await solve(TASK, ws, model, max_turns=5)
    assert run.wall_ms > 0


async def test_a_run_separates_model_time_from_tool_time(ws):
    """run_tests spawns pytest, which is real time the model did not spend."""
    model = ScriptedModel(calls("run_tests"), fix(ws), calls("finish", summary="d"))
    run = await solve(TASK, ws, model, max_turns=5)
    assert run.tool_ms > 0, "time spent running tests was not counted"
    assert run.wall_ms >= run.tool_ms


async def test_model_time_comes_from_the_replies(ws):
    """Measured by the client around the call, so it excludes the harness's own
    bookkeeping and can be compared against the endpoint's own numbers."""
    model = ScriptedModel(
        Reply(tool_calls=(ToolCall(id="c1", name="run_tests"),), latency_ms=120),
        Reply(content="done", latency_ms=80),
    )
    run = await solve(TASK, ws, model, max_turns=2)
    assert run.model_ms == 200


async def test_generation_rate_is_derived_from_model_time_only(ws):
    """Dividing tokens by wall-clock would fold pytest into the tok/s figure and
    make a slow test suite look like a slow model."""
    model = ScriptedModel(Reply(content="done", completion_tokens=100, latency_ms=1000))
    run = await solve(TASK, ws, model, max_turns=1)
    assert 95 < run.tokens_per_second < 105


async def test_generation_rate_is_zero_when_nothing_was_generated(ws):
    model = ScriptedModel(Reply(content=""))
    run = await solve(TASK, ws, model, max_turns=1)
    assert run.tokens_per_second == 0.0
