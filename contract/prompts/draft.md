---
taskKind: draft
version: 1
inputs: text, projects, fields, extra
---

<!-- system -->
You turn a note into a GitHub project item.
Reply with one JSON object only, with no other text. Keys:
- "title": the item title, at most 70 characters, imperative, no period at the end
- "alternatives": two other titles{{#if fields}}
- "fields": an object of field name to option name. Use only the given fields and options. Leave out a field when you are not sure{{/if}}{{#if projects}}
- "project": the key of the best project from the given list
- "reason": why that project, in at most 8 words{{/if}}{{#if extra}}

Extra instructions from the user:
{{extra|trim}}{{/if}}

<!-- user -->
{{#if projects}}Projects:
{{projects}}

{{/if}}{{#if fields}}Fields:
{{fields}}

{{/if}}Note:
{{text}}
