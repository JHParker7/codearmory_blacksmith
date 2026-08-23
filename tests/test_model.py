"""The model client, against a real HTTP server.

An OpenAI-compatible chat endpoint — which is what llama-server, ollama and vLLM
all speak, so the same client reaches whatever is actually running beside the
GPUs.

The behaviour under test is mostly about MALFORMED REPLIES, because that is what
a small local model produces and what decides whether a run survives. A harness
that crashes on a bad tool call cannot measure whether the model is any good; it
only measures that it is not perfect.
"""

import json
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

import pytest

from blacksmith.errors import Denied, Transient
from blacksmith.model import Model, ModelError


class Server:
    def __init__(self) -> None:
        self.replies: list[tuple[int, object]] = []
        self.requests: list[dict] = []
        outer = self

        class Handler(BaseHTTPRequestHandler):
            protocol_version = "HTTP/1.1"

            def do_POST(self) -> None:
                length = int(self.headers.get("Content-Length") or 0)
                raw = self.rfile.read(length).decode()
                outer.requests.append(
                    {"path": self.path, "headers": dict(self.headers), "body": json.loads(raw or "{}")}
                )
                status, body = outer.replies.pop(0) if outer.replies else (200, _completion("ok"))
                payload = body.encode() if isinstance(body, str) else json.dumps(body).encode()
                self.send_response(status)
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(payload)))
                self.end_headers()
                self.wfile.write(payload)

            def log_message(self, *_a) -> None:
                pass

        self._server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        self._server.daemon_threads = True
        self._thread = threading.Thread(
            target=self._server.serve_forever, kwargs={"poll_interval": 0.01}, daemon=True
        )
        self._thread.start()

    @property
    def url(self) -> str:
        host, port = self._server.server_address[:2]
        return f"http://{host}:{port}/v1"

    def close(self) -> None:
        self._server.shutdown()
        self._server.server_close()
        self._thread.join(timeout=5)


def _completion(content="", *, tool_calls=None, reasoning=None, finish="stop"):
    message = {"role": "assistant", "content": content}
    if tool_calls is not None:
        message["tool_calls"] = tool_calls
    if reasoning is not None:
        message["reasoning"] = reasoning
    return {
        "choices": [{"index": 0, "message": message, "finish_reason": finish}],
        "usage": {"prompt_tokens": 10, "completion_tokens": 3},
    }


def _call(name, arguments, cid="call_1"):
    return [{"id": cid, "type": "function", "function": {"name": name, "arguments": arguments}}]


@pytest.fixture
def server():
    s = Server()
    try:
        yield s
    finally:
        s.close()


def model(server, **kw) -> Model:
    return Model(server.url, **{"model": "qwen3-coder", "retries": 0, "retry_backoff": 0.0, **kw})


# --- the request ----------------------------------------------------------


async def test_it_posts_to_the_chat_completions_route(server):
    server.replies.append((200, _completion("hi")))
    await model(server).chat([{"role": "user", "content": "hello"}])
    assert server.requests[0]["path"].endswith("/chat/completions")


async def test_the_model_name_and_messages_are_sent(server):
    server.replies.append((200, _completion("hi")))
    await model(server).chat([{"role": "user", "content": "hello"}])
    body = server.requests[0]["body"]
    assert body["model"] == "qwen3-coder"
    assert body["messages"] == [{"role": "user", "content": "hello"}]


async def test_tools_are_sent_when_given(server):
    server.replies.append((200, _completion("hi")))
    schema = {"type": "function", "function": {"name": "read_file", "description": "d", "parameters": {}}}
    await model(server).chat([{"role": "user", "content": "x"}], tools=[schema])
    assert server.requests[0]["body"]["tools"] == [schema]


async def test_no_tools_key_is_sent_when_there_are_none(server):
    """Some servers reject an empty tools array outright."""
    server.replies.append((200, _completion("hi")))
    await model(server).chat([{"role": "user", "content": "x"}])
    assert "tools" not in server.requests[0]["body"]


async def test_a_key_is_sent_when_configured(server):
    server.replies.append((200, _completion("hi")))
    await model(server, api_key="sk-local").chat([{"role": "user", "content": "x"}])
    assert server.requests[0]["headers"]["Authorization"] == "Bearer sk-local"


async def test_no_authorization_header_when_there_is_no_key(server):
    """A local llama-server started without one rejects a bearer token it did not
    expect, which reads as a wrong key rather than an unnecessary one."""
    server.replies.append((200, _completion("hi")))
    await model(server).chat([{"role": "user", "content": "x"}])
    assert "Authorization" not in server.requests[0]["headers"]


