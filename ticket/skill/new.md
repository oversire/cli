# oversire new: make a planned ticket

Goal: one ticket with a full spec at the interface level, made from the work of this session.

## 1. Write the brief

Write `brief.md`. It holds what this session knows and a fresh agent does not:

- The problem or the idea, in the words of the user.
- The repro steps and the evidence: errors, logs, `file:line` references.
- The root cause, if you found it. Mark a guess as a guess.
- The decisions that the user made in this session, and the constraints.
- The repositories and the files that you read.

If the session has no investigation (the user only gave an idea), the brief is the idea and the folder.

## 2. Plan

{{template "planning" .}}
## 3. Make the ticket

- Title: short, in the imperative, says the change.
- Description: the context from the brief, then the spec.
- State: the Todo state (see below). Use "In Progress" only if the user said to work it now.
- Follow the rules of the user for the lead and the assignee.
- Sub-items: make one for each part that the spec proposes, with `--parent <KEY> --state "<Todo>"`. Each sub-item holds its own Changes and Tests.

{{template "todo" .}}
Then run `oversire ticket new <KEY>`. It binds this session to the ticket.

## 4. Report

Give the URL of the ticket, the keys of the sub-items, and two or three lines about the plan. Do not paste the spec.

If the user said to work it now, run `oversire ticket skill work` and continue. Else stop.
