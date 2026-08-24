"""The tools an agent is given, and the workspace they act inside.

This is the smallest surface that lets a model do real work: look, read, change,
and find out whether the change worked. Everything about the shape of these tools
is chosen to make a wrong move IMPOSSIBLE rather than to explain it afterwards —
a message telling a model not to do something does not reliably stop it, and a
schema or a filesystem check does.

Unlike the full harness these run on the workstation directly, with no sandbox.
The workspace boundary is therefore load-bearing, not decoration.
"""

import pytest

from blacksmith.tools import ARCHITECT, TOOLS, SuiteResult, ToolError, Workspace, run_tool, tool_schemas


@pytest.fixture
def ws(tmp_path):
    (tmp_path / "src").mkdir()
    (tmp_path / "src" / "calc.py").write_text("def add(a, b):\n    return 0\n")
    (tmp_path / "test_calc.py").write_text("from src.calc import add\n\ndef test_add():\n    assert add(1, 2) == 3\n")
    return Workspace(tmp_path)


def call(ws: Workspace, name: str, **args) -> str:
    return run_tool(ws, name, args)


# --- the boundary ---------------------------------------------------------


def test_a_path_outside_the_workspace_is_refused(ws):
    """No sandbox stands between this and the operator's home directory."""
    with pytest.raises(ToolError) as raised:
        call(ws, "read_file", path="../../../etc/passwd")
    assert "outside the workspace" in str(raised.value)


def test_an_absolute_path_is_refused(ws):
    with pytest.raises(ToolError):
        call(ws, "read_file", path="/etc/passwd")


def test_a_symlink_out_of_the_workspace_is_refused(ws, tmp_path):
    """Checking the string and not the resolved path is the classic hole."""
    secret = tmp_path.parent / "secret.txt"
    secret.write_text("private")
    (ws.root / "link.txt").symlink_to(secret)
    with pytest.raises(ToolError):
        call(ws, "read_file", path="link.txt")


def test_a_path_that_merely_starts_with_the_root_name_is_refused(ws, tmp_path):
    """`/tmp/x/../x-evil` shares a prefix with `/tmp/x` and is not inside it."""
    sibling = tmp_path.parent / (tmp_path.name + "-evil")
    sibling.mkdir(exist_ok=True)
    (sibling / "f.txt").write_text("nope")
    with pytest.raises(ToolError):
        call(ws, "read_file", path=f"../{sibling.name}/f.txt")


def test_a_normal_nested_path_is_fine(ws):
    assert "def add" in call(ws, "read_file", path="src/calc.py")


# --- looking --------------------------------------------------------------


def test_listing_shows_the_files_a_model_needs_to_find(ws):
    listed = call(ws, "list_files", path=".")
    assert "src/calc.py" in listed and "test_calc.py" in listed


def test_listing_hides_noise_that_would_fill_the_context(ws):
    """A model given 400 lines of .venv has spent its context before it starts."""
    (ws.root / ".git").mkdir()
    (ws.root / ".git" / "config").write_text("x")
    (ws.root / "__pycache__").mkdir()
    (ws.root / "__pycache__" / "calc.pyc").write_text("x")
    listed = call(ws, "list_files", path=".")
    assert ".git" not in listed and "__pycache__" not in listed


def test_reading_a_missing_file_says_so_and_says_what_is_there(ws):
    """Naming the symptom alone sends a correct agent to the wrong place; the
    listing is what lets it recover on the next turn instead of guessing again."""
    with pytest.raises(ToolError) as raised:
        call(ws, "read_file", path="src/calculator.py")
    message = str(raised.value)
    assert "src/calculator.py" in message and "calc.py" in message


def test_a_read_file_is_numbered(ws):
    """An edit is addressed by content, but a model reasoning about a file needs
    to be able to say where it is looking."""
    assert "1" in call(ws, "read_file", path="src/calc.py").splitlines()[0]


# --- changing -------------------------------------------------------------


def test_an_edit_replaces_exactly_what_was_quoted(ws):
    call(ws, "edit_file", path="src/calc.py", old="return 0", new="return a + b")
    assert "return a + b" in (ws.root / "src" / "calc.py").read_text()


