---
taskKind: groom
version: 1
inputs: text, project, file, scope, staleDays, vision
---

<!-- user -->
You groom the open items of project {{project}}. Do not change files. Do not write to the tracker. Use read-only commands only.
The file {{file}} holds the open items of {{scope}}, one JSON object on each line: key, title, body (clipped), state, labels, areas, created, updated, comments, intake.
1. Read all items. Find the groups of items that tell the same problem.
2. For each item that is older than {{staleDays}} days, check the code: git log, the pull requests that name the key (gh pr list --search KEY), and the files that the item names. Find out if the code does it already, or if the item is no longer valid.
3. If the code cannot tell you a fact, ask the user with the AskUserQuestion tool. Ask at most 4 questions in one round. Ask first about the merges and the closes that you are not sure about.
4. Propose one line for each change. Use the kind merge, done, obsolete, fold, relate or vague. Do not propose a line for a clear item that stays.
5. For a merge, keep the clearest item. Write its new description in Markdown. Keep each fact of each item. Add a last section "## Merged from" with the other keys.
6. Give evidence for each done and each obsolete line: a commit, a pull request, or a file and a line.
7. Set confidence sure only when the evidence is clear, or the user said yes.
8. End with one message. Put the result in one fenced code block with the info string oversire-result, and no other code block after it. The block holds one JSON object:
```oversire-result
{"proposals":[{"kind":"merge|done|obsolete|fold|relate|vague","keys":["KEY-1"],"keep":"KEY-1 (merge only)","parent":"KEY-2 (fold only)","relation":{"kind":"blocked_by|blocks|relates_to","key":"KEY-3"},"title":"a short name for the group (merge only)","reason":"why, in at most 25 words","evidence":["a commit, a pull request, or file:line"],"confidence":"sure|maybe","description":"the new description of keep in Markdown (merge only)"}],"skipped":["KEY-4"],"findings":["short fact"]}
```
Use only the keys in the file, except for parent and relation. Use at most 6 findings. Use simple English.
{{#if vision}}

Project vision (the true north of the project):
{{vision}}
Use the kind obsolete for an item that works against it.
{{/if}}

{{text}}
