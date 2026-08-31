package anthropic

import "testing"

// Server-side tools carry a versioned "type" and usually no input_schema.
// Reporting them as ordinary functions understates the offered tool surface,
// which is what the enumeration phase of a test is measuring.
func TestServerToolsKeepTheirKind(t *testing.T) {
	body := []byte(`{"model":"m","tools":[
		{"type":"web_search_20250305","name":"web_search","max_uses":5},
		{"type":"text_editor_20250124","name":"str_replace_editor"},
		{"type":"custom","name":"Explicit","input_schema":{"type":"object"}},
		{"name":"Bash","description":"d","input_schema":{"type":"object"}}],
		"messages":[]}`)
	req, err := (&Adapter{}).Normalize(nil, body)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"web_search":         "web_search_20250305",
		"str_replace_editor": "text_editor_20250124",
		"Explicit":           "function",
		"Bash":               "function",
	}
	if len(req.Tools) != len(want) {
		t.Fatalf("normalized %d tools, want %d", len(req.Tools), len(want))
	}
	for _, tool := range req.Tools {
		if got := tool.Kind; got != want[tool.Name] {
			t.Errorf("%s kind = %q, want %q", tool.Name, got, want[tool.Name])
		}
	}
}
