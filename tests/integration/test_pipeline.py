"""End-to-end tests against the shipped binary.

EVERY TEST HERE IS A REGRESSION TEST FOR A BUG THAT REACHED A LIVE RUN, and each
was invisible to the Go tests for the same reason: those build the object they
test, so they agree with whatever the department gets wrong. These start the real
binary and read what it actually does.
"""

from __future__ import annotations

import json
import re
import time

import pytest


class Agent:
    """A model that behaves like an agent which finishes.

    A reply that never changes makes the loop read the same file until its
    ceiling — which exercises the ceiling, not the pipeline. This reads once,
    writes once, and lets the verification carry it to the end.
    """

    def __init__(self):
        self.turns = 0

    def __call__(self, messages) -> str:
        system = next((m.get("content", "") for m in messages
                       if m.get("role") == "system"), "")
        if "specification author" in system or "software developer" in system \
                or "coverage author" in system or "reconciling" in system:
            self.turns += 1
            if self.turns == 1:
                return json.dumps({"action": "read_files", "paths": ["main.go"]})
            return json.dumps({
                "action": "write_files",
                "summary": "add the store",
                "type": "feat",
                "edits": [{"path": "main.go", "decl": "main",
                           "replace": "func main() {\n\tprintln(\"ready\")\n}"}],
            })
        # Anything else — the product manager, the reviewer — gets a shape it
        # can parse and nothing to do.
        return json.dumps({"tasks": [], "sections": [],
                           "verdict": "clean", "summary": "nothing found",
                           "findings": []})


# --- the bug that started this ------------------------------------------------

def test_every_stage_is_given_a_system_prompt(department, plane):
    """A stage with no brief has no rules, and guesses at them for many turns.

    Options.SystemPrompt was a field nothing in the department set, so every
    stage the dev loop serves ran with an EMPTY system message. On a live run a
    developer spent fifteen turns on one failing assertion, inventing rules it
    had never been told.
    """
    plane.reply = Agent()
    tid = plane.add_ticket("Define the store", "ready_for_dev",
                           description="Implement it.")

    svc = department()
    svc.wait_for("stage finished", timeout=60)

    prompts = plane.rec.system_prompts()
    assert prompts, "the department made no model call at all"

    empty = [i for i, p in enumerate(prompts) if not p.strip()]
    assert not empty, (
        f"{len(empty)} of {len(prompts)} model calls were made with an empty "
        f"system prompt — those stages have no statement of their job, no rule "
        f"about test files and no account of the edit tool"
    )
    assert plane.rec.moves, "the ticket was never moved out of its queue"


def test_the_developers_brief_states_the_rule_the_pipeline_rests_on(department, plane):
    """It may not edit test files. That is the one rule everything else assumes."""
    plane.reply = Agent()
    plane.add_ticket("Define the store", "ready_for_dev", description="Implement it.")

    svc = department()
    svc.wait_for("stage finished", timeout=40)

    dev_prompts = [p for p in plane.rec.system_prompts() if "software developer" in p]
    assert dev_prompts, "no developer prompt was ever sent"
    brief = dev_prompts[0]
    assert "_test.go" in brief, f"the developer is never told about test files:\n{brief}"
    assert "old_str" in brief, (
        "the brief does not describe the edit tool this build actually offers; a "
        "brief documenting a different editor is worse than none"
    )


# --- the sandbox bugs ---------------------------------------------------------

def test_leased_commands_run_in_the_checkout(department, plane):
    """A lease already holds a checkout; moving elsewhere runs outside the repo.

    On a live run every command ran in an empty /tmp/work while the clone sat at
    /workspace, so `git status` answered "not a git repository", every
    verification failed, and the agent ground to its turn ceiling.
    """
    plane.reply = Agent()
    plane.add_ticket("Define the store", "ready_for_dev", description="Implement it.")

    svc = department()
    svc.wait_for("stage finished", timeout=40)

    leased = [s for s in plane.rec.scripts() if "git ls-files" in s or "===FILE " in s]
    assert leased, "no leased command was ever run"
    for script in leased:
        assert "cd /tmp/work" not in script, (
            f"a leased command moves out of the checkout the lease made for it:\n{script}"
        )


