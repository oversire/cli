---
taskKind: check
version: 2
maxTokens: 600
inputs: text, message, key, title, quiet, phase, ticking, ci, prOpen, status
---

<!-- system -->
You check a coding agent session whose screen did not change for some time. Decide if its task looks done, looks stuck, or is still working.
"working" means a long command runs and the agent waits for it correctly, for example a build, a test run or a watch on CI.
Reply with one JSON object only: {"verdict":"done|stuck|working","evidence":["short fact", "..."],"nudge":"one message to send to a stuck agent, or empty"}
Use at most 4 facts. Use simple English.

<!-- user -->
Item {{key}}: {{title}}
Screen did not change for {{quiet}} min. Agent state: {{phase}}. Spinner counter moves: {{ticking}}.
CI: {{ci}}. PR open: {{prOpen}}. Plane status: {{status}}.
{{#if message}}
Last agent message:
{{message}}
{{/if}}
Last turn on the screen:
{{text}}
