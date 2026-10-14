# clear-herd

`clear-herd` closes supported coding agents in a [Herdr](https://herdr.dev/) session, clears their terminal panes, and prints commands to resume the saved conversations. It uses Herdr's agent and pane CLI, so the same executable works whether Herdr is displayed in Terminal, iTerm2, Windows Terminal, or a Linux terminal emulator.

## Requirements

- [Herdr](https://herdr.dev/docs/quick-start/) installed and running, with its `herdr` CLI on `PATH`.
- Agents running **inside Herdr panes**. Ordinary terminal windows outside Herdr are not visible to this tool.
- A native session ID reported by a Herdr agent integration, or a resume command printed in the agent's terminal output. Install available integrations with `herdr integration install <name>` and check `herdr agent list` for `agent_session`.

The built-in command profiles are Codex (`/clear`, `/exit`, `codex resume ID`), Claude Code (`/clear`, `/exit`, `claude --resume ID`), and Cursor Agent (`/clear`, `/exit`, `agent --resume=ID`). Other agent kinds can be added through a profiles file.

## Install and build

With Go 1.23 or newer:

```sh
go build -o clear-herd ./cmd/clear-herd
```

On Windows, use `go build -o clear-herd.exe ./cmd/clear-herd`. Move the resulting executable to a directory on `PATH`.

The [Build clear-herd workflow](.github/workflows/build.yml) runs tests and uploads six executables as GitHub Actions artifacts for macOS, Linux, and Windows, each on Intel/AMD (`amd64`) and ARM (`arm64`). Open a successful workflow run's **Artifacts** section and download the archive for your operating system and CPU. On macOS and Linux, make the downloaded executable executable with `chmod +x`.

## Use

From a shell where the Herdr CLI can reach the desired server:

```sh
clear-herd --dry-run
clear-herd
```

`--dry-run` lists agents and any resume commands already available from Herdr session metadata. The normal command:

1. Lists agents from `herdr agent list`.
2. Saves each agent's native session ID before `/clear` can start a fresh chat.
3. Sends the agent's clear and exit commands, then waits for the agent to disappear from its pane.
4. Reads exit output for a resume command if Herdr did not report an ID.
5. Runs `clear` in the freed shell pane (`cls` on Windows).
6. Prints a bulleted list with agent kind, pane ID, and resume command.

For example:

```text
Resume commands:
- codex [w1:p1]: codex resume 01a13c1d-73ec-72b3-8159-5eb85647d384
- claude [w1:p2]: claude --resume 229a1d26-70d2-4758-9b10-58eeb31d1648
```

Run it from a regular shell pane. If the command runs inside a pane currently hosting an agent, that pane is skipped so the command can finish. Agents with an active approval prompt are also skipped. Working agents are given up to `--timeout` to become idle. Errors, including missing resume commands, are printed to stderr and cause exit status 1; other panes continue processing.

Options:

```text
--dry-run          Show available commands without changing panes
--profiles FILE    Load additional or replacement agent command profiles
--herdr PATH       Use a Herdr CLI at PATH (default: herdr)
--timeout 15s      Maximum time to wait for an agent to become idle or exit
--version          Show the tool version
```

Herdr's `HERDR_SESSION` and `HERDR_SOCKET_PATH` environment variables are inherited by the CLI, so they can target a named/local session. This command handles one Herdr server per run.

## Add an agent profile

Create a JSON file mapping Herdr's `agent` kind to its interactive clear and exit commands and its resume command. The resume arguments must contain `{id}`, `{path}`, or `{session}`; those placeholders are filled from Herdr's `agent_session` field. `{id}` only accepts ID references, `{path}` only path references, and `{session}` accepts either.

```json
{
  "myagent": {
    "clear": "/reset",
    "exit": "/quit",
    "resume": ["myagent", "resume", "{id}"]
  }
}
```

Then run `clear-herd --profiles profiles.json --dry-run` to check session metadata before using `clear-herd --profiles profiles.json`. A profile replaces a built-in one with the same kind. An agent without a profile is left untouched and reported as an error.

## Development

```sh
go test ./...
go build ./cmd/clear-herd
```

The CLI uses only the Go standard library. Its tests use a scripted Herdr CLI, so they do not close live agents.