async def test_generation_is_bounded(server):
    """An unbounded local model will happily produce until the context ends, and
    a turn that never returns is indistinguishable from a hung box."""
    server.replies.append((200, _completion("hi")))
    await model(server, max_tokens=256).chat([{"role": "user", "content": "x"}])
    assert server.requests[0]["body"]["max_tokens"] == 256


# --- the reply ------------------------------------------------------------


async def test_plain_content_comes_back(server):
    server.replies.append((200, _completion("the answer")))
    assert (await model(server).chat([])).content == "the answer"


async def test_a_tool_call_is_decoded(server):
    server.replies.append((200, _completion(tool_calls=_call("read_file", '{"path":"a.py"}'), finish="tool_calls")))
    reply = await model(server).chat([])
    assert len(reply.tool_calls) == 1
    call = reply.tool_calls[0]
    assert (call.name, call.arguments, call.id) == ("read_file", {"path": "a.py"}, "call_1")
    assert call.error == ""


async def test_arguments_that_are_already_an_object_are_accepted(server):
    """The spec says a JSON string; several servers send the object. A client that
    handles one of them fails against half the stack it is meant to run on."""
    server.replies.append((200, _completion(tool_calls=_call("read_file", {"path": "a.py"}))))
    assert (await model(server).chat([])).tool_calls[0].arguments == {"path": "a.py"}


async def test_malformed_arguments_become_a_reportable_call_not_a_crash(server):
    """THE failure of a small model. It must cost the model one turn and a message
    it can act on — a harness that dies here cannot measure anything."""
    server.replies.append((200, _completion(tool_calls=_call("edit_file", '{"path": "a.py", "old": }'))))
    call = (await model(server).chat([])).tool_calls[0]
    assert call.name == "edit_file"
    assert call.arguments == {}
    assert "not valid JSON" in call.error
    assert '"old": }' in call.error, "the model is not shown what it actually sent"


async def test_arguments_that_decode_to_a_non_object_are_reported(server):
    server.replies.append((200, _completion(tool_calls=_call("read_file", '"a.py"'))))
    assert "object" in (await model(server).chat([])).tool_calls[0].error


async def test_a_tool_call_with_no_name_is_reported(server):
    server.replies.append((200, _completion(tool_calls=[{"id": "c1", "function": {"arguments": "{}"}}])))
    assert (await model(server).chat([])).tool_calls[0].error


async def test_a_call_with_no_id_still_gets_one(server):
    """The id is how the tool result is matched back to the call. Without one the
    conversation becomes unanswerable."""
    server.replies.append((200, _completion(tool_calls=[{"function": {"name": "run_tests", "arguments": "{}"}}])))
    assert (await model(server).chat([])).tool_calls[0].id


async def test_thinking_is_captured_separately_from_the_answer(server):
    """A thinking model puts its reasoning in its own field. Folded into content it
    would be fed back as if it were an answer."""
    server.replies.append((200, _completion("done", reasoning="the user wants a file")))
    reply = await model(server).chat([])
    assert reply.reasoning == "the user wants a file" and reply.content == "done"


async def test_usage_and_timing_come_back(server):
    server.replies.append((200, _completion("hi")))
    reply = await model(server).chat([])
    assert (reply.prompt_tokens, reply.completion_tokens) == (10, 3)
    assert reply.latency_ms >= 0


async def test_the_finish_reason_is_reported(server):
    """`length` means the reply was cut off, which looks exactly like a model that
    stopped early and is a completely different problem."""
    server.replies.append((200, _completion("half a", finish="length")))
    assert (await model(server).chat([])).finish_reason == "length"


# --- when the server misbehaves ------------------------------------------


async def test_a_reply_with_no_choices_is_reported(server):
    server.replies.append((200, {"choices": []}))
    with pytest.raises(ModelError):
        await model(server).chat([])


async def test_a_body_that_is_not_json_is_reported_with_what_arrived(server):
    server.replies.append((200, "<html>502 from the proxy</html>"))
    with pytest.raises(ModelError) as raised:
        await model(server).chat([])
    assert "502 from the proxy" in str(raised.value)


async def test_a_rejected_key_is_denied_and_not_retried(server):
    server.replies.append((401, {"error": "bad key"}))
    with pytest.raises(Denied):
        await model(server, retries=3).chat([])
    assert len(server.requests) == 1


async def test_a_server_error_is_retried(server):
    server.replies.append((503, {"error": "loading model"}))
    server.replies.append((200, _completion("hi")))
    assert (await model(server, retries=2).chat([])).content == "hi"