def test_an_edit_that_matches_nothing_is_refused_with_the_reason(ws):
    """The single most common failure of a small model: quoting text it
    remembered rather than text that is there."""
    with pytest.raises(ToolError) as raised:
        call(ws, "edit_file", path="src/calc.py", old="return None", new="return a + b")
    assert "did not match" in str(raised.value)


def test_an_edit_matching_more_than_once_is_refused(ws):
    """Replacing the first of three is a silent wrong answer. Refusing makes the
    model quote more context, which is the thing that actually fixes it."""
    (ws.root / "dup.py").write_text("x = 1\ny = 1\n")
    with pytest.raises(ToolError) as raised:
        call(ws, "edit_file", path="dup.py", old="= 1", new="= 2")
    assert "2 places" in str(raised.value)


def test_an_edit_that_changes_nothing_is_refused(ws):
    """A no-op edit reads to the model as progress, and it will make it again."""
    with pytest.raises(ToolError) as raised:
        call(ws, "edit_file", path="src/calc.py", old="return 0", new="return 0")
    assert "identical" in str(raised.value)


def test_writing_a_new_file_works(ws):
    call(ws, "write_file", path="src/util.py", content="VALUE = 1\n")
    assert (ws.root / "src" / "util.py").read_text() == "VALUE = 1\n"


def test_writing_creates_missing_directories(ws):
    call(ws, "write_file", path="a/b/c.py", content="x = 1\n")
    assert (ws.root / "a" / "b" / "c.py").exists()


def test_writing_over_an_existing_file_is_refused(ws):
    """Whole-file writes are how a model destroys work it cannot see. If it wants
    to change a file it has to quote the part it is changing."""
    with pytest.raises(ToolError) as raised:
        call(ws, "write_file", path="src/calc.py", content="def add(a, b): return a + b\n")
    assert "already exists" in str(raised.value) and "edit_file" in str(raised.value)


def test_the_tests_are_not_writable_by_the_agent(ws):
    """THE LOAD-BEARING RULE. An agent that can edit the test can make it pass
    without making the code work, and it will."""
    for name, args in (
        ("edit_file", {"path": "test_calc.py", "old": "== 3", "new": "== 0"}),
        ("write_file", {"path": "test_calc.py", "content": "def test_add(): pass\n"}),
        ("write_file", {"path": "tests/test_new.py", "content": "def test_x(): pass\n"}),
    ):
        with pytest.raises(ToolError) as raised:
            call(ws, name, **args)
        assert "test" in str(raised.value).lower()


# --- finding out whether it worked ---------------------------------------


def test_running_the_tests_reports_failure_with_the_assertion(ws):
    out = call(ws, "run_tests")
    assert "FAIL" in out.upper()
    assert "test_add" in out


def test_running_the_tests_reports_success_once_the_code_is_right(ws):
    call(ws, "edit_file", path="src/calc.py", old="return 0", new="return a + b")
    assert "PASS" in call(ws, "run_tests").upper()


def test_test_output_is_clipped_so_one_failure_cannot_eat_the_context(ws):
    (ws.root / "test_noisy.py").write_text(
        "def test_noisy():\n    print('x' * 200)\n    assert False\n" * 1
    )
    out = call(ws, "run_tests")
    assert len(out) < 8000


def test_the_workspace_reports_whether_it_is_green(ws):
    """The agent's own claim to have finished is not evidence. This is."""
    assert not ws.tests_pass()
    call(ws, "edit_file", path="src/calc.py", old="return 0", new="return a + b")
    assert ws.tests_pass()


# --- the schema the model is given ---------------------------------------


def test_every_tool_has_a_schema_the_api_will_accept():
    for tool in TOOLS:
        schema = tool.schema()
        assert schema["type"] == "function"
        fn = schema["function"]
        assert fn["name"] and fn["description"]
        assert fn["parameters"]["type"] == "object"
        for name in fn["parameters"].get("required", []):
            assert name in fn["parameters"]["properties"], (tool.name, name)


def test_there_is_a_way_for_the_agent_to_say_it_is_done():
    assert "finish" in {t.name for t in TOOLS}


