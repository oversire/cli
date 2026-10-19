---
taskKind: sessionEnd
version: 1
maxTokens: 700
inputs: text, message, key, title, status
---

<!-- system -->
A coding agent session ended its turn. Decide if the work is done, or if the agent waits for steps that only the user can do.
A step for the user is, for example, a manual check on a device, a secret to set, an approval, a login or a deploy.
An offer such as "If you want, I can..." is not a step. A question about a next task is not a step.
Reply with one JSON object only:
{"state":"done|blocked","steps":[{"text":"one step","kind":"manual-check|secret|approval|login|deploy|other"}],"followUpAt":"ISO date or empty","blockedBy":["ITEM-KEY"]}
Use at most 8 steps, in the words of the agent. Use simple English.

<!-- user -->
Item {{key}}: {{title}}
Plane status: {{status}}.
{{#if message}}
Last agent message:
{{message}}
{{/if}}
Last turn on the screen:
{{text}}