def test_a_public_repository_is_cloned_without_an_empty_credential(department, plane):
    """Sending secret_refs {"GIT_CLONE_URL": ""} earns a 400 and kills the ticket."""
    plane.reply = Agent()
    plane.add_ticket("Define the store", "ready_for_dev", description="Implement it.")

    svc = department()
    svc.wait_for("stage finished", timeout=40)

    assert plane.rec.leases, "no lease was ever created"
    for lease in plane.rec.leases:
        refs = lease.get("secret_refs") or {}
        assert refs.get("GIT_CLONE_URL", "x") != "", (
            "an EMPTY credential reference was sent; the forge rejects it and the "
            "ticket dies before anything runs"
        )
        if not refs:
            env = lease.get("env") or {}
            assert env.get("GIT_CLONE_URL"), (
                "with no credential the clone URL must travel as a plain env var, "
                "or the sandbox has nothing to clone"
            )


def test_the_lease_carries_the_toolchain_environment(department, plane):
    """A leased command exports nothing itself, so the lease must carry it."""
    plane.reply = Agent()
    plane.add_ticket("Define the store", "ready_for_dev", description="Implement it.")

    svc = department()
    svc.wait_for("stage finished", timeout=40)

    assert plane.rec.leases, "no lease was ever created"
    env = plane.rec.leases[0].get("env") or {}
    for name in ("HOME", "GOCACHE", "GOMODCACHE"):
        assert env.get(name), (
            f"the lease does not set {name}; against a read-only root the "
            f"toolchain fails in several unrelated-looking ways"
        )


# --- what the board can tell you ---------------------------------------------

def test_the_work_a_stage_does_reaches_the_transcript(department, plane, tmp_path):
    """No stage recorded a single action until this was wired.

    The board could say which column a ticket sat in and never what was being
    done to it, because the activity feed rendered from a stream with no
    producer.
    """
    plane.reply = Agent()
    plane.add_ticket("Define the store", "ready_for_dev", description="Implement it.")

    svc = department()
    svc.wait_for("stage finished", timeout=40)

    files = list((tmp_path / "transcripts").glob("transcripts-*.jsonl"))
    assert files, "no transcript was written at all"

    kinds = []
    for f in files:
        for line in f.read_text(errors="replace").splitlines():
            try:
                kinds.append(json.loads(line).get("kind"))
            except Exception:
                pass
    assert "action" in kinds, (
        f"the transcript holds no action records, only {sorted(set(kinds))} — "
        f"nothing can say what any stage actually did"
    )


def test_a_ticket_that_fails_is_tried_again(department, plane):
    """A failed attempt must return the ticket to a queue that will select it.

    The two ceilings have to agree: selection refuses a ticket once its spent
    attempts reach the maximum, so escalation has to happen exactly then. If they
    disagree the ticket goes back to a queue that will never look at it again and
    nothing on the board says why — a stall with no explanation, which is the
    single most expensive failure this pipeline has.
    """
    plane.reply = lambda _messages: "this is not a tool call"
    plane.add_ticket("Define the store", "ready_for_dev", description="Implement it.")

    svc = department()
    deadline = time.time() + 90
    while time.time() < deadline:
        if svc.output().count("status=failed") >= 2:
            break
        time.sleep(0.5)
    else:
        pytest.fail(
            "a ticket failed once and was never attempted again. It is not "
            f"blocked and not moving:\n{svc.output()}"
        )


def test_an_escalated_ticket_says_why_on_itself(department, plane):
    """A ticket that stops needs its reason where a person reads it.

    One was escalated carrying nothing but its claim comments: the board said
    "BLOCKED — needs you" and the ticket gave no reason at all. The cause was in
    the service log, which is the one place a person reading the board is not
    looking, and on a restarted host is not kept either.

    The attempt ceiling is not configurable, so this waits out all three.
    """
    plane.reply = lambda _messages: "this is not a tool call"
    tid = plane.add_ticket("Define the store", "ready_for_dev", description="Implement it.")

    svc = department()
    deadline = time.time() + 90
    while time.time() < deadline and plane.status_of(tid) != "blocked":
        time.sleep(0.5)
    assert plane.status_of(tid) == "blocked", (
        f"the ticket ended in {plane.status_of(tid)}, not blocked:\n{svc.output()}"
    )

    said = "\n".join(plane.rec.comments_on(tid))
    assert "stopped after" in said, (
        f"the ticket was escalated with no explanation on it:\n{said}"
    )
    assert "dev-agent" in said, f"the note does not say which stage gave up:\n{said}"
    assert "unparseable" in said, (
        "the note does not quote what actually went wrong. A note that names the "
        f"symptom and not the cause sends a correct reader to the wrong place:\n{said}"
    )
