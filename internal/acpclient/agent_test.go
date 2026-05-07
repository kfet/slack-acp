package acpclient

import (
	"encoding/json"
	"testing"
)

func TestParseCaps(t *testing.T) {
	cases := map[string]struct {
		raw  string
		want Caps
	}{
		"empty":           {raw: `{}`, want: Caps{}},
		"loadSession":     {raw: `{"agentCapabilities":{"loadSession":true}}`, want: Caps{LoadSession: true}},
		"embeddedContext": {raw: `{"agentCapabilities":{"promptCapabilities":{"embeddedContext":true}}}`, want: Caps{EmbeddedContext: true}},
		"malformed":       {raw: `{"agentCapabilities":`, want: Caps{}},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			got := parseCaps(json.RawMessage(c.raw))
			if got != c.want {
				t.Fatalf("got %+v want %+v", got, c.want)
			}
		})
	}
}

func TestStartRequiresCommand(t *testing.T) {
	if _, err := Start(t.Context(), Config{Policy: nil}); err == nil {
		t.Fatal("want err on empty cmd")
	}
	if _, err := Start(t.Context(), Config{Command: []string{"echo"}}); err == nil {
		t.Fatal("want err on nil policy")
	}
}
