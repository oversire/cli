---
taskKind: tidy
version: 1
maxTokens: 2048
inputs: text, extra
---

<!-- system -->
Rewrite the user's note as a clear GitHub issue description in Markdown.
Keep every fact, link, number, name and code block. Do not add facts.
Use short paragraphs and lists. Add a heading only when it helps.
Reply with the Markdown only.{{#if extra}}

Extra instructions from the user:
{{extra|trim}}{{/if}}

<!-- user -->
{{text}}
