# oversire done [KEY]: close a ticket

## 1. Find the ticket

Use the key of the arguments. With no key, use the ticket of this session ("Tickets of this session" in `oversire ticket skill`). With two or more tickets and no key, ask the user.

## 2. Check

- The tests of the spec exist and pass.
- All changes are committed, and merged if the user asked for a merge.

If a check fails, tell the user and stop.

## 3. Close

- Post a result comment on the ticket with `oversire item comment <KEY> --body-file <file>`: what changed, the commits, the tests, and each difference from the spec.
- Set the ticket to Done with `oversire item done <KEY>`. If the rules of the root say that a different tool sets Done (for example after a merge), do not set it.
- List the sub-items with `oversire item show <KEY> --json` (`subItems`). For each finished sub-item, run `oversire item comment <SUB> --body-file -` with a comment that names the commit, then `oversire item done <SUB>`.

## 4. Stop the worker and attach the transcripts

If `oversire ticket skill delegate` ran for this ticket:

- Run `oversire worker stop <KEY>`. The daemon keeps the worker record and its turns after the stop. If the command says "no such session", the worker is already gone.
- If the Overseer agent still runs, stop it (the TaskStop tool, with its agent id).

Run `oversire ticket done <KEY>`. It writes the transcript of this session as gzip files and prints one line for each file: `<path><TAB><name>`. If the ticket has a worker, it also writes the turns of the worker (`<KEY>-worker-turns`). A line with no TAB is a note, for example that there is no worker. Show it to the user and go on.

- On Plane, list the attachments of the ticket: `oversire vendor run --project <PROJECT> -- wi attachment list <KEY> --json`. Delete each attachment whose name starts with the session part of the names (for example `claude-session-<id>`, or `<KEY>-worker-turns`): `oversire vendor run --project <PROJECT> -- wi attachment delete <KEY> <ID>`. <PROJECT> is the key prefix of <KEY>. Thus the ticket keeps only the latest transcript of this session. The protocol of the app has no delete. On other trackers, skip this step.
- Upload each file: `oversire item attach <KEY> <path> --name <name>`. If it answers "<Tracker> has no attachments", skip the upload and tell the user.

## 5. Report

Give the URL and one line.
