"""Reading this host's configuration.

The file is a systemd `EnvironmentFile`, and the parser must match systemd's
rules rather than a shell's. Getting that wrong is not a theoretical risk: an
inline comment after a value made blacksmith crash-loop silently, because
`FOO=2 # note` sets `2 # note` and nothing reported a bad integer until much
later and somewhere else.
"""

import pytest

from blacksmith.config import Config, ModelClass, load_config, read_env_file


def write(tmp_path, body: str):
    path = tmp_path / "env"
    path.write_text(body)
    return path


# --- systemd's EnvironmentFile rules -------------------------------------


def test_key_value_lines_are_read(tmp_path):
    assert read_env_file(write(tmp_path, "AGENTS_HOST=box-a\n")) == {"AGENTS_HOST": "box-a"}


def test_an_inline_comment_is_part_of_the_value(tmp_path):
    """systemd does not strip it. Pretending otherwise here would make the file
    behave one way under the unit and another way under the department, and the
    department's reading is the one that is wrong."""
    assert read_env_file(write(tmp_path, "AGENTS_SLOTS=2 # four is too many\n")) == {
        "AGENTS_SLOTS": "2 # four is too many"
    }


def test_a_whole_line_comment_is_skipped(tmp_path):
    assert read_env_file(write(tmp_path, "# the large class\nAGENTS_HOST=box-a\n")) == {
        "AGENTS_HOST": "box-a"
    }


def test_blank_lines_are_skipped(tmp_path):
    assert read_env_file(write(tmp_path, "\n\nAGENTS_HOST=box-a\n\n")) == {"AGENTS_HOST": "box-a"}


def test_surrounding_quotes_are_removed(tmp_path):
    assert read_env_file(write(tmp_path, 'A="one two"\nB=\'three\'\n')) == {"A": "one two", "B": "three"}


def test_the_value_is_otherwise_literal(tmp_path):
    """A shell-style unquote would turn a password containing $ into something
    else — usually into nothing, which reads as an empty credential."""
    assert read_env_file(write(tmp_path, "AGENTS_FORGE_PASSWORD=pa$$word`x`\n")) == {
        "AGENTS_FORGE_PASSWORD": "pa$$word`x`"
    }


def test_a_value_containing_an_equals_sign_survives(tmp_path):
    assert read_env_file(write(tmp_path, "CODEARMORY_TOKEN=abc=def==\n")) == {
        "CODEARMORY_TOKEN": "abc=def=="
    }


def test_a_line_with_no_equals_is_ignored(tmp_path):
    assert read_env_file(write(tmp_path, "this is not a setting\nA=1\n")) == {"A": "1"}


def test_an_empty_value_is_kept(tmp_path):
    """Empty and absent are different: an empty value is a deliberate override of
    a default."""
    assert read_env_file(write(tmp_path, "AGENTS_BOARD_ID=\n")) == {"AGENTS_BOARD_ID": ""}


def test_a_missing_file_is_not_an_error(tmp_path):
    """Plenty of hosts configure the process some other way."""
    assert read_env_file(tmp_path / "nope") == {}


def test_a_directory_where_the_file_should_be_is_not_an_error(tmp_path):
    assert read_env_file(tmp_path) == {}


# --- what wins ------------------------------------------------------------


def test_the_file_fills_in_what_the_environment_does_not_set(tmp_path):
    cfg = load_config(env={}, env_file=write(tmp_path, "AGENTS_HOST=from-file\n"))
    assert cfg.host == "from-file"


def test_an_explicit_setting_beats_the_file(tmp_path):
    """An export on the command line is a deliberate override and must win."""
    cfg = load_config(env={"AGENTS_HOST": "from-env"}, env_file=write(tmp_path, "AGENTS_HOST=from-file\n"))
    assert cfg.host == "from-env"


def test_the_file_location_can_be_moved(tmp_path):
    """Nothing exports the operator file into an interactive shell, so a command
    run by hand saw none of it and failed on configuration that was plainly
    present."""
    moved = write(tmp_path, "AGENTS_HOST=elsewhere\n")
    assert load_config(env={"AGENTS_ENV_FILE": str(moved)}).host == "elsewhere"


# --- the model classes ----------------------------------------------------


def test_a_class_with_an_endpoint_is_configured():
    cfg = load_config(
        env={
            "AGENTS_LARGE_ENDPOINT": "http://localhost:8080",
            "AGENTS_LARGE_MODEL": "qwen3-coder",
            "AGENTS_LARGE_SLOTS": "4",
            "AGENTS_LARGE_QUEUE": "16",
        }
    )
    large = cfg.classes["large"]
    assert large == ModelClass(
        name="large", endpoint="http://localhost:8080", model="qwen3-coder", slots=4, queue=16
    )


def test_a_class_with_no_endpoint_is_simply_absent():
    """A supported deployment, not an error: a host running one card serves one or
    two classes. A role assigned to a missing class fails loudly at dispatch
    rather than silently falling back to a weaker model, which would be an
    invisible quality regression."""
    cfg = load_config(env={"AGENTS_LARGE_ENDPOINT": "http://localhost:8080"})
    assert set(cfg.classes) == {"large"}


