# Add a runtime

An agent runtime hosts interactive coding sessions in workspaces, for example Orca. The daemon owns the runtimes of its machine. The OverSire app does not drive a runtime. It talks only to the daemons, through one adapter (`RuntimeDaemon`, one `MachineRuntime` for each machine).

Thus a new runtime (Conductor, Superset, Emdash) is a new driver in this repo. The app needs no change, except for a new capability (see step 6).

## Rules

- A driver drives only the runtime on this machine. It has no remote environments and no `--environment` flag.
- Run each child process through an injected runner, as `runtime/orca` does with its `Runner` interface. `proc.Local` is the real one. The tests give a fake.
- Secrets go to a child process on stdin, never in argv.
- Map a missing workspace or session to `runtime.ErrNotFound`. Map a call that the runtime cannot do to `runtime.ErrUnsupported`.
- Body text is Markdown at the interface.

## 1. Make the package

1. Make the folder `runtime/<name>/`, for example `runtime/conductor/`.
2. Add a `const ID = "<name>"`. The id goes into work orders, the allow list and the app. Do not change it after a release.
3. Add `New(exe string, run Runner) *T` and a `FindCLI() (string, bool)` that finds the program of the runtime on this machine.

## 2. Implement the base interface

`runtime.Runtime` in `runtime/runtime.go`:

| Method | What it does |
|---|---|
| `ID()` | The const `ID`. |
| `Start` | Opens a session for a `wire.LaunchRequest` and gives its `wire.SessionRef`. Use `LaunchRequest.Command` when it is set. Else use the default command of the agent. |
| `Stop` | Ends the session. |
| `Read` | The last lines of the session screen or transcript. |
| `Send` | Gives the text, or the permission answer, to the session. |
| `History` | The digest of the harness transcript of the session, in the budget. |

## 3. Add the optional parts

Each part is a small interface in `runtime/runtime.go`. `runtime.Capabilities` gives the app a capability for each part that the driver has. Each runtime has `sessionInput`, `sessionTail`, `sessionHistory`, `hostFiles` and `sessionKey`. The daemon writes the files and keeps the start keys itself.

Enhance needs only a start and `History`: it starts one keyed session and reads the result from the last message. A runtime needs no extra part for it.

| Interface | Capability | Route |
|---|---|---|
| `Prober` | none; it fills `installed`, `reachable`, `version` and `error` | `GET /v1/runtimes` |
| `Catalog` | `workspaces` | `GET /v1/repos`, `GET /v1/workspaces` |
| `WorkspaceRemover` | `removeWorkspace` | `DELETE /v1/workspaces/{id}` |
| `Revealer` | `reveal` | `POST /v1/reveal` (loopback only) |
| `Launcher`, `Submitter`, `ScrollbackReader` | none; better results for start, send and read | direct orders |

A runtime that is not reachable gives no capabilities. The app then hides the features of that machine.

## 4. Register the driver

Add the driver to `cli.registry` in `cmd/oversire/run.go`. Add it only when `FindCLI` finds the program:

```go
if exe, ok := conductor.FindCLI(); ok {
	rts = append(rts, conductor.New(exe, proc.Local{}))
}
```

The serve role and the runner role use the same registry. The runtime is now in `GET /v1/runtimes`, and the app shows it on the card of the machine in Settings › Machines.

## 5. Allow list

Orders from the hub need the runtime id in `runtimes` of `allow.json`. Orders from loopback and from the apps of the group do not use the list. Tell the user to add the id in the README section of the runtime.

## 6. A new capability

When the runtime gives a feature that no capability names yet:

1. Add the capability to `Capability` in `oversire/Sources/OverSireCore` and to `oversire/Contract/` first.
2. Copy the contract to `contract/` and run `scripts/contract-check.sh`.
3. Add the interface and its `Cap...` const here, and the route.
4. Add the call to `MachineRuntime` in the app.

## 7. Test

- Record the output of the runtime CLI into `runtime/<name>/testdata/`, as `runtime/orca/testdata/` does. The tests replay it through a fake runner.
- `runtime/routes_test.go` runs the routes against a small fake runtime. Add a case for each new part.
- Run `go vet ./... && go test ./...`.
- Manual check: run a second daemon with `OVERSIRE_DAEMON_HOME=<dir>` and its own `serve.port`. Do not test with the installed service.
