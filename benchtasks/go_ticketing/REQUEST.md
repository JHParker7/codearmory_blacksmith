Build a ticketing system in this repository.

Package it so it RUNS: a `main` package that starts an HTTP server on the port
given by the `PORT` environment variable, defaulting to 8080. `go build ./...`
must produce that binary. Everything below is served by it.

A ticket has an id, a title, a description and a status. Status is one of
`open`, `in_progress` or `closed`, and nothing else may be stored.

Three things are needed:

1. **A store.** Create, fetch by id, update and list tickets, held in memory.
   Ids are assigned by the store, not by the caller, and are unique. Listing can
   be filtered by status. A ticket with no title is refused, as is a status
   outside the allowed set. Fetching or updating an id that does not exist is an
   error the caller can tell apart from a valid empty result.

2. **A JSON API** over HTTP: list tickets at `GET /tickets`, create one at
   `POST /tickets`, fetch one at `GET /tickets/{id}`, and update one at
   `PUT /tickets/{id}`. Filtering by status is the `status` query parameter. A
   request that is malformed, or that names a status outside the allowed set, is
   the caller's fault and must come back as a 4xx rather than a 500. An unknown
   id is a 404.

3. **An HTML page** at `/` showing the tickets grouped by status, with a form to
   create one. Titles are written by people, so they must be escaped.

Handle the edges as well as the happy path: empty and whitespace-only titles,
oversized input, a body that is not an object, a duplicate or missing field, and
a path that does not exist.
