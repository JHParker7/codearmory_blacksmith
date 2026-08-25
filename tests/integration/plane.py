"""A fake CodeArmory plane: the ticket store, the forge and the model endpoint.

WHY A FAKE PLANE RATHER THAN THE REAL ONE. These tests exist to catch wiring —
a stage assembled without a brief, a branch published under a marker nothing
reads, a leased command run outside the checkout. Every one of those bugs was
invisible to the Go unit tests because each built its own object, and invisible
to a live run because a live run only tells you it was slow.

So the plane here is a RECORDER as much as a server. It answers well enough for
the department to make progress, and it keeps every request, which is what the
assertions are actually about.
"""

from __future__ import annotations

import json
import re
import threading
from datetime import datetime, timezone
from urllib.parse import unquote
import uuid
from dataclasses import dataclass, field
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer


@dataclass
class Recorded:
    """Everything the plane was asked to do, for the assertions to read."""

    chats: list[dict] = field(default_factory=list)
    executions: list[dict] = field(default_factory=list)
    leases: list[dict] = field(default_factory=list)
    comments: list[tuple[str, str]] = field(default_factory=list)
    moves: list[tuple[str, str]] = field(default_factory=list)
    requests: list[str] = field(default_factory=list)

    def system_prompts(self) -> list[str]:
        """The system message of every model call, in order."""
        out = []
        for c in self.chats:
            for m in c.get("messages", []):
                if m.get("role") == "system":
                    out.append(m.get("content") or "")
                    break
            else:
                out.append("")
        return out

    def scripts(self) -> list[str]:
        """Every shell script the department asked the forge to run."""
        out = []
        for e in self.executions:
            cmd = e.get("command") or []
            if len(cmd) >= 3:
                out.append(cmd[2])
        return out

    def comments_on(self, ticket_id: str) -> list[str]:
        return [body for tid, body in self.comments if tid == ticket_id]


