# oversire plan <KEY>: plan an existing ticket

Goal: an existing ticket (for example from Intake) gets the same full spec as a new ticket.

## 1. Read the ticket

- Run `oversire ticket plan <KEY>`. It binds this session and prints the earlier sessions of the ticket.
- Read the ticket with `plane-cli`: the description, the comments, the links and the sub-items.

## 2. Write the brief

Write `brief.md` from the ticket: the request, the facts from the comments and the links, and the earlier sessions. Read the code that the ticket names only as far as the brief needs it. The planner reads the rest.

If the ticket already has a full spec (Changes with signatures, and Tests), tell the user and stop, unless the user asked to plan again. Before you stop, set the ticket to Todo if it is in Backlog or Triage (see step 4).

## 3. Plan

{{template "planning" .}}
## 4. Update the ticket

- Write the new description: the context, the spec, then the old text under "## Original request". Keep the OverSire block at the end.
- Make the sub-items that the spec proposes, with `--state "<Todo>"`. If the ticket has sub-items, update them. Do not make a second copy of a sub-item.
- Set the ticket to Todo if it is in Backlog or Triage: `plane-cli wi update <KEY> --state "<Todo>"`.
- Move the existing sub-items in Backlog or Triage to Todo. List them with `plane-cli -p <PROJECT> wi list --parent-id <uuid of the ticket> --state-group-in backlog,triage`, then run `wi update` on each.
- Add a comment: "Planned: <one line>. Set to Todo."

{{template "todo" .}}

## 5. Report

Give the URL, the keys of the sub-items, and two or three lines about the plan.
