"""Integration tests the pipeline never sees, run against what it delivered.

WHY BLACK BOX. The Go runs invent their own internals: r80 built `package store`
with NewHandler(*Store), r85 built `package ticket` with a different surface, and
neither produced a binary. A Go test file dropped into the repository cannot even
compile against both, so a hidden suite written that way would measure naming
rather than behaviour.

So this builds the repository, starts the server it is asked to produce, and
speaks HTTP to it. Nothing here depends on a type name, a package name or a
function signature — only on what the request actually asked for.

WHY IT IS HIDDEN. The pipeline writes its own tests, so "21 of 21 passing" means
it satisfied the specification it wrote. r80 passed 23 of 23 while silently
accepting a 5MB title and never rejecting a non-object body — both named in the
request in plain prose. A suite the authors never see is the only measure of that
which cannot be gamed.

Every case below is drawn from the request text. Nothing is invented; if the
request does not ask for it, it is not tested.

Usage: go_integration.py <repo-dir> [--verbose]
"""

import json
import os
import random
import shutil
import socket
import subprocess
import sys
import time
import urllib.error
import urllib.request

REPO = sys.argv[1] if len(sys.argv) > 1 else "."
VERBOSE = "--verbose" in sys.argv

results = []


def check(name, ok, detail=""):
    results.append((name, bool(ok), detail))
    if VERBOSE or not ok:
        print(f"  {'PASS' if ok else 'FAIL'}  {name}" + (f"  — {detail}" if detail and not ok else ""))


def free_port():
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


def call(method, path, body=None, raw=None, port=0, timeout=15):
    """One HTTP call. Returns (status, headers, text)."""
    url = f"http://127.0.0.1:{port}{path}"
    data = raw if raw is not None else (json.dumps(body).encode() if body is not None else None)
    req = urllib.request.Request(url, data=data, method=method)
    if data is not None:
        req.add_header("Content-Type", "application/json")
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:
            return r.status, dict(r.headers), r.read().decode("utf-8", "replace")
    except urllib.error.HTTPError as e:
        text = e.read().decode("utf-8", "replace")
        hdrs = dict(e.headers or {})
        e.close()
        return e.code, hdrs, text
    except Exception as exc:  # connection refused, timeout, reset
        return 0, {}, str(exc)


BINARY = "/tmp/tracker-under-test"


def main_package():
    """The import path of the repository's main package, or "" if it has none."""
    out = subprocess.run(["go", "list", "-f", "{{.ImportPath}} {{.Name}}", "./..."],
                         cwd=REPO, capture_output=True, text=True, timeout=300,
                         env={**os.environ, "GOWORK": "off", "GOFLAGS": "-mod=mod"})
    if out.returncode != 0:
        return ""
    for line in out.stdout.splitlines():
        parts = line.split()
        if len(parts) == 2 and parts[1] == "main":
            return parts[0]
    return ""


def build_and_start():
    """Build the repository and start its server. Returns (proc, port) or (None, reason).

    THE MAIN PACKAGE IS LOOKED UP RATHER THAN ASSUMED. `go build -o X ./...`
    writes a DIRECTORY at X when the repository is a library, which then exists,
    is not executable, and fails at exec with a bare permission error — so the
    check for "did it produce a binary" has to be about what was built, not about
    whether the path appeared.
    """
    shutil.rmtree(BINARY, ignore_errors=True)
    if os.path.exists(BINARY):
        os.remove(BINARY)

    pkg = main_package()
    if not pkg:
        return None, ("no main package — the repository is a library and cannot be run. "
                      "The request asks for a main package serving on $PORT.")

    out = subprocess.run(["go", "build", "-o", BINARY, pkg],
                         cwd=REPO, capture_output=True, text=True, timeout=600,
                         env={**os.environ, "GOWORK": "off", "GOFLAGS": "-mod=mod"})
    if out.returncode != 0:
        # A repository that does not build is a total failure, and saying which
        # is more useful than every case below failing to connect.
        return None, f"go build failed: {(out.stderr or out.stdout).strip()[:400]}"
    if not os.path.isfile(BINARY) or not os.access(BINARY, os.X_OK):
        return None, "go build produced no runnable binary"

    port = free_port()
    proc = subprocess.Popen([BINARY], cwd=REPO,
                            env={**os.environ, "PORT": str(port)},
                            stdout=subprocess.PIPE, stderr=subprocess.PIPE)
    # Wait for it to answer rather than sleeping a fixed time.
    for _ in range(60):
        if proc.poll() is not None:
            err = (proc.stderr.read() or b"").decode("utf-8", "replace")[:300]
            return None, f"server exited immediately: {err}"
        st, _, _ = call("GET", "/", port=port, timeout=1)
        if st != 0:
            return proc, port
        time.sleep(0.5)
    proc.kill()
    return None, "server never answered on $PORT"


