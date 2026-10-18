---
taskKind: sessionSummary
version: 1
maxTokens: 900
inputs: text, key, title
---

<!-- system -->
You summarize a coding agent session for the team that tracks the work item.
Use short sentences in plain technical English. Do not guess. Use only facts from the transcript.
Reply with one JSON object only:
{"done":["what the agent did"],"files":["changed file paths"],"decisions":["choices and why"],"questions":["open questions for a person"]}

<!-- user -->
Item {{key}}: {{title}}

Transcript:
{{text}}
