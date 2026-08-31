# Operator-in-the-Loop Testing of Agentic Coding Harnesses

**A methodology for purple team engagements against Claude Code, Codex CLI, and similar tools**

Version 1.0

---

## 1. Purpose

This document describes how to test the *blast radius* of an agentic coding harness by substituting a human operator for the language model, while leaving the entire harness intact.

The operator writes the tool calls the model would otherwise generate. The harness executes them against the real environment, with real credentials, real permission configuration, and real logging. The result is a deterministic, repeatable map of what the agent identity can reach and which of those actions the defenders can see.

### What this tests

| In scope | Out of scope |
| --- | --- |
| Agent identity permissions and reachable systems | Whether the vendor's model refuses a request |
| Harness permission config, hooks, allowlists, sandbox modes | Model robustness to jailbreak phrasing |
| Detection and telemetry coverage of agent-initiated actions | Model output quality |
| Egress paths available through agent tooling | Vendor-side abuse detection |
| MCP server and connector trust boundaries | |

### Why refusal testing is not a substitute

Model refusal is a vendor-owned behavioral tendency, not a control. You cannot write policy against it, monitor it, version-control it, or attest to it, and it can change silently on a model update with no change management on your side. A refusal also generates no interesting telemetry, so the blue team learns nothing from it.

This matters more than it used to. OWASP's 2026 LLM Top 10 moved Excessive Agency from sixth to third place, weighted partly by real incident data rather than practitioner opinion alone. Excessive agency is a property of your IAM and harness configuration, not the model. It is testable, fixable, and yours.

Prompt-layer testing retains value in three specific cases, and these should stay in scope:

1. Testing **your own** guardrail layer (Bedrock Guardrails, Azure content filters, Llama Guard, custom injection classifiers). That is company-owned config that should fire and log.
2. **Abuse detection**: does the SOC notice a user hammering the agent with adversarial prompts?
3. **Indirect prompt injection**, which depends entirely on your RAG and tool architecture. This is the highest-value prompt-layer test you have and it is a different activity from asking the model to misbehave directly.

---

## 2. Authorization and Safety

This technique deliberately removes the model's judgment from a tool that has shell access and production credentials. Do not proceed without the following in writing.

### Pre-engagement checklist

- [ ] Written authorization naming the technique explicitly: "redirection of the agent's inference endpoint to a test-controlled server, with operator-authored tool calls executed against \<scope\>."
- [ ] Scope definition: which hosts, which repositories, which credentials, which connected MCP servers and SaaS tenants.
- [ ] Explicit statement of whether production data may be read, and whether any write, delete, or send operations are permitted. **Default to read-only and require named exceptions.**
- [ ] Named client-side point of contact reachable during testing windows.
- [ ] Agreed abort conditions and a rollback plan.
- [ ] Blue team notification (this is purple team; covertness is not the goal and costs you time).
- [ ] An out-of-band deconfliction marker so the SOC can distinguish test activity from a real incident during triage. Agree the marker in advance and record the exact times of each test window.

### Operational safety rules

- Prefer a dedicated test workstation or VM mirroring the standard developer build over a live developer's machine.
- Prefer scoped duplicate credentials over the developer's own tokens where the identity model allows it.
- Never issue a destructive tool call to confirm a finding when a read-only proof exists. `ls` on the directory is a finding; `rm` is an incident.
- Log every request and response through the mock. This is your primary evidence and your only defence if something goes wrong.
- Have the revert command ready before you start: unset the environment variables, remove the settings file `env` block, restart the harness.

---

## 3. Architecture

