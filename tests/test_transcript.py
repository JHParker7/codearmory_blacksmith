"""JSONL transcripts: what the department did, and what it was refused.

A transcript is the only durable record of an attempt. The platform keeps the
ticket and the branch; this keeps the turns, the actions and — the part that was
missing and cost the most — the rejections.
"""

import json
from datetime import datetime, timezone

import pytest

from blacksmith.outcomes import Outcome, RefusalKind
from blacksmith.transcript import JSONLSink, Record, RecordKind, Recorder

NOW = datetime(2026, 8, 21, 12, 0, tzinfo=timezone.utc)


class MemorySink:
    def __init__(self) -> None:
        self.records: list[Record] = []
        self.closed = False

    def write(self, record: Record) -> None:
        self.records.append(record)

    def close(self) -> None:
        self.closed = True


class BrokenSink:
    def __init__(self) -> None:
        self.attempts = 0

    def write(self, record: Record) -> None:
        self.attempts += 1
        raise OSError("no space left on device")

    def close(self) -> None:
        pass


def recorder(sink=None) -> tuple[Recorder, MemorySink]:
    sink = sink or MemorySink()
    return Recorder(sink, host="box-a"), sink


# --- what a record carries ------------------------------------------------


def test_every_record_carries_the_host():
    """Several agent hosts may attach to one CodeArmory. Without this, dev-agent
    on three machines is one identity for three actors and a bad patch cannot be
    traced to the box that made it."""
    rec, sink = recorder()
    rec.start("T-1@box-a", task_id="T-1", role="dev-agent")
    assert all(r.host == "box-a" for r in sink.records)


def test_records_are_sequenced_within_the_host():
    rec, sink = recorder()
    t = rec.start("T-1@box-a", task_id="T-1", role="dev-agent")
    t.action("shell", "go test ./...")
    t.finish(Outcome.SUCCESS, "tests pass")
    assert [r.seq for r in sink.records] == [1, 2, 3]


def test_a_record_is_stamped_when_it_is_written():
    rec, sink = recorder()
    rec.start("T-1@box-a", task_id="T-1", role="dev-agent")
    assert sink.records[0].at is not None and sink.records[0].at.tzinfo is not None


def test_a_transcript_opens_with_what_the_task_is_and_who_is_doing_it():
    rec, sink = recorder()
    rec.start("T-1@box-a", task_id="T-1", role="dev-agent")
    opened = sink.records[0]
    assert opened.kind is RecordKind.START
    assert (opened.transcript_id, opened.task_id, opened.role) == ("T-1@box-a", "T-1", "dev-agent")


def test_an_outcome_closes_it():
    """Its ABSENCE in the log is itself a signal: it marks a task that never
    finished."""
    rec, sink = recorder()
    rec.start("T-1@box-a", task_id="T-1", role="dev-agent").finish(Outcome.FAILED, "gave up")
    closed = sink.records[-1]
    assert closed.kind is RecordKind.OUTCOME
    assert (closed.status, closed.detail) == ("failed", "gave up")


def test_the_transcript_id_is_inherited_by_every_record():
    """It is the join key. A turn that lost it is a turn belonging to no attempt."""
    rec, sink = recorder()
    t = rec.start("T-1@box-a", task_id="T-1", role="dev-agent")
    t.turn(model="qwen", completion="ok", prompt_tokens=10, completion_tokens=2)
    t.refusal(RefusalKind.TEST_FILE, target="thing_test.go", detail="tests are not yours")
    assert {r.transcript_id for r in sink.records} == {"T-1@box-a"}
    assert {r.task_id for r in sink.records} == {"T-1"}
    assert {r.role for r in sink.records} == {"dev-agent"}


def test_a_refusal_names_what_was_refused_and_why():
    """What an agent attempted and why it was refused is the single most useful
    thing to know about a failed attempt, and it was the one thing not written
    down."""
    rec, sink = recorder()
    rec.start("T-1@box-a", task_id="T-1", role="dev-agent").refusal(
        RefusalKind.TEST_FILE, target="thing_test.go", detail="the developer may not write tests"
    )
    refused = sink.records[-1]
    assert refused.kind is RecordKind.REFUSAL
    assert (refused.status, refused.detail) == ("test_file", "the developer may not write tests")
    assert refused.tool == "thing_test.go"


def test_a_failed_turn_is_recorded_rather_than_dropped():
    """A failed turn is training signal, not noise."""
    rec, sink = recorder()
    rec.start("T-1@box-a", task_id="T-1", role="dev-agent").turn(
        model="qwen", error="context deadline exceeded"
    )
    assert sink.records[-1].error == "context deadline exceeded"


def test_a_turn_carries_its_timings_and_usage():
    rec, sink = recorder()
    rec.start("T-1@box-a", task_id="T-1", role="dev-agent").turn(
        model="qwen", completion="ok", prompt_tokens=1200, completion_tokens=64,
        queued_ms=90, latency_ms=4100,
    )
    turn = sink.records[-1]
    assert (turn.prompt_tokens, turn.completion_tokens) == (1200, 64)
    assert (turn.queued_ms, turn.latency_ms) == (90, 4100)


# --- a sink failure never fails the task ---------------------------------


def test_a_broken_sink_does_not_take_the_agent_down():
    """Losing a transcript line is bad; killing a running agent because a disk is
    full is worse."""
    rec, _ = recorder(BrokenSink())
    rec.start("T-1@box-a", task_id="T-1", role="dev-agent").finish(Outcome.SUCCESS, "")


