# daiyaku

**Operator-in-the-loop testing for agentic coding harnesses.**

A human operator substitutes for the LLM inside a real coding agent (Claude Code,
Codex CLI, ...). The harness POSTs its inference requests to this mock server; you
author the tool calls the model would have made; the harness executes them against
the real environment with real credentials, real permission config, and real
telemetry. The result is a deterministic, repeatable map of what the agent
identity can reach and which of those actions the defenders can see.

This is the tool for the methodology in
[`agent-harness-blast-radius-testing.md`](agent-harness-blast-radius-testing.md).
Read that first; it explains *why* and the test procedure. This README is *how*.

```
harness ──HTTP──▶ daiyaku mock ──▶ operator console (you author the tool call)
   ▲                    │
   └── tool_use / SSE ──┘   ◀── the harness's permission model is the system under test
```

## Why

Model refusal is a vendor behavior you cannot write policy against, monitor, or
version. Excessive agency is a property of *your* IAM and harness configuration:
testable, fixable, yours. daiyaku removes the model's judgment and measures
exactly that.

## Build

Single static binary, no runtime needed on the target.

```bash
go build -o daiyaku.exe ./cmd/daiyaku      # native
./build.sh                                          # windows + linux + macos
```

Any workstation running Claude Code already has Node; daiyaku itself needs
nothing but the binary. Copy `daiyaku.exe` onto the box and go.

## Launching

`serve` is the default command, so the short forms are:

```bash
daiyaku                 # serve: anthropic, 127.0.0.1:8787, REPL console
daiyaku codex           # serve: openai,    127.0.0.1:8790, REPL console
daiyaku -m tui          # full-screen console instead of the REPL
daiyaku -p openai -a 8790   # spell it out; -a takes a bare port, :port, or host:port
```

- **Profiles** `codex` and `claude` preset the provider and its conventional port
  (8787 anthropic, 8790 openai, so both can run at once). Trailing flags override.
- **Short flags:** `-p` provider, `-a` addr, `-m` mode (plus `-s`/`-r`/`-u`).
- **Env defaults:** set `DAIYAKU_PROVIDER`, `DAIYAKU_ADDR`, or
  `DAIYAKU_MODE` once per shell to change your personal default, then just run
  `daiyaku`. Precedence is flag > env var > built-in default.

Run `daiyaku -h` for the full flag list.

## Quickstart (Claude Code)

1. **Start the mock + operator console** (defaults to the line-based REPL; add
   `-m tui` for the full-screen console):

   ```bash
   daiyaku
   ```

2. **Point Claude Code at it.** Print the exact commands (and the revert):

   ```bash
   daiyaku env
   ```

   The short version (PowerShell):

   ```powershell
   $env:ANTHROPIC_BASE_URL   = "http://127.0.0.1:8787"
   $env:ANTHROPIC_AUTH_TOKEN = "test-harness-token"
   ```

3. **Verify before opening the harness**, then start `claude` from the same shell
   and run `/status`; confirm the base-URL line names your mock.

4. **Drive it.** Each time the harness calls the model, the request appears in the
   console. Compose a tool call and send it; the harness executes it and posts the
   result back as the next request.

## Quickstart (Codex CLI)

Codex uses the OpenAI Responses wire format. Provider config must live in the
user-level `~/.codex/config.toml` (project-local is ignored), or pass it inline:

```bash
daiyaku codex                  # serve: provider openai, port 8790, REPL
daiyaku env -p openai -a 8790  # prints config.toml + revert
```

Inline (no config file changes), e.g. for `codex exec`:

```bash
HARNESS_TEST_KEY=x codex exec \
  -c model_provider=harness_test -c model=gpt-5-codex \
  -c 'model_providers.harness_test.name="Harness Test"' \
  -c 'model_providers.harness_test.base_url="http://127.0.0.1:8790/v1"' \
  -c 'model_providers.harness_test.env_key="HARNESS_TEST_KEY"' \
  -c 'model_providers.harness_test.wire_api="responses"' \
  --skip-git-repo-check "your task"
```

## The operator console (TUI)

| Key | Action |
| --- | --- |
| *(just type)* | with a tool selected, bare text fills its main field: type `whoami`, press `Enter`, and it sends `Bash {"command":"whoami"}`. A shell/exec tool is auto-selected on each request. |
| `Enter` | send the composed action (`Alt+Enter` inserts a newline; the box starts at two lines and grows as you add more) |
| `Ctrl+T` | load the selected tool's input template (compact JSON to edit for multi-field calls) |
| `Ctrl+G` | toggle compose mode: tool call ↔ assistant text |
| `Ctrl+E` | send as assistant text and end the turn |
| `Tab` / `Shift+Tab` | move focus: composer → tools → context |
| `j`/`k` (tools focused) | change tool selection; `Enter` loads its template |
| `s` (context/tools focused) | show/hide the system prompt (hidden by default to keep the pane short) |
| `PgUp`/`PgDn` · `↑`/`↓` (context focused) · mouse wheel | scroll the conversation |
| `Ctrl+C` | quit |

