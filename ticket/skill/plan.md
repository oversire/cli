# oversire plan [KEY...]: plan one ticket or a batch

Goal: an existing ticket (for example from Intake) gets the same full spec as a new ticket.

Choose the mode. With one key, do "One ticket". With no key, or with two or more keys, do "Many tickets".

## One ticket

### 1. Read the ticket

- Run `oversire ticket plan <KEY>`. It binds this session and prints the earlier sessions of the ticket.
- Read the ticket with `plane-cli`: the description, the comments, the links and the sub-items.

### 2. Write the brief

Write `brief.md` from the ticket: the request, the facts from the comments and the links, and the earlier sessions. Read the code that the ticket names only as far as the brief needs it. The planner reads the rest.

If the ticket already has a full spec (Changes with signatures, and Tests), tell the user and stop, unless the user asked to plan again. Before you stop, set the ticket to Todo if it is in Backlog or Triage (see step 4).

### 3. Plan

{{template "planning" .}}
### 4. Update the ticket

- Write the new description: the context, the spec, then the old text under "## Original request". Keep the OverSire block at the end.
- Make the sub-items that the spec proposes, with `--state "<Todo>"`. If the ticket has sub-items, update them. Do not make a second copy of a sub-item.
- Set the ticket to Todo if it is in Backlog or Triage: `plane-cli wi update <KEY> --state "<Todo>"`.
- Move the existing sub-items in Backlog or Triage to Todo. List them with `plane-cli -p <PROJECT> wi list --parent-id <uuid of the ticket> --state-group-in backlog,triage`, then run `wi update` on each.
- Add a comment: "Planned: <one line>. Set to Todo."

{{template "todo" .}}

### 5. Report

Give the URL, the keys of the sub-items, and two or three lines about the plan.

## Many tickets

### 1. Choose the scope

If the arguments (or Groom) give the keys, skip this step. Else ask with AskUserQuestion. The options are "Created today", "Since yesterday", "Last 7 days" and "All not planned".

### 2. Find the candidates

- Read the work items with `plane-cli wi list --state-group-in backlog,unstarted`. For a date scope, add `--created-at-gte <date>`.
- Add the pending intake items: `plane-cli intake list --status -2`.
- Keep only the items whose description has no `## Changes` and no `## Tests`.
- Show a numbered table: #, key, title, created, state.
- The user types `all`, `1-5,8` or `all but 3`. Tell back the selected keys.
- Above 20 keys, ask the user to split the batch.

### 3. Bind

Run `oversire ticket plan <KEY> <KEY>...`. It binds this session to each key and prints one line of earlier sessions for each key.

### 4. Write the group brief

- Write one brief for each item, as in "One ticket" step 2.
- Add one shared part to each brief: the list of all items of the batch (key, title, one line), and the repositories and files that they share.

### 5. Plan

Before the start, tell the user: "N tickets: about 2N agents, at most 5 at a time."

Follow "Plan with a subagent" below, with these changes:

- Each planner also gets the shared part.
- Tell each planner to name overlaps with the other items, and to propose `blocked_by` relations to keys of the batch.
- Tell each planner to return its questions and not to ask them.
- Run at most 5 planners at one time. Start the next planner when one ends.
- Each item keeps its checker.

{{template "planning" .}}
### 6. Settle the open questions in one round

- Combine the same questions of different items.
- Ask with AskUserQuestion: at most 4 questions in one call, and more calls if necessary.
- Put each answer into each spec that it touches.

### 7. Update the tickets

- Do "One ticket" step 4 for each item.
- Add the agreed relations with `plane-cli wi relation create --relation-type blocked_by --issues <ids>`. `--issues` takes work item ids, not keys: get the ids first.

### 8. Report

Give one table: key, one line, sub-items, new relations. Then give the URLs.
