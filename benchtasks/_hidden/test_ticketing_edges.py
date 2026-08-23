"""Edge cases the model never saw.

The graded spec is 37 tests and the model satisfied all of them. This is the
other half of the question: does the code hold up against things a competent
engineer would have thought of anyway?

Every case here is something a reasonable reviewer would expect to work — not a
gotcha. Nothing depends on an implementation detail; each drives the public
surface the spec already established.
"""

import io
import json

import pytest

import ticketing
from ticketing import Store, app


@pytest.fixture
def store():
    return Store()


def call(path, method="GET", body=None, form=None):
    captured = {}

    def start_response(status, headers):
        captured["status"] = status
        captured["headers"] = dict(headers)

    if form is not None:
        payload = form.encode()
        content_type = "application/x-www-form-urlencoded"
    else:
        payload = json.dumps(body).encode() if body is not None else b""
        content_type = "application/json"
    path, _, query = path.partition("?")
    environ = {
        "REQUEST_METHOD": method,
        "PATH_INFO": path,
        "QUERY_STRING": query,
        "CONTENT_TYPE": content_type,
        "CONTENT_LENGTH": str(len(payload)),
        "wsgi.input": io.BytesIO(payload),
    }
    raw = b"".join(app(environ, start_response)).decode()
    return captured["status"], captured.get("headers", {}), raw


# --- input the world will actually send -----------------------------------


def test_a_unicode_title_survives(store):
    """Ticket titles are written by people, and people use their own alphabet."""
    assert store.create(title="Añadir límite de peticiones 🎫").title.startswith("Añadir")


def test_a_very_long_title_is_not_truncated_silently(store):
    long_title = "x" * 5000
    assert store.create(title=long_title).title == long_title


def test_a_whitespace_only_title_is_refused(store):
    """'   ' is not a title. Refusing empty but accepting a space is a rule that
    only catches the honest mistake."""
    with pytest.raises(ValueError):
        store.create(title="   ")


def test_a_non_string_title_is_refused(store):
    with pytest.raises((ValueError, TypeError)):
        store.create(title=12345)


# --- the store's invariants ------------------------------------------------


def test_mutating_a_listed_ticket_does_not_corrupt_the_store(store):
    """`list` hands out its own objects. If they are the live ones, any caller
    can rewrite the store by accident."""
    made = store.create(title="original")
    listed = store.list()[0]
    listed.title = "tampered"
    assert store.get(made.id).title == "original"


def test_mutating_the_returned_list_does_not_corrupt_the_store(store):
    store.create(title="a")
    store.list().clear()
    assert len(store.list()) == 1


def test_to_dict_is_a_copy(store):
    made = store.create(title="a")
    made.to_dict()["title"] = "tampered"
    assert store.get(made.id).title == "a"


def test_an_update_cannot_change_the_id(store):
    """The id is the key. Letting a caller rewrite it detaches the ticket from
    its own entry in the store.

    EITHER ANSWER COUNTS — ignore the field, or reject it. Both protect the
    invariant, and insisting on one made this test measure which of two
    reasonable designs was chosen rather than whether the case was handled.
    """
    made = store.create(title="a")
    try:
        store.update(made.id, id="hijacked")
    except (ValueError, TypeError):
        pass  # rejected outright, which is stricter and also correct
    assert store.get(made.id).id == made.id


def test_an_update_with_an_unknown_field_does_not_invent_one(store):
    """Ignored or rejected — either is fine, inventing an attribute is not."""
    made = store.create(title="a")
    try:
        store.update(made.id, nonsense="x")
    except (ValueError, TypeError):
        pass
    assert not hasattr(store.get(made.id), "nonsense")


def test_filtering_by_an_invalid_status_is_refused(store):
    with pytest.raises(ValueError):
        store.list(status="banana")


def test_ids_stay_unique_at_volume(store):
    assert len({store.create(title=f"t{i}").id for i in range(500)}) == 500


# --- HTTP the world will actually send -------------------------------------


def test_deleting_is_rejected_cleanly_not_with_a_crash():
    status, _, _ = call("/api/tickets/whatever", "DELETE")
    assert status.startswith(("404", "405"))