class Plane:
    """The three services blacksmith talks to, on one port each."""

    def __init__(self, reply):
        # reply(messages) -> str is the model's answer, supplied per test.
        self.reply = reply
        self.rec = Recorded()
        self.tickets: dict[str, dict] = {}
        self._lock = threading.Lock()
        self._seq = 0
        self._servers: list[ThreadingHTTPServer] = []
        self.urls: dict[str, str] = {}

    # ---- lifecycle -------------------------------------------------------

    def start(self) -> "Plane":
        for name in ("tickets", "forge", "gatekeeper", "model"):
            srv = ThreadingHTTPServer(("127.0.0.1", 0), self._handler(name))
            threading.Thread(target=srv.serve_forever, daemon=True).start()
            self._servers.append(srv)
            self.urls[name] = f"http://127.0.0.1:{srv.server_address[1]}"
        return self

    def stop(self) -> None:
        for s in self._servers:
            s.shutdown()

    # ---- board -----------------------------------------------------------

    def add_ticket(self, title: str, status: str, **extra) -> str:
        tid = str(uuid.uuid4())
        with self._lock:
            self.tickets[tid] = {
                "ticket_id": tid,
                "title": title,
                "status": status,
                "priority": "high",
                "description": extra.pop("description", ""),
                "board_id": "board-1",
                "comments": [],
                "depends_on": [],
                "created_at": self._stamp(),
                "updated_at": self._stamp(),
                "version": 1,
                **extra,
            }
        return tid

    def _stamp(self) -> str:
        """A REAL timestamp, monotonic within the run.

        Claims older than record.StaleAfter are pruned as abandoned, and the
        attempt ceiling counts what survives. Stamping comments at a fixed
        wall-clock time made every claim look fifteen hours old, so the count
        never rose and a ticket that could not succeed retried forever instead
        of escalating — a fake that cannot age its own records cannot exercise
        the ceiling at all.

        The microsecond field carries the sequence so ordering stays total even
        when two comments land in the same millisecond; the claim protocol
        arbitrates on created_at first and a tie there is decided by id.
        """
        now = datetime.now(timezone.utc)
        return now.replace(microsecond=self._seq % 1000000).isoformat().replace(
            "+00:00", "Z")

    def ticket(self, tid: str) -> dict:
        with self._lock:
            return json.loads(json.dumps(self.tickets[tid]))

    def status_of(self, tid: str) -> str:
        return self.ticket(tid)["status"]

    # ---- request handling ------------------------------------------------

    def _handler(plane, service):  # noqa: N805 - the outer self is the plane
        class H(BaseHTTPRequestHandler):
            protocol_version = "HTTP/1.1"

            def log_message(self, *_):  # silence
                pass

            def _send(self, code, payload=None):
                body = b"" if payload is None else json.dumps(payload).encode()
                try:
                    self.send_response(code)
                    self.send_header("Content-Type", "application/json")
                    self.send_header("Content-Length", str(len(body)))
                    self.end_headers()
                    if body:
                        self.wfile.write(body)
                except (BrokenPipeError, ConnectionResetError):
                    # EXPECTED AT TEARDOWN. The department is terminated while a
                    # poll is in flight, so the client goes away mid-reply. That
                    # is the test ending, not a fault, and raising here turns
                    # every passing test into a failure about a closed socket.
                    pass

            def handle_one_request(self):
                try:
                    super().handle_one_request()
                except (BrokenPipeError, ConnectionResetError):
                    self.close_connection = True

            def _read(self):
                n = int(self.headers.get("Content-Length") or 0)
                if not n:
                    return {}
                try:
                    return json.loads(self.rfile.read(n))
                except Exception:
                    return {}

            def _note(self, verb):
                with plane._lock:
                    plane.rec.requests.append(f"{verb} {service}{self.path}")

            def do_GET(self):
                self._note("GET")
                getattr(plane, f"_get_{service}")(self)

            def do_POST(self):
                self._note("POST")
                getattr(plane, f"_post_{service}")(self)

            def do_PUT(self):
                self._note("PUT")
                getattr(plane, f"_put_{service}")(self)

            def do_DELETE(self):
                self._send(204)

        return H

    # -- gatekeeper: a token, always ---------------------------------------

    def _post_gatekeeper(self, h):
        h._send(200, {"token": "test-token"})

    def _get_gatekeeper(self, h):
        h._send(200, {})

    # -- model -------------------------------------------------------------

    def _post_model(self, h):
        req = h._read()
        with self._lock:
            self.rec.chats.append(req)
        content = self.reply(req.get("messages") or [])
        h._send(200, {
            "choices": [{"message": {"role": "assistant", "content": content},
                         "finish_reason": "stop"}],
            "usage": {"prompt_tokens": 10, "completion_tokens": 10},
        })

    def _get_model(self, h):
        h._send(200, {"object": "list", "data": []})

    # -- forge -------------------------------------------------------------

    def _post_forge(self, h):
        body = h._read()
        if h.path.rstrip("/").endswith("/leases"):
            with self._lock:
                self.rec.leases.append(body)
            h._send(200, {"lease_id": "lease-1", "status": "ready"})
            return
        with self._lock:
            self.rec.executions.append(body)
        h._send(200, {"execution_id": "exec-1", "status": "completed",
                      "exit_code": 0, "stdout": self._forge_stdout(body)})

    def _get_forge(self, h):
        if "/leases/" in h.path:
            h._send(200, {"lease_id": "lease-1", "status": "ready"})
            return
        h._send(200, {"execution_id": "exec-1", "status": "completed",
                      "exit_code": 0, "stdout": ""})

    def _put_forge(self, h):
        h._send(200, {})

    def _forge_stdout(self, body) -> str:
        """Answer a script with something plausible for what it asked."""
        cmd = (body.get("command") or ["", "", ""])[-1]
        if "git ls-files" in cmd:
            return "main.go\nmain_test.go\n"
        if "===FILE " in cmd:
            return "===FILE main.go\npackage main\n\nfunc main() {}\n"
        return ""

    # -- tickets -----------------------------------------------------------

    def _get_tickets(self, h):
        m = re.match(r"^/tickets/([0-9a-f-]+)$", h.path.split("?")[0])
        if m:
            with self._lock:
                t = self.tickets.get(m.group(1))
            h._send(200 if t else 404, t or {"error": "no such ticket"})
            return
        if h.path.split("?")[0] == "/tickets":
            # A BARE ARRAY, and filtered by the query the caller sent. The store
            # answers listings that way, and a dispatcher polls one column at a
            # time — a fake that returns every ticket to every stage would have
            # them all claiming the same work.
            q = {}
            if "?" in h.path:
                for part in h.path.split("?", 1)[1].split("&"):
                    if "=" in part:
                        k, v = part.split("=", 1)
                        q[k] = unquote(v)
            with self._lock:
                rows = list(self.tickets.values())
            if q.get("board_id"):
                rows = [t for t in rows if t.get("board_id") == q["board_id"]]
            if q.get("status"):
                rows = [t for t in rows if t.get("status") == q["status"]]
            h._send(200, rows)
            return
        h._send(200, [])

    def _post_tickets(self, h):
        body = h._read()
        m = re.match(r"^/tickets/([0-9a-f-]+)/comments$", h.path)
        if m:
            tid = m.group(1)
            with self._lock:
                self.rec.comments.append((tid, body.get("body", "")))
                # ORDERED, AND STABLY SO. The claim protocol arbitrates by
                # created_at and then by id, so comments that all share a
                # timestamp and carry random ids make the winner random — the
                # claimant then concludes it lost to itself, releases, and
                # retries forever. A fake that cannot order its own appends
                # cannot exercise claiming at all.
                self._seq += 1
                seq = self._seq
                cid = f"c-{seq:06d}"
                created = self._stamp()
                if tid in self.tickets:
                    self.tickets[tid]["comments"].append({
                        "comment_id": cid,
                        "ticket_id": tid,
                        "body": body.get("body", ""),
                        "created_at": created,
                    })
            h._send(201, {"comment_id": cid, "ticket_id": tid,
                          "body": body.get("body", ""), "created_at": created})
            return
        tid = self.add_ticket(body.get("title", ""), body.get("status", "inbox"),
                              description=body.get("description", ""))
        h._send(201, self.ticket(tid))

    def _put_tickets(self, h):
        body = h._read()
        m = re.match(r"^/tickets/([0-9a-f-]+)$", h.path)
        if not m:
            h._send(404, {})
            return
        tid = m.group(1)
        with self._lock:
            t = self.tickets.get(tid)
            if t is None:
                h._send(404, {})
                return
            if "status" in body and body["status"]:
                self.rec.moves.append((tid, body["status"]))
                t["status"] = body["status"]
            t["version"] += 1
        h._send(200, self.ticket(tid))