def test_the_tool_set_is_small():
    """Every extra tool is another thing a small model can pick wrongly, so the
    bar for adding one is evidence that its ABSENCE cost something.

    search_files cleared that bar: the model asked for grep eight times in one
    run and stalled on run_tests each turn because there was nothing else to do.
    That is the only reason the cap moved from six.
    """
    assert len(TOOLS) <= 7


def test_an_unknown_tool_is_refused_by_name(ws):
    with pytest.raises(ToolError) as raised:
        call(ws, "delete_everything")
    assert "delete_everything" in str(raised.value)


def test_a_missing_required_argument_is_refused_by_name(ws):
    """The model gets told which argument, because 'invalid arguments' is not
    something it can act on."""
    with pytest.raises(ToolError) as raised:
        run_tool(ws, "read_file", {})
    assert "path" in str(raised.value)


def test_an_unexpected_argument_does_not_crash_the_loop(ws):
    """Small models add fields. That should cost a turn at worst, never the run."""
    assert "def add" in run_tool(ws, "read_file", {"path": "src/calc.py", "encoding": "utf-8"})


def test_arguments_of_the_wrong_type_are_refused_rather_than_coerced(ws):
    with pytest.raises(ToolError):
        run_tool(ws, "read_file", {"path": ["src/calc.py"]})


# --- searching, and reading part of a file --------------------------------
#
# BOTH OF THESE CAME FROM READING THE MODEL'S REASONING, not from guessing at its
# behaviour. On the web-GUI task against the real codebase it spent turns 3-10
# saying "I need to grep for WORKFLOW_COLUMNS ... let me try grepping" — eight
# times — and called run_tests each turn as a stall, because there was no search
# tool and nothing else to do. Then at turn 12: "The middle part is omitted ...
# read_file doesn't support ranges."
#
# Two "fixes" had already been derived from the tool calls alone, without reading
# any of that, and both made the task worse. The model had been saying exactly
# what it needed the whole time.


def test_search_finds_a_definition_by_name(ws):
    """The thing the model asked for eight times."""
    found = call(ws, "search_files", pattern="def add")
    assert "src/calc.py" in found


def test_search_reports_the_line_number(ws):
    """A hit with no line number cannot be turned into a read."""
    found = call(ws, "search_files", pattern="def add")
    assert ":1:" in found


def test_search_reports_the_matching_line(ws):
    assert "def add(a, b):" in call(ws, "search_files", pattern="def add")


def test_search_covers_every_file_in_the_workspace(ws):
    (ws.root / "src" / "other.py").write_text("MARKER = 1\n")
    assert "src/other.py" in call(ws, "search_files", pattern="MARKER")


def test_search_says_plainly_when_there_is_nothing(ws):
    """An empty result that looks like an error sends the model looking for a
    bug in its query rather than in its assumption."""
    assert "no match" in call(ws, "search_files", pattern="ZZZ_NOT_HERE").lower()


def test_search_skips_the_noise_directories(ws):
    (ws.root / "__pycache__").mkdir(exist_ok=True)
    (ws.root / "__pycache__" / "x.py").write_text("def add(a, b): pass\n")
    assert "__pycache__" not in call(ws, "search_files", pattern="def add")


def test_search_is_capped_so_one_common_word_cannot_fill_the_context(ws):
    for i in range(200):
        (ws.root / f"f{i}.py").write_text("common\n" * 20)
    found = call(ws, "search_files", pattern="common")
    assert len(found) < 8000
    assert "more" in found.lower()


def test_a_bad_pattern_is_refused_rather_than_raising(ws):
    """A model writing a regex will eventually write a broken one."""
    with pytest.raises(ToolError) as raised:
        call(ws, "search_files", pattern="(unclosed")
    assert "pattern" in str(raised.value).lower()


def test_search_can_be_scoped_to_a_directory(ws):
    (ws.root / "elsewhere.py").write_text("def add(x): pass\n")
    found = call(ws, "search_files", pattern="def add", path="src")
    assert "src/calc.py" in found and "elsewhere.py" not in found


def test_reading_a_line_range(ws):
    (ws.root / "long.py").write_text("".join(f"line{i}\n" for i in range(1, 101)))
    text = call(ws, "read_file", path="long.py", start_line=50, end_line=52)
    assert "line50" in text and "line52" in text
    assert "line49" not in text and "line53" not in text


