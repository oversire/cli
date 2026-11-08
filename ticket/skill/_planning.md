{{define "planning"}}### Plan with a subagent

A fresh subagent writes the spec, so that its context is clean. Run `oversire ticket skill spec` to get the spec template.

Start one planner subagent (type `Plan` if it exists, else `general-purpose`). Give it the brief, the spec template and the folder. Tell it to:

- Read the rules files first: `CLAUDE.md`, `AGENTS.md` or `CONTRIBUTING.md` of the root and of each repository that the change touches.
- Read the code. Do not change files.
- Fill the template at the interface level. Each changed function gets its old and new signature, its new behavior and its callers. Each new function or type gets its name, parameters, return types and errors. Give no function bodies.
- Give a test for each change, with the test name and the case.
- Mark each fact that it could not check with "(not checked)".
- Return the open design questions apart from the spec. Give each question its options and a recommendation.
- Propose sub-items when the change is large: more than one repository, or parts that can merge alone. Each sub-item gets its own Changes and Tests.
- Keep the detail in proportion. A one-line fix gets a short spec, but it still names the function and the test.

### Check with a second subagent

When the spec changes more than one file, start a checker subagent (type `Explore` if it exists, else `general-purpose`). Give it only the spec and the folder, not the brief. Tell it to compare the spec with the code and return a list of corrections:

- Each "Changed" signature is the current signature in the code.
- Each caller in the spec exists, and no caller is missing. Search the code for the callers.
- Each file path exists, or the spec marks it as new.
- The tests follow the test layout and the conventions of the repository.

Apply the corrections to the spec.

### Settle the open questions

If open questions remain, ask the user in one prompt (use AskUserQuestion when it exists). Put each answer into the spec. Do not ask about a point that the code or the brief settles. If no question remains, write "None." under "Open questions".
{{end}}
