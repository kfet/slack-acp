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
}
