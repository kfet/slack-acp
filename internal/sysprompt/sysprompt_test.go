package sysprompt

import (
	"strings"
	"testing"
)

func TestDefaultNonEmpty(t *testing.T) {
	if Default() == "" {
		t.Fatal("Default() empty")
	}
	if !strings.Contains(Default(), "Slack") {
		t.Fatal("Default() doesn't mention Slack")
	}
	if !strings.Contains(Default(), "mrkdwn") {
		t.Fatal("Default() should name Slack's format (mrkdwn)")
	}
}

func TestBuild(t *testing.T) {
	cases := map[string]struct {
		extra      string
		wantSuffix string
	}{
		"empty":      {extra: "", wantSuffix: ""},
		"whitespace": {extra: "   \n\t  ", wantSuffix: ""},
		"extra":      {extra: "you are @ops", wantSuffix: "\n\nyou are @ops"},
		"trimmed":    {extra: "  hello  ", wantSuffix: "\n\nhello"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			got := Build(c.extra)
			if !strings.HasPrefix(got, Default()) {
				t.Fatal("Build doesn't lead with Default")
			}
			if c.wantSuffix == "" {
				if got != Default() {
					t.Fatalf("expected Default() exactly; got len=%d", len(got))
				}
				return
			}
			if !strings.HasSuffix(got, c.wantSuffix) {
				t.Fatalf("missing suffix %q", c.wantSuffix)
			}
		})
	}
}

func TestResolve(t *testing.T) {
	if Resolve("hi", true, "") != "" {
		t.Fatal("disabled must return empty")
	}
	if Resolve("", false, "") != Default() {
		t.Fatal("enabled+empty must equal Default")
	}
	got := Resolve("extra", false, "")
	if !strings.HasSuffix(got, "\n\nextra") {
		t.Fatalf("Resolve didn't append extra: %q", got)
	}
	// Catalog appended after extra.
	got = Resolve("extra", false, "  <available_skills/>  ")
	if !strings.HasSuffix(got, "\n\n<available_skills/>") {
		t.Fatalf("Resolve didn't append catalog: %q", got)
	}
	// Whitespace-only catalog is treated as empty.
	if Resolve("", false, "  \n  ") != Default() {
		t.Fatal("whitespace catalog must collapse to empty")
	}
	// Disabled wins over catalog.
	if Resolve("", true, "<x/>") != "" {
		t.Fatal("disabled must beat catalog")
	}
}
