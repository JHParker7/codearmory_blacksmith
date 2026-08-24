"""A ticketing system: a store, a JSON API, and an HTML GUI.

THE SPECIFICATION. `ticketing.py` does not exist yet; making these pass is the
task.

Deliberately built in three layers so the score has RESOLUTION. A model that gets
the store right and the API wrong scores differently from one that gets nothing,
and a baseline that cannot tell those apart cannot tell you whether a change
helped.

Everything is driven in-process — the WSGI app is called directly, with no
socket — so the result is the model's code and never the network.
"""

import json

import pytest

from ticketing import Store, Ticket, app

# ---------------------------------------------------------------- the store


@pytest.fixture
def store():
    return Store()


def test_a_new_store_is_empty(store):
    assert store.list() == []


def test_creating_a_ticket_returns_it(store):
    ticket = store.create(title="Fix the login redirect")
    assert ticket.title == "Fix the login redirect"


def test_a_created_ticket_gets_an_id(store):
    assert store.create(title="a").id


def test_ids_are_unique(store):
    ids = {store.create(title=f"t{i}").id for i in range(20)}
    assert len(ids) == 20


def test_a_new_ticket_starts_open(store):
    assert store.create(title="a").status == "open"


def test_a_ticket_can_be_created_with_a_status(store):
    assert store.create(title="a", status="in_progress").status == "in_progress"


def test_a_ticket_carries_a_description(store):
    assert store.create(title="a", description="the details").description == "the details"


def test_a_description_defaults_to_empty(store):
    assert store.create(title="a").description == ""


def test_created_tickets_are_listed(store):
    store.create(title="one")
    store.create(title="two")
    assert {t.title for t in store.list()} == {"one", "two"}


def test_a_ticket_can_be_fetched_by_id(store):
    made = store.create(title="findable")
    assert store.get(made.id).title == "findable"


def test_fetching_an_unknown_id_raises_key_error(store):
    with pytest.raises(KeyError):
        store.get("nope")


def test_a_ticket_can_be_updated(store):
    made = store.create(title="a")
    updated = store.update(made.id, status="closed")
    assert updated.status == "closed"
    assert store.get(made.id).status == "closed"


def test_an_update_leaves_other_fields_alone(store):
    made = store.create(title="keep me", description="and me")
    store.update(made.id, status="closed")
    fetched = store.get(made.id)
    assert (fetched.title, fetched.description) == ("keep me", "and me")


def test_updating_an_unknown_id_raises_key_error(store):
    with pytest.raises(KeyError):
        store.update("nope", status="closed")


def test_a_status_outside_the_allowed_set_is_refused(store):
    """A closed set, because status drives the board columns and free text there
    is a column nobody watches."""
    with pytest.raises(ValueError):
        store.create(title="a", status="whatever")


def test_the_allowed_statuses_are_open_in_progress_and_closed(store):
    for status in ("open", "in_progress", "closed"):
        assert store.create(title="a", status=status).status == status


def test_a_ticket_with_no_title_is_refused(store):
    """A ticket nobody can identify is not a ticket."""
    with pytest.raises(ValueError):
        store.create(title="")


def test_listing_can_be_filtered_by_status(store):
    store.create(title="a", status="open")
    store.create(title="b", status="closed")
    assert [t.title for t in store.list(status="closed")] == ["b"]


def test_a_ticket_converts_to_a_plain_dict(store):
    made = store.create(title="a", description="d")
    as_dict = made.to_dict()
    assert as_dict["title"] == "a"
    assert as_dict["status"] == "open"
    assert as_dict["id"] == made.id


# ------------------------------------------------------------------ the API


def call(path, method="GET", body=None):
    """Drive the WSGI app directly. No socket, so this tests the code."""
    captured = {}

    def start_response(status, headers):
        captured["status"] = status
        captured["headers"] = dict(headers)

    payload = json.dumps(body).encode() if body is not None else b""
    # WSGI keeps the path and the query string apart, so they are split here
    # rather than handed over glued together.
    path, _, query = path.partition("?")
    environ = {
        "REQUEST_METHOD": method,
        "PATH_INFO": path,
        "QUERY_STRING": query,
        "CONTENT_LENGTH": str(len(payload)),
        "wsgi.input": __import__("io").BytesIO(payload),
        "SERVER_NAME": "localhost",
        "SERVER_PORT": "80",
        "wsgi.url_scheme": "http",
    }
    chunks = app(environ, start_response)
    raw = b"".join(chunks).decode()
    return captured["status"], captured["headers"], raw