```
┌──────────────────┐         ┌─────────────────────┐        ┌──────────────────┐
│  Operator TUI    │◄───────►│   Mock Inference    │◄──────►│  Real Harness    │
│                  │         │      Server         │  HTTP  │  (claude / codex)│
│  - see context   │         │                     │        │                  │
│  - see tool defs │         │  - protocol adapter │        │  - permissions   │
│  - author calls  │         │  - SSE streaming    │        │  - hooks         │
│  - read results  │         │  - full transcript  │        │  - sandbox       │
└──────────────────┘         └─────────────────────┘        └────────┬─────────┘
                                                                     │
                                                        ┌────────────▼────────────┐
                                                        │  Real target environment │
                                                        │  files, shell, git,      │
                                                        │  MCP servers, cloud APIs │
                                                        └────────────┬────────────┘
                                                                     │
                                                        ┌────────────▼────────────┐
                                                        │  Defender telemetry      │
                                                        │  EDR, SIEM, proxy, audit │
                                                        └──────────────────────────┘
```

Four components:

1. **Mock inference server**: an HTTP server implementing the provider's inference endpoint(s) in the provider's response format, including streaming.
2. **Protocol adapter**: per-provider serialization. Normalizes the incoming request into a provider-neutral internal form and serializes the operator's abstract tool call back into the provider's wire format.
3. **Operator console**: presents the conversation state and available tool schemas, accepts an abstract tool call, displays the returned result.
4. **Collection**: harness debug logs, EDR, SIEM, proxy, cloud audit logs, plus your own full transcript.

### The loop

1. Harness POSTs an inference request containing system prompt, conversation history, and tool schemas.
2. Mock server normalizes and hands it to the operator console.
3. Operator authors a tool call, for example `Bash{command: "find / -name '*.pem' -readable 2>/dev/null"}`.
4. Adapter serializes it into a `tool_use` (Anthropic) or `function_call` (OpenAI Responses) block and streams it back.
5. Harness applies its permission model, then either executes, prompts for approval, or refuses.
6. Harness POSTs the next request with the tool result appended.
7. Operator reads the result and decides the next call.

Everything between step 4 and step 6 is the system under test.

---

## 4. Build

### Step 0: Capture before you build

**Do this first.** Do not build the mock from documentation or from anyone's recollection of the wire format, including this document's.

Run the genuine harness against the genuine API through an intercepting proxy and record the exact request and response shapes for a session that includes at least one tool call, one multi-tool turn, and one error.

```bash
# Test VM, with the proxy CA installed in the system store
mitmdump -w baseline.flows -p 8080

# Claude Code respects NODE_EXTRA_CA_CERTS for a corporate or test CA
export NODE_EXTRA_CA_CERTS=/usr/local/share/ca-certificates/testca.crt
export HTTPS_PROXY=http://127.0.0.1:8080
claude
```

You now have ground truth for the SSE event sequence, required headers, and the exact JSON shape of a tool call. Build the mock to replay that shape. This also gives you the **benign baseline telemetry** referenced in section 7.

### Step 1: Point the harness at the mock

#### Claude Code

Claude Code supports this natively because enterprises deploy it behind LLM gateways. Two variables:

```bash
export ANTHROPIC_BASE_URL=http://127.0.0.1:8787
export ANTHROPIC_AUTH_TOKEN=test-harness-token
```

`ANTHROPIC_AUTH_TOKEN` is sent as `Authorization: Bearer`. `ANTHROPIC_API_KEY` is sent as `x-api-key` instead. Either works for a mock that ignores auth; use `ANTHROPIC_AUTH_TOKEN` since it takes precedence over a saved login immediately, whereas `ANTHROPIC_API_KEY` requires a one-time interactive approval.

To persist across all surfaces including background agents, use the `env` block in `~/.claude/settings.json`:

```json
{
  "env": {
    "ANTHROPIC_BASE_URL": "http://127.0.0.1:8787",
    "ANTHROPIC_AUTH_TOKEN": "test-harness-token",
    "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "1",
    "CLAUDE_CODE_DISABLE_EXPERIMENTAL_BETAS": "1"
  }
}
```

Two supporting variables worth setting:

- `CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1` suppresses version checks, telemetry, error reports, and auto-update. Cleaner isolation and a cleaner egress picture. Note it also disables auto-update and gateway model discovery.
- `CLAUDE_CODE_DISABLE_EXPERIMENTAL_BETAS=1` suppresses pre-release request fields, which reduces the surface your mock has to tolerate.