async def test_a_model_that_is_not_loaded_says_which_model(server):
    """'model not found' with no name is unactionable when three are configured."""
    server.replies.append((404, {"error": {"message": "model not found"}}))
    with pytest.raises(ModelError) as raised:
        await model(server).chat([])
    assert "qwen3-coder" in str(raised.value)


async def test_an_unreachable_endpoint_is_transient_and_names_the_url():
    """The most common local failure by far: the server is simply not running."""
    unreachable = Model("http://127.0.0.1:1/v1", model="m", retries=0, retry_backoff=0.0)
    with pytest.raises(Transient) as raised:
        await unreachable.chat([])
    assert "127.0.0.1:1" in str(raised.value)


# --- construction ---------------------------------------------------------


def test_an_endpoint_without_a_scheme_is_refused():
    with pytest.raises(ValueError):
        Model("192.168.58.1:11434/v1", model="m")


def test_a_trailing_slash_does_not_double_up(server):
    assert Model(server.url + "/", model="m")._base == server.url


def test_the_key_is_not_in_the_repr(server):
    assert "sk-local" not in repr(model(server, api_key="sk-local"))


def test_a_key_can_come_from_a_file(tmp_path):
    """The operator env file uses AGENTS_LARGE_API_KEY_FILE, because a key in an
    environment variable is a key in every child process's environment."""
    key = tmp_path / "llama.key"
    key.write_text("sk-from-file\n")
    assert Model("http://x/v1", model="m", api_key_file=key)._api_key == "sk-from-file"


# --- the text-protocol fallback ------------------------------------------
#
# Some models emit a perfectly correct tool call as ORDINARY CONTENT, because the
# server's chat template does not wrap it into the structured field. Measured:
# qwen2.5-coder:32b scored 0/4 on the graded tasks while emitting exactly
# `{"name": "read_file", "arguments": {"path": "calc.py"}}` every turn. That is a
# harness failure being reported as a model failure, which is the worst kind of
# measurement error — it sends you to replace a model that was right.


async def test_a_call_emitted_as_plain_text_is_recognised(server):
    server.replies.append((200, _completion('{"name": "read_file", "arguments": {"path": "a.py"}}')))
    reply = await model(server).chat([])
    assert len(reply.tool_calls) == 1
    assert (reply.tool_calls[0].name, reply.tool_calls[0].arguments) == ("read_file", {"path": "a.py"})


async def test_the_text_fallback_accepts_parameters_as_well_as_arguments(server):
    """Both spellings are in the wild, and which one you get is the template's
    choice rather than the model's."""
    server.replies.append((200, _completion('{"name": "run_tests", "parameters": {}}')))
    assert (await model(server).chat([])).tool_calls[0].name == "run_tests"


async def test_the_text_fallback_reads_a_fenced_block(server):
    """Instruction-tuned models wrap JSON in a code fence by reflex."""
    server.replies.append((200, _completion('```json\n{"name": "run_tests", "arguments": {}}\n```')))
    assert (await model(server).chat([])).tool_calls[0].name == "run_tests"


async def test_the_text_fallback_finds_a_call_after_a_sentence(server):
    """A model that explains itself first is still asking for the tool."""
    server.replies.append(
        (200, _completion('I will read the file.\n{"name": "read_file", "arguments": {"path": "a.py"}}'))
    )
    assert (await model(server).chat([])).tool_calls[0].arguments == {"path": "a.py"}


async def test_the_text_fallback_does_not_fire_on_ordinary_prose(server):
    """A model reasoning about JSON must not be mistaken for one calling a tool."""
    server.replies.append((200, _completion('The config file holds {"port": 8080} as its content.')))
    assert (await model(server).chat([])).tool_calls == ()


async def test_the_text_fallback_ignores_json_that_is_not_a_call(server):
    server.replies.append((200, _completion('{"path": "a.py", "old": "x"}')))
    assert (await model(server).chat([])).tool_calls == ()


async def test_a_structured_call_is_never_second_guessed(server):
    """The fallback is a fallback. A server that populated the field correctly
    must not have its answer re-derived from the prose beside it."""
    server.replies.append(
        (200, _completion('{"name": "finish", "arguments": {}}', tool_calls=_call("read_file", '{"path":"a.py"}')))
    )
    reply = await model(server).chat([])
    assert len(reply.tool_calls) == 1 and reply.tool_calls[0].name == "read_file"


async def test_the_text_fallback_keeps_the_prose_out_of_the_content(server):
    """Feeding the call back as content as well would have the model read its own
    request as though it were a result."""
    server.replies.append((200, _completion('{"name": "run_tests", "arguments": {}}')))
    assert (await model(server).chat([])).content == ""
