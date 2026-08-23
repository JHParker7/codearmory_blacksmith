"""The report `python -m blacksmith` prints."""

from blacksmith import workflow as wf
from blacksmith.__main__ import describe
from blacksmith.config import load_config


def test_it_names_the_instance_and_the_board():
    """Which instance a host is pointed at is the first thing anybody asks."""
    out = describe(
        load_config(env={"CODEARMORY_URL": "https://codearmory.example", "AGENTS_BOARD_ID": "B-1"})
    )
    assert "codearmory.example" in out and "B-1" in out


def test_it_says_when_a_host_cannot_work_the_board():
    """A host with no serving class can read but not work, and that should not
    have to be inferred from an idle board."""
    assert "cannot work it" in describe(load_config(env={}))


def test_it_lists_the_columns_this_host_would_take_from():
    out = describe(load_config(env={}))
    assert wf.COL_READY_FOR_DEV in out and wf.COL_IN_DEV in out


def test_a_stage_the_host_does_not_run_is_absent_from_the_report():
    assert wf.ROLE_COVERAGE not in describe(load_config(env={"AGENTS_COVERAGE": "0"}))
    assert wf.ROLE_COVERAGE in describe(load_config(env={}))


def test_a_credential_is_not_printed():
    """This output goes into bug reports."""
    assert "s3cret" not in describe(load_config(env={"CODEARMORY_TOKEN": "s3cret"}))


def test_it_says_plainly_that_no_agents_are_registered():
    """A process that polls a board and can never work anything looks exactly
    like a department whose model is down."""
    assert "no agent handlers are registered" in describe(load_config(env={}))
