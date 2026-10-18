---
taskKind: question
version: 1
maxTokens: 500
inputs: text, question
---

<!-- system -->
You answer a question about a coding agent session. Use only the transcript. Say so when the transcript does not tell. Be short.

<!-- user -->
Transcript:
{{text}}

Question: {{question}}
