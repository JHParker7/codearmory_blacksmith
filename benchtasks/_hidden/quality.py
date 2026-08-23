"""Structural quality of a generated module: documentation, typing, size."""

import ast
import sys
from pathlib import Path

path = Path(sys.argv[1])
source = path.read_text()
tree = ast.parse(source)

funcs = [n for n in ast.walk(tree) if isinstance(n, (ast.FunctionDef, ast.AsyncFunctionDef))]
classes = [n for n in ast.walk(tree) if isinstance(n, ast.ClassDef)]
documented = [n for n in funcs + classes if ast.get_docstring(n)]
annotated = [
    f
    for f in funcs
    if f.returns is not None or any(a.annotation for a in f.args.args)
]
comments = sum(1 for line in source.splitlines() if line.strip().startswith("#"))

print(f"file              {path.name}")
print(f"lines             {len(source.splitlines())}")
print(f"module docstring  {'yes' if ast.get_docstring(tree) else 'NO'}"
      f" ({len((ast.get_docstring(tree) or '').splitlines())} lines)")
print(f"classes           {len(classes)}")
print(f"functions         {len(funcs)}")
print(f"documented        {len(documented)}/{len(funcs) + len(classes)}")
print(f"type-annotated    {len(annotated)}/{len(funcs)}")
print(f"inline comments   {comments}")

longest = max(funcs, key=lambda f: (f.end_lineno or 0) - f.lineno, default=None)
if longest is not None:
    print(f"longest function  {longest.name} ({(longest.end_lineno or 0) - longest.lineno} lines)")

# Exception ordering: `except ValueError` before `except json.JSONDecodeError`
# shadows it, since JSONDecodeError subclasses ValueError.
for handler_owner in funcs:
    for node in ast.walk(handler_owner):
        if not isinstance(node, ast.Try):
            continue
        seen = []
        for handler in node.handlers:
            names = []
            t = handler.type
            for part in (t.elts if isinstance(t, ast.Tuple) else [t]) if t else []:
                names.append(ast.unparse(part))
            if "ValueError" in seen and any("JSONDecodeError" in n for n in names):
                print(
                    f"UNREACHABLE       line {handler.lineno}: JSONDecodeError after ValueError "
                    f"in {handler_owner.name}() — it is a subclass, so this never runs"
                )
            seen.extend(names)
