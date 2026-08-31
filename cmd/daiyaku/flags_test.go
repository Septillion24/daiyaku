package main

import "testing"

// A launch profile seeds defaults; it must not overrule a DAIYAKU_ADDR the
// operator exported for the session. Precedence is flag > env > profile >
// built-in default.
func TestAddrPrecedence(t *testing.T) {
	codex := profiles["codex"]
	for _, tc := range []struct {
		name     string
		prof     profile
		env      string
		args     []string
		wantAddr string
		wantProv string
	}{
		{"built-in default", profile{}, "", nil, "127.0.0.1:8787", "anthropic"},
		{"profile", codex, "", nil, "127.0.0.1:8790", "openai"},
		{"env beats profile", codex, "9999", nil, "127.0.0.1:9999", "openai"},
		{"flag beats env", codex, "9999", []string{"-a", "7777"}, "127.0.0.1:7777", "openai"},
		{"env with no profile", profile{}, "9999", nil, "127.0.0.1:9999", "anthropic"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("DAIYAKU_ADDR", tc.env)
			t.Setenv("DAIYAKU_PROVIDER", "")
			f, err := parseServeFlags(tc.prof, tc.args)
			if err != nil {
				t.Fatal(err)
			}
			if f.addr != tc.wantAddr {
				t.Errorf("addr = %q, want %q", f.addr, tc.wantAddr)
			}
			if f.provider != tc.wantProv {
				t.Errorf("provider = %q, want %q", f.provider, tc.wantProv)
			}
		})
	}
}

func TestNormalizeAddr(t *testing.T) {
	ok := map[string]string{
		"8787":           "127.0.0.1:8787",
		":8787":          "127.0.0.1:8787",
		"127.0.0.1:8787": "127.0.0.1:8787",
		"0.0.0.0:8787":   "0.0.0.0:8787",
	}
	for in, want := range ok {
		got, err := normalizeAddr(in)
		if err != nil || got != want {
			t.Errorf("normalizeAddr(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"99999", "0", "-1", "not-an-addr", "127.0.0.1:banana"} {
		if _, err := normalizeAddr(bad); err == nil {
			t.Errorf("normalizeAddr(%q) accepted an unusable address", bad)
		}
	}
}

func TestIsLoopback(t *testing.T) {
	for addr, want := range map[string]bool{
		"127.0.0.1:8787": true,
		"localhost:8787": true,
		"[::1]:8787":     true,
		"0.0.0.0:8787":   false,
		":8787":          false,
		"192.168.1.5:87": false,
	} {
		if got := isLoopback(addr); got != want {
			t.Errorf("isLoopback(%q) = %v, want %v", addr, got, want)
		}
	}
}
