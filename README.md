# oversire-runner

`oversire-runner` is a headless program for an always-on machine (macOS first, also Linux and Windows).
It connects to the OverSire hub over one WebSocket, takes typed work orders and drives agent runtimes on the machine.
The runner dials out. The hub never connects in. There is no shell order.

There are two runtimes:

- `orca` starts agent sessions in Orca workspaces, reads them, sends text to them and closes them. The runner has it when the Orca CLI is on the machine. For `sessionHistory`, the runner finds the transcript of the agent through `orca search`. See [History](#history).
- `oversire` runs agent sessions without Orca. See [runtime:oversire](#runtimeoversire).

## Install

Download the archive for your platform from the [releases](https://github.com/oversire/runner/releases) and check it against `checksums.txt`:

```bash
tar -xzf oversire-runner_<version>_darwin_arm64.tar.gz
sudo install oversire-runner /usr/local/bin/
```

On Debian, Ubuntu, Fedora or RHEL, install the `.deb` or `.rpm` package from the release.
On Windows, extract the `.zip` and put `oversire-runner.exe` in a folder of your choice.

The macOS binaries have a Developer ID signature and are notarized.

## Pair

1. In the OverSire console, make a pairing code with the role **runner**.
2. Pair the runner:

   ```bash
   oversire-runner pair ABCD-1234 --hub https://<your hub> --name studio
   ```

   To keep the code out of argv, give `-` and write the code on stdin:

   ```bash
   echo ABCD-1234 | oversire-runner pair - --hub https://<your hub>
   ```

The default name is the host name. A code for a client (the Mac app) does not pair a runner.
`pair` restarts the service when it is installed. `pair` keeps the hub URL, so a second `pair` needs no `--hub`. The environment variable `OVERSIRE_HUB` also gives the hub URL.

The runner writes `config.json` (hub, device, account, name) with mode 0600 in its config folder:

| OS | Config folder |
|---|---|
| macOS, Linux | `~/.config/oversire-runner/` |
| Windows | `%AppData%\oversire-runner\` |

The device token goes into the login Keychain (macOS) or the Secret Service through `secret-tool` (Linux).
When no keychain is available, and on Windows for now, the token goes into the file `token` (mode 0600) in the config folder.

## Allow list

The runner does only the orders that the local allow list permits. Without the file, the runner refuses all orders.
Write `allow.json` in the config folder:

```json
{
  "runtimes": ["orca", "oversire"],
  "repos": ["/Users/ada/Projects/app", "/Users/ada/work"],
  "agents": ["claude", "codex"],
  "environments": ["local"]
}
```

| Field | Rule |
|---|---|
| `runtimes` | The runtime of each order must be in the list. |
| `repos` | A `sessionStart` repo must be a listed path or in a listed folder. The runner cleans the path first. A remote URL must be the same text. |
| `agents` | A `sessionStart` agent must be in the list. |
| `environments` | The runtime environment of each order must be in the list. `local` is this machine. Other names are paired Orca environments. The `oversire` runtime uses only `local`. |

The runner reads the file again for each order. An edit applies without a restart.
The hub shows a summary of the list (the runtimes, the agents and the number of repos). The full list stays on the machine.

## runtime:oversire

The `oversire` runtime drives the agent CLI directly through [agent-wire](https://github.com/trebi-ai/agent-wire), over the structured protocol of the CLI.
It uses the CLI and the login that are on the machine.

| Topic | Rule |
|---|---|
| Agents | `claude`, `codex`, `opencode`, `opencode2`, `pi`, `copilot`, `cursor` and `gemini` (the agent-wire harness names). The allow list must also have the agent. |
| Worktree | Each item gets one git worktree at `<data>/worktrees/<repo name>/<item key>`. The branch is the item key in lowercase, for example `towertail-2`. Without an item, the launch name is the key. A second launch for the same key uses the worktree and the branch again. `sessionStop` keeps the worktree. |
| Session | `sessionStart` gives the session at once. The workspace id is `<repo name>/<key>`, and the session id starts with `s-`. The state goes from `queued` to `starting` to `working`, then `waitingInput` at the end of each turn, and `done` or `failed` at the end. |
| Slots | At most `maxSessions` sessions run at the same time (default 2). A new session waits in `queued`. If the order expires before a slot is free, the session is `failed`. |
| Permissions | The agent asks before it uses a tool. The session goes to `waitingPermission` with a prompt. Answer it with `sessionSend` and a `permission` answer. With no answer in 10 minutes, the runner denies the request. |
| Transcript | `sessionRead` gives the last lines of the transcript. The full transcript is in `<data>/sessions/<session id>.log`. |
| History | `sessionHistory` reads the harness transcript of the session as a digest. The runner keeps the harness session id in `<data>/sessions/<session id>.json`, so the order also works after a restart. See [History](#history). |
| Reports | Each change of state, summary or permission prompt goes to the hub as a `session` message. Without a connection, the runner keeps the latest state and sends it after the next welcome. |

The data folder is `~/.local/share/oversire-runner` (macOS and Linux) or `%LocalAppData%\oversire-runner` (Windows).
With `OVERSIRE_RUNNER_HOME`, it is `$OVERSIRE_RUNNER_HOME/data`.
To change the data folder or the number of slots, set `dataDir` or `maxSessions` in `config.json`. `pair` keeps these fields.

## History

A `sessionHistory` order gives a `SessionDigest` (`contract/schema/session-digest.json`): the earlier turns in short, and the last turns with the full agent text.
The runner reads the transcript file of the harness on this machine. Only the `claude` harness is supported. For other harnesses, the order fails with the reason.
The transcript of a Claude session is the newest `~/.claude/projects/*/<session id>.jsonl`.
The budget of the order has caps: 50 earlier turns, 10 last turns and 20000 characters for each last turn.

| Runtime | How the runner finds the transcript |
|---|---|
| `oversire` | agent-wire gives the harness session id at the start of the session. |
| `orca` | `orca terminal show` gives the agent, the workspace path and the title of the terminal. Then `orca search` gives the newest session of the agent at that path with the title as its name, and its transcript path. Orca gives the path only for a session on this machine. For a paired environment, the order fails. |

## Commands

| Command | What it does |
|---|---|
| `oversire-runner pair <code> [--hub URL] [--name NAME]` | Pair with a runner code. |
| `oversire-runner pair - [--hub URL] [--name NAME]` | Pair, and read the code from stdin. |
| `oversire-runner run` | Connect to the hub and do orders. The service runs this. |
| `oversire-runner status [--json]` | Show the version, the pairing, the connection, the service and the runtimes. |
| `oversire-runner install [--keep-awake]` | Install and start the service. |
| `oversire-runner uninstall` | Stop and remove the service. |
| `oversire-runner digest --harness claude --session <id or file> [--markers] [--source S] [--earlier N] [--full N] [--max-chars N]` | Print the digest of a session as JSON. See below. |
| `oversire-runner version` | Show the version. |

`digest` reads a session id (the newest transcript with that id) or the path of a transcript file.
It gives the same JSON as `oversire/scripts/digest.py`, and it needs no Python and no pairing.
The defaults are `--source file`, `--earlier 12`, `--full 3` and `--max-chars 4000`.
With `--markers`, it prints `OVERSIRE-DIGEST-BEGIN`, the gzip and base64 of the JSON in lines of 76 characters, then `OVERSIRE-DIGEST-END`.
On an error with `--markers`, it prints `OVERSIRE-DIGEST-ERROR <message>` and the exit code is 1.

```bash
oversire-runner digest --harness claude --session 5b6e0f3a-1c2d-4e5f-8a9b-0c1d2e3f4a5b
oversire-runner digest --session ~/.claude/projects/-Users-ada-app/5b6e0f3a-1c2d-4e5f-8a9b-0c1d2e3f4a5b.jsonl --markers
```

`run` writes `state.json` in the config folder. `status` reads it.
`status --json` prints:

```json
{
  "version": "0.1.0",
  "paired": true,
  "device": "0A1B2C3D4E5F6071",
  "hub": "https://hub.example.com",
  "connected": true,
  "since": "2026-10-17T09:00:00Z",
  "lastError": "",
  "service": { "installed": true, "running": true },
  "runtimes": ["orca", "oversire"]
}
```

## Service

`install` starts `oversire-runner run` at login and restarts it after a failure:

| OS | Service |
|---|---|
| macOS | LaunchAgent `~/Library/LaunchAgents/com.oversire.runner.plist`. The log is `~/Library/Logs/oversire-runner.log`. |
| Linux | systemd user unit `~/.config/systemd/user/oversire-runner.service`. Read the log with `journalctl --user -u oversire-runner`. To run without a login session, run `loginctl enable-linger $USER`. |
| Windows | Logon task `OverSireRunner`. |

On macOS, `install --keep-awake` keeps the Mac awake while the runner runs (`caffeinate -s`). `install` without the flag removes this setting.

## Security model

- **Dial-out only.** The runner opens the connection. It needs no open port.
- **Typed orders.** An order is one of `sessionStart`, `sessionStop`, `sessionRead`, `sessionSend` and `sessionHistory`. There is no shell order. The `oversire` runtime asks the hub before an agent uses a tool, and denies it when no answer comes. An order with a field that the contract does not know fails.
- **Checked start command.** A `sessionStart` order can carry `command`, the start command that the user set in OverSire (Settings › Runtime). The `orca` runtime types it into the terminal in place of its default. It accepts the command only if the first word is the agent of the order (or a path that ends in `/<agent>`), and the text has no shell operator (`;` `|` `&` `` ` `` `$(` `>` `<` or a newline). Else the order fails with the reason. An empty field gives the default command. The `oversire` runtime starts the agent over agent-wire, without a command line, and ignores the field.
- **Local allow list.** The machine owner decides which runtimes, repos, agents and environments the hub can use. The hub cannot change the list.
- **Expiry.** Each order has an expiry time. The runner never does an expired order.
- **At most once.** The runner keeps the last 500 finished orders in `orders.json`. An order that the hub sends again does not run again. The runner only sends its result again.
- **Secrets on stdin.** The token goes to `security` and `secret-tool` on stdin, never in argv.
- **Signed updates.** `run` checks the latest GitHub release one time each day and when the hub tells it about a new version. It verifies the sha256 checksum. On macOS it also verifies the code signature and the Team ID `Q56WK6TB88`. A `dev` build never updates.

## Development

```bash
go vet ./...
go test ./...
OVERSIRE_LIVE=1 go test -run TestLive ./runtime/oversire   # a real harness (OVERSIRE_LIVE_AGENT, default claude)
scripts/contract-check.sh   # compare contract/ with ../oversire/Contract (OVERSIRE_APP overrides)
```

Set `OVERSIRE_RUNNER_HOME` to use another config folder, for example for a test run.

`contract/` is a copy of `Contract/` from [oversire/oversire](https://github.com/oversire/oversire): `schema/`, `fixtures/` and `prompts/`.
Do not edit it here. Change the contract in the app first, then copy it again and run the tests.
The `wire` tests decode and encode each fixture of `work-order/`, `runner-message/`, `runner-session/` and `launch-request/`.
The `digest` tests read each `session-digest/claude/<case>.jsonl` and compare the digest with `session-digest/<case>.json`.

The layout:

| Package | What it holds |
|---|---|
| `cmd/oversire-runner` | The commands. |
| `wire` | The Go types of the wire contract. |
| `hub` | The WebSocket client and the journal of finished orders. |
| `runtime` | The `Runtime` interface and the registry. `runtime/orca` is the Orca driver. `runtime/oversire` is the agent-wire driver. |
| `digest` | The transcript readers that make a `SessionDigest`. |
| `allow` | The allow list. |
| `config` | The config folder, `config.json` and `state.json`. |
| `secret` | The token storage. |
| `service` | LaunchAgent, systemd unit and logon task. |
| `update` | The self-update. |
| `proc` | The child process runner. |

## Release

Push a tag `v*` to make a release. GoReleaser builds darwin, linux and windows for amd64 and arm64.
Each archive is `oversire-runner_<version>_<os>_<arch>.tar.gz` (`.zip` on Windows) with the binary `oversire-runner`, plus `checksums.txt` and Linux `.deb` and `.rpm` packages.
The macOS binaries get a Developer ID signature and notarization through the secrets `MACOS_SIGN_P12`, `MACOS_SIGN_PASSWORD`, `MACOS_NOTARY_ISSUER_ID`, `MACOS_NOTARY_KEY_ID` and `MACOS_NOTARY_KEY`.
Windows Authenticode signatures need a certificate. See the TODO in `.goreleaser.yaml`.

## License

MIT. See [LICENSE](LICENSE).