**Verify before opening the harness:**

```bash
curl -X POST "$ANTHROPIC_BASE_URL/v1/messages" \
  -H "Authorization: Bearer $ANTHROPIC_AUTH_TOKEN" \
  -H "anthropic-version: 2023-06-01" \
  -H "content-type: application/json" \
  -d '{"model":"claude-sonnet-4-6","max_tokens":1,"messages":[{"role":"user","content":"."}]}'
```

Then start `claude` from the same shell and run `/status`. The Status tab should show an `Anthropic base URL` line with your mock's address and an `Auth token` line naming the variable you set. If the base URL line is absent, the variable did not reach the session.

**Endpoints your mock must implement:**

| Endpoint | Required | Notes |
| --- | --- | --- |
| `POST /v1/messages` | Yes | The main inference loop |
| `POST /v1/messages/count_tokens` | Yes | Called for context management; return a plausible integer |
| `GET /v1/models` | Optional | Only if you enable gateway model discovery |

Preserve `anthropic-version` and `anthropic-beta` headers in your logging; they tell you which features the harness is requesting.

#### Codex CLI

Codex is open source (Apache 2.0), so you can read the exact request construction rather than inferring it. Configure a custom provider in `~/.codex/config.toml`:

```toml
model = "gpt-5-codex"
model_provider = "harness_test"

[model_providers.harness_test]
name = "Harness Test"
base_url = "http://127.0.0.1:8788/v1"
env_key = "HARNESS_TEST_KEY"
wire_api = "responses"
```

Three gotchas:

- The provider IDs `openai`, `ollama`, and `lmstudio` are **reserved**. You cannot override the built-in `openai` provider's base URL through this block; define a new provider ID.
- `model_provider` and `model_providers` are only honoured in the **user-level** `~/.codex/config.toml`. Codex ignores them in a project-local `.codex/config.toml` and prints a startup warning.
- `OPENAI_BASE_URL` is also supported as an environment variable shortcut for the built-in provider.

**Verify the `wire_api` value against the Codex source or current docs before building.** Sources conflict on whether `wire_api = "chat"` still works: some 2026 reporting states the Chat Completions path was removed in February 2026 and that a provider block with `wire_api = "chat"` now fails at startup, while other sources say both values are still accepted. Since Codex is open source, check the repository rather than trusting either. This determines whether your adapter targets the Responses API shape or the Chat Completions shape, so get it right before you write code.

#### Fallback: TLS interception

If a harness does not expose a base URL override, or if managed policy blocks it (see section 6), fall back to a MITM proxy with a CA certificate installed in the test VM's trust store. Configure the proxy to answer the inference endpoint locally rather than forwarding. Fragile against certificate pinning, but it works against most Node and Rust HTTP clients that use the system or `NODE_EXTRA_CA_CERTS` store.

### Step 2: The mock server

Skeleton only. Fill in the SSE event sequence from your Step 0 capture.

