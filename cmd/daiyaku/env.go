package main

import (
	"fmt"
	"os"
)

func runEnv(args []string) {
	fs := newFlagSet("env")
	provDef := envOr("DAIYAKU_PROVIDER", "anthropic")
	addrDef := envOr("DAIYAKU_ADDR", "127.0.0.1:8787")
	var provider, addr string
	fs.StringVar(&provider, "provider", provDef, "provider: anthropic, openai")
	fs.StringVar(&provider, "p", provDef, "alias for -provider")
	fs.StringVar(&addr, "addr", addrDef, "mock listen address: host:port, :port, or bare port")
	fs.StringVar(&addr, "a", addrDef, "alias for -addr")
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}
	addr = normalizeAddr(addr)
	switch provider {
	case "anthropic":
		envAnthropic(addr)
	case "openai":
		envOpenAI(addr)
	default:
		fmt.Fprintf(os.Stderr, "unknown provider %q\n", provider)
		os.Exit(2)
	}
}

func envAnthropic(addr string) {
	base := "http://" + addr
	fmt.Printf(`# ─── Point Claude Code at the daiyaku mock (Anthropic) ───
# The mock ignores auth, so any token works. ANTHROPIC_AUTH_TOKEN takes
# precedence over a saved login immediately (no interactive approval).

# PowerShell (current shell):
  $env:ANTHROPIC_BASE_URL = "%[1]s"
  $env:ANTHROPIC_AUTH_TOKEN = "test-harness-token"
  $env:CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC = "1"
  $env:CLAUDE_CODE_DISABLE_EXPERIMENTAL_BETAS = "1"

# bash / zsh:
  export ANTHROPIC_BASE_URL="%[1]s"
  export ANTHROPIC_AUTH_TOKEN="test-harness-token"
  export CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1
  export CLAUDE_CODE_DISABLE_EXPERIMENTAL_BETAS=1

# Persist across all surfaces (incl. background agents) via ~/.claude/settings.json:
  {
    "env": {
      "ANTHROPIC_BASE_URL": "%[1]s",
      "ANTHROPIC_AUTH_TOKEN": "test-harness-token",
      "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "1",
      "CLAUDE_CODE_DISABLE_EXPERIMENTAL_BETAS": "1"
    }
  }

# Verify the mock answers BEFORE opening the harness (single line; the mock
# ignores auth). bash: use 'curl'  ·  PowerShell: use 'curl.exe' (the bare name
# 'curl' is an alias for Invoke-WebRequest there and will not work):
  curl -X POST "%[1]s/v1/messages" -H "anthropic-version: 2023-06-01" -H "content-type: application/json" -d '{"model":"claude-sonnet-4-6","max_tokens":1,"messages":[{"role":"user","content":"."}]}'
# Then start 'claude' from the same shell and run /status; confirm the base URL line.

# ─── Revert ───
# PowerShell:
  Remove-Item Env:ANTHROPIC_BASE_URL, Env:ANTHROPIC_AUTH_TOKEN, Env:CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC, Env:CLAUDE_CODE_DISABLE_EXPERIMENTAL_BETAS
# bash:
  unset ANTHROPIC_BASE_URL ANTHROPIC_AUTH_TOKEN CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC CLAUDE_CODE_DISABLE_EXPERIMENTAL_BETAS
# And remove the "env" block from ~/.claude/settings.json, then restart the harness.
`, base)
}

func envOpenAI(addr string) {
	base := "http://" + addr + "/v1"
	fmt.Printf(`# ─── Point Codex CLI at the daiyaku mock (OpenAI Responses) ───
# Provider config MUST live in the USER-level ~/.codex/config.toml (project-local
# is ignored). The ids 'openai', 'ollama', 'lmstudio' are reserved; use a new id.

# ~/.codex/config.toml:
  model = "gpt-5-codex"
  model_provider = "harness_test"

  [model_providers.harness_test]
  name = "Harness Test"
  base_url = "%[1]s"
  env_key = "HARNESS_TEST_KEY"
  wire_api = "responses"

# The mock ignores auth, but env_key must be set to a non-empty value:
# PowerShell:  $env:HARNESS_TEST_KEY = "test-harness-token"
# bash:        export HARNESS_TEST_KEY=test-harness-token

# Verify (single line; bash: 'curl'  ·  PowerShell: 'curl.exe'):
  curl -X POST "%[1]s/responses" -H "content-type: application/json" -d '{"model":"gpt-5-codex","input":"ping","stream":false}'

# ─── Revert ───
# Remove the [model_providers.harness_test] block and the model_provider line
# from ~/.codex/config.toml, unset HARNESS_TEST_KEY, and restart Codex.
`, base)
}
