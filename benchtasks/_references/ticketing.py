"""Reference solution — proves the spec is satisfiable. Never shown to a model."""

import json
import uuid
from dataclasses import dataclass, field
from html import escape
from urllib.parse import parse_qs

STATUSES = ("open", "in_progress", "closed")


@dataclass
class Ticket:
    id: str
    title: str
    description: str = ""
    status: str = "open"

    def to_dict(self):
        return {
            "id": self.id,
            "title": self.title,
            "description": self.description,
            "status": self.status,
        }


class Store:
    def __init__(self):
        self._tickets = {}

    def create(self, title, description="", status="open"):
        if not title:
            raise ValueError("a ticket needs a title")
        if status not in STATUSES:
            raise ValueError(f"unknown status {status!r}")
        ticket = Ticket(id=uuid.uuid4().hex[:8], title=title, description=description, status=status)
        self._tickets[ticket.id] = ticket
        return ticket

    def get(self, ticket_id):
        return self._tickets[ticket_id]

    def update(self, ticket_id, **changes):
        ticket = self._tickets[ticket_id]
        if "status" in changes and changes["status"] not in STATUSES:
            raise ValueError("bad status")
        for key, value in changes.items():
            setattr(ticket, key, value)
        return ticket

    def list(self, status=None):
        found = list(self._tickets.values())
        return [t for t in found if status is None or t.status == status]


_store = Store()


def _json(start_response, status, payload):
    body = json.dumps(payload).encode()
    start_response(status, [("Content-Type", "application/json"), ("Content-Length", str(len(body)))])
    return [body]


def _html(start_response, status, text):
    body = text.encode()
    start_response(
        status, [("Content-Type", "text/html; charset=utf-8"), ("Content-Length", str(len(body)))]
    )
    return [body]


def _page():
    groups = []
    for status in STATUSES:
        items = "".join(
            f"<li>{escape(t.title)}</li>" for t in _store.list(status=status)
        )
        groups.append(f"<section><h2>{status.replace('_', ' ')}</h2><ul>{items}</ul></section>")
    return (
        "<!doctype html>\n<html><head><meta charset='utf-8'><title>Tickets</title></head><body>"
        "<h1>Tickets</h1>"
        '<form method="post" action="/tickets">'
        '<input name="title"><input name="description">'
        '<button type="submit">Create</button></form>'
        + "".join(groups)
        + "</body></html>"
    )


def app(environ, start_response):
    path = environ.get("PATH_INFO", "/")
    method = environ.get("REQUEST_METHOD", "GET")
    query = parse_qs(environ.get("QUERY_STRING", ""))

    if path.startswith("/api/tickets"):
        rest = path[len("/api/tickets") :].strip("/")
        if not rest:
            if method == "POST":
                try:
                    length = int(environ.get("CONTENT_LENGTH") or 0)
                    body = json.loads(environ["wsgi.input"].read(length) or b"{}")
                except (ValueError, TypeError):
                    return _json(start_response, "400 Bad Request", {"error": "malformed body"})
                try:
                    ticket = _store.create(
                        title=body.get("title", ""),
                        description=body.get("description", ""),
                        status=body.get("status", "open"),
                    )
                except ValueError as err:
                    return _json(start_response, "400 Bad Request", {"error": str(err)})
                return _json(start_response, "201 Created", ticket.to_dict())
            status = query.get("status", [None])[0]
            return _json(start_response, "200 OK", [t.to_dict() for t in _store.list(status=status)])

        try:
            ticket = _store.get(rest)
        except KeyError:
            return _json(start_response, "404 Not Found", {"error": "no such ticket"})
        if method == "PATCH":
            length = int(environ.get("CONTENT_LENGTH") or 0)
            try:
                body = json.loads(environ["wsgi.input"].read(length) or b"{}")
            except ValueError:
                return _json(start_response, "400 Bad Request", {"error": "malformed body"})
            try:
                ticket = _store.update(rest, **body)
            except ValueError as err:
                return _json(start_response, "400 Bad Request", {"error": str(err)})
        return _json(start_response, "200 OK", ticket.to_dict())

    if path == "/":
        return _html(start_response, "200 OK", _page())
    if path == "/tickets" and method == "POST":
        return _html(start_response, "303 See Other", "")
    return _html(start_response, "404 Not Found", "<!doctype html><html><body>Not found</body></html>")
