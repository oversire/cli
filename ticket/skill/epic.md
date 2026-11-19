# oversire epic: break a goal into tickets

Goal: one epic for one goal, and one planned ticket for each part of the goal. On Plane, an epic is a module.

The OverSire app on this Mac reads and changes the epics: use the `oversire epic` commands. Each command takes `--json`. Exit code 3 means that no daemon or no app answers: tell the user to open the OverSire app, and stop. When the app answers that it has no loaded item, wait one minute for its refresh and run the command again.

## 1. Find or make the epic

- List the epics: `oversire epic list --project <PROJECT>`. If the command fails because the project has no epics (modules are off), tell the user and stop.
- If the user names an epic that exists, use it.
- Else make it: `oversire epic create --project <PROJECT> --title "<title>" --goal "<goal>"`. Add `--target YYYY-MM-DD`, `--start YYYY-MM-DD` and `--lead <person id>` when the user gives them.
- The goal is Markdown: what is true when the epic is done, in one to three sentences. Add a line "Done when: <check>".

## 2. Break the goal into tickets

Write `brief.md` with the goal, the decisions of the user, and the code that you read. Then plan the parts.

{{template "planning" .}}
Each part is one ticket that can merge alone. Prefer three to eight tickets. If a part has its own parts, make sub-items of that ticket, not more tickets in the epic.

## 3. Make the tickets

For each part:

- Make the ticket as `oversire ticket skill new` tells (title, description with its spec, state, lead, assignee).
- Add the tickets to the epic: `oversire epic add "<epic>" <KEY>...`. One command takes all the keys. `<epic>` is the title or the id of the epic.

An item is in one epic. If an item is in a different epic already, ask the user before you move it. `oversire epic add` moves it. `oversire epic remove <KEY>...` takes items out of their epic.

{{template "todo" .}}
## 4. Report

Run `oversire epic show "<epic>" --project <PROJECT>`. It gives the URL, the progress, the numbered steps in the order to do them, and the forecast. `oversire epic plan --project <PROJECT>` gives the plan of all open epics.

Give the name and the URL of the epic, the keys of the tickets in the order of the steps, and one line for each ticket. Do not paste the specs.
