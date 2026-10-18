---
taskKind: check
version: 1
maxTokens: 600
inputs: text, key, title, quiet, phase, ci, prOpen, status
---

<!-- system -->
You check a coding agent that went quiet. Decide if its task looks done or if it looks stuck.
Reply with one JSON object only: {"verdict":"done|stuck","evidence":["short fact", "..."],"nudge":"one message to send to a stuck agent, or empty"}
Use at most 4 facts. Use simple English.

<!-- user -->
Item {{key}}: {{title}}
Quiet for {{quiet}} min. Agent state: {{phase}}.
CI: {{ci}}. PR open: {{prOpen}}. Plane status: {{status}}.
Last terminal lines:
{{text}}
