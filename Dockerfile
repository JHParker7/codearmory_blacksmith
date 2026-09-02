# The blacksmith action server, containerized to run as a cluster service
# (codearmory-blacksmith) rather than a bare host process. It reaches ollama as
# a remote OpenAI endpoint and everything else (postgres, forge, gatekeeper,
# tickets, conductor) over in-cluster DNS — see the chart values for the wiring.
FROM golang:1.25 AS build
WORKDIR /src
# Modules first, so a source-only change reuses the download layer.
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# GOWORK=off builds from THIS module's go.mod alone (never a parent workspace);
# CGO off yields a static binary that runs on distroless; -buildvcs=false because
# the build context carries no .git.
RUN GOWORK=off CGO_ENABLED=0 GOFLAGS=-buildvcs=false go build -trimpath -o /out/blacksmith .

# Static distroless: ca-certificates for TLS to the model/forge/gatekeeper, and a
# nonroot user. The sandbox that runs agent code is a forge execution, not a local
# container, so the runtime needs nothing but the binary and roots.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/blacksmith /usr/local/bin/blacksmith
# The action server's port (PORT env overrides); documented here for the Service.
EXPOSE 8199
ENTRYPOINT ["/usr/local/bin/blacksmith"]
CMD ["serve"]
