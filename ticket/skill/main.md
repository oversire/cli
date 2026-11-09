# OverSire tickets (oversire {{.Version}})

The `oversire` skill makes, plans, works and closes tracker tickets. The user does not plan by hand: each new ticket gets a full spec.

## Commands

| Command | Use |
| --- | --- |
| `new [text]` | Make a planned ticket from the work of this session. |
| `plan [KEY...]` | Plan an existing ticket that has no full spec, or a batch of them (no key: choose by date). |
| `work <KEY>` | Work a ticket, in this session or in a new session. |
| `groom [PROJECT] [--backlog]` | Groom the intake of the project: merge, close and fold items, after your yes. |
| `done [KEY]` | Close the ticket and attach the transcript of this session. |

Find the command in the arguments of the skill. With no command:

- The user asks for a ticket in plain words ("create a ticket", "make a ticket for this"): use `new`.
- The user names a ticket to work ("pick up OVERSIRE-12"): use `work`.
- The user asks to plan more items ("plan the new items", "plan these"): use `plan`.
- The user asks to "groom the intake", "clean up the intake" or "combine the intake items": use `groom`.
- Else show the table above and stop.

Then run `oversire ticket skill <command>` and do its steps.

## This session

- Harness: {{.Harness}}. Session: {{or .Session "unknown"}}.
- Machine: {{.Machine}}. Folder: {{.Cwd}}.
- Tickets of this session: {{if .Items}}{{join .Items ", "}}{{else}}none{{end}}.

## Rules for all commands

- The tracker is Plane. Use `plane-cli`. If you do not know a command, load the `plane-cli` skill. Send Markdown with `--description-md @<file>`.
- Get the Plane workspace and project for this folder from the instructions of the user (CLAUDE.md). If they do not tell, ask the user one time.
- Use the writing rules of the user and of the repository for all ticket text.
- Do not change the OverSire block of a description: the lines from "OverSire block ▾" to "OverSire block ▴". When you write a new description, keep the block at the end with no change.
- Write the files for Plane in your scratchpad folder if you have one, else in a temporary folder.
- Do not ask the user to approve a plan that they asked for. Ask only about open design decisions.
- In commit messages, add the trailer line `Item: <KEY>`, unless the repository has a different rule.
