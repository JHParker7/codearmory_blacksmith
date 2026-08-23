"""An in-memory tickets service.

It is a fake rather than a mock: it enforces the two properties the claim
protocol actually depends on — versions that move on every write, and comments
that are append-only with server-assigned ids and increasing timestamps — so a
test that passes against it is testing the protocol, not its own expectations.

`supports_if_match=False` reproduces an older instance with no conditional write,
which is the branch the append fallback exists for and the one no amount of
local testing against a current server would ever reach.
"""

from __future__ import annotations

import asyncio
from datetime import datetime, timedelta, timezone

from blacksmith.errors import Conflict, NotFound, Transient
from blacksmith.tickets import Comment, ListOpts, Ticket, TicketUpdate

#: The clock the fakes hand out, anchored to NOW rather than to a fixed date.
#:
#: It used to be a literal `datetime(2026, 8, 21, 12, 0)`, and that made the
#: suite a time bomb: `attempts()` forgives a claim older than the stale window,
#: so every fixture claim silently stopped counting 45 minutes after that
#: timestamp. Two dispatch tests passed all morning and failed in the afternoon,
#: on unrelated work, for a reason nothing in them mentioned.
#:
#: Truncated to the second so ordering stays reproducible within a run.
EPOCH = datetime.now(timezone.utc).replace(microsecond=0)


class FakePlatform:
    def __init__(self, *, supports_if_match: bool = True) -> None:
        self.tickets: dict[str, Ticket] = {}
        self.supports_if_match = supports_if_match
        #: Calls made, for the tests that assert a poll did not pay for a read.
        self.calls: list[tuple[str, str]] = []
        #: Set to raise from the next call of that name — one failure, then normal.
        self.fail_once: dict[str, Exception] = {}
        #: Set to raise from every call of that name.
        self.fail_always: dict[str, Exception] = {}
        #: Runs before a named call completes; the hook for interleaving two hosts.
        self.before: dict[str, callable] = {}
        self._clock = 0
        self._comments = 0

    # --- test-side helpers ------------------------------------------------

    def add(self, ticket: Ticket) -> Ticket:
        self.tickets[ticket.ticket_id] = ticket
        return ticket

    def status_of(self, ticket_id: str) -> str:
        return self.tickets[ticket_id].status

    def bodies_on(self, ticket_id: str) -> list[str]:
        return [c.body for c in self.tickets[ticket_id].comments]

    def _tick(self) -> datetime:
        self._clock += 1
        return EPOCH + timedelta(seconds=self._clock)

    async def _enter(self, name: str, target: str = "") -> None:
        self.calls.append((name, target))
        # A real call crosses the network, so another host's call can land between
        # any two of ours. Without a suspension point here two "concurrent" claims
        # would run one after the other and the version race would never be tested.
        await asyncio.sleep(0)
        hook = self.before.get(name)
        if hook is not None:
            result = hook(target)
            if asyncio.iscoroutine(result):
                await result
        err = self.fail_once.pop(name, None) or self.fail_always.get(name)
        if err is not None:
            raise err

    def _must_get(self, ticket_id: str) -> Ticket:
        try:
            return self.tickets[ticket_id]
        except KeyError:
            raise NotFound(f"ticket {ticket_id}") from None

    # --- the platform surface ---------------------------------------------

    async def list_tickets(self, opts: ListOpts) -> list[Ticket]:
        await self._enter("list_tickets", opts.status)
        found = []
        for ticket in self.tickets.values():
            if opts.status and ticket.status != opts.status:
                continue
            if opts.board_id and ticket.board_id != opts.board_id:
                continue
            # A LISTING CARRIES NO COMMENTS. This is the property that made a
            # missed hydration silently invert a predicate, so the fake refuses to
            # hand them over here.
            found.append(ticket.with_(comments=[]))
        return found

    async def get_ticket(self, ticket_id: str) -> Ticket:
        await self._enter("get_ticket", ticket_id)
        return self._must_get(ticket_id)

    async def get_ticket_with_etag(self, ticket_id: str) -> tuple[Ticket, str]:
        await self._enter("get_ticket_with_etag", ticket_id)
        ticket = self._must_get(ticket_id)
        return ticket, (str(ticket.version) if self.supports_if_match else "")

    async def update_ticket(
        self, ticket_id: str, update: TicketUpdate, *, if_match: str = ""
    ) -> Ticket:
        await self._enter("update_ticket", ticket_id)
        ticket = self._must_get(ticket_id)
        if if_match and if_match != str(ticket.version):
            raise Conflict(f"ticket {ticket_id}: version moved")
        changed = {k: v for k, v in update.payload().items()}
        if "assignee_id" in changed and changed["assignee_id"] == "":
            changed["assignee_id"] = None
        updated = ticket.with_(version=ticket.version + 1, **changed)
        self.tickets[ticket_id] = updated
        return updated

    async def add_comment(self, ticket_id: str, body: str) -> Comment:
        await self._enter("add_comment", ticket_id)
        ticket = self._must_get(ticket_id)
        self._comments += 1
        comment = Comment(
            comment_id=f"c{self._comments}",
            ticket_id=ticket_id,
            author_id="agent",
            body=body,
            created_at=self._tick(),
        )
        self.tickets[ticket_id] = ticket.with_(comments=[*ticket.comments, comment])
        return comment


def transient(message: str = "502 from the tickets service") -> Transient:
    return Transient(message)