def test_a_range_keeps_the_real_line_numbers(ws):
    """Renumbering from 1 would make every line number the model quotes wrong."""
    (ws.root / "long.py").write_text("".join(f"line{i}\n" for i in range(1, 101)))
    text = call(ws, "read_file", path="long.py", start_line=50, end_line=51)
    assert "50 |" in text


def test_a_range_past_the_end_is_not_an_error(ws):
    (ws.root / "long.py").write_text("a\nb\n")
    assert "b" in call(ws, "read_file", path="long.py", start_line=1, end_line=999)


def test_a_start_beyond_the_file_says_how_long_the_file_is(ws):
    (ws.root / "long.py").write_text("a\nb\n")
    with pytest.raises(ToolError) as raised:
        call(ws, "read_file", path="long.py", start_line=50)
    assert "2" in str(raised.value)


def test_a_clipped_file_tells_the_model_how_to_get_the_rest(ws):
    """THE FAILURE THAT COST EIGHT TURNS. The middle of a long file was dropped
    with no indication that a range could be asked for, and the model said so:
    'The middle part is omitted ... read_file doesn't support ranges.'"""
    (ws.root / "huge.py").write_text("".join(f"line{i} = {i}\n" for i in range(1, 4000)))
    text = call(ws, "read_file", path="huge.py")
    assert "omitted" in text
    assert "start_line" in text, "the model is not told it can read a range"


def test_a_clipped_file_says_how_many_lines_it_has(ws):
    """Without the total the model cannot choose a range to ask for."""
    (ws.root / "huge.py").write_text("".join(f"line{i} = {i}\n" for i in range(1, 4000)))
    assert "3999" in call(ws, "read_file", path="huge.py")


# --- a graded result ------------------------------------------------------
#
# A baseline for tracking whether a change helped needs RESOLUTION. Pass/fail on
# one task cannot show that a change took a model from 4 of 30 tests to 22 — it
# reports the same "FAIL" either way, so a real improvement looks like no change
# and the next change is made blind.


def test_a_green_workspace_reports_every_test_passing(ws):
    call(ws, "edit_file", path="src/calc.py", old="return 0", new="return a + b")
    report = ws.test_report()
    assert report.passed == 1 and report.failed == 0
    assert report.total == 1 and report.all_passed


def test_a_red_workspace_reports_the_failure_count(ws):
    report = ws.test_report()
    assert report.failed == 1 and report.passed == 0
    assert not report.all_passed


def test_a_partially_green_workspace_is_scored_in_between(tmp_path):
    """The case the whole thing exists for. Written into the SPEC file, because
    the score is measured against the specification as it shipped — see
    `spec_tests`."""
    (tmp_path / "calc.py").write_text("def add(a, b):\n    return 0\n")
    (tmp_path / "test_calc.py").write_text(
        "from calc import add\n\n\n"
        "def test_wrong():\n    assert add(1, 2) == 3\n\n\n"
        "def test_a():\n    assert True\n\n\n"
        "def test_b():\n    assert True\n"
    )
    report = Workspace(tmp_path).test_report()
    assert report.passed == 2 and report.failed == 1 and report.total == 3
    assert 0 < report.score < 1


def test_the_score_is_a_fraction_of_the_tests(ws):
    call(ws, "edit_file", path="src/calc.py", old="return 0", new="return a + b")
    assert ws.test_report().score == 1.0


def test_a_collection_error_scores_zero_rather_than_crashing(ws):
    """The state every one of these tasks STARTS in: the module under test does
    not exist yet, so pytest cannot even import the spec. Scoring that as an
    exception rather than a zero would make the first measurement impossible."""
    (ws.root / "test_broken.py").write_text("import does_not_exist_anywhere\n")
    report = ws.test_report()
    assert report.score == 0.0 or report.errors >= 1
    assert not report.all_passed


def test_a_workspace_with_no_tests_is_not_a_pass(ws, tmp_path):
    """'No tests ran' is green by exit code in some runners and must never count
    as done — that is the failure that merged 168 lines with nothing checking
    them."""
    empty = Workspace(tmp_path / "empty")
    (tmp_path / "empty").mkdir()
    report = empty.test_report()
    assert report.total == 0 and not report.all_passed