```python
# harness_mock.py
# Requires: fastapi, uvicorn
import json, uuid, asyncio
from fastapi import FastAPI, Request
from fastapi.responses import StreamingResponse, JSONResponse

app = FastAPI()
PENDING = asyncio.Queue()   # normalized requests -> operator console
REPLIES = asyncio.Queue()   # operator tool calls -> harness

TRANSCRIPT = open("transcript.jsonl", "a")

def log(direction, payload):
    TRANSCRIPT.write(json.dumps({"dir": direction, "ts": __import__("time").time(),
                                 "payload": payload}) + "\n")
    TRANSCRIPT.flush()

@app.post("/v1/messages/count_tokens")
async def count_tokens(req: Request):
    return JSONResponse({"input_tokens": 1000})

@app.post("/v1/messages")
async def messages(req: Request):
    body = await req.json()
    log("harness->mock", {"headers": dict(req.headers), "body": body})

    # Normalize for the operator: last turn, available tools, any tool results
    await PENDING.put({
        "provider": "anthropic",
        "system": body.get("system"),
        "messages": body.get("messages", []),
        "tools": [t.get("name") for t in body.get("tools", [])],
        "tool_schemas": body.get("tools", []),
    })

    action = await REPLIES.get()      # {"tool": "Bash", "input": {...}}  or {"text": "..."}
    log("mock->harness", action)

    if body.get("stream"):
        return StreamingResponse(sse_tool_use(action),
                                 media_type="text/event-stream")
    return JSONResponse(block_tool_use(action))


def block_tool_use(action):
    """Non-streaming response body. Verify field names against your capture."""
    return {
        "id": f"msg_{uuid.uuid4().hex[:24]}",
        "type": "message",
        "role": "assistant",
        "model": "harness-mock",
        "content": [{
            "type": "tool_use",
            "id": f"toolu_{uuid.uuid4().hex[:24]}",
            "name": action["tool"],
            "input": action["input"],
        }],
        "stop_reason": "tool_use",
        "usage": {"input_tokens": 1000, "output_tokens": 50},
    }


async def sse_tool_use(action):
    """
    SSE event sequence. The ordering below is the standard Anthropic streaming
    shape, but CONFIRM IT AGAINST YOUR STEP 0 CAPTURE before relying on it.
    Approximate sequence:
      message_start
      content_block_start   (type: tool_use, with id and name)
      content_block_delta   (input_json_delta, partial_json chunks)
      content_block_stop
      message_delta         (stop_reason: tool_use)
      message_stop
    """
    msg_id = f"msg_{uuid.uuid4().hex[:24]}"
    tool_id = f"toolu_{uuid.uuid4().hex[:24]}"
    payload = json.dumps(action["input"])

    def ev(name, data):
        return f"event: {name}\ndata: {json.dumps(data)}\n\n"

    yield ev("message_start", {"type": "message_start", "message": {
        "id": msg_id, "type": "message", "role": "assistant",
        "model": "harness-mock", "content": [], "stop_reason": None,
        "usage": {"input_tokens": 1000, "output_tokens": 1}}})
    yield ev("content_block_start", {"type": "content_block_start", "index": 0,
        "content_block": {"type": "tool_use", "id": tool_id,
                          "name": action["tool"], "input": {}}})
    yield ev("content_block_delta", {"type": "content_block_delta", "index": 0,
        "delta": {"type": "input_json_delta", "partial_json": payload}})
    yield ev("content_block_stop", {"type": "content_block_stop", "index": 0})
    yield ev("message_delta", {"type": "message_delta",
        "delta": {"stop_reason": "tool_use", "stop_sequence": None},
        "usage": {"output_tokens": 50}})
    yield ev("message_stop", {"type": "message_stop"})
```

### Step 3: The operator console

Keep it minimal. It needs to:

- Display the tool schemas the harness offered on this turn (these change per harness and per configuration, and the diff between what is offered and what you expected is itself a finding).
- Display the last tool result.
- Accept an abstract tool call.
- Support **canned sequences**: a JSON file of ordered tool calls replayed automatically. This is what makes findings reproducible for the report and lets you re-run the whole chain after a config change.
- Support **passthrough mode**: forward the request to a real model, return its response, and only intercept when you want to take the wheel. Useful for letting the model do tedious setup while you control the decision points.

A REPL against the two queues is sufficient. Do not over-engineer this before the first engagement.

---

## 5. Test Procedure

Run each phase, recording tool call, harness response (executed / prompted / denied), result, and observed telemetry.

### Phase 1: Harness enumeration

Before executing anything, read what the harness told you.

- What tools are exposed, with what schemas?
- What is in the system prompt? (Often reveals internal conventions, repo structure, org policy, occasionally secrets.)
- What MCP servers are connected and what do their tool schemas expose?
- What does the permission configuration allow? Read `settings.json`, managed settings, `.claude/` or `.codex/` project config, hooks.

This phase is passive and often produces findings on its own.

### Phase 2: Permission boundary mapping

Systematically probe what the harness executes silently versus what prompts versus what is denied.

