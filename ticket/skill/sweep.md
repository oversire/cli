# oversire sweep [--project P]: clean up started tickets

Goal: started tickets show the true state. Sweep sets Done what is merged, ends the old sessions, and fixes wrong states. Sweep touches only started items, plus the local work of items that are not started. Groom never touches started items.

## 1. Find the project

- A project in the arguments wins.
- Else use the map of the folder to the Plane project in the instructions of the user (CLAUDE.md).
- Ask the user one time only when the map has no line for this folder.

## 2. List the started items

Run `plane-cli wi list --state-group started`. Keep the key, the state name, the parent and the sub-items of each item.

## 3. Get the facts

- Run `oversire ticket sweep <KEY>... --json` with the keys of step 2.
- Run it one more time with `--project <PROJECT>` and no keys. This finds local work whose item is not started. These items are the orphans.

Each row has a signal: merged, conflict, approved, ready, working, stalled, open or none. It also has the sessions, the review state, the worktree group, the repos and the commits on the base branches.

## 4. Check the PRs

For a repository with a remote, run `gh pr list --search <KEY> --state all`. A merged PR counts as merged.

## 5. Sort into buckets

Show one table for each bucket: key, Plane state, signal and the proposed action.

| Bucket | Rule | Proposed action |
| --- | --- | --- |
| Merged | Signal merged, or a merged PR | Set Done with a comment that names the commits. End the live sessions. Remove the group (worktree-groups step 4) only when each repo has ahead 0 and is not dirty. |
| Approved, not merged | Signal approved | Ask the user to merge in OverSire. Do not merge. |
| Wrong state | In Progress and signal ready or approved, or In Review and signal working | Move the item to the state that agrees (Needs Review if the project has it). |
| Stalled | Signal stalled | Ask the user: keep, move back to the backlog, or Cancelled. |
| Active | Signal working | No action. List only. |
| Orphans | Local work of an item that is Done, Cancelled or not started | End the sessions. Remove the group if it is merged, else ask. |

## 6. Confirm each bucket one time

Ask with AskUserQuestion: one question for each bucket, at most 4 in one call. The options are "Apply all", "Pick lines (type the numbers)" and "Skip".

## 7. Apply

- Set the state and add the comment with `plane-cli`.
- Close the open sub-items before their parent, because the parent guard reopens a Done parent.
- Run `oversire ticket stop <SESSION>` for each stoppable live session. For a `cli` session, or a session on a different machine, tell the user.
- Do not merge. Do not push. Never use `rm -r` in `.wt/`. When `git branch -d` refuses, stop and tell the user.

Then write the event rows:

- Put one line for each changed item in `applied.json`: a JSON array of `{"item", "parent", "summary", "payload"}`. The summary tells the change, for example "Done: merged in oversire". The payload has `bucket` (merged, wrong_state, stalled or orphan) and `action` (done, state, stop or remove_group), and when they apply, `state`, `sessions` (at most 10) and `group`.
- Run `oversire ticket applied sweep --file applied.json --project <PROJECT>`.

## 8. Report

Give one table: key, bucket, what changed.
