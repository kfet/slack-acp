package policy

import (
	"context"
	"testing"

	acp "github.com/coder/acp-go-sdk"
)

func req(title string) acp.RequestPermissionRequest {
	t := title
	return acp.RequestPermissionRequest{
		ToolCall: acp.ToolCallUpdate{Title: &t},
		Options: []acp.PermissionOption{
			{OptionId: "a", Name: "Allow once", Kind: "allow_once"},
			{OptionId: "r", Name: "Reject", Kind: "reject_once"},
		},
	}
}

func selected(r acp.RequestPermissionResponse) acp.PermissionOptionId {
	if r.Outcome.Selected == nil {
		return ""
	}
	return r.Outcome.Selected.OptionId
}

func TestParse(t *testing.T) {
	for _, n := range []string{"", "allow-all", "read-only", "deny-all"} {
		if _, err := Parse(n); err != nil {
			t.Fatalf("parse %q: %v", n, err)
		}
	}
	if _, err := Parse("nope"); err == nil {
		t.Fatal("want err")
	}
}

func TestAllowDeny(t *testing.T) {
	if got := selected(AllowAll{}.Decide(context.Background(), req("write file"))); got != "a" {
		t.Fatalf("allow: got %q", got)
	}
	if got := selected(DenyAll{}.Decide(context.Background(), req("write file"))); got != "r" {
		t.Fatalf("deny: got %q", got)
	}
}

func TestReadOnly(t *testing.T) {
	if got := selected(ReadOnly{}.Decide(context.Background(), req("read foo.go"))); got != "a" {
		t.Fatalf("read: got %q", got)
	}
	if got := selected(ReadOnly{}.Decide(context.Background(), req("write foo.go"))); got != "r" {
		t.Fatalf("write: got %q", got)
	}
	// Title-less request: no write keyword match → allow.
	rq := acp.RequestPermissionRequest{
		Options: []acp.PermissionOption{{OptionId: "a", Name: "Allow once", Kind: "allow_once"}},
	}
	if got := selected(ReadOnly{}.Decide(context.Background(), rq)); got != "a" {
		t.Fatalf("nil-title: got %q", got)
	}
}

// TestPickFallbacks exercises the branches inside pick that aren't
// otherwise reached: empty-options → empty selected; and the "no option
// matched, fall back to first" branch.
func TestPickFallbacks(t *testing.T) {
	// Empty options → outcome.Selected has empty OptionId.
	out := AllowAll{}.Decide(context.Background(), acp.RequestPermissionRequest{})
	if out.Outcome.Selected == nil || out.Outcome.Selected.OptionId != "" {
		t.Fatalf("empty-options: got %+v", out.Outcome.Selected)
	}

	// No option matches "allow" → fall back to first.
	rq := acp.RequestPermissionRequest{
		Options: []acp.PermissionOption{
			{OptionId: "x", Name: "weird", Kind: "weird"},
			{OptionId: "y", Name: "more weird", Kind: "weird"},
		},
	}
	if got := selected(AllowAll{}.Decide(context.Background(), rq)); got != "x" {
		t.Fatalf("no-match fallback: got %q want x", got)
	}
}

// TestParseTrim covers Parse's trim/lowercase normalisation paths.
func TestParseTrim(t *testing.T) {
	for _, n := range []string{"  Allow-All  ", "Read-Only", "Deny-All", "Allow", "ReadOnly", "Deny"} {
		if _, err := Parse(n); err != nil {
			t.Fatalf("parse %q: %v", n, err)
		}
	}
}
