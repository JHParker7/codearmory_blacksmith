"""Design, then build.

The stage boundary is what these pin: what the architect can reach, what reaches
the developer, and that neither can do the other's job.
"""

import pytest

from blacksmith.architect import ARCHITECT_PROMPT, DESIGN_NOTE
from blacksmith.model import Reply, ToolCall
from blacksmith.pipeline import design_and_build


class ScriptedModel:
    """Answers from a script, and records the prompt each stage was given."""

    def __init__(self, *replies: Reply) -> None:
        self.script = list(replies)
        self.system_prompts: list[str] = []
        self.openings: list[str] = []
        self.tools_offered: list[list[str]] = []
        self.model = "scripted"
        self.max_tokens = 2048

    async def chat(self, messages, *, tools=None, max_tokens=None) -> Reply:
        if messages and messages[0]["role"] == "system":
            self.system_prompts.append(messages[0]["content"])
        if len(messages) > 1 and messages[1]["role"] == "user":
            self.openings.append(messages[1]["content"])
        self.tools_offered.append([t["function"]["name"] for t in tools or []])
        if not self.script:
            return Reply(content="nothing left to say")
        return self.script.pop(0)


def calls(name, **arguments) -> Reply:
    return Reply(
        tool_calls=(ToolCall(id=f"c_{name}", name=name, arguments=arguments),),
        finish_reason="tool_calls",
    )


@pytest.fixture
def root(tmp_path):
    (tmp_path / "calc.py").write_text("def add(a, b):\n    return 0\n")
    (tmp_path / "test_calc.py").write_text(
        "from calc import add\n\n\ndef test_add():\n    assert add(1, 2) == 3\n"
    )
    return tmp_path


TASK = "Make the failing test pass."

WRITE_NOTE = calls("write_file", path=DESIGN_NOTE, content="# Plan\n\nUse a + b. Validate inputs.\n")
FIX = calls("edit_file", path="calc.py", old="return 0", new="return a + b")


def script(*extra):
    return ScriptedModel(WRITE_NOTE, calls("finish", summary="designed"), *extra)


# --- the two stages -------------------------------------------------------


async def test_the_architect_runs_before_the_developer(root):
    model = script(FIX, calls("finish", summary="built"))
    await design_and_build(TASK, root, model, max_turns=4, design_turns=3)
    assert ARCHITECT_PROMPT in model.system_prompts[0]
    assert ARCHITECT_PROMPT not in model.system_prompts[-1]


async def test_the_developers_brief_contains_the_design_note(root):
    """Inlined, not left to be discovered — an instruction nobody reads is the
    same as one nobody wrote."""
    model = script(FIX, calls("finish", summary="built"))
    await design_and_build(TASK, root, model, max_turns=4, design_turns=3)
    assert "Use a + b" in model.openings[-1]


async def test_the_note_is_returned_for_reading(root):
    model = script(FIX, calls("finish", summary="built"))
    run = await design_and_build(TASK, root, model, max_turns=4, design_turns=3)
    assert "Use a + b" in run.note


async def test_the_architect_is_not_offered_run_tests(root):
    model = script(FIX, calls("finish", summary="built"))
    await design_and_build(TASK, root, model, max_turns=4, design_turns=3)
    assert "run_tests" not in model.tools_offered[0]
    assert "run_tests" in model.tools_offered[-1]


async def test_the_build_stage_decides_the_result(root):
    model = script(FIX, calls("finish", summary="built"))
    run = await design_and_build(TASK, root, model, max_turns=4, design_turns=3)
    assert run.verified and run.result.passed == 1


async def test_a_failed_build_is_not_rescued_by_a_good_design(root):
    """The score is the code, never the plan."""
    model = script(Reply(content="I would fix it"))
    run = await design_and_build(TASK, root, model, max_turns=3, design_turns=3)
    assert not run.verified


# --- what the architect cannot do ----------------------------------------


async def test_an_architect_that_writes_code_is_refused_and_carries_on(root):
    """Refused, not fatal — it costs a turn and the run continues."""
    model = ScriptedModel(
        calls("write_file", path="calc2.py", content="x = 1\n"),
        WRITE_NOTE,
        calls("finish", summary="designed"),
        FIX,
        calls("finish", summary="built"),
    )
    run = await design_and_build(TASK, root, model, max_turns=4, design_turns=4)
    assert not (root / "calc2.py").exists()
    assert run.design.refusals >= 1
    assert run.verified


async def test_an_architect_that_reads_code_is_refused(root):
    """THE LOAD-BEARING RULE: a plan written after reading the code describes the
    code rather than the requirement."""
    model = ScriptedModel(
        calls("read_file", path="calc.py"),
        WRITE_NOTE,
        calls("finish", summary="designed"),
        FIX,
        calls("finish", summary="built"),
    )
    run = await design_and_build(TASK, root, model, max_turns=4, design_turns=4)
    assert any("architect" in s.detail.lower() for s in run.design.steps if s.kind == "refusal")


async def test_the_architect_cannot_finish_without_writing_anything(root):
    """Its claim to have designed something is not evidence either. The file is."""
    model = ScriptedModel(
        calls("finish", summary="all done"),
        WRITE_NOTE,
        calls("finish", summary="designed"),
        FIX,
        calls("finish", summary="built"),
    )
    run = await design_and_build(TASK, root, model, max_turns=4, design_turns=4)
    refusals = [s for s in run.design.steps if s.kind == "refusal"]
    assert refusals and "design note" in refusals[0].detail


# --- what the developer cannot do ----------------------------------------


async def test_the_developer_cannot_rewrite_its_own_brief(root):
    """An agent that can edit its instructions can make the job easier by
    changing what it was asked to do."""
    model = script(
        calls("write_file", path=DESIGN_NOTE, content="# Plan\n\nDo nothing.\n"),
        FIX,
        calls("finish", summary="built"),
    )
    run = await design_and_build(TASK, root, model, max_turns=5, design_turns=3)
    assert "Use a + b" in (root / DESIGN_NOTE).read_text()
    assert run.build.refusals >= 1


# --- the totals -----------------------------------------------------------


async def test_the_cost_of_both_stages_is_added_up(root):
    model = ScriptedModel(
        Reply(tool_calls=(ToolCall(id="c1", name="write_file", arguments={"path": DESIGN_NOTE, "content": "# P\n"}),), completion_tokens=10, latency_ms=100),
        Reply(tool_calls=(ToolCall(id="c2", name="finish", arguments={"summary": "d"}),), completion_tokens=5, latency_ms=50),
        Reply(tool_calls=(ToolCall(id="c3", name="edit_file", arguments={"path": "calc.py", "old": "return 0", "new": "return a + b"}),), completion_tokens=20, latency_ms=200),
        Reply(tool_calls=(ToolCall(id="c4", name="finish", arguments={"summary": "b"}),), completion_tokens=5, latency_ms=50),
    )
    run = await design_and_build(TASK, root, model, max_turns=4, design_turns=3)
    assert run.completion_tokens == 40
    assert run.model_ms == 400
    assert run.turns == run.design.turns + run.build.turns


async def test_the_stages_stay_separable(root):
    """'The architect burned twelve turns and the developer three' and the
    reverse are very different results with the same total."""
    model = script(FIX, calls("finish", summary="built"))
    run = await design_and_build(TASK, root, model, max_turns=4, design_turns=3)
    assert run.design.turns and run.build.turns
