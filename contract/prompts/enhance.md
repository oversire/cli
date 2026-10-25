---
taskKind: enhance
version: 3
inputs: text, kind, key, title, url, vision
---

<!-- user -->
You enhance {{kind}}. Do not change files. Use read-only commands only.
1. Read the item. Then read the code in this folder and the related items to understand it.
2. If it is a bug, find the steps to reproduce it. Reproduce it with read-only commands if you can.
3. If the code cannot tell you a fact, ask the user with the AskUserQuestion tool. Ask at most 4 questions in one round. You can ask more rounds.
4. Write a new description in Markdown. Keep the original text of the item, unchanged, in a last section "## Original note".
5. End with one message. Put the result in one fenced code block with the info string oversire-result, and no other code block after it. The block holds one JSON object:
```oversire-result
{"verdict":"clear|vague|duplicate|off_vision|reproduced|not_reproduced","description":"the new description in Markdown","priority":"urgent|high|medium|low|none","labels":["..."],"relations":[{"kind":"blocked_by|blocks|duplicate|relates_to","key":"KEY-1","reason":"why, in at most 10 words"}],"findings":["short fact"]}
```
Use at most 6 findings. Name a relation only to an item that you found. Use simple English.
{{#if vision}}

Project vision (the true north of the project):
{{vision}}
Use the verdict off_vision when the item works against it.
{{/if}}

Item {{key}}: {{title}}
{{url}}

{{text}}
