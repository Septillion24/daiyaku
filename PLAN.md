# daiyaku: Implementation Plan

Operator-in-the-loop testing harness for agentic coding tools. A human operator
substitutes for the LLM: the real harness (Claude Code, Codex, ...) POSTs
inference requests to our mock server, the operator authors the tool calls, and
the harness executes them against the real environment with real permissions and
real telemetry. See `agent-harness-blast-radius-testing.md` for the methodology.

## Decisions (locked)

- **Language:** Go. Single static binary (`daiyaku.exe`), no runtime on the
  target, trivial cross-compile (`GOOS=windows`), first-class TUI ecosystem
  (Charm Bubble Tea / Lipgloss / Bubbles). Plop one file into the client env, run.
- **Primary target OS:** Windows (also builds for macOS/Linux).
- **Provider v1:** Anthropic Messages (Claude Code). Neutral core + adapter
  interface so Codex (OpenAI Responses), Gemini, and OpenAI-compatible runners
  drop in behind the same core.
- **Live test rig:** Codex CLI is installed on this box and is open source with a
  provider-override config. We build the OpenAI Responses adapter early and use
  real Codex as an end-to-end integration test of the whole architecture, while
  Anthropic remains the documented primary and is validated with a high-fidelity
  synthetic client + curl SSE checks (and a live `claude` smoke test if available).

## Architecture

```
harness (claude/codex) --HTTP--> mock server --normalize--> neutral request
                                                                  |
                                          operator console <------+  (engine broker)
                                                                  |
harness <----serialize (JSON/SSE)-- mock server <--OperatorAction-+
```

- **neutral**: provider-neutral types (`Request`, `Turn`, `Block`, `ToolDef`,
  `OperatorAction`). Nothing outside adapters knows the wire format.
- **adapter**: `Normalize(headers, body) -> neutral.Request` and
  `WriteResponse(w, req, action)` (blocking + SSE). One per provider. Registers
  its own routes (main endpoint + aux like `count_tokens`).
- **server**: HTTP listener, routing, transcript logging (JSONL, both directions,
  headers + bodies + timing). Session dir under `runs/<ts>/`.
- **engine**: broker with channels. `Submit(req) -> OperatorAction`. Decouples the
  HTTP goroutine from whichever console frontend is attached.
- **console frontends** (selectable modes, "multiple options for usage"):
  - `tui`   full-screen Bubble Tea: context pane, offered-tools pane (+ diff vs
              last turn), call composer with schema-templated JSON, canned-snippet
              inserts, live "record to sequence" for reproducible chains.
  - `repl`  line-based, robust over SSH/serial; `:tools`, `:last`, `:call`, etc.
  - `canned` replay an ordered JSON sequence file; fall through to interactive.
  - `passthrough` forward the raw request to a real upstream, intercept on demand.

## Neutral types (sketch)

```
Request{ Provider, Model, System, Turns[], Tools[]ToolDef, Stream, Raw[]byte, Headers }
Turn{ Role, Blocks[] }
Block{ Type: text|tool_call|tool_result, Text, Call *ToolCall, Result *ToolResult }
ToolCall{ ID, Name, Input json.RawMessage }   // Input normalized to an OBJECT
ToolResult{ CallID, Content, IsError }
ToolDef{ Name, Description, Schema json.RawMessage }
OperatorAction{ Kind: tool_call|text|end, ToolName, ToolInput, Text }
```

Wire-format notes to honor (from the methodology's comparison table):
- Anthropic: `content[]` with `tool_use` (input = object); result = `tool_result`
  block in a **user** turn; correlation `tool_use_id`; SSE typed events.
- OpenAI Responses: `function_call` item in `output[]`; `arguments` = **JSON
  string** (normalize to object on ingest); result = `function_call_output`;
  correlation `call_id`.

## Build / test loop

1. Core + Anthropic adapter + REPL + canned. Unit tests for normalize/serialize
   round-trips. `curl` SSE validation.
2. OpenAI Responses adapter. **Live end-to-end against installed Codex.**
3. Bubble Tea TUI.
4. Passthrough, sequence recording, evidence bundle, tool-schema diff.
5. Gemini adapter; polish; recon sequence library; README + revert script.

## Milestones (implementation ↔ testing, loop as needed)

- [x] M1 core loop working (REPL, Anthropic + OpenAI), transcript logging; unit + full-loop HTTP tests green
- [x] M2 live Codex integration proven: real `codex exec` executed an operator-authored `exec_command` and returned the result
- [x] M3 TUI usable for a real operator: Bubble Tea console (context / offered-tools+diff / schema-templated composer / live REC); headless model + render tests
- [x] M4 passthrough (Step-0 capture, verified against api.anthropic.com) + `--record` + `report` (finding-chain + replayable reconstructed sequence)
- [x] M5 README operator guide, `env` setup/revert helper, Phase 2/3/5 sequences, `build.sh` cross-compiles win/linux/mac (amd64+arm64)
- [x] BONUS live Claude Code (primary target): real `claude -p` executed an operator-authored `Bash` call end-to-end through the mock

## Future work (not blocking)

- Gemini adapter (`functionCall`/`functionResponse` in `parts[]`): the neutral
  core + adapter interface already accommodate it.
- OpenAI `count_tokens`-equivalent if a client probes it; interactive
  intercept-toggle in passthrough (proxy until you take the wheel).
- Field-by-field schema form in the TUI composer (currently a templated JSON blob).
```
