# oversire

`oversire` is the OverSire CLI. It is one Go binary for each machine that runs coding agents (macOS first, also Linux and Windows).
The service runs `oversire daemon run`. The daemon has two roles:

- `serve`: it serves the live state of the agent sessions on this machine to OverSire clients, on loopback and on Tailscale.
- `runner`: it connects to the OverSire hub over one WebSocket, takes typed work orders and drives agent runtimes on the machine. The daemon dials out. The hub never connects in. There is no shell order.

A new install has the role `serve`. `oversire hub pair` adds the role `runner`.

There are two runtimes:

- `orca` starts agent sessions in Orca workspaces, reads them, sends text to them and closes them. The daemon has it when the Orca CLI is on the machine. For `sessionHistory`, the daemon finds the transcript of the agent through `orca search`. See [History](#history).
- `oversire` runs agent sessions without Orca. See [runtime:oversire](#runtimeoversire).

The executable inside `OverSire.app` is also named `oversire`. It is not on PATH, so the names do not clash.

## Install

The OverSire app installs the CLI in Settings › Machines. To install it by hand, download the archive for your platform from the [releases](https://github.com/oversire/cli/releases) and check it against `checksums.txt`:

```bash
tar -xzf oversire_<version>_darwin_arm64.tar.gz
sudo install oversire /usr/local/bin/
oversire daemon install
```

On Debian, Ubuntu, Fedora or RHEL, install the `.deb` or `.rpm` package from the release.
On Windows, extract the `.zip` and put `oversire.exe` in a folder of your choice.

The macOS binaries have a Developer ID signature and are notarized.

## Folders

The app owns `~/.config/oversire/` (`config.json`, `store/`). The CLI uses only the subfolder `daemon/` there and never writes the files of the app.

| Item | macOS, Linux | Windows |
|---|---|---|
| Config folder | `~/.config/oversire/daemon/` | `%AppData%\oversire\daemon\` |
| Data folder | `~/.local/share/oversire/` | `%LocalAppData%\oversire\` |

`OVERSIRE_DAEMON_HOME` sets another config folder. The data folder is then `$OVERSIRE_DAEMON_HOME/data`. The old name `OVERSIRE_RUNNER_HOME` works for one release.

`config.json` has the fields `roles`, `hub`, `device`, `account`, `name`, `keepAwake`, `maxSessions`, `dataDir` and `serve`. The file has mode 0600.

## Pair with the hub

1. In the OverSire console, make a pairing code with the role **runner**.
2. Pair the machine:

   ```bash
   oversire hub pair ABCD-1234 --hub https://<your hub> --name studio
   ```

   To keep the code out of argv, give `-` and write the code on stdin:

   ```bash
   echo ABCD-1234 | oversire hub pair - --hub https://<your hub>
   ```

The default name is the host name. A code for a client (the Mac app) does not pair a runner.
`hub pair` restarts the service when it is installed. It keeps the other fields of `config.json`, so a second `hub pair` needs no `--hub`. The environment variable `OVERSIRE_HUB` also gives the hub URL.
The hub role name stays `runner`.

The device token goes into the login Keychain as the service `oversire.daemon` (macOS) or the Secret Service through `secret-tool` (Linux).
When no keychain is available, and on Windows for now, the token goes into the file `token` (mode 0600) in the config folder.

## Migration from oversire-runner

`oversire daemon run`, `daemon status` and `daemon install` move an `oversire-runner` install at the start:

- They copy `config.json`, `allow.json`, `orders.json` and `state.json` from `~/.config/oversire-runner` and rename the old folder to `oversire-runner.migrated`. A migrated runner gets the roles `serve` and `runner`.
- They move the data folder and repair its git worktrees.
- They copy the device token from the Keychain service `oversire-runner` to `oversire.daemon` through stdin, then delete the old entry.
- They replace the old LaunchAgent, unit or task with the new one.
- They remove `/usr/local/bin/oversire-runner` only if it is a regular file with the OverSire signature. Else they leave it and print a note.

A second run does nothing. The old commands (`run`, `pair`, `install`, `uninstall`) print the new name.

## Allow list

The daemon does only the orders that the local allow list permits. Without the file, the daemon refuses all orders.
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
| `repos` | A `sessionStart` repo must be a listed path or in a listed folder. The daemon cleans the path first. A remote URL must be the same text. |
| `agents` | A `sessionStart` agent must be in the list. |
| `environments` | The runtime environment of each order must be in the list. `local` is this machine. Other names are paired Orca environments. The `oversire` runtime uses only `local`. |

The daemon reads the file again for each order. An edit applies without a restart.
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
| Permissions | The agent asks before it uses a tool. The session goes to `waitingPermission` with a prompt. Answer it with `sessionSend` and a `permission` answer. With no answer in 10 minutes, the daemon denies the request. |
| Transcript | `sessionRead` gives the last lines of the transcript. The full transcript is in `<data>/sessions/<session id>.log`. |
| History | `sessionHistory` reads the harness transcript of the session as a digest. The daemon keeps the harness session id in `<data>/sessions/<session id>.json`, so the order also works after a restart. See [History](#history). |
| Reports | Each change of state, summary or permission prompt goes to the hub as a `session` message. Without a connection, the daemon keeps the latest state and sends it after the next welcome. |

To change the data folder or the number of slots, set `dataDir` or `maxSessions` in `config.json`. `hub pair` keeps these fields.

## History

A `sessionHistory` order gives a `SessionDigest` (`contract/schema/session-digest.json`): the earlier turns in short, and the last turns with the full agent text.
The daemon reads the transcript file of the harness on this machine. Only the `claude` harness is supported. For other harnesses, the order fails with the reason.
The transcript of a Claude session is the newest `~/.claude/projects/*/<session id>.jsonl`.
The budget of the order has caps: 50 earlier turns, 10 last turns and 20000 characters for each last turn.

| Runtime | How the daemon finds the transcript |
|---|---|
| `oversire` | agent-wire gives the harness session id at the start of the session. |
| `orca` | `orca terminal show` gives the agent, the workspace path and the title of the terminal. Then `orca search` gives the newest session of the agent at that path with the title as its name, and its transcript path. Orca gives the path only for a session on this machine. For a paired environment, the order fails. |

## Commands

| Command | What it does |
|---|---|
| `oversire daemon run` | Run the daemon. The service runs this. |
| `oversire daemon install [--keep-awake]` | Install and start the service. |
| `oversire daemon uninstall` | Stop and remove the service. |
| `oversire daemon status [--json]` | Show the version, the roles, the pairing, the connection, the service and the runtimes. `oversire status` is the short form. |
| `oversire hub pair <code> [--hub URL] [--name NAME]` | Pair with a runner code. |
| `oversire hub pair - [--hub URL] [--name NAME]` | Pair, and read the code from stdin. |
| `oversire digest --harness claude --session <id or file> [--markers] [--source S] [--earlier N] [--full N] [--max-chars N]` | Print the digest of a session as JSON. See below. |
| `oversire hooks install [--harness claude,codex]` | Add or update the OverSire agent hooks. Other hooks stay. |
| `oversire hooks remove [--harness claude,codex]` | Remove only the OverSire agent hooks. |
| `oversire hooks status [--json]` | Show the hook state of each harness. |
| `oversire group show [--json]` | Show the machines and the apps of the group. |
| `oversire group invite` | Make an invite code for a new machine. |
| `oversire group requests`, `approve <id>`, `deny <id>` | List and decide the join requests. |
| `oversire group request-code`, `admit <machine> <code>` | Join the other way: the new machine shows a code, and a member admits it. |
| `oversire group remove <machine or device>`, `home <machine>`, `rotate` | Remove a member or an app, move home, or make a new group secret. |
| `oversire join <host> <code>` | Join the group of the member at host. `-` reads the code from stdin. |
| `oversire update` | Install the latest release now. The running daemon does the update and restarts. Without a daemon, the command updates the binary itself. |
| `oversire version` | Show the version. |

`digest` reads a session id (the newest transcript with that id) or the path of a transcript file.
It gives the same JSON as `oversire/scripts/digest.py`, and it needs no Python and no pairing.
The defaults are `--source file`, `--earlier 12`, `--full 3` and `--max-chars 4000`.
With `--markers`, it prints `OVERSIRE-DIGEST-BEGIN`, the gzip and base64 of the JSON in lines of 76 characters, then `OVERSIRE-DIGEST-END`.
On an error with `--markers`, it prints `OVERSIRE-DIGEST-ERROR <message>` and the exit code is 1.

```bash
oversire digest --harness claude --session 5b6e0f3a-1c2d-4e5f-8a9b-0c1d2e3f4a5b
oversire digest --session ~/.claude/projects/-Users-ada-app/5b6e0f3a-1c2d-4e5f-8a9b-0c1d2e3f4a5b.jsonl --markers
```

`daemon run` writes `state.json` in the config folder. `daemon status` reads it.
`daemon status --json` prints:

```json
{
  "version": "0.4.0",
  "roles": ["serve", "runner"],
  "paired": true,
  "device": "0A1B2C3D4E5F6071",
  "hub": "https://hub.example.com",
  "connected": true,
  "since": "2026-10-17T09:00:00Z",
  "lastError": "",
  "service": { "installed": true, "running": true },
  "runtimes": ["orca", "oversire"],
  "serve": { "loopback": "127.0.0.1:7690", "tailscale": "100.97.81.23:7690", "group": "3f9c0a1b2c3d4e5f", "home": "mbp" }
}
```

`serve` comes from `GET /v1/whoami` of the running daemon. Its fields are empty when the daemon does not answer.

## Local API

With the role `serve`, `daemon run` serves the API `/v1` of the local hub. The port is 7690. `serve.port` in `config.json` changes it.

| Listener | Address | Auth |
|---|---|---|
| Loopback | `127.0.0.1` and `[::1]` | None. The daemon gives 403 to a request with an `Origin` header, a `Host` that is not `127.0.0.1`, `[::1]` or `localhost` with the port, or a POST, PUT or DELETE that is not `application/json`. |
| Tailscale | Each tailnet address (`tailscale ip`, else the interface addresses in `100.64.0.0/10` and `fd7a:115c:a1e0::/48`) | `Authorization: Bearer <group credential>`, except `GET /v1/whoami` and the join routes. A bad or revoked credential gets 401. Loopback-only routes get 404. |

The daemon looks for the tailnet addresses again each 30 s. It never binds `0.0.0.0`. `serve.tailscale: false` stops the Tailscale listener.

`GET /v1/connect` is a WebSocket with the frames of the cloud hub: `ping` gets `pong`, and `{"type":"poke"}` and `{"type":"store"}` tell the client to read again. A revoked credential closes its socket with code 4001.

### Hooks and questions

The agent hooks call `oversire hook <harness>` with the event on stdin. The command posts it to `POST /v1/hooks/{harness}` and ends at once. When the daemon does not answer, it writes the event to a spool file, and the daemon reads the spool each 10 s. The app reads, installs and removes the hooks with `GET`, `POST` and `DELETE /v1/hooks/install`, body `{"harnesses":[...]}` (loopback only).

The daemon finds the open questions of the agent sessions: forms and permission prompts on the Orca screen, and a turn that ends with a question. `GET /v1/questions` lists them. `POST /v1/questions/{id}/answer` types the answer into the terminal. `GET /v1/questions/{id}/given` tells the answer that the user gave in the terminal.

`POST /v1/sessions/{id}/ask` (member access) sends a question to a session and waits for the end of its turn. The body is a `SessionAsk` `{text, timeoutSec?, readOnly?}` and the answer a `SessionAskReply` of `contract/schema/daemon.json`. With `readOnly` (the default) the daemon adds the line "Answer only. Do not change files." The daemon types the text into the Orca terminal, or gives it to an `oversire` session as a `sessionSend`. The turn ends when the session book shows the session out of `working` after the send (a `Stop` hook or a runtime report). The wait is `timeoutSec`, default 180 s, at most 600 s.

| Status | Body |
|---|---|
| 200 | `{reply, turnEndedAt, screenTail?}`. `reply` is the last agent message (at most 16000 characters). `screenTail` is the last 40 lines of the screen or transcript (at most 8000 characters). |
| 400 | `error: badRequest`. |
| 404 | `error: noSession`. |
| 409 | `error: busy`, with `screenTail`: a turn is active and the runtime takes no steering (Orca). An `oversire` session adds the text to the active turn. `error: unsupported`: the session has no input path (a `cli` session, or an Orca session with no terminal). |
| 410 | `error: gone`: the session went away during the wait. |
| 502 | `error: sendFailed`. |
| 504 | `error: timeout`, with `screenTail`. |

### Group

The machines of one user form a group. Each member keeps the roster and the group secret. A member that joins gets a group credential for the Tailscale listener. The roster syncs between members each 60 s, and at once after a `poke`. The home member keeps the shared store (see below).

Each member writes its CLI version into its own roster entry (`version`). The app compares the versions of the members.

### Updates

`GET /v1/whoami` has `update`, the last self-update check: `checkedAt`, `latest`, `installed` and `error`. It is absent before the first check. `POST /v1/update` (member access) checks and installs now, and answers the same object. After an install, the daemon restarts with the new version.

### Home store

The home member of a group without cloud keeps the synced store of the apps in `<data>/store.db` (SQLite). It has the API and the rules of the cloud hub: `POST /v1/store/ops` (at most 100 ops) and `GET /v1/store/ops?after=&limit=`. Last write wins for each collection and id, by HLC. A delete is a tombstone for 30 days, and the daemon prunes each day. The collections that only the hub writes get 400. An accepted op sends `{"type":"store","head":N}`.

A member that is not home answers 409 `{"error":"<machine> is home","home":"<machine>"}`. When the group has cloud, each member answers 409 `{"error":"the cloud is home","cloud":{...}}`. `POST /v1/group/home` with this machine copies all records of the old home before it sets home. When the old home does not answer, the move goes on and the answer has `"offline":true`.

### Direct orders

An app of the group can give work orders to the daemon without the cloud hub. The bodies are `WorkOrder` and `WorkOrderResult` of `contract/schema/work-order.json`. The orders go through the same allow list, expiry and journal (`orders.json`) as the hub orders. A daemon credential gets 403: daemons do not order each other.

| Route | What it does |
|---|---|
| `POST /v1/orders` | Takes a `WorkOrder` and answers 202 `{order}`. The order runs in the background. A known order id does not run again: the daemon answers 200 `{state, result?}` and sends the `result` frame again. |
| `GET /v1/orders/{id}` | Answers `{state, result?}`. `state` is `queued` or `acked` while the order runs, then `done`, `failed` or `expired`. An unknown id gets 404. |
| `POST /v1/runner-sessions/{id}/permission` | Body `{permission, allow}`. Answers the pending permission prompt of an `oversire` session with a `sessionSend` order, and answers 200 with that order. 404: no such session. 409: no such prompt is pending. 410: the prompt has expired. |
| `POST /v1/runner-sessions/{id}/answer` | Body: a `RunnerAnswer` `{id, items, note?}`. Answers the open question of an `oversire` session with a `sessionSend` order that expires in 60 min, and answers 200 with that order. 404: no such session. 409: no such question is open. |

`GET /v1/connect` also sends the `runner.json` frames `{"type":"ack","order":…}`, `{"type":"result","order":…,"state":…,"result":…}` and `{"type":"session","session":…}`. Each change of an `oversire` session goes out as a `session` frame, and into the session book (`/v1/sessions`, source `wire`).

## Service

`daemon install` starts `oversire daemon run` at login and restarts it after a failure:

| OS | Service |
|---|---|
| macOS | LaunchAgent `~/Library/LaunchAgents/com.oversire.daemon.plist`. The log is `~/Library/Logs/oversire-daemon.log`. |
| Linux | systemd user unit `~/.config/systemd/user/oversire-daemon.service`. Read the log with `journalctl --user -u oversire-daemon`. To run without a login session, run `loginctl enable-linger $USER`. |
| Windows | Logon task `OverSireDaemon`. |

On macOS, `daemon install --keep-awake` keeps the Mac awake while the daemon runs (`caffeinate -s`). `daemon install` without the flag removes this setting.

## Security model

- **Dial-out to the hub.** The daemon opens the hub connection. The hub needs no open port on the machine.
- **Typed orders.** An order is one of `sessionStart`, `sessionStop`, `sessionRead`, `sessionSend` and `sessionHistory`. There is no shell order. The `oversire` runtime asks the hub before an agent uses a tool, and denies it when no answer comes. An order with a field that the contract does not know fails.
- **Checked start command.** A `sessionStart` order can carry `command`, the start command that the user set in OverSire (Settings › Runtime). The `orca` runtime types it into the terminal in place of its default. It accepts the command only if the first word is the agent of the order (or a path that ends in `/<agent>`), and the text has no shell operator (`;` `|` `&` `` ` `` `$(` `>` `<` or a newline). Else the order fails with the reason. An empty field gives the default command. The `oversire` runtime starts the agent over agent-wire, without a command line, and ignores the field.
- **Local allow list.** The machine owner decides which runtimes, repos, agents and environments the hub can use. The hub cannot change the list.
- **Expiry.** Each order has an expiry time. The daemon never does an expired order.
- **At most once.** The daemon keeps the last 500 finished orders in `orders.json`. An order that the hub or an app sends again does not run again. The daemon only sends its result again.
- **Secrets on stdin.** The token goes to `security` and `secret-tool` on stdin, never in argv.
- **Signed updates.** `daemon run` checks the latest GitHub release one minute after the start, then each 6 hours, on `POST /v1/update` and when the hub tells it about a new version. It reads the release tag from the redirect of `github.com/oversire/cli/releases/latest`, so the limit of the GitHub API does not apply. It verifies the sha256 checksum. On macOS it also verifies the code signature and the Team ID `Q56WK6TB88`. A `dev` build never updates.

## Development

```bash
go vet ./...
go test ./...
OVERSIRE_LIVE=1 go test -run TestLive ./runtime/oversire   # a real harness (OVERSIRE_LIVE_AGENT, default claude)
scripts/contract-check.sh   # compare contract/ with ../oversire/Contract (OVERSIRE_APP overrides)
```

Set `OVERSIRE_DAEMON_HOME` to use another config folder, for example for a test run.

`contract/` is a copy of `Contract/` from [oversire/oversire](https://github.com/oversire/oversire): `schema/`, `fixtures/` and `prompts/`.
Do not edit it here. Change the contract in the app first, then copy it again and run the tests.
The `wire` tests decode and encode each fixture of `work-order/`, `runner-message/`, `runner-session/`, `launch-request/`, the daemon types and `session-ask/` and `session-ask-reply/`.
The `digest` tests read each `session-digest/claude/<case>.jsonl` and compare the digest with `session-digest/<case>.json`.

The layout:

| Package | What it holds |
|---|---|
| `cmd/oversire` | The commands. |
| `wire` | The Go types of the wire contract. |
| `hub` | The WebSocket client and the journal of finished orders. |
| `ask` | The route `POST /v1/sessions/{id}/ask`: it sends a question to a session and waits for the turn end. |
| `orders` | The one run path of each order (`Runner`), and the direct orders from the apps of the group (`Direct`). |
| `runtime` | The `Runtime` interface and the registry. `runtime/orca` is the Orca driver. `runtime/oversire` is the agent-wire driver. |
| `digest` | The transcript readers that make a `SessionDigest`. |
| `store` | The home store: the op log in SQLite and the `/v1/store/ops` routes. |
| `allow` | The allow list. |
| `config` | The config folder, `config.json` and `state.json`. |
| `secret` | The token storage. |
| `service` | LaunchAgent, systemd unit and logon task. |
| `update` | The self-update. |
| `migrate` | The move from an `oversire-runner` install. |
| `proc` | The child process runner. |

## Release

Push a tag `v*` to make a release. GoReleaser builds darwin, linux and windows for amd64 and arm64.
Each archive is `oversire_<version>_<os>_<arch>.tar.gz` (`.zip` on Windows) with the binary `oversire`, plus `checksums.txt` and Linux `.deb` and `.rpm` packages.
The macOS binaries get a Developer ID signature and notarization through the secrets `MACOS_SIGN_P12`, `MACOS_SIGN_PASSWORD`, `MACOS_NOTARY_ISSUER_ID`, `MACOS_NOTARY_KEY_ID` and `MACOS_NOTARY_KEY`.
Windows Authenticode signatures need a certificate. See the TODO in `.goreleaser.yaml`.

## License

MIT. See [LICENSE](LICENSE).
