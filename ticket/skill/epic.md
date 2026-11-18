# oversire epic: break a goal into tickets

Goal: one epic for one goal, and one planned ticket for each part of the goal. On Plane, an epic is a module.

## 1. Find or make the epic

- List the modules: `plane-cli -w <ws> -p <PROJECT> module list`. If the call fails because modules are off in the project, tell the user and stop.
- If the user names an epic that exists, use it.
- Else make it: `plane-cli module create --name "<title>" --description "<goal>" --status planned`. Add `--target-date YYYY-MM-DD` and `--lead-id <user id>` when the user gives them.
- The goal is Markdown: what is true when the epic is done, in one to three sentences. Add a line "Done when: <check>".

## 2. Break the goal into tickets

Write `brief.md` with the goal, the decisions of the user, and the code that you read. Then plan the parts.

{{template "planning" .}}
Each part is one ticket that can merge alone. Prefer three to eight tickets. If a part has its own parts, make sub-items of that ticket, not more tickets in the epic.

## 3. Make the tickets

For each part:

- Make the ticket as `oversire ticket skill new` tells (title, description with its spec, state, lead, assignee).
- Add it to the epic: `plane-cli module manage-work-items <module id> --add <work item id>`. The work item id is the UUID, not the key.

An item is in one epic. If an item is in a different module already, ask the user before you move it. To move it, add it to the new module, then remove it from the old one with `--remove <work item id>`.

{{template "todo" .}}
## 4. Report

Give the name and the URL of the epic, the keys of the tickets in the order to do them, and one line for each ticket. Do not paste the specs.
