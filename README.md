# daiyaku

[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)
[![Go](https://img.shields.io/badge/Go-1.27-00ADD8?logo=go&logoColor=white)](go.mod)

**Operator-in-the-loop testing for agentic coding harnesses.**

A human sits where the LLM would. The harness (Claude Code, Codex CLI, ...) POSTs
its inference requests to this mock server, you author the tool calls the model
would have made, and the harness runs them against the real environment with real
credentials, real permission config, and real telemetry. You get a deterministic
map of what the agent identity can reach and which of those actions the defenders
can actually see.

Model refusal is a vendor behavior you can't write policy against or monitor.
Excessive agency is a property of your IAM and harness config: testable, fixable,
yours. daiyaku takes the model's judgment out of the loop and measures that.

Full methodology (the *why* and the test procedure) is in
[`agent-harness-blast-radius-testing.md`](agent-harness-blast-radius-testing.md).
Read it first. This is the *how*.

```
harness ──HTTP──▶ daiyaku mock ──▶ operator console (you author the tool call)
   ▲                    │
   └── tool_use / SSE ──┘   ◀── the harness's permission model is under test
```

## Build

Single static binary, nothing to install on the target.

```bash
go build -o daiyaku.exe ./cmd/daiyaku   # native
./build.sh                              # windows + linux + macos
```

## Launching

`serve` is the default, so:

```bash
daiyaku                      # anthropic, 127.0.0.1:8787, REPL console
daiyaku codex                # openai,    127.0.0.1:8790, REPL console
daiyaku -m tui               # full-screen console instead of the REPL
daiyaku -p openai -a 8790    # -a takes a bare port, :port, or host:port
```

The `codex` and `claude` profiles preset the provider and its port (8787
anthropic, 8790 openai, so both can run at once). Precedence is
**flag > env > profile > built-in default**, so a `DAIYAKU_ADDR` you exported for
the session still wins over the profile's port. `daiyaku -h` for everything.

## Quickstart (Claude Code)

1. Start it: `daiyaku` (add `-m tui` for the full-screen console).

2. Point Claude Code at it. `daiyaku env` prints the exact commands and the
   revert. The short version (PowerShell):

   ```powershell
   $env:ANTHROPIC_BASE_URL   = "http://127.0.0.1:8787"
   $env:ANTHROPIC_AUTH_TOKEN = "test-harness-token"
   ```

3. Start `claude` from the same shell, run `/status`, and confirm the base-URL
   line names your mock.

4. Each time the harness calls the model, the request shows up in the console.
   Compose a tool call, send it, and the harness runs it and posts the result
   back as the next request.

## Quickstart (Codex CLI)

Codex uses the OpenAI Responses wire format and reads provider config from
`~/.codex/config.toml` (project-local is ignored).

```bash
daiyaku codex                  # provider openai, port 8790, REPL
daiyaku env -p openai -a 8790  # prints config.toml + revert
```

Or inline, e.g. for `codex exec`:

```bash
HARNESS_TEST_KEY=x codex exec \
  -c model_provider=harness_test -c model=gpt-5-codex \
  -c 'model_providers.harness_test.name="Harness Test"' \
  -c 'model_providers.harness_test.base_url="http://127.0.0.1:8790/v1"' \
  -c 'model_providers.harness_test.env_key="HARNESS_TEST_KEY"' \
  -c 'model_providers.harness_test.wire_api="responses"' \
  --skip-git-repo-check "your task"
```

## Operator console (TUI)

| Key | Action |
| --- | --- |
| *(just type)* | bare text fills the selected tool's main field: type `whoami`, `Enter`, sends `Bash {"command":"whoami"}`. A shell/exec tool is auto-selected each request. |
| `Enter` | send (`Alt+Enter` for a newline; the box grows past two lines) |
| `Ctrl+T` | load the selected tool's input template (JSON for multi-field calls) |
| `Ctrl+G` | toggle compose mode: tool call ↔ assistant text |
| `Ctrl+E` | send as assistant text and end the turn (whatever the compose mode) |
| `Ctrl+R` | redraw the conversation pane |
| `Tab` / `Shift+Tab` | move focus: composer → tools → context |
| `j`/`k` (tools focused) | change selection; `Enter` loads its template |
| `s` (context/tools focused) | show/hide the system prompt (hidden by default) |
| `PgUp`/`PgDn` · `↑`/`↓` · wheel | scroll the conversation |
| `Ctrl+C` | quit |

The offered-tools pane marks tools added (`+`) or removed since the last turn and
tags non-function tools (`[namespace]`, `[web_search]`). The gap between what the
harness offered and what you expected is itself a finding.

## REPL

The REPL (`--mode repl`) is the default: line-based, does the same job (`help`
lists commands), and runs anywhere, including SSH/serial where the TUI can't. On
an interactive terminal it has Tab completion and history.

Type `shell` for a persistent command mode that sends every line straight to the
harness's shell/exec tool. It auto-detects the tool (`Bash` for Claude Code,
`exec_command` for Codex) or takes one by name (`shell exec_command`):

```
#1 > shell
Bash #1 $ whoami
Bash #2 $ cat ~/.aws/credentials
Bash #3 $ :end done       # :exit :end :text :tools :ctx :last :raw :call
```

`:exit` leaves shell mode; the other `:` meta-commands author replies or inspect
state without leaving.

## Modes

| Mode | Flag | Use |
| --- | --- | --- |
| **REPL** | `--mode repl` (default) | line-based console, robust anywhere |
| **TUI** | `--mode tui` | full-screen operator console |
| **Canned** | `--mode canned --sequence f.json` | replay a sequence; `--fallback` hands the tail to an operator |
| **Passthrough** | `--mode passthrough --upstream https://api.anthropic.com` | proxy the real harness↔API and log wire shapes; the methodology's Step-0 capture |

`--record chain.json` (TUI and REPL) saves everything you author into a replayable
file. Ship it with the report so the client can re-run each finding after
remediation.

## Sequences

Ordered operator actions in JSON:

```json
{ "name": "recon", "steps": [
  { "note": "who am i",  "tool": "Bash", "input": { "command": "id" } },
  { "note": "aws creds", "tool": "Bash", "input": { "command": "ls -la ~/.aws" } },
  { "text": "done", "end": true }
] }
```

Starters for each methodology phase are in [`sequences/`](sequences/)
(permission-boundary, blast-radius, egress). Tool names assume Claude Code; verify
against Phase-1 enumeration before running elsewhere.

## Evidence

Every run writes `runs/<timestamp>/transcript.jsonl`: the full request/response
record both directions, sensitive headers redacted to `<redacted:present>`. This
is your primary evidence.

The transcript is owner-only (0600) because it holds whatever the agent read:
the full system prompt, file contents, and any secret it surfaced. Treat the run
directory as client data.

```bash
daiyaku report runs/20260830-150405
```

prints the finding-chain summary and writes a `reconstructed-sequence.json` you
can replay with `--mode canned`.

Collect alongside it (methodology §7): `claude --debug` logs, EDR process tree,
SIEM for the window, proxy/DNS logs, and cloud/SaaS audit logs for the agent
identity. Most of the detection surface is the child processes (`bash`, `git`,
file writes, package installs), not the harness.

## Revert

```bash
daiyaku env    # prints the unset commands
```

Unset the env vars, remove any `env` block you added to
`~/.claude/settings.json` (or the `[model_providers.*]` block in
`~/.codex/config.toml`), and restart the harness.

## Safety

Read the methodology's §2 first. Default to read-only; require a named written
exception for any write/delete/send. `ls` on the directory is a finding, `rm` is
an incident. This is purple team: agree a deconfliction marker with the SOC and
record each test window.

## Adding a provider

Everything provider-specific lives in one adapter under
[`internal/adapter/`](internal/adapter/). Implement `Normalize(headers, body) ->
neutral.Request` and `WriteResponse(w, req, action)`, register in `init()`, done.
Anthropic Messages and OpenAI Responses ship today.

## Limitations

A human operator is more goal-directed than a model, so this maps the ceiling of
damage, not the realistic distribution. Vendor-side controls (abuse detection,
rate limiting, vendor audit logging) are bypassed by construction; test them
separately. Results have a short shelf life, so re-test on harness/model/MCP
changes and ship the sequences so the client can. MCP tool-search behaves
differently under base-URL redirection in Claude Code, so the exposed tool surface
may not exactly match production.