def test_the_report_does_not_leave_files_in_the_workspace(ws):
    """The agent lists this directory. A report file appearing next to the code
    is something it will try to read, and eventually to edit."""
    # Compared through list_files, because that is what the agent can actually
    # see — bytecode caches are already filtered out of its view.
    before = call(ws, "list_files", path=".")
    ws.test_report()
    assert call(ws, "list_files", path=".") == before


def test_tests_pass_still_answers_the_simple_question(ws):
    """The boolean is what the loop gates `finish` on; the graded number is for
    the score. Both come from one run so they cannot disagree."""
    assert not ws.tests_pass()
    call(ws, "edit_file", path="src/calc.py", old="return 0", new="return a + b")
    assert ws.tests_pass()


# --- who may write what ---------------------------------------------------
#
# THE SPLIT IS ENFORCED BY THE TOOL, NOT BY THE PROMPT. An agent that can write
# both the test and the code will write the test its code already passes, and it
# cannot notice it has done so. Asking it not to does not work; refusing the
# write does.
#
# The architect writes the specification and may not implement it. The developer
# implements and may never touch a test. Neither can move the other's half to
# make its own pass.


def architect(root) -> Workspace:
    return Workspace(root, role=ARCHITECT)


def test_the_architect_may_not_write_a_test_file(tmp_path):
    """It specifies in prose. Letting it add executable tests would also let it
    add an impossible one, and the developer would be unable to finish through no
    fault of its own — with the graded denominator moving underneath the
    baseline at the same time."""
    ws = architect(tmp_path)
    with pytest.raises(ToolError):
        run_tool(ws, "write_file", {"path": "test_new.py", "content": "def test_b(): pass\n"})


def test_the_architect_may_not_write_an_implementation(tmp_path):
    """Otherwise it satisfies its own specification and the developer inherits
    nothing to do."""
    ws = architect(tmp_path)
    with pytest.raises(ToolError) as raised:
        run_tool(ws, "write_file", {"path": "ticketing.py", "content": "x = 1\n"})
    assert "implementation" in str(raised.value).lower()


def test_the_architect_may_not_edit_an_implementation(tmp_path):
    (tmp_path / "app.py").write_text("VALUE = 1\n")
    ws = architect(tmp_path)
    with pytest.raises(ToolError):
        run_tool(ws, "edit_file", {"path": "app.py", "old": "1", "new": "2"})


def test_the_architect_may_write_a_design_note(tmp_path):
    """Prose is not an implementation. The note is how the specification reaches
    the developer as something other than assertions."""
    ws = architect(tmp_path)
    run_tool(ws, "write_file", {"path": "DESIGN.md", "content": "# Design\n"})
    assert (tmp_path / "DESIGN.md").exists()


def test_the_developer_may_not_write_the_design_note(ws):
    """Its instructions are not its work. An agent that can edit its own brief
    can make the job easier by rewriting what it was asked to do — the same
    failure as editing the test, one level up."""
    with pytest.raises(ToolError) as raised:
        run_tool(ws, "write_file", {"path": "DESIGN.md", "content": "do less\n"})
    assert "instructions" in str(raised.value).lower()


def test_the_developer_may_read_the_design_note(ws):
    (ws.root / "DESIGN.md").write_text("# Design\n\nUse a dataclass.\n")
    assert "dataclass" in run_tool(ws, "read_file", {"path": "DESIGN.md"})


def test_the_architect_may_read_the_tests_it_designs_against(tmp_path):
    """The tests ARE the requirement. An architect that could not read them
    would be designing blind."""
    (tmp_path / "test_spec.py").write_text("def test_a():\n    assert True\n")
    assert "test_a" in run_tool(architect(tmp_path), "read_file", {"path": "test_spec.py"})


def test_the_architect_may_not_read_the_implementation(tmp_path):
    """THE LOAD-BEARING RULE of this stage. A plan written after reading the code
    describes the code rather than the requirement."""
    (tmp_path / "ticketing.py").write_text("SECRET = 1\n")
    with pytest.raises(ToolError) as raised:
        run_tool(architect(tmp_path), "read_file", {"path": "ticketing.py"})
    assert "architect" in str(raised.value).lower()


