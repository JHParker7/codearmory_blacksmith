# blacksmith — three test tiers, each answering a different question.
#
#   unit         Is the logic right?                  no I/O beyond the filesystem
#   integration  Do the parts talk to each other?     in-process HTTP fakes
#   e2e          Does it work against the real thing? live CodeArmory + llama-server
#
# `test` runs unit + integration: hermetic, fast, safe to run anywhere. e2e is
# separate because it creates real tickets on a real board.

.PHONY: build test unit integration e2e race vet fmt all

BIN := blacksmith

all: fmt vet test

build:
	GOWORK=off go build -o $(BIN) .

# Unit only. -short skips anything that stands up an HTTP server, so a green run
# here really does mean the logic is right independent of the wiring.
unit:
	GOWORK=off go test -short ./...

# Unit + integration.
test:
	GOWORK=off go test ./...

# Same, under the race detector. Dispatch and the admission queue are concurrent;
# this is the tier where that gets checked.
race:
	GOWORK=off go test -race -count=2 ./...

# Integration only, verbosely — useful when a fake's behaviour is in question.
integration:
	GOWORK=off go test -run 'Test' -v ./... 2>&1 | grep -v 'SKIP.*short'

# E2E. Requires a live stack AND explicit configuration:
#
#   CODEARMORY_URL, CODEARMORY_TOKEN   the platform and an account credential
#   BLACKSMITH_E2E_BOARD               scratch board — no default, on purpose
#   AGENTS_<CLASS>_ENDPOINT etc.       the serving stack (see README)
#
# Fixtures are deleted on the way out; a leftover ticket on the scratch board
# means a cleanup failed and is worth investigating.
e2e:
	GOWORK=off go test -tags e2e -v ./...

vet:
	GOWORK=off go vet ./...
	GOWORK=off go vet -tags e2e ./...

# RECURSIVE. This formatted only the root *.go for as long as the whole program
# lived there; it has not since the packages moved under internal/, so `make fmt`
# was quietly skipping almost every file it was meant to cover.
fmt:
	gofmt -w .
