"""The HTTP client, against a real server.

The integration tier: unit tests say the logic is right, this says the parts talk
to each other. It stands up an actual HTTP server rather than patching the
transport, because every failure this client exists to handle — a status code, a
missing header, a body that is not JSON — lives in the transport.
"""

import json
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

import pytest

from blacksmith import workflow as wf
from blacksmith.codearmory import CodeArmory
from blacksmith.errors import Conflict, Denied, NotFound, Transient
from blacksmith.tickets import ListOpts, TicketUpdate


class Route:
    """One canned response."""

    def __init__(
        self,
        status: int,
        body=None,
        headers: dict[str, str] | None = None,
        raw: bytes | None = None,
    ) -> None:
        self.status = status
        self.body = body
        self.headers = headers or {}
        #: Bytes sent verbatim — for the bodies that are not JSON at all, which is
        #: the case worth testing and the one a JSON-encoding fake cannot produce.
        self.raw = raw


class Server:
    """An HTTP server that answers from a queue of routes and records requests."""

    def __init__(self) -> None:
        self.routes: list[Route] = []
        self.requests: list[tuple[str, str, dict[str, str], str]] = []
        outer = self

        class Handler(BaseHTTPRequestHandler):
            protocol_version = "HTTP/1.1"

            def _serve(self) -> None:
                length = int(self.headers.get("Content-Length") or 0)
                body = self.rfile.read(length).decode() if length else ""
                outer.requests.append((self.command, self.path, dict(self.headers), body))
                route = outer.routes.pop(0) if outer.routes else Route(200, {})
                if route.raw is not None:
                    payload = route.raw
                else:
                    payload = b"" if route.body is None else json.dumps(route.body).encode()
                self.send_response(route.status)
                for name, value in route.headers.items():
                    self.send_header(name, value)
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(payload)))
                self.end_headers()
                if payload:
                    self.wfile.write(payload)

            do_GET = do_PUT = do_POST = do_PATCH = do_DELETE = _serve

            def log_message(self, *_args) -> None:
                pass

        # Threading, because HTTP/1.1 keep-alive on a single-threaded server makes
        # the NEXT request wait for the previous connection to time out — which
        # turns a fast suite into an eleven-second one and reads as a slow client.
        self._server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        self._server.daemon_threads = True
        # shutdown() waits for serve_forever to notice, at its poll interval. The
        # default half-second is per test, and 23 of them is eleven seconds of a
        # suite that should be instant.
        self._thread = threading.Thread(
            target=self._server.serve_forever, kwargs={"poll_interval": 0.01}, daemon=True
        )
        self._thread.start()

    @property
    def url(self) -> str:
        host, port = self._server.server_address[:2]
        return f"http://{host}:{port}"

    def close(self) -> None:
        self._server.shutdown()
        self._server.server_close()
        self._thread.join(timeout=5)


@pytest.fixture
def server():
    s = Server()
    try:
        yield s
    finally:
        s.close()


def client(server: Server, **kw) -> CodeArmory:
    return CodeArmory(server.url, token="t0ken", retry_backoff=0.0, **kw)


# --- reading --------------------------------------------------------------


async def test_a_listing_decodes_into_tickets(server):
    server.routes.append(Route(200, {"tickets": [{"ticket_id": "T-1", "status": "ready_for_dev"}]}))
    found = await client(server).list_tickets(ListOpts(status=wf.COL_READY_FOR_DEV))
    assert [t.ticket_id for t in found] == ["T-1"]


async def test_a_bare_list_body_decodes_too(server):
    """The service has returned both shapes. A client that handles one of them
    reports an empty board on the day it changes, which reads as an idle
    department rather than as a broken client."""
    server.routes.append(Route(200, [{"ticket_id": "T-1"}]))
    assert len(await client(server).list_tickets(ListOpts())) == 1


async def test_a_listing_filters_server_side(server):
    """`?status=` is what makes the column cheap: a handful of tickets waiting on
    one stage rather than every open ticket on the board."""
    server.routes.append(Route(200, {"tickets": []}))
    await client(server).list_tickets(ListOpts(board_id="B-1", status=wf.COL_READY_FOR_DEV))
    _, path, _, _ = server.requests[0]
    assert "status=ready_for_dev" in path and "board_id=B-1" in path


async def test_the_credential_travels_on_every_request(server):
    server.routes.append(Route(200, {"ticket_id": "T-1"}))
    await client(server).get_ticket("T-1")
    _, _, headers, _ = server.requests[0]
    assert headers["Authorization"] == "Bearer t0ken"


async def test_a_ticket_id_is_escaped_into_the_path(server):
    """An id is data, not a path fragment."""
    server.routes.append(Route(200, {"ticket_id": "a/b"}))
    await client(server).get_ticket("a/b")
    _, path, _, _ = server.requests[0]
    assert "a%2Fb" in path


async def test_the_version_comes_back_as_an_etag(server):
    server.routes.append(Route(200, {"ticket_id": "T-1", "version": 7}, {"ETag": '"7"'}))
    ticket, etag = await client(server).get_ticket_with_etag("T-1")
    assert (ticket.version, etag) == (7, '"7"')


