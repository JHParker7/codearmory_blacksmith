# How an agent writes code

The single richest source of failure in this project, and the clearest
demonstration of its central principle: **prefer making a mistake unrepresentable
over explaining it.**

## The grammar is the interface, not the tool list

The developer's reply is produced under a **grammar-constrained sampler**
(`ReplySchema`), not offered as tool calls.

Tool calls were tried first, on the reasoning that one arrives typed and named
and the server validates it. **That is not true of this backend**: the gateway
drops `response_format` whenever tools are sent, so the *arguments* are generated
completely unconstrained.

What that permits, read from a live completion:

```
...Encode(task)\n\t}\n}'}], ","start_line": 89
```

The model closed the `replace` string with an apostrophe and carried on writing
JSON. The recovered value takes `'}], ` into the Go source, which comes back as
`rune literal not terminated` — **14 of 23 refusals in one window**, plus the
turns counted as unparseable.

A grammar-constrained sampler cannot emit that: it can only produce tokens that
keep the JSON valid, so the mistake is unrepresentable rather than repaired
afterwards. This path — the one carrying whole Go files through a string field —
is where it was most needed.

Keep the tool definitions and keep accepting a tool call, so a backend that
answers that way still works. Simply do not offer them.

## One shape per way of pointing at the code

The `edits` schema **branches**. It does not have one object with every
addressing field optional.

With `old_str` and `replace` both required and adjacent, a constrained sampler
walks the fields in order, and the highest-probability continuation after a long
quote is **that same quote again**. Measured: under the flat shape the model sent
`old_str` identical to `replace` in **39 of 47 turns**, and the no-progress
ceiling then killed the ticket — twice, taking the board with it. A message
naming the mistake did not move it, and neither did a length cap.

Branching removes the opportunity rather than discouraging it:

| shape | address | why it cannot copy |
|---|---|---|
| **decl** (preferred) | name a declaration | has no `old_str` to copy |
| **anchor** | a short unique snippet | the quote is capped short, so it cannot duplicate a long replacement |
| **line range** | `start_line`/`end_line` | carries no quote at all |

`replace` is the only place new code goes, in every shape.

## Bound every field

The schema becomes a grammar, and **an unbounded string in a grammar is an
unbounded reply**. Measured when a backend without tool support first fell
through to it: the model filled `type` with
`replace_all_content_in_file_if_exists_...` and kept going for **5,000 tokens and
348 seconds** — schema-valid the whole way, because nothing said how long a
string may be.

Every field carries a length or an enum.

## Require what the action needs

One shape per action, not a single object requiring only `action`. A flat object
was harmless while it was a fallback and became the whole contract when tools
were dropped: the grammar happily admitted `{"action":"write_files"}` — **25
refusals of "write_files with no edits" in the first window after the switch**,
every one of them schema-valid.

## Repair what you can, visibly

A reply carrying a good file must not be thrown away over one character.

- Raw control characters inside strings are escaped rather than rejected.
- An **unknown escape is preserved as a literal backslash**, not dropped.
  Dropping is tempting — a stray `\` before a space was probably meant as a
  space — but the same rule silently rewrites `\d` to `d` and corrupts a regex in
  code about to be committed. Preserving can leave a literal backslash where none
  was wanted, which fails to compile and is a failure the agent can see and fix.
  **Visible beats silent.**
- `\u` is only an escape if four hex digits follow. Accepting it unchecked cost
  the whole reply for any string containing `\users`, `\usr` or a Windows path:
  `invalid character 's' in \u hexadecimal character escape`, edit discarded.
  This one survived a hundred runs and was found by reading the function against
  its own stated principle.

Only ONE value is decoded, not the whole payload. A model that appends prose or a
second object after a valid one is common; `json.Decoder` reads exactly one value
and stops where `Unmarshal` insists the input contain nothing else. A **truncated**
value still fails, which is the case that genuinely cannot be recovered.

## Undo is progress, not a refusal

`undo_edit` restores the file to what it was before the last write. It costs a
turn and buys a file the agent can address again — strictly better than editing
blind against a file it has broken, where every subsequent `old_str` misses and
every line number is wrong.

It must not count toward any ceiling.