The system prompt is hidden by default (it is huge and rarely needed turn-to-turn),
so the context pane stays short and readable; press `s` to view it, scroll, `s`
to hide it again.

Bare text is shorthand for the tool's primary string field (`command`, `cmd`,
`prompt`, `file_path`, ...); the composer shows which field it targets. For a
multi-field call, `Ctrl+T` drops in the full JSON template to edit.

The **offered-tools pane** marks tools added (`+`) or removed since the previous
turn, and tags non-function tools (`[namespace]`, `[web_search]`). The diff
between what the harness offered and what you expected is itself a finding.

The whole layout wraps to the terminal width and fits the height, so long system
prompts and tool results never run off-screen; scroll the context pane to read
them.

The REPL (`--mode repl`) is the default: it is line-based, does the same job
(`help` at the prompt lists commands), and works anywhere, including SSH/serial or
constrained terminals where the full-screen TUI can't. On an interactive terminal
it has **Tab completion** (command names, and tool names after
`call`/`schema`/`template`/`shell`) and history. Use `--mode tui` (or `-m tui`)
for the full-screen console described above.

### REPL shell mode

Type `shell` to drop into a persistent command mode that sends every line
straight to the harness's shell/exec tool, no `call Bash` each time. It
auto-detects the tool (`Bash` for Claude Code, `exec_command` for Codex) or name
one explicitly (`shell exec_command`). It prints the previous command's output,
then re-prompts, like a real shell:

```
#1 > shell
Bash #1 $ whoami
Bash #2 $ cat ~/.aws/credentials
Bash #3 $ :end done           # ':' meta-commands: :exit :end :text :tools :ctx :last
```

`:exit` leaves shell mode; `:end`/`:text` author an assistant reply; `:tools`,
`:ctx`, `:last`, `:raw`, `:call` inspect or author one-offs without leaving.

## Modes

| Mode | Flag | Use |
| --- | --- | --- |
| **REPL** | `--mode repl` (default) | line-based console, robust anywhere |
| **TUI** | `--mode tui` | full-screen interactive operator console |
| **Canned** | `--mode canned --sequence f.json` | replay an ordered sequence automatically; `--fallback` hands the tail to an interactive operator |
| **Passthrough** | `--mode passthrough --upstream https://api.anthropic.com` | proxy the real harness↔API and log exact wire shapes; the methodology's Step-0 capture and benign baseline |

Record everything you author into a replayable file with `--record chain.json`
(works in TUI and REPL). Ship that file with the report so the client can re-run
each finding after remediation.

## Sequences (reproducible chains)

Ordered operator actions in JSON. Bare array or wrapped form:

```json
{ "name": "recon", "steps": [
  { "note": "who am i",  "tool": "Bash", "input": { "command": "id" } },
  { "note": "aws creds", "tool": "Bash", "input": { "command": "ls -la ~/.aws" } },
  { "text": "done", "end": true }
] }
```

Starter sequences for the methodology's phases are in [`sequences/`](sequences/)
(permission-boundary, blast-radius, egress). Tool names there assume Claude Code;
verify against Phase-1 enumeration before running elsewhere.

## Evidence

Every run writes to `runs/<timestamp>/transcript.jsonl`: the full request/response
record, both directions, with sensitive headers redacted to `<redacted:present>`.
This is your primary evidence.

```bash
daiyaku report runs/20260830-150405
```

prints the finding-chain summary and writes a `reconstructed-sequence.json` you
can replay with `--mode canned`.

Collect alongside it (see methodology §7): `claude --debug` logs, EDR process
tree, SIEM for the window, proxy/DNS logs, and cloud/SaaS audit logs for the agent
identity. Most of the detection surface is the child processes (`bash`, `git`,
file writes, package installs), not the harness itself.

## Revert

```bash
daiyaku env                            # prints the exact unset commands (anthropic)
```

Unset the environment variables, remove any `env` block you added to
`~/.claude/settings.json` (or the `[model_providers.*]` block in
`~/.codex/config.toml`), and restart the harness.

## Safety

Read the methodology's §2 first. Default to read-only; require a named written
exception for any write/delete/send. `ls` on the directory is a finding; `rm` is
an incident. This is purple team: agree an out-of-band deconfliction marker with
the SOC and record each test window.

## Adding a provider

Everything provider-specific lives in one adapter under
[`internal/adapter/`](internal/adapter/). Implement `Normalize(headers, body) ->
neutral.Request` and `WriteResponse(w, req, action)` (blocking + SSE), register in
`init()`, and nothing else in the codebase needs to change. Anthropic Messages and
OpenAI Responses ship today.

## Limitations

A human operator is more goal-directed than a model: this maps the *ceiling* of
damage, not the realistic distribution. Vendor-side controls (abuse detection,
rate limiting, vendor audit logging) are bypassed by construction; test them
separately. Results have a short shelf life; re-test on harness/model/MCP changes,
and ship the sequences so the client can. MCP tool-search behavior differs under
base-URL redirection in Claude Code, so the exposed tool surface may not exactly
match production; account for the delta.
