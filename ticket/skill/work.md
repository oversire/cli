# oversire work <KEY>: work a ticket

Goal: do the spec of the ticket. The spec is the plan. Do not plan again.

## 1. Load the ticket

- If this session made the ticket, you have its context. Skip the digest.
- Else run `oversire ticket work <KEY>`. It binds this session and prints the earlier sessions and a digest of the last session. If the transcript is on a different machine, the output tells how to get it.
- Read the ticket with `plane-cli`: the description, the comments and the sub-items.
- If the spec is thin (no Changes with signatures, or no Tests), tell the user and do `oversire ticket skill plan` first, unless the user said to work it as it is.

## 2. Work

- Set the ticket to "In Progress".
- Follow the rules of the root and of each repository: worktrees, build, tests, commits.
- Work one sub-item at a time. When its tests pass, set it to Done with a comment that names the commit.
- Write the tests of the spec. Run them and the tests of the repository.
- If the code must be different from the spec, change the spec in the description and add a comment with the reason. The description must agree with the code.

## 3. Finish

Tell the user the result in a few lines. If the user asked to close the ticket, run `oversire ticket skill done` and continue. Else hand off as the rules of the repository tell.
