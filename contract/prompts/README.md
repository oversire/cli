# Prompts

One file for each AI task kind: `<taskKind>.md`. The app (Swift), the cloud (TypeScript) and the runner (Go) render the same files.
The caller builds the `AIInput`. The file turns the input into the system text and the user text.

`jobs/<kind>.md` holds the prompts of the cloud jobs, one for each `Job` kind. Only the cloud renders them. The app does not embed them.
Each job prompt replies `{"suggest":false}` or `{"suggest":true,"title","body","proposal"?}`. The cloud makes a `Suggestion` from the reply.
`jobs/plan.md` (paid) is the Planner. It proposes a plan change for one queue lane: order moves, relations and area tags. Its reply names items by key (`"item":"KEY-1"`). The hub maps each key to an `ItemRef`. Schema 1 named it `sequence`.

The input `vision` is the project vision: the memory entry with kind `vision`, max 500 characters. The job prompts `triage` and `plan` use it in an `{{#if vision}}` block. Thus a project without a vision gives the same text as before.
In the app, the vision goes into the intake prompt (`AgentPrompts`) and into the `text` of the queue order task.

## File format

```text
---
taskKind: failureReason
version: 1
maxTokens: 400
inputs: text, workflow, number
---

<!-- system -->
The system template.

<!-- user -->
The user template.
```

- The front matter has one `key: value` on each line.
  - `taskKind`: the `AITask` kind. It is the same as the file name.
  - `version`: increase it when the output of the prompt changes.
  - `maxTokens`: optional. Missing: the limit of the engine settings.
  - `inputs`: the names that the templates use. `text` is `AIInput.text`. The other names are keys of `AIInput.context`.
- `<!-- system -->` starts the system template. It is optional. Missing: an empty system text.
- `<!-- user -->` starts the user template.
- A template is the text after its marker line, up to the next marker line or the end of the file.
  The renderer removes the newlines at the end of a template.

## Template syntax

- `{{name}}`: the value of the input. A missing context key gives an empty text.
- `{{name|trim}}`: the value without white space at the start and at the end.
- `{{#if name}}…{{/if}}`: the text in the block shows only when the value, without white space at the ends, is not empty.
  Blocks do not nest.

There is no other syntax. A single `{` or `}` is plain text.

## Rules

- The output of each prompt must not change by accident. `Tests/OverSireAITests/PromptGoldenTests.swift` renders each prompt
  with fixed inputs and compares the result with `Golden/prompts.json`.
- `cloud/src/contract/prompts/` holds a copy. `npm run contract:check` compares it.
- In the app, `Sources/OverSireAI/Prompts/PromptFiles.swift` holds the files as Swift text.
  `scripts/gen-prompts.sh` writes it. `ContractTests` checks that it is current.
