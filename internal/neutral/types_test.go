package neutral

import "testing"

func TestIsSafetyClassifier(t *testing.T) {
	// The real classifier: sentinel system prompt (here after the billing-header
	// block decodeSystem prepends) and no tools offered.
	classifier := &Request{
		System: "x-anthropic-billing-header: cc_version=2.1;\nYou are a security monitor for autonomous AI coding agents.\n\n## Context",
	}
	if !classifier.IsSafetyClassifier() {
		t.Error("classifier request not detected")
	}

	cases := []struct {
		name string
		req  *Request
	}{
		{"tool-bearing request with sentinel", &Request{
			System: classifier.System,
			Tools:  []ToolDef{{Name: "Bash"}},
		}},
		{"normal agent turn", &Request{
			System: "You are Claude Code, an interactive CLI tool.",
			Tools:  []ToolDef{{Name: "Bash"}},
		}},
		{"empty request", &Request{}},
	}
	for _, tc := range cases {
		if tc.req.IsSafetyClassifier() {
			t.Errorf("%s: misdetected as safety classifier", tc.name)
		}
	}
}