def as_json(raw):
    return json.loads(raw)


def test_the_api_lists_tickets():
    status, headers, raw = call("/api/tickets")
    assert status.startswith("200")
    assert "application/json" in headers.get("Content-Type", "")
    assert isinstance(as_json(raw), list)


def test_the_api_creates_a_ticket():
    status, _, raw = call("/api/tickets", "POST", {"title": "from the api"})
    assert status.startswith("201")
    assert as_json(raw)["title"] == "from the api"


def test_a_created_ticket_is_then_listed():
    call("/api/tickets", "POST", {"title": "listed later"})
    _, _, raw = call("/api/tickets")
    assert any(t["title"] == "listed later" for t in as_json(raw))


def test_the_api_fetches_one_ticket():
    _, _, made = call("/api/tickets", "POST", {"title": "fetch me"})
    ticket_id = as_json(made)["id"]
    status, _, raw = call(f"/api/tickets/{ticket_id}")
    assert status.startswith("200") and as_json(raw)["title"] == "fetch me"


def test_fetching_an_unknown_ticket_is_a_404():
    status, _, _ = call("/api/tickets/does-not-exist")
    assert status.startswith("404")


def test_the_api_updates_a_ticket():
    _, _, made = call("/api/tickets", "POST", {"title": "update me"})
    ticket_id = as_json(made)["id"]
    status, _, raw = call(f"/api/tickets/{ticket_id}", "PATCH", {"status": "closed"})
    assert status.startswith("200") and as_json(raw)["status"] == "closed"


def test_creating_without_a_title_is_a_400():
    """A validation failure is the client's fault, and must not be a 500."""
    status, _, _ = call("/api/tickets", "POST", {"title": ""})
    assert status.startswith("400")


def test_an_invalid_status_is_a_400():
    status, _, _ = call("/api/tickets", "POST", {"title": "a", "status": "banana"})
    assert status.startswith("400")


def test_a_malformed_body_is_a_400_not_a_crash():
    """Bodies come off the network. A broken one must not take the server down."""
    captured = {}

    def start_response(status, headers):
        captured["status"] = status

    payload = b"{not json"
    environ = {
        "REQUEST_METHOD": "POST",
        "PATH_INFO": "/api/tickets",
        "CONTENT_LENGTH": str(len(payload)),
        "wsgi.input": __import__("io").BytesIO(payload),
    }
    b"".join(app(environ, start_response))
    assert captured["status"].startswith("400")


def test_an_unknown_api_path_is_a_404():
    status, _, _ = call("/api/nothing")
    assert status.startswith("404")


def test_the_api_filters_by_status():
    call("/api/tickets", "POST", {"title": "open one", "status": "open"})
    call("/api/tickets", "POST", {"title": "closed one", "status": "closed"})
    _, _, raw = call("/api/tickets?status=closed")
    titles = [t["title"] for t in as_json(raw)]
    assert "closed one" in titles and "open one" not in titles


# ------------------------------------------------------------------ the GUI


def test_the_gui_serves_a_page():
    status, headers, raw = call("/")
    assert status.startswith("200")
    assert "text/html" in headers.get("Content-Type", "")
    assert raw.lstrip().lower().startswith("<!doctype html")


def test_the_gui_lists_the_tickets():
    call("/api/tickets", "POST", {"title": "visible in the gui"})
    _, _, raw = call("/")
    assert "visible in the gui" in raw


def test_the_gui_shows_each_status_as_a_group():
    """A list with no grouping is not a board."""
    call("/api/tickets", "POST", {"title": "an open one", "status": "open"})
    call("/api/tickets", "POST", {"title": "a closed one", "status": "closed"})
    _, _, raw = call("/")
    for status in ("open", "in_progress", "closed"):
        assert status.replace("_", " ") in raw.lower()


def test_the_gui_has_a_form_to_create_a_ticket():
    """A read-only page is a report, not a GUI."""
    _, _, raw = call("/")
    assert "<form" in raw.lower()
    assert 'name="title"' in raw.lower()


def test_the_gui_escapes_a_ticket_title():
    """Titles are written by people and must not become markup."""
    call("/api/tickets", "POST", {"title": "<script>alert(1)</script>"})
    _, _, raw = call("/")
    assert "<script>alert(1)</script>" not in raw
    assert "&lt;script&gt;" in raw


def test_the_gui_form_posts_somewhere_useful():
    _, _, raw = call("/")
    assert "/tickets" in raw


def test_an_unknown_page_is_a_404():
    status, _, _ = call("/nowhere")
    assert status.startswith("404")
