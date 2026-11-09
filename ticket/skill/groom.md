# oversire groom [PROJECT] [--backlog] [--stale N]: groom the intake

Goal: a short, clear intake. Groom merges the items that tell the same problem, closes the items that are done or obsolete, and folds items under a parent. It does the same work as Groom in the app (`oversire/docs/groom.md`), in this chat.

## 1. Find the project

- A project in the arguments wins.
- Else use the map of the folder to the Plane project in the instructions of the user (CLAUDE.md).
- Ask the user one time only when the map has no line for this folder.

## 2. Check the Groom key

Run `oversire ticket groom <PROJECT>`.

- When it fails with "A Groom session ... runs", tell the user and stop.
- Keep its list of items with a live session. These items are busy.

## 3. Read the items

- Read the pending intake: `plane-cli intake list --status -2`. To include snoozed items, use `--status-in -2,0`.
- With `--backlog`, also read `plane-cli wi list --state-group-in backlog,unstarted`. `--per-page` allows at most 200.
- Skip the items in a started, completed or cancelled state, and the busy items.
- The skill cannot see the queue lanes of the app. Tell the user this one time.
- Read at most 200 items. With more, tell the user and use the intake only.
- For each item, read the key, title, description, state, labels, created, updated and comments.

## 4. Check old items against the code

For each item older than N days (`--stale`, default 14, as in the app):

- Run `git log --all --grep <KEY>` in each repository.
- Run `gh pr list --search <KEY> --state all`.
- Read the files that the item names.

Use read-only commands only. Do not change files.

## 5. Propose

Use the kinds and the rules of the Groom prompt (`Contract/prompts/groom.md`):

- The kinds are merge, done, obsolete, fold, relate and vague.
- Find the groups of items that tell the same problem.
- A merge keeps the clearest item. Its new description keeps each fact of each item and ends with "## Merged from" and the other keys.
- Each done line and each obsolete line has evidence: a commit, a PR, or file:line.
- The confidence is sure only with clear evidence or a yes of the user. Else it is maybe.
- Make no line for a clear item that stays.
- Use only keys of the list, except for a parent or a relation.

## 6. Show and ask

- Show one table: #, kind, keys, reason (at most 25 words), evidence, confidence.
- Ask with AskUserQuestion only about the maybe lines. Ask at most 4 questions in one call. Ask about merges and closes first. A yes makes the line sure. A no removes the line.
- Then ask one time. The options are "Apply all N", "Apply some (type the numbers)" and "Cancel".
- Nothing goes to Plane before this yes.

## 7. Apply

Show one table of the writes. Then write one item at a time, as the app does:

| Kind | Work item | Intake item |
| --- | --- | --- |
| merge | The kept item gets the new description. Keep its OverSire block. Each other item gets the `duplicate` relation to the kept item, the comment "Merged into <KEEP> by Groom. <reason>", and the Cancelled state. | Each other item: `plane-cli intake update --status 2 --duplicate-to-id <id of KEEP>` (2 = Duplicate). |
| done | The comment "Done: <reason>" plus the evidence lines, then the Done state. | The same comment, then `--status -1` (Rejected). |
| obsolete | The comment "Obsolete: <reason>", then the Cancelled state. | The same comment, then `--status -1` (Rejected). |
| fold | Set the parent. | Set the parent, then `--status 1` (Accepted). |
| relate | Add the relation. | Add the relation. |
| vague | No write. | No write. |

- A failed line stays in the list with its error. Offer to try again only the failed lines.
- The Undo of the app does not cover these writes. Tell the user this.

Then write the event rows:

- Put one line for each written item in `applied.json`: a JSON array of `{"item", "parent", "summary", "payload"}`. The summary tells the change, for example "Merged into OVERSIRE-10". The payload has `kind` and, when they apply, `keep`, `parent`, `relation` (`{"kind", "key"}`) and `evidence` (at most 6 lines).
- A failed line gets no row.
- Run `oversire ticket applied groom --file applied.json --project <PROJECT>`.

## 8. Next

- Report the counts for each kind.
- For the vague items, offer Enhance in the app, or `plan`.
- Ask: "Plan the N items that stay?" On yes, run `oversire ticket skill plan` and do "Many tickets" with these keys. Skip the scope question.
