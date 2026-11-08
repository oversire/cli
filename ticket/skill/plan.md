# oversire plan <KEY>: plan an existing ticket

Goal: an existing ticket (for example from Intake) gets the same full spec as a new ticket.

## 1. Read the ticket

- Run `oversire ticket plan <KEY>`. It binds this session and prints the earlier sessions of the ticket.
- Read the ticket with `plane-cli`: the description, the comments, the links and the sub-items.

## 2. Write the brief

Write `brief.md` from the ticket: the request, the facts from the comments and the links, and the earlier sessions. Read the code that the ticket names only as far as the brief needs it. The planner reads the rest.

If the ticket already has a full spec (Changes with signatures, and Tests), tell the user and stop, unless the user asked to plan again.

## 3. Plan

{{template "planning" .}}
## 4. Update the ticket

- Write the new description: the context, the spec, then the old text under "## Original request". Keep the OverSire block at the end.
- Make the sub-items that the spec proposes. If the ticket has sub-items, update them. Do not make a second copy of a sub-item.
- Add a comment: "Planned: <one line>".

## 5. Report

Give the URL, the keys of the sub-items, and two or three lines about the plan.
