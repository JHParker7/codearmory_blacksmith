"""Fixtures: build the real binary, point it at a fake plane, run it.

THE BINARY IS THE REAL ONE. These tests are here because the Go tests each build
their own object and so agree with whatever the department gets wrong; running
the shipped binary is the only way to see what it is actually configured with.
"""

from __future__ import annotations

import os
import subprocess
import sys
import time
from pathlib import Path

import pytest

sys.path.insert(0, str(Path(__file__).parent))
from plane import Plane  # noqa: E402

REPO = Path(__file__).resolve().parents[2]


@pytest.fixture(scope="session")
def binary(tmp_path_factory) -> Path:
    """The blacksmith binary, built from this working tree."""
    out = tmp_path_factory.mktemp("bin") / "blacksmith"
    r = subprocess.run(
        ["go", "build", "-o", str(out), "."],
        cwd=REPO, capture_output=True, text=True,
        env={**os.environ, "GOWORK": "off"},
    )
    if r.returncode != 0:
        pytest.fail(f"could not build blacksmith:\n{r.stderr}")
    return out


@pytest.fixture
def plane():
    """A plane whose model answers nothing useful, so a test can set its reply."""
    p = Plane(reply=lambda _messages: "")
    p.start()
    yield p
    p.stop()


def env_for(plane: Plane, tmp_path: Path, **overrides) -> dict:
    """The operator environment pointing blacksmith at the fake plane.

    NO INLINE COMMENTS AFTER A VALUE anywhere near this: systemd does not strip
    them, and blacksmith reads the same file systemd would.
    """
    env = {
        "AGENTS_HOST": "test-host",
        "AGENTS_BOARD_ID": "board-1",
        "AGENTS_TICKETS_URL": plane.urls["tickets"],
        "AGENTS_FORGE_URL": plane.urls["forge"],
        "AGENTS_FORGE_GATEKEEPER_URL": plane.urls["gatekeeper"],
        "AGENTS_FORGE_EMAIL": "test@blacksmith.invalid",
        "AGENTS_FORGE_PASSWORD": "test",
        "AGENTS_LARGE_ENDPOINT": plane.urls["model"] + "/v1",
        "AGENTS_LARGE_MODEL": "test-model",
        "AGENTS_LARGE_SLOTS": "1",
        "AGENTS_SMALL_ENDPOINT": plane.urls["model"] + "/v1",
        "AGENTS_SMALL_MODEL": "test-model",
        "AGENTS_SMALL_SLOTS": "1",
        "AGENTS_REPO_URL": "git://git.invalid/demo.git",
        "AGENTS_REPO_BRANCH": "dev",
        "AGENTS_REPO_IMAGE": "golang:1.25",
        "AGENTS_TRANSCRIPT_DIR": str(tmp_path / "transcripts"),
        "AGENTS_POLL": "1s",
        "AGENTS_ARCHITECT": "false",
    }
    env.update(overrides)
    return env


class Service:
    """A running department, stopped when the test ends."""

    def __init__(self, proc: subprocess.Popen, log: Path):
        self.proc = proc
        self.log = log

    def output(self) -> str:
        return self.log.read_text(errors="replace")

    def wait_for(self, needle: str, timeout: float = 25.0) -> str:
        """Wait for a line, and fail with the log rather than a bare timeout."""
        end = time.time() + timeout
        while time.time() < end:
            out = self.output()
            if needle in out:
                return out
            if self.proc.poll() is not None:
                pytest.fail(f"the department exited early "
                            f"(code {self.proc.returncode}):\n{out}")
            time.sleep(0.2)
        pytest.fail(f"never saw {needle!r} within {timeout}s. Log:\n{self.output()}")

    def stop(self) -> None:
        if self.proc.poll() is None:
            self.proc.terminate()
            try:
                self.proc.wait(timeout=10)
            except subprocess.TimeoutExpired:
                self.proc.kill()
                self.proc.wait(timeout=5)


@pytest.fixture
def department(binary, plane, tmp_path):
    """Start `blacksmith service` against the fake plane."""
    started: list[Service] = []

    def start(**overrides) -> Service:
        envfile = tmp_path / "env"
        envfile.write_text(
            "\n".join(f"{k}={v}" for k, v in env_for(plane, tmp_path, **overrides).items())
            + "\n"
        )
        log = tmp_path / "service.log"
        fh = log.open("w")
        proc = subprocess.Popen(
            [str(binary), "service"],
            stdout=fh, stderr=subprocess.STDOUT, cwd=tmp_path,
            env={**os.environ, "AGENTS_ENV_FILE": str(envfile)},
        )
        svc = Service(proc, log)
        started.append(svc)
        svc.wait_for("department ready")
        return svc

    yield start
    for s in started:
        s.stop()
