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
- Give each sub-item to a subagent. Call the Agent tool with `model: sonnet`. If the ticket has no sub-items, give the full spec to one subagent. If you do not have the Agent tool, do the work yourself.
- Start one subagent at a time. Two subagents in the same worktree make conflicts.
- The prompt of the subagent holds:
  - The spec of the sub-item, and the parts of the parent spec that it needs. Do not include the OverSire block.
  - Your decisions from this session that the spec does not hold, and the work that is done.
  - The absolute path of the worktree group, the build and test commands, and the commit trailer `Item: <KEY>`.
  - The rules: write the code and the tests of the spec, run the tests, commit, do not push or merge, do not use the tracker. Report the commits, the test result and each change from the spec.
- When the subagent ends, read its diff and run the tests yourself. If a test fails or the diff does not agree with the spec, send the subagent the problem, or fix it yourself.
- When the tests pass, set the sub-item to Done with a comment that names the commit.
- Make sure that the tests of the spec exist. Run them and the tests of the repository.
- If the code must be different from the spec, change the spec in the description and add a comment with the reason. The description must agree with the code.

## 3. Finish

Tell the user the result in a few lines. If the user asked to close the ticket, run `oversire ticket skill done` and continue. Else hand off as the rules of the repository tell.
