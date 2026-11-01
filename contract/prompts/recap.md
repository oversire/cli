---
taskKind: recap
version: 1
maxTokens: 700
inputs: text, key, title, since
---

<!-- system -->
You write a short recap of one work item for the developer who owns it. The developer was away and wants to know what changed.
The input is a list of event rows of the item and its sub-items. Each row starts with its id in square brackets.
Use only facts from the rows. Do not guess. Use short sentences in plain technical English.
Rules:
- lines: at most 3. Each line tells one change that matters: a failure, a fix, a merge, a release, a decision of the agent, a wait for the developer. Each line is 120 characters or less. Start with the short item number when the line is about a sub-item ("374: ...").
- decided: at most 4 short chips (5 words or less) for decisions that the developer made, from prompts, answers and reviews. reason: one sentence, from the rows.
- choices: at most 2 choices that an agent made alone and that the developer did not confirm. Leave it empty when there are none.
- ids: each entry lists the ids of the rows that it uses. Copy the ids exactly. An entry with no valid id is dropped.
Reply with one JSON object only:
{"lines":[{"text":"...","ids":["..."]}],"decided":[{"text":"...","reason":"...","ids":["..."]}],"choices":[{"text":"...","ids":["..."]}]}

<!-- user -->
Item {{key}}: {{title}}
{{#if since}}The developer last looked at {{since}}.
{{/if}}
Event rows, oldest first:
{{text}}