def run(port):
    # ---- the happy path, which everything else needs -------------------
    st, _, body = call("POST", "/tickets", {"title": "first", "description": "d", "status": "open"}, port=port)
    check("POST /tickets creates a ticket (2xx)", 200 <= st < 300, f"got {st}")
    made = {}
    try:
        made = json.loads(body)
    except ValueError:
        check("POST /tickets returns the created ticket as JSON", False, body[:120])
    else:
        check("POST /tickets returns the created ticket as JSON", isinstance(made, dict))
    tid = made.get("id") or made.get("ID") or made.get("ticket_id")
    check("the store assigns an id", tid is not None, f"no id field in {list(made)[:6]}")

    st, hdr, body = call("GET", "/tickets", port=port)
    check("GET /tickets returns 200", st == 200, f"got {st}")
    try:
        listed = json.loads(body)
        check("GET /tickets returns a JSON array", isinstance(listed, list), type(listed).__name__)
    except ValueError:
        check("GET /tickets returns a JSON array", False, body[:120])

    if tid is not None:
        st, _, _ = call("GET", f"/tickets/{tid}", port=port)
        check("GET /tickets/{id} returns the ticket", st == 200, f"got {st}")
        st, _, _ = call("PUT", f"/tickets/{tid}",
                        {"title": "renamed", "description": "d", "status": "closed"}, port=port)
        check("PUT /tickets/{id} updates it (2xx)", 200 <= st < 300, f"got {st}")

    # ---- "an unknown id is a 404" --------------------------------------
    st, _, _ = call("GET", "/tickets/999999", port=port)
    check("GET an unknown id is 404", st == 404, f"got {st}")
    st, _, _ = call("PUT", "/tickets/999999", {"title": "x", "status": "open"}, port=port)
    check("PUT an unknown id is 404", st == 404, f"got {st}")

    # ---- "filtering by status is a query parameter" ---------------------
    call("POST", "/tickets", {"title": "open one", "status": "open"}, port=port)
    call("POST", "/tickets", {"title": "closed one", "status": "closed"}, port=port)
    st, _, body = call("GET", "/tickets?status=closed", port=port)
    check("GET /tickets?status=closed returns 200", st == 200, f"got {st}")
    try:
        got = json.loads(body)
        only = all((t.get("status") or t.get("Status")) == "closed" for t in got) if isinstance(got, list) else False
        check("filtering by status returns only that status", only, body[:160])
    except ValueError:
        check("filtering by status returns only that status", False, body[:120])

    # ---- "a status outside the allowed set is the caller's fault" -------
    st, _, _ = call("GET", "/tickets?status=not_a_status", port=port)
    check("an unknown status filter is 4xx, not 500", 400 <= st < 500, f"got {st}")
    st, _, _ = call("POST", "/tickets", {"title": "x", "status": "not_a_status"}, port=port)
    check("creating with an unknown status is 4xx, not 500", 400 <= st < 500, f"got {st}")

    # ---- "empty and whitespace-only titles" -----------------------------
    st, _, _ = call("POST", "/tickets", {"title": "", "status": "open"}, port=port)
    check("an empty title is refused with 4xx", 400 <= st < 500, f"got {st}")
    st, _, _ = call("POST", "/tickets", {"title": "   \t  ", "status": "open"}, port=port)
    check("a whitespace-only title is refused with 4xx", 400 <= st < 500, f"got {st}")

    # ---- "a body that is not an object" ---------------------------------
    for label, payload in (("an array", b"[1,2,3]"), ("a string", b'"hello"'), ("a number", b"42")):
        st, _, _ = call("POST", "/tickets", raw=payload, port=port)
        check(f"a body that is {label} is 4xx, not 500", 400 <= st < 500, f"got {st}")

    # ---- "malformed" ----------------------------------------------------
    st, _, _ = call("POST", "/tickets", raw=b"{not json", port=port)
    check("malformed JSON is 4xx, not 500", 400 <= st < 500, f"got {st}")

    # ---- "oversized input" ----------------------------------------------
    huge = json.dumps({"title": "x" * 5_000_000, "status": "open"}).encode()
    st, _, body = call("POST", "/tickets", raw=huge, port=port, timeout=30)
    check("a 5MB title is refused rather than stored", 400 <= st < 500,
          f"got {st}" + (" and echoed it back" if 200 <= st < 300 else ""))

    # ---- "a missing field" ----------------------------------------------
    st, _, _ = call("POST", "/tickets", {"description": "no title at all"}, port=port)
    check("a missing title is 4xx, not 500", 400 <= st < 500, f"got {st}")

    # ---- "a path that does not exist" -----------------------------------
    st, _, _ = call("GET", "/no/such/path", port=port)
    check("an unknown path is 404", st == 404, f"got {st}")

    # ---- "an HTML page at / ... grouped by status ... a form" -----------
    st, hdr, page = call("GET", "/", port=port)
    check("GET / returns 200", st == 200, f"got {st}")
    check("GET / is served as HTML", "text/html" in (hdr.get("Content-Type") or ""),
          hdr.get("Content-Type", "(none)"))
    check("the page shows a create form", "<form" in page.lower(), "no <form> element")
    lowered = page.lower()
    check("the page groups by status", all(s in lowered for s in ("open", "closed")),
          "status headings not found")

    # ---- "titles are written by people, so they must be escaped" --------
    call("POST", "/tickets", {"title": "<script>alert(1)</script>", "status": "open"}, port=port)
    st, _, page = call("GET", "/", port=port)
    check("a title containing markup is escaped, not rendered",
          "<script>alert(1)</script>" not in page, "raw <script> reached the page")


print(f"integration suite against {REPO}\n")
proc, port_or_reason = build_and_start()
if proc is None:
    print(f"  FAIL  the delivered repository does not run — {port_or_reason}\n")
    print("0/1 passed")
    raise SystemExit(1)

try:
    run(port_or_reason)
finally:
    proc.kill()
    shutil.rmtree(BINARY, ignore_errors=True)

passed = sum(1 for _, ok, _ in results if ok)
print(f"\n{passed}/{len(results)} passed")
if passed != len(results):
    print("\nfailed:")
    for name, ok, detail in results:
        if not ok:
            print(f"  - {name}" + (f"  ({detail})" if detail else ""))
raise SystemExit(0 if passed == len(results) else 1)
