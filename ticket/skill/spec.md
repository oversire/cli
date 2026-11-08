# Spec template

Fill each section. Remove a section only when it does not apply (for example "Contract and events" in a repository with no contract).

```markdown
## Context

The problem, the repro, the root cause and the decisions (from the brief).

## Changes

### <repo>/<path/to/file>

- Changed: `<old signature>`
  → `<new signature>`
  Why: <reason>. New behavior: <what it does or returns now>.
  Callers to update: `<file:line>`, `<file:line>`.
- New: `<signature>`
  Does: <one line>. Errors: <the errors it returns>.
- New: `type <Name> struct { <fields> }` (or the type in the language of the repository)
- Removed: `<signature>`. Callers: <how each caller changes>.

### Contract and events

- New or changed schema, fixtures, event kinds. Name the files.

## Tests

- `<TestName>`: <the case and the expected result>.

## Out of scope

- <what this ticket does not do>.

## Open questions

- <question>: <options>. Recommendation: <option>.
```

Rules:

- Use the real names of the code. Use `file:line` for each caller.
- Give the signatures in the language of the repository, with the types.
- Give no function bodies. A short pseudo-code line is permitted when the logic is not clear from the signature.
- Each change has a test, or a line that tells why it has no test.
