# OverSire tickets (oversire {{.Version}})

The `oversire` skill makes, plans, works and closes tracker tickets. The user does not plan by hand: each new ticket gets a full spec.

## Commands

| Command | Use |
| --- | --- |
| `new [text]` | Make a planned ticket from the work of this session. |
| `epic [goal]` | Make an epic (a Plane module) for one goal and break the goal into planned tickets. |
| `plan [KEY...]` | Plan an existing ticket that has no full spec, or a batch of them (no key: choose by date). |
| `work <KEY>` | Work a ticket, in this session or in a new session. |
| `delegate <KEY> [--harness codex]` | Give the work of a planned ticket to a worker agent on the daemon. A background Overseer drives it to ready for review. |
| `groom [PROJECT] [--backlog]` | Groom the intake of the project: merge, close and fold items, after your yes. |
| `sweep [--project P]` | Clean up started tickets: set Done what is merged, end the old sessions, fix wrong states. |
| `done [KEY]` | Close the ticket and attach the transcript of this session. |

Find the command in the arguments of the skill. With no command:

- The user asks for a ticket in plain words ("create a ticket", "make a ticket for this"): use `new`.
- The user names a ticket to work ("pick up OVERSIRE-12"): use `work`.
- The user asks to hand a planned ticket to a worker ("delegate OVERSIRE-12", "give it to codex"): use `delegate`.
- The user asks to make an epic, or to break a goal into tickets: use `epic`.
- The user asks to plan more items ("plan the new items", "plan these"): use `plan`.
- The user asks to "groom the intake", "clean up the intake" or "combine the intake items": use `groom`.
- The user asks to "clean up the tickets", asks "what is done", or asks to "check the tickets in progress": use `sweep`.
- Else show the table above and stop.

Then run `oversire ticket skill <command>` and do its steps.

## This session

- Harness: {{.Harness}}. Session: {{or .Session "unknown"}}.
- Machine: {{.Machine}}. Folder: {{.Cwd}}.
- Tickets of this session: {{if .Items}}{{join .Items ", "}}{{else}}none{{end}}.

## Rules for all commands

- Use `oversire item` and `oversire project` for the items of each tracker (Plane, Big Timer). Run `oversire item` with no arguments to see the commands. Put long text in a file: `--body-file <file>`, or `--body-file -` for stdin. The OverSire app on this Mac runs them. Exit 3 means that no app answers: tell the user to open the OverSire app.
- When a command answers "<Tracker> has no X", the tracker does not have that feature. Skip the step and tell the user.
- `plane-cli` is only for three jobs on Plane: the intake, the list and delete of attachments, and the created-date scope. The `oversire` commands do all other item work. If you do not know a `plane-cli` command, load its skill.
- For the epics and the north star of a project, use `oversire epic` and `oversire project north-star`. The OverSire app on this Mac runs them. Run `oversire epic` with no arguments to see the commands.
- For CI, use `oversire ci status`, `oversire ci wait` and `oversire ci why`. When a run fails, run `oversire ci why RUN`. For a short cause and fix, run `oversire ci explain RUN`. Do not poll with `gh run watch`, `gh run view` or `gh pr checks`: these calls use the shared GitHub quota. Exit 3 means that no OverSire app answers.
- Get the tracker workspace and project for this folder from the instructions of the user (CLAUDE.md). If they do not tell, ask the user one time.
- Use the writing rules of the user and of the repository for all ticket text.
- Do not change the OverSire block of a description: the lines from "OverSire block ▾" to "OverSire block ▴". When you write a new description, keep the block at the end with no change.
- Write the files for the tracker in your scratchpad folder if you have one, else in a temporary folder.
- Do not ask the user to approve a plan that they asked for. Ask only about open design decisions.
- In commit messages, add the trailer line `Item: <KEY>`, unless the repository has a different rule.
