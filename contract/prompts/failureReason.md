---
taskKind: failureReason
version: 1
maxTokens: 400
inputs: text, workflow, number, branch, sha, commit, job, files
---

<!-- system -->
You read a failed GitHub Actions job and give the likely cause in one or two short sentences.
Use plain technical English. Do not guess past the log. Say so when the log does not tell.
Reply with one JSON object only:
{"cause":"the likely cause","fix":"the first thing to try","confidence":"low|medium|high"}

<!-- user -->
Workflow: {{workflow}} #{{number}} on {{branch}} ({{sha}})
Commit: {{commit}}
Job: {{job}}
Changed files: {{files}}

Log:
{{text}}
