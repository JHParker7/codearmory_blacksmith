Create an API to manage tasks in this repository.

Put `go.mod` and the packages at the repository ROOT, not in a subdirectory.

A task has a title, a description, a due date, a priority and a status. Status is
one of `todo`, `in_progress`, `done` or `blocked`, and nothing else may be
stored. A task also has comments: each comment has an author, a body and the time
it was written, and a task may have any number of them.

Serve it as a JSON API over HTTP: create, fetch, update and list tasks, and add
and list the comments on one. Listing can be filtered by status. A `main` package
must start the server on the port given by the `PORT` environment variable,
defaulting to 8080, and `go build ./...` must produce that binary.

Handle the edges as well as the happy path: a task with no title, a status or
priority outside the allowed set, a due date that is not a date, a comment on a
task that does not exist, a body that is not an object, and an unknown id. A
request that is the caller's fault must come back as a 4xx rather than a 500.