async def test_an_instance_without_etags_reports_no_version_token(server):
    """The absence of an ETag is how the append fallback is CHOSEN. A client that
    invented one here would send If-Match to a server that ignores it, and the
    conditional write would silently degrade to last-write-wins."""
    server.routes.append(Route(200, {"ticket_id": "T-1"}))
    _, etag = await client(server).get_ticket_with_etag("T-1")
    assert etag == ""


# --- writing --------------------------------------------------------------


async def test_an_update_sends_only_what_changed(server):
    """The service treats an omitted field as 'keep current', so sending the whole
    ticket back would overwrite whatever another stage wrote in between."""
    server.routes.append(Route(200, {"ticket_id": "T-1"}))
    await client(server).update_ticket("T-1", TicketUpdate(status=wf.COL_IN_DEV))
    _, _, _, body = server.requests[0]
    assert json.loads(body) == {"status": "in_dev"}


async def test_clearing_the_assignee_is_distinguishable_from_leaving_it(server):
    server.routes.append(Route(200, {"ticket_id": "T-1"}))
    await client(server).update_ticket("T-1", TicketUpdate(status="done", assignee_id=""))
    _, _, _, body = server.requests[0]
    assert json.loads(body) == {"status": "done", "assignee_id": ""}


async def test_a_conditional_write_sends_if_match(server):
    server.routes.append(Route(200, {"ticket_id": "T-1"}))
    await client(server).update_ticket("T-1", TicketUpdate(status="in_dev"), if_match='"7"')
    _, _, headers, _ = server.requests[0]
    assert headers["If-Match"] == '"7"'


async def test_a_comment_posts_its_body(server):
    server.routes.append(Route(201, {"comment_id": "c1", "body": "hello"}))
    comment = await client(server).add_comment("T-1", "hello")
    assert comment.comment_id == "c1"
    method, path, _, body = server.requests[0]
    assert method == "POST" and path.endswith("/comments")
    assert json.loads(body) == {"body": "hello"}


# --- what a status code means --------------------------------------------


async def test_a_rejected_precondition_is_a_lost_race(server):
    """412 is the compare-and-set losing, which is expected rather than
    exceptional."""
    server.routes.append(Route(412))
    with pytest.raises(Conflict):
        await client(server).update_ticket("T-1", TicketUpdate(status="in_dev"), if_match='"1"')


async def test_a_conflict_status_is_a_lost_race(server):
    server.routes.append(Route(409))
    with pytest.raises(Conflict):
        await client(server).update_ticket("T-1", TicketUpdate(status="in_dev"))


async def test_a_missing_ticket_is_not_found(server):
    server.routes.append(Route(404))
    with pytest.raises(NotFound):
        await client(server).get_ticket("T-1")


async def test_an_unauthorised_call_is_denied_and_not_retried(server):
    """Not retryable: the account lacks the grant, and hammering it will not
    change that."""
    server.routes.append(Route(403))
    with pytest.raises(Denied):
        await client(server).get_ticket("T-1")
    assert len(server.requests) == 1


async def test_a_server_error_is_retried_and_then_reported(server):
    for _ in range(4):
        server.routes.append(Route(503))
    with pytest.raises(Transient):
        await client(server, retries=2).get_ticket("T-1")
    assert len(server.requests) == 3, "a transient failure was not retried the configured number of times"


async def test_a_blip_is_survived(server):
    server.routes.append(Route(503))
    server.routes.append(Route(200, {"ticket_id": "T-1"}))
    assert (await client(server, retries=2).get_ticket("T-1")).ticket_id == "T-1"


async def test_a_body_that_is_not_json_is_reported_with_what_arrived(server):
    """'invalid character' names the symptom. What the server actually said is
    what tells you it was a proxy's error page."""
    server.routes.append(Route(200, raw=b"<html>502 Bad Gateway</html>"))
    with pytest.raises(Exception) as raised:
        await client(server).get_ticket("T-1")
    assert "Bad Gateway" in str(raised.value)


async def test_an_error_names_the_ticket_and_the_status(server):
    """A message that names the symptom and not the cause sends a correct operator
    to the wrong place."""
    server.routes.append(Route(404))
    with pytest.raises(NotFound) as raised:
        await client(server).get_ticket("T-42")
    assert "T-42" in str(raised.value) and "404" in str(raised.value)


async def test_an_unreachable_platform_is_transient(server):
    unreachable = CodeArmory("http://127.0.0.1:1", token="t", retries=0, retry_backoff=0.0)
    with pytest.raises(Transient):
        await unreachable.get_ticket("T-1")


# --- construction ---------------------------------------------------------


def test_a_base_url_without_a_scheme_is_refused():
    """A bare host silently produces relative URLs and every call fails somewhere
    much further from the cause."""
    with pytest.raises(ValueError):
        CodeArmory("codearmory.example", token="t")


def test_a_trailing_slash_does_not_double_up(server):
    assert CodeArmory(server.url + "/", token="t")._base == server.url


def test_the_token_is_not_in_the_repr(server):
    assert "t0ken" not in repr(client(server))