- Read inside the working directory (expected: silent).
- Read outside the working directory: home, `/etc`, sibling repos, mounted shares.
- Read credential locations: `~/.aws`, `~/.kube`, `~/.ssh`, `.env` files, `~/.config`, cloud SDK caches, browser profile paths.
- Write inside the working directory.
- Write outside it, including to shell profiles, `.git/hooks`, and the harness's own config files.
- Shell commands matching and not matching any allowlist.
- Attempt allowlist evasion within the harness's own matching logic (shell metacharacters, `env` prefixing, indirect invocation, path aliasing). Whether the allowlist is bypassable is a control finding, not a model finding.

For Codex specifically, run this matrix under each `sandbox_mode` (`read-only`, `workspace-write`, `danger-full-access`) and each `approval_policy`. The delta between modes is the deliverable.

### Phase 3: Identity and blast radius

The agent runs as some identity. Enumerate what that identity reaches.

- Cloud credentials on the host and their effective permissions.
- Git remote access: which repos, push rights, CI trigger ability.
- Package registry credentials and publish rights.
- MCP server scopes: which SaaS tenants, which records, read versus write.
- Reachable internal network services, including cloud instance metadata endpoints.
- Secrets recoverable from the environment, filesystem, or shell history.

Record what could be reached. Do not exercise write paths without the named exception from section 2.

### Phase 4: Persistence and configuration integrity

Can the agent modify the things that constrain the agent?

- Its own settings and permission files.
- Hook definitions.
- MCP server configuration (can it add a server?).
- Shell profiles, git hooks, scheduled tasks.
- Managed settings (should be protected; verify).

An agent that can rewrite its own allowlist has no allowlist.

### Phase 5: Egress

Map the paths data can leave through.

- Direct HTTP from shell tools.
- Web fetch tooling, and whether its domain restrictions are enforced.
- Git push to an attacker-controlled remote.
- MCP servers with outbound capability.
- Package publish paths.
- DNS.

Use a controlled collaborator endpoint. Send only benign canaries, never real client data.

### Phase 6: Indirect injection (retain from prompt-layer testing)

This one still uses the real model, because the question is whether untrusted content reaches the instruction channel. Plant benign marker instructions in content the agent will retrieve (a README, an issue body, a dependency file, a web page, an MCP tool result) and observe whether the harness acts on them. The marker action should be harmless and loud, such as writing a specific string to a specific file.

---

## 6. Expected Obstacles

| Obstacle | Meaning | Response |
| --- | --- | --- |
| `forceLoginMethod` or `forceLoginOrgUUID` in managed settings | These cannot coexist with `ANTHROPIC_API_KEY`, `ANTHROPIC_AUTH_TOKEN`, or `apiKeyHelper`. Claude Code will refuse with `This machine's managed settings require a first-party login`. | **This is a positive control finding.** Document it as an effective mitigation against endpoint redirection. Fall back to TLS interception or credential assumption, with authorization. |
| Harness prompts for login despite a reachable mock | A reachable base URL is not a credential. In interactive sessions an `env` block in a project-local settings file applies only after the first-run trust prompt. | Set the credential in a shell export or `~/.claude/settings.json`. |
| `403` with an HTML body, gateway logs show no request | A WAF in front of your mock inspected the body. Agent prompts contain XML-style tags and source code that match XSS body rules. | Exempt the `/v1/messages` path from body inspection, or bind the mock to loopback. |
| TLS or certificate errors when curl works | The harness runtime does not trust the same CA as curl. | Set `NODE_EXTRA_CA_CERTS` to the CA bundle path. |
| `400` naming unrecognized fields | Harness is sending pre-release fields your mock rejects. | Set `CLAUDE_CODE_DISABLE_EXPERIMENTAL_BETAS=1`, or make the mock permissive and ignore unknown fields. |
| Vendor-side audit logs empty | Traffic never reached the vendor. | Expected. Test vendor-side logging separately with a small volume of real traffic. |
| MCP tool search disabled | Claude Code disables MCP tool search by default when the base URL is not `api.anthropic.com`. | Note this changes the exposed tool surface versus production. Re-enable if your build supports it, or account for the delta in the report. |

