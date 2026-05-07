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
		"listSessions":    {raw: `{"agentCapabilities":{"sessionCapabilities":{"list":{}}}}`, want: Caps{ListSessions: true}},
		"resumeSession":   {raw: `{"agentCapabilities":{"sessionCapabilities":{"resume":{}}}}`, want: Caps{ResumeSession: true}},
		"listAndResume":   {raw: `{"agentCapabilities":{"sessionCapabilities":{"list":{},"resume":{}}}}`, want: Caps{ListSessions: true, ResumeSession: true}},
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
