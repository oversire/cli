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

## 4. Attach the transcript

Run `oversire ticket done <KEY>`. It writes the transcript of this session as gzip files and prints one line for each file: `<path><TAB><name>`.

- List the attachments of the ticket (`plane-cli wi attachment list <KEY>`). Delete each attachment whose name starts with the session part of the names (for example `claude-session-<id>`). Thus the ticket keeps only the latest transcript of this session.
- Upload each file: `plane-cli wi attachment upload <KEY> <path> --name <name>`.

## 5. Report

Give the URL and one line.