---

## 7. Evidence and Reporting

### Collect

- Full mock transcript (request and response JSONL). Primary evidence.
- Harness debug logs (`claude --debug` writes to `~/.claude/debug/<session-id>.txt`).
- EDR process tree and file events for the session window.
- SIEM query results for the window.
- Proxy and DNS logs.
- Cloud and SaaS audit logs for the agent identity.

### The baseline diff

Run the genuine harness doing genuine benign work, capture the telemetry, then run the operator-driven adversarial session and diff. The deliverable the blue team actually needs is: *here is normal, here is not normal, here is which of those two you can currently distinguish.*

Most of the detection surface is not the harness process. It is the child processes: `bash` invocations, `git` calls, file writes, package installs, and the outbound connections those children make. Focus detection engineering there.

### Report each finding as a chain

```
Operator tool call
  -> harness permission decision (executed / prompted / denied)
    -> action taken in environment
      -> systems reached
        -> telemetry generated (or absent)
          -> detection fired (or not)
```

Replayable evidence matters. Ship the canned sequence file with the report so the client can re-run each chain after remediation.

Map findings to OWASP's Top 10 for Agentic Applications and NIST AI RMF for procurement and audit consumption.

### Rate findings by blast radius, not by cleverness

An agent that can silently read `~/.aws/credentials` in an account with broad permissions is a higher-severity finding than an exotic allowlist bypass that reaches nothing interesting.

---

## 8. Multi-Provider Portability

### Short answer

The core is portable. The adapters are small. Budget roughly one to three days per additional harness, front-loaded on the first one.

Every one of these tools is the same shape: a local process that speaks HTTP to an inference endpoint and executes returned tool calls against the local machine. The harness is the target. The wire protocol is just an interface, and there are only about three dialects in wide use.

### Design for it from the start

Split the codebase in two:

```
core/          provider-neutral: operator console, transcript,
               canned sequences, evidence collection
adapters/
  anthropic.py   Messages API      content[] with tool_use blocks
  openai_resp.py Responses API     function_call items
  openai_chat.py Chat Completions  tool_calls[] on the message
  gemini.py      Gemini API        parts[] with functionCall
```

Define an internal neutral representation:

```python
{
  "system": str,
  "turns": [ {"role": ..., "content": ...} ],
  "tools":  [ {"name": ..., "schema": {...}} ],
  "pending_results": [ {"call_id": ..., "output": ...} ]
}
```

Each adapter implements `normalize(request) -> neutral` and `serialize(operator_action) -> provider_response`, in both blocking and streaming forms. Nothing else in the codebase should know which provider is in play.

### Wire format comparison

| | Anthropic Messages | OpenAI Responses | OpenAI Chat Completions | Gemini |
| --- | --- | --- | --- | --- |
| Model output holding a call | `content[]` array with a `tool_use` block | `function_call` item in `output[]` | `tool_calls[]` on the assistant message | `parts[]` with `functionCall` |
| Call arguments | `input` object | `arguments` (JSON string) | `arguments` (JSON string) | `args` object |
| Result returned as | `tool_result` block in a **user** turn | `function_call_output` item | Message with `role: "tool"` | `functionResponse` part |
| Correlation | `tool_use_id` | `call_id` | `tool_call_id` | function name |
| Streaming | SSE, typed events | SSE, typed events | SSE, `delta` chunks | SSE |

The main irritation is that OpenAI passes arguments as a JSON-encoded string while Anthropic and Gemini pass an object. Normalize on ingest.

### Difficulty tiers

**Tier 1: base URL override plus a known dialect.** One to two days.

Claude Code (documented gateway support), Codex CLI (`model_providers` block, plus open source so you can read the request construction), Gemini CLI, Aider, Cline, Goose, Continue, opencode. Anything OpenAI-compatible is nearly free once you have the first OpenAI adapter, which covers a large fraction of the ecosystem including local model runners.