def test_posting_to_a_single_ticket_is_rejected_cleanly():
    status, _, _ = call("/api/tickets/abc", "POST", {"title": "x"})
    assert status.startswith(("404", "405"))


def test_a_trailing_slash_still_lists():
    """`/api/tickets/` is the same collection as `/api/tickets`, and a client
    library will append the slash without asking."""
    status, _, raw = call("/api/tickets/")
    assert status.startswith("200") and isinstance(json.loads(raw), list)


def test_a_patch_with_a_malformed_body_is_a_400_not_a_500():
    _, _, made = call("/api/tickets", "POST", {"title": "patch target"})
    ticket_id = json.loads(made)["id"]
    captured = {}

    def start_response(status, headers):
        captured["status"] = status

    payload = b"{not json"
    environ = {
        "REQUEST_METHOD": "PATCH",
        "PATH_INFO": f"/api/tickets/{ticket_id}",
        "QUERY_STRING": "",
        "CONTENT_LENGTH": str(len(payload)),
        "wsgi.input": io.BytesIO(payload),
    }
    b"".join(app(environ, start_response))
    assert captured["status"].startswith("400")


def test_a_patch_to_an_unknown_ticket_is_a_404():
    status, _, _ = call("/api/tickets/nope", "PATCH", {"status": "closed"})
    assert status.startswith("404")


def test_a_post_with_a_json_array_body_is_a_400():
    status, _, _ = call("/api/tickets", "POST", [1, 2, 3])
    assert status.startswith("400")


def test_a_post_with_no_body_at_all_is_a_400():
    status, _, _ = call("/api/tickets", "POST")
    assert status.startswith("400")


def test_an_unknown_query_parameter_is_ignored():
    status, _, _ = call("/api/tickets?sort=title")
    assert status.startswith("200")


def test_every_json_response_declares_its_length():
    """A response with no Content-Length forces the server to close the
    connection to signal the end, which breaks keep-alive."""
    _, headers, raw = call("/api/tickets")
    assert headers.get("Content-Length") == str(len(raw.encode()))


# --- the form the page actually posts --------------------------------------


def test_a_form_title_with_a_space_is_decoded():
    """THE FORM ITS OWN PAGE POSTS. Browsers percent-encode and use + for spaces;
    a hand-rolled split on & and = does not decode either."""
    call("/tickets", "POST", form="title=fix+the+login+redirect")
    _, _, raw = call("/api/tickets")
    titles = [t["title"] for t in json.loads(raw)]
    assert "fix the login redirect" in titles


def test_a_form_title_with_an_ampersand_survives():
    call("/tickets", "POST", form="title=tea%20%26%20biscuits")
    _, _, raw = call("/api/tickets")
    assert "tea & biscuits" in [t["title"] for t in json.loads(raw)]


def test_an_empty_form_title_is_refused_not_stored():
    before = len(json.loads(call("/api/tickets")[2]))
    status, _, _ = call("/tickets", "POST", form="title=")
    after = len(json.loads(call("/api/tickets")[2]))
    assert status.startswith("400") and after == before


# --- the page --------------------------------------------------------------


def test_the_page_escapes_a_description_too():
    """Titles were specified. Descriptions are rendered the same way and come
    from the same place."""
    call("/api/tickets", "POST", {"title": "safe", "description": "<img src=x onerror=1>"})
    _, _, raw = call("/")
    assert "<img src=x" not in raw


def test_the_page_renders_with_no_tickets_at_all():
    """Reloaded rather than reaching for a module global by name, so this does
    not depend on what the implementation happened to call its store."""
    import importlib

    fresh = importlib.reload(ticketing)
    captured = {}

    def start_response(status, headers):
        captured["status"] = status

    environ = {
        "REQUEST_METHOD": "GET",
        "PATH_INFO": "/",
        "QUERY_STRING": "",
        "CONTENT_LENGTH": "0",
        "wsgi.input": io.BytesIO(b""),
    }
    raw = b"".join(fresh.app(environ, start_response)).decode()
    assert captured["status"].startswith("200") and "<form" in raw