def test_dropped_records_are_counted_rather_than_lost_silently():
    """Non-zero means the training corpus has holes in it."""
    rec, _ = recorder(BrokenSink())
    rec.start("T-1@box-a", task_id="T-1", role="dev-agent").finish(Outcome.SUCCESS, "")
    assert rec.dropped == 2


def test_a_recorder_with_no_sink_is_a_working_no_op():
    """Capture is a setting a host may turn off. It must not become a branch in
    every call site."""
    rec = Recorder(None, host="box-a")
    rec.start("T-1@box-a", task_id="T-1", role="dev-agent").finish(Outcome.SUCCESS, "")
    assert rec.dropped == 0


# --- the file on disk -----------------------------------------------------


def test_records_land_one_json_object_per_line(tmp_path):
    sink = JSONLSink(tmp_path)
    rec = Recorder(sink, host="box-a")
    t = rec.start("T-1@box-a", task_id="T-1", role="dev-agent")
    t.action("shell", "go test ./...")
    t.finish(Outcome.SUCCESS, "tests pass")
    sink.close()

    lines = _only_file(tmp_path).read_text().splitlines()
    assert len(lines) == 3
    assert [json.loads(line)["kind"] for line in lines] == ["start", "action", "outcome"]


def test_a_days_records_are_answerable_from_the_filename(tmp_path):
    """'What did the department do on this date' should not need a grep."""
    sink = JSONLSink(tmp_path)
    sink.write(Record(transcript_id="T-1@box-a", kind=RecordKind.START, at=NOW, host="box-a"))
    sink.close()
    assert _only_file(tmp_path).name == "transcripts-2026-08-21.jsonl"


def test_a_record_from_another_day_opens_another_file(tmp_path):
    sink = JSONLSink(tmp_path)
    sink.write(Record(transcript_id="a", kind=RecordKind.START, at=NOW, host="box-a"))
    sink.write(Record(transcript_id="b", kind=RecordKind.START, at=NOW.replace(day=22), host="box-a"))
    sink.close()
    assert sorted(p.name for p in tmp_path.iterdir()) == [
        "transcripts-2026-08-21.jsonl",
        "transcripts-2026-08-22.jsonl",
    ]


def test_a_second_run_appends_rather_than_truncating(tmp_path):
    """Restarting the host is a normal event. Losing yesterday's evidence to it is
    not."""
    for _ in range(2):
        sink = JSONLSink(tmp_path)
        sink.write(Record(transcript_id="a", kind=RecordKind.START, at=NOW, host="box-a"))
        sink.close()
    assert len(_only_file(tmp_path).read_text().splitlines()) == 2


def test_every_record_is_flushed_as_it_is_written(tmp_path):
    """The process being killed is a normal event on a workstation, and a buffered
    final record is exactly the one worth keeping."""
    sink = JSONLSink(tmp_path)
    sink.write(Record(transcript_id="a", kind=RecordKind.START, at=NOW, host="box-a"))
    assert _only_file(tmp_path).read_text().endswith("\n")  # readable without closing
    sink.close()


def test_the_directory_is_created_if_it_is_not_there(tmp_path):
    JSONLSink(tmp_path / "nested" / "transcripts").close()
    assert (tmp_path / "nested" / "transcripts").is_dir()


def test_the_transcript_directory_is_not_world_readable(tmp_path):
    """Transcripts carry source, prompts and diffs from private repositories."""
    target = tmp_path / "transcripts"
    JSONLSink(target).close()
    assert target.stat().st_mode & 0o077 == 0


def test_empty_fields_are_left_out_of_the_line(tmp_path):
    """A file where every record carries thirty nulls is one nobody reads."""
    sink = JSONLSink(tmp_path)
    sink.write(Record(transcript_id="a", kind=RecordKind.START, at=NOW, host="box-a"))
    sink.close()
    written = json.loads(_only_file(tmp_path).read_text())
    assert "completion" not in written and "prompt_tokens" not in written
    assert written["transcript_id"] == "a"


def test_closing_twice_is_a_no_op(tmp_path):
    sink = JSONLSink(tmp_path)
    sink.close()
    sink.close()


def test_a_write_after_close_reopens_rather_than_raising(tmp_path):
    """Shutdown races the last outcome record. Dropping it loses precisely the
    line that says how the run ended."""
    sink = JSONLSink(tmp_path)
    sink.close()
    sink.write(Record(transcript_id="a", kind=RecordKind.OUTCOME, at=NOW, host="box-a"))
    sink.close()
    assert len(_only_file(tmp_path).read_text().splitlines()) == 1


def test_a_record_encodes_its_enums_as_their_labels(tmp_path):
    sink = JSONLSink(tmp_path)
    sink.write(
        Record(transcript_id="a", kind=RecordKind.OUTCOME, at=NOW, host="box-a", status=Outcome.BLOCKED)
    )
    sink.close()
    assert json.loads(_only_file(tmp_path).read_text())["status"] == "blocked"


def test_an_unwritable_directory_fails_where_it_is_configured(tmp_path):
    """A transcript directory that cannot be created is a misconfiguration, and
    the place to find out is startup rather than the first refusal of the run."""
    blocked = tmp_path / "file"
    blocked.write_text("not a directory")
    with pytest.raises(OSError):
        JSONLSink(blocked / "transcripts")


def _only_file(directory):
    files = sorted(p for p in directory.iterdir() if p.is_file())
    assert len(files) == 1, files
    return files[0]