def test_a_host_with_no_classes_says_so_rather_than_starting():
    cfg = load_config(env={})
    assert cfg.classes == {} and not cfg.can_serve


def test_a_bad_slot_count_is_reported_where_it_is_configured():
    """This is the failure the inline-comment trap produced. A number that will
    not parse must not become a default that quietly halves the box."""
    with pytest.raises(ValueError) as raised:
        load_config(env={"AGENTS_LARGE_ENDPOINT": "http://x", "AGENTS_LARGE_SLOTS": "4 # per card"})
    assert "AGENTS_LARGE_SLOTS" in str(raised.value)


def test_slots_default_to_one():
    cfg = load_config(env={"AGENTS_LARGE_ENDPOINT": "http://x"})
    assert cfg.classes["large"].slots == 1


# --- the department -------------------------------------------------------


def test_concurrency_defaults_to_the_large_classes_slots():
    """Serving slots are the real constraint, so working more tickets than the box
    can serve only queues them somewhere less visible."""
    cfg = load_config(env={"AGENTS_LARGE_ENDPOINT": "http://x", "AGENTS_LARGE_SLOTS": "4"})
    assert cfg.concurrency == 4


def test_concurrency_can_be_set_explicitly():
    cfg = load_config(
        env={"AGENTS_LARGE_ENDPOINT": "http://x", "AGENTS_LARGE_SLOTS": "4", "AGENTS_CONCURRENCY": "2"}
    )
    assert cfg.concurrency == 2


def test_the_poll_interval_has_a_default():
    assert load_config(env={}).poll == 15.0


def test_capture_is_on_by_default():
    """Opt-out rather than opt-in, because a transcript nobody enabled cannot be
    recovered after the fact."""
    assert load_config(env={}).capture_enabled


def test_capture_is_disabled_by_the_word_off():
    cfg = load_config(env={"AGENTS_TRANSCRIPT_DIR": "off"})
    assert not cfg.capture_enabled


def test_the_host_falls_back_to_the_machine_name():
    """It identifies this host in transcripts and ticket attribution, and 'which
    box made this patch' is not answerable from the agent account alone."""
    assert load_config(env={}).host


def test_agent_authors_have_a_default_and_can_be_replaced():
    assert "pm-agent" in load_config(env={}).agent_authors
    cfg = load_config(env={"AGENTS_AUTHORS": "pm-agent, dev-agent ,sec-agent"})
    assert cfg.agent_authors == ("pm-agent", "dev-agent", "sec-agent")


def test_the_optional_stages_are_off_and_on_by_flag():
    assert load_config(env={}).coverage_enabled
    assert not load_config(env={}).architect_enabled
    assert load_config(env={"AGENTS_ARCHITECT": "1"}).architect_enabled
    assert not load_config(env={"AGENTS_COVERAGE": "0"}).coverage_enabled


def test_a_flag_accepts_the_words_a_person_would_write():
    for yes in ("1", "true", "TRUE", "yes", "on"):
        assert load_config(env={"AGENTS_ARCHITECT": yes}).architect_enabled, yes
    for no in ("0", "false", "no", "off", ""):
        assert not load_config(env={"AGENTS_ARCHITECT": no}).architect_enabled, no


def test_an_unrecognised_flag_value_is_reported_rather_than_guessed():
    """Silently reading 'maybe' as false turns a typo into a stage that is simply
    not running, which on a board looks like a stage that is broken."""
    with pytest.raises(ValueError):
        load_config(env={"AGENTS_ARCHITECT": "maybe"})


# --- secrets --------------------------------------------------------------


def test_a_credential_does_not_appear_in_the_repr():
    """Configuration is logged at startup and dumped into bug reports."""
    cfg = load_config(env={"CODEARMORY_TOKEN": "s3cret", "CODEARMORY_URL": "https://x"})
    assert "s3cret" not in repr(cfg)
    assert cfg.platform_token == "s3cret"


def test_the_url_is_still_visible():
    """Which instance a host is pointed at is the first thing anybody asks."""
    cfg = load_config(env={"CODEARMORY_URL": "https://codearmory.example"})
    assert "codearmory.example" in repr(cfg)


def test_a_config_is_immutable():
    with pytest.raises((AttributeError, TypeError)):
        load_config(env={}).host = "somewhere-else"


def test_surrounding_whitespace_is_trimmed_from_keys_and_values(tmp_path):
    assert read_env_file(write(tmp_path, "  AGENTS_HOST  =  box-a  \n")) == {"AGENTS_HOST": "box-a"}


def test_a_config_reports_which_columns_it_will_work():
    """A host should be able to say what it is running without starting it."""
    cfg = load_config(env={"AGENTS_COVERAGE": "0"})
    assert isinstance(cfg, Config)
    assert "coverage-agent" not in {s.role for s in cfg.routing().stages()}