def test_the_architect_cannot_grep_the_implementation_either(tmp_path):
    """Search returns source lines. Enforcing the rule on read_file and leaving
    search open closes one door and leaves the next one open."""
    (tmp_path / "ticketing.py").write_text("SECRET_MARKER = 1\n")
    (tmp_path / "test_spec.py").write_text("# SECRET_MARKER appears here too\n")
    found = run_tool(architect(tmp_path), "search_files", {"pattern": "SECRET_MARKER"})
    assert "ticketing.py" not in found
    assert "test_spec.py" in found


def test_the_architect_may_read_its_own_design_note(tmp_path):
    (tmp_path / "DESIGN.md").write_text("# Design\n")
    assert "Design" in run_tool(architect(tmp_path), "read_file", {"path": "DESIGN.md"})


def test_the_architect_is_not_offered_run_tests():
    """It has nothing to run, and a traceback would show it the source anyway."""
    offered = {t["function"]["name"] for t in tool_schemas(ARCHITECT)}
    assert "run_tests" not in offered
    assert {"read_file", "write_file", "finish"} <= offered


def test_the_developer_is_offered_everything():
    from blacksmith.tools import DEVELOPER

    offered = {t["function"]["name"] for t in tool_schemas(DEVELOPER)}
    assert offered == {t.name for t in TOOLS}


def test_the_developer_still_may_not_write_tests(ws):
    with pytest.raises(ToolError):
        run_tool(ws, "write_file", {"path": "test_new.py", "content": "def test_x(): pass\n"})


def test_the_developer_may_write_implementation(ws):
    run_tool(ws, "write_file", {"path": "src/extra.py", "content": "VALUE = 1\n"})
    assert (ws.root / "src" / "extra.py").exists()


def test_the_refusal_says_which_role_it_is_addressing(tmp_path):
    """A refusal that names the symptom and not the cause sends a correct agent
    to the wrong place."""
    ws = architect(tmp_path)
    with pytest.raises(ToolError) as raised:
        run_tool(ws, "write_file", {"path": "thing.py", "content": "x = 1\n"})
    message = str(raised.value).lower()
    assert "architect" in message and "developer" in message


# --- the score is measured against the ORIGINAL specification -------------
#
# Once an architect can add tests, "how many tests passed" stops being
# comparable: a run where the architect wrote 12 extra tests has a different
# denominator from one where it wrote none, and the baseline dies. So the SCORE
# is always the task's own spec, snapshotted before anything runs.


def test_the_spec_files_are_snapshotted_at_construction(tmp_path):
    (tmp_path / "test_spec.py").write_text("def test_a():\n    assert True\n")
    ws = Workspace(tmp_path)
    (tmp_path / "test_added_later.py").write_text("def test_b():\n    assert True\n")
    assert ws.spec_tests == ["test_spec.py"]


def test_the_score_ignores_tests_added_after_construction(tmp_path):
    """The denominator has to stay fixed or the history is meaningless."""
    (tmp_path / "test_spec.py").write_text("def test_a():\n    assert True\n")
    ws = Workspace(tmp_path)
    (tmp_path / "test_added.py").write_text(
        "def test_b():\n    assert True\n\n\ndef test_c():\n    assert True\n"
    )
    assert ws.test_report().total == 1


def test_the_finish_gate_does_include_the_added_tests(tmp_path):
    """The point of letting the architect write tests is that they BIND the
    developer. A gate that ignored them would make the whole stage decorative."""
    (tmp_path / "test_spec.py").write_text("def test_a():\n    assert True\n")
    ws = Workspace(tmp_path)
    assert ws.tests_pass()
    (tmp_path / "test_added.py").write_text("def test_b():\n    assert False\n")
    assert not ws.tests_pass()


def test_a_workspace_with_no_spec_files_scores_everything(tmp_path):
    """A plain `solve` against someone's own directory has no snapshot to speak
    of, and should still report what ran."""
    ws = Workspace(tmp_path)
    (tmp_path / "test_later.py").write_text("def test_a():\n    assert True\n")
    assert ws.test_report().total == 1
