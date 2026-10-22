# Claude screen fixtures

These files are recorded screens and transcripts of Claude Code in an Orca terminal.
Two parsers read them: the app (`OrcaFormsTests`, `OrcaPromptsTests`, `OrcaTurnTests` and `ClaudeScreensTests` in `RuntimeOrcaTests`)
and the daemon (the Go package `questions` in `oversire/cli`). The two parsers must agree.
The tests also replay the keys that answer each pause.
This folder is not a wire type. The contract tests skip it.

## Versions

| Files | Claude Code | Orca |
|---|---|---|
| `form-local.txt`, `form-multi.txt`, `form-review.txt`, `false-footer.txt`, `no-form-answered.txt` | 2.1.305 | 1.4.223 |
| `ask-open.jsonl`, `ask-answered.jsonl` | 2.1.305 | 1.4.223 |
| Each file with `2.1.308` in its name | 2.1.308 | 1.4.223 |
| `ask-flow-open.jsonl`, `ask-flow-answered.jsonl` | 2.1.308 | 1.4.223 |

The 2.1.305 files have no version in their name. Keep them: they show that the parser also reads the old screen.

## Files

- `form-*.txt`: the AskUserQuestion form. One page for each file: a single question, a multi question, the "Type something" row, the review page.
- `flow-2.1.308-00.txt` to `-10.txt`: one form from the first key to the last key. File 00 is the screen before the first key. Each next file is the screen after one more key. File 09 is the review page. File 10 is the screen after the submit key.
- `ask-flow-open.jsonl` and `ask-flow-answered.jsonl`: the transcript of that flow before and after the answer.
- `permission-*.txt`: tool permission prompts (Bash, Edit, Write, Read). `permission-amend-typing` is the "No" choice after Tab, with text in it.
- `plan-approval-*.txt` and `plan-feedback-typing-*.txt`: the plan approval, and the plan approval with text in "Tell Claude what to change".
- `turn-question-*.txt`: a turn that ends with a question.
- `no-form-answered.txt` and `false-footer.txt`: screens with no pause. The parser must give nil for them.
- `<name>.expect.json`: the parsed form and the parsed prompt of `<name>.txt` (headers, items, review, cursor, boxes; title, body, question, options, typed, amending), or null.
- `flow-2.1.308.keys.json`: the answer of the flow and the keys that answer it, for the full form and for each page.

## Make a new fixture

Do these steps when Claude Code changes its screen, or when a test of a new pause needs a screen.

1. Make an empty folder. Start Claude Code in it in an Orca terminal:

   ```bash
   orca terminal create --worktree path:<a worktree of Orca> --title fixtures \
     --command "cd <folder> && claude --model haiku --permission-mode default" --json
   ```

   Use `--permission-mode default`. In auto mode, Claude Code shows no permission prompts.
2. Send a prompt that makes the pause. For example: `Use AskUserQuestion once with two questions. ...` or `Run this exact Bash command and nothing else: date > stamp.txt`.
3. Record the screen with the same read that the adapter uses:

   ```bash
   orca terminal read --terminal <handle> --limit 40 --json \
     | python3 -c 'import json,sys; print("\n".join(json.load(sys.stdin)["result"]["terminal"]["tail"]))' \
     > <name>-<claude version>.txt
   ```

4. For a flow, send one key with `orca terminal send --terminal <handle> --text <key>`. Then record the screen again. Do this for each key.
5. For a transcript, copy the lines of the AskUserQuestion call and its result from `~/.claude/projects/<folder>/<session>.jsonl`. Remove the other lines.
6. Put the Claude Code version in the file name (`claude --version`). Add a row to the table above.
7. Write the `.expect.json` files with the Swift parser. Then read each new file and make sure that it is correct:

   ```bash
   OVERSIRE_RECORD_SCREENS=1 swift test --filter ClaudeScreensTests
   ```

8. Add the file to `testRecordedScreensParse` in `OrcaFormsTests`, and add a test for the keys if the keys change.
9. Copy the contract to the CLI and the cloud. Then run the Go tests of the package `questions`.

## Live test

`OrcaLiveTests/testNativeQuestions` runs the full flow on a real Claude Code session in Orca.
It answers a form page by page, sees a form that is answered in Orca, answers a Bash permission prompt and answers a turn question.

```bash
OVERSIRE_LIVE=1 ORCA_CLAUDE_TERMINAL=<handle> ORCA_CLAUDE_DIR=<folder> \
  swift test --filter OrcaLiveTests/testNativeQuestions
```

The session must be idle at its prompt when the test starts.