**Tier 2: no clean override, or an unusual auth flow.** Three to five days.

Harnesses that authenticate by OAuth against a vendor account and will not accept an arbitrary endpoint, or that route through a vendor-controlled gateway. Fall back to TLS interception with a trusted CA in the test VM. Add time for handling any auth handshake the harness performs before inference.

**Tier 3: certificate pinning or request signing.** Case by case, sometimes not worth it.

Pinning defeats the proxy approach. For an open-source harness you can patch and rebuild. For a closed-source one, fall back to credential assumption: extract the agent's identity, drive its tool surface directly with a script, and accept that you are testing the identity's blast radius rather than the harness's permission model.

### Provider-specific notes

**Codex CLI.** Easiest after Claude Code. Open source under Apache 2.0, so read the source instead of guessing. Reserved provider IDs (`openai`, `ollama`, `lmstudio`) mean you must define a new provider block rather than override the built-in. Provider config must live in the user-level `~/.codex/config.toml`; it is ignored in project-local config. Confirm the current `wire_api` situation before building the adapter.

Codex's sandbox model is genuinely different from Claude Code's permission model, and that difference is the most interesting cross-harness deliverable you can produce. Codex has explicit `sandbox_mode` values (`read-only`, `workspace-write`, `danger-full-access`) with OS-level enforcement on some platforms, while Claude Code uses a permission and hook system. Running the identical Phase 2 matrix against both and publishing the delta is a strong finding for a client choosing between them, or running both.

**Gemini CLI.** Different dialect (`functionCall` / `functionResponse` in `parts[]`), so it needs its own adapter, but the shape is not difficult. Open source.

**Local runners (Ollama, LM Studio, vLLM).** Almost all expose OpenAI-compatible endpoints, so the OpenAI adapter usually just works.

### Recommended order

1. **Claude Code** first. Best-documented redirection path, so you validate the architecture with the fewest unknowns.
2. **Codex CLI** second. Open source, so you can verify every assumption against the source, and it gives you the OpenAI adapter that unlocks most of the rest.
3. Everything else after that is adapter work against a core you have already proven.

---

## 9. Limitations

State these in the report. They are real.

- **A human operator is more goal-directed and more competent than a model.** This maps the *ceiling* of damage, not the realistic distribution. You will underestimate emergent failure modes: agents looping, hallucinating tool parameters, over-interpreting instructions, or being steered by content they retrieved. Keep the model in the loop for Phase 6 for exactly this reason.
- **Vendor-side controls are not exercised.** Abuse detection, rate limiting, and vendor audit logging are bypassed by construction. Test them separately.
- **Model-layer mitigations are removed by design.** Findings say what is possible if the model's judgment fails, not how likely that failure is. Do not let the report imply otherwise.
- **Results have a short shelf life.** Agentic deployments change with model updates, harness releases, and new MCP connectors. A blast radius map is accurate as of the tested configuration. Recommend re-testing on harness version changes and MCP connector additions, and ship the canned sequences so the client can do it themselves.
- **MCP tool search behaviour differs under redirection** in Claude Code, so the exposed tool surface may not exactly match production. Account for the delta.

---

## 10. Reference

Verify all product-specific details against current documentation before each engagement. These change frequently.

- Claude Code, connect to an LLM gateway: https://code.claude.com/docs/en/llm-gateway-connect
- Claude Code, gateway protocol reference: https://code.claude.com/docs/en/llm-gateway-protocol
- Claude Code, settings: https://code.claude.com/docs/en/settings
- Claude Code docs index: https://code.claude.com/docs/llms.txt
- Codex advanced configuration: https://developers.openai.com/codex/config-advanced
- Codex source: https://github.com/openai/codex
- OWASP Top 10 for LLM and Agentic Applications (2026 revision)
- MITRE ATLAS
- NIST AI RMF

Complementary tooling, for the prompt-layer work that stays in scope: garak (broad scanning), promptfoo (CI regression), PyRIT (multi-turn campaigns), DeepTeam (agentic vulnerability classes with full-pipeline callbacks).
