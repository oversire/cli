# oversire done [KEY]: close a ticket

## 1. Find the ticket

Use the key of the arguments. With no key, use the ticket of this session ("Tickets of this session" in `oversire ticket skill`). With two or more tickets and no key, ask the user.

## 2. Check

- The tests of the spec exist and pass.
- All changes are committed, and merged if the user asked for a merge.

If a check fails, tell the user and stop.

## 3. Close

- Post a result comment on the ticket: what changed, the commits, the tests, and each difference from the spec.
- Set the ticket to Done. If the rules of the root say that a different tool sets Done (for example after a merge), do not set it.
- List the sub-items (`plane-cli wi list --parent-id <id>`). Set each finished sub-item to Done with a comment that names the commit.

## 4. Stop the worker and attach the transcripts

If `oversire ticket skill delegate` ran for this ticket:

- Run `oversire worker stop <KEY>`. The daemon keeps the worker record and its turns after the stop. If the command says "no such session", the worker is already gone.
- If the Overseer agent still runs, stop it (the TaskStop tool, with its agent id).

Run `oversire ticket done <KEY>`. It writes the transcript of this session as gzip files and prints one line for each file: `<path><TAB><name>`. If the ticket has a worker, it also writes the turns of the worker (`<KEY>-worker-turns`). A line with no TAB is a note, for example that there is no worker. Show it to the user and go on.

- List the attachments of the ticket (`plane-cli wi attachment list <KEY>`). Delete each attachment whose name starts with the session part of the names (for example `claude-session-<id>`, or `<KEY>-worker-turns`). Thus the ticket keeps only the latest transcript of this session.
- Upload each file: `plane-cli wi attachment upload <KEY> <path> --name <name>`.

## 5. Report

Give the URL and one line.
