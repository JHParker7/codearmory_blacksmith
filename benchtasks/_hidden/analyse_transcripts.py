"""What happened in a pipeline run, read from its own transcripts.

Written to answer one question — why did the Go pipeline block on the same tasks
— and it found the answer in a few minutes: every stage succeeded except the
developer (18 of 26 attempts failed, every other role 100%), and 41 of the
developer's 51 refusals were `noop_edit`, the same change applied over and over.

Streams the JSONL rather than loading it: that corpus is 399MB.

NOTE the field names. A refusal's KIND is in `tool`, not `status` — reading the
obvious field gives every refusal as "?" and hides the whole finding.
"""

import json
import sys
from collections import Counter, defaultdict
from pathlib import Path

paths = [Path(p) for p in sys.argv[1:]]

outcomes = Counter()
by_role = defaultdict(Counter)
refusals = Counter()
refusal_by_role = defaultdict(Counter)
turns_per_transcript = Counter()
task_outcomes = defaultdict(list)
errors = Counter()

for path in paths:
    with path.open(encoding="utf-8", errors="replace") as fh:
        for line in fh:
            try:
                rec = json.loads(line)
            except ValueError:
                continue
            kind = rec.get("kind")
            role = rec.get("role", "?")
            if kind == "turn":
                turns_per_transcript[rec.get("transcript_id", "")] += 1
                if rec.get("error"):
                    errors[rec["error"][:90]] += 1
            elif kind == "outcome":
                status = rec.get("status", "?")
                outcomes[status] += 1
                by_role[role][status] += 1
                task_outcomes[rec.get("task_id", "?")].append((role, status))
            elif kind == "refusal":
                refusals[rec.get("status", "?")] += 1
                refusal_by_role[role][rec.get("status", "?")] += 1

print("=== outcomes")
for status, n in outcomes.most_common():
    print(f"  {status:<12} {n}")

print("\n=== outcomes by role")
for role, counts in sorted(by_role.items()):
    total = sum(counts.values())
    bad = counts.get("blocked", 0) + counts.get("failed", 0)
    print(f"  {role:<18} {total:>4} attempts   blocked/failed {bad:>4}   "
          + " ".join(f"{k}={v}" for k, v in counts.most_common()))

print("\n=== refusals (why the harness said no)")
for kind, n in refusals.most_common(15):
    print(f"  {kind:<20} {n}")

print("\n=== refusals by role")
for role, counts in sorted(refusal_by_role.items()):
    print(f"  {role:<18} {sum(counts.values()):>6}   " + " ".join(f"{k}={v}" for k, v in counts.most_common(5)))

print("\n=== longest attempts (turns in one transcript)")
for tid, n in turns_per_transcript.most_common(10):
    print(f"  {n:>5} turns   {tid}")

if errors:
    print("\n=== turn errors")
    for msg, n in errors.most_common(8):
        print(f"  {n:>5}x  {msg}")

blocked_tasks = [t for t, rs in task_outcomes.items() if any(s == "blocked" for _, s in rs)]
print(f"\n=== tasks that ended blocked: {len(blocked_tasks)} of {len(task_outcomes)}")
for t in blocked_tasks[:15]:
    print(f"  {t}: " + ", ".join(f"{r}={s}" for r, s in task_outcomes[t]))
