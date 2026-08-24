# blacksmith (python) — two test tiers, each answering a different question.
#
#   unit         Is the logic right?               no I/O beyond a tmpdir
#   test         Do the parts talk to each other?  + an in-process HTTP server
#
# There is no e2e tier yet. When there is, it belongs OUT of `test`, because it
# creates real tickets on a real board.
#
# Used as: make -f Makefile.py test

PY := .venv/bin/python

.PHONY: venv test unit status clean all

all: test

# pytest is the only development dependency, and the harness itself has none.
venv:
	python3 -m venv .venv
	$(PY) -m pip install -q -e '.[dev]'

# Unit + integration. Hermetic, fast, safe to run anywhere.
test:
	$(PY) -m pytest -q

# Unit only: -k excludes the tier that stands up an HTTP server, so a green run
# here means the logic is right independent of the wiring.
unit:
	$(PY) -m pytest -q --ignore=tests/test_codearmory.py

# What this host is configured to do, without starting anything.
status:
	$(PY) -m blacksmith

clean:
	rm -rf .pytest_cache
	find . -name __pycache__ -type d -prune -exec rm -rf {} +
