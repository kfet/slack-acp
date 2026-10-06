package slackmcp

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/slack-go/slack"
)

func TestHistoryTool(t *testing.T) {
	ctrl := &fakeCtrl{result: "[]"}
	h, tok := liveHost(t, ctrl, false)
	res := callTool(t, h, tok, ToolHistory, `{"origin":true,"limit":4}`)
	if isErr(res) || ctrl.called != ToolHistory || !ctrl.origin || ctrl.limit != 4 || ctrl.sessionKey != "C1/9.9" {
		t.Fatalf("res = %v ctrl = %+v", res, ctrl)
	}
	if res := callTool(t, h, tok, ToolHistory, `{"origin":"yes"}`); !isErr(res) || !strings.HasPrefix(text(res), "invalid params:") {
		t.Fatalf("res = %v", res)
	}
}

func TestBranchTool(t *testing.T) {
	br := &fakeBrancher{res: []BranchResult{{Channel: "C1", ThreadTS: "5.0", Link: "https://x"}}}
	h, tok := liveHostWith(t, &fakeCtrl{}, br, false)
	res := callTool(t, h, tok, ToolBranch, `{"tasks":[{"title":"  a   b ","seed":" go ","from_msg":" 1.5 "}]}`)
	if isErr(res) {
		t.Fatalf("res = %v", res)
	}
	if br.sessionKey != "C1/9.9" {
		t.Fatalf("sessionKey = %q", br.sessionKey)
	}
	if want := (BranchTask{Title: "a b", Seed: "go", FromMsg: "1.5"}); len(br.tasks) != 1 || br.tasks[0] != want {
		t.Fatalf("tasks = %+v", br.tasks)
	}
	var got []BranchResult
	if err := json.Unmarshal([]byte(text(res)), &got); err != nil || len(got) != 1 || got[0].ThreadTS != "5.0" {
		t.Fatalf("result %q: %v", text(res), err)
	}
}

func TestBranchToolValidation(t *testing.T) {
	br := &fakeBrancher{err: errors.New("boom")}
	h, tok := liveHostWith(t, &fakeCtrl{}, br, false)
	eleven := "[" + strings.TrimSuffix(strings.Repeat(`{"seed":"x"},`, MaxBranchTasks+1), ",") + "]"
	for _, tc := range []struct{ args, want string }{
		{`{"tasks":"x"}`, "invalid params:"},
		{`{"tasks":[]}`, "tasks must hold at least one task"},
		{`{"tasks":` + eleven + `}`, fmt.Sprintf("%d tasks is over the maximum of %d", MaxBranchTasks+1, MaxBranchTasks)},
		{`{"tasks":[{"title":"` + strings.Repeat("t", maxTitleRunes+1) + `"}]}`, "task 1: title is over"},
		{`{"tasks":[{"seed":"x"}]}`, "boom"},
	} {
		res := callTool(t, h, tok, ToolBranch, tc.args)
		if !isErr(res) || !strings.HasPrefix(text(res), tc.want) {
			t.Fatalf("args %s → %v, want %q", tc.args, res, tc.want)
		}
	}
}

func TestRelayHistoryOwnThread(t *testing.T) {
	api := &fakeAPI{replies: []slack.Message{mkMsg("9.9", "", "hello")}}
	r, logs := newRelay(t, api, RelayConfig{AllowedChannelIDs: map[string]struct{}{"C2": {}}})
	out, err := r.History(t.Context(), "C1/9.9", false, 0)
	if err != nil {
		t.Fatal(err)
	}
	// Own thread: the allowlist does not apply (C1 is not in it).
	if m := msgs(t, out); len(m) != 1 || m[0].Text != "hello" {
		t.Fatalf("out = %s", out)
	}
	if p := api.lastReplies; p.ChannelID != "C1" || p.Timestamp != "9.9" || p.Latest != "" {
		t.Fatalf("params = %+v", p)
	}
	if !strings.Contains(logs.joined(), "(this thread)") {
		t.Fatalf("logs = %s", logs.joined())
	}
}

func TestRelayHistoryOrigin(t *testing.T) {
	api := &fakeAPI{replies: []slack.Message{
		mkMsg("1.0", "", "parent"), mkMsg("1.5", "", "branch point"), mkMsg("1.7", "", "later"),
	}}
	r, logs := newRelay(t, api, RelayConfig{Origin: func(key string) (Origin, bool) {
		return Origin{ChannelID: "C1", ThreadTS: "1.0", BranchTS: "1.5"}, key == "C1/9.9"
	}})
	out, err := r.History(t.Context(), "C1/9.9", true, 500)
	if err != nil {
		t.Fatal(err)
	}
	m := msgs(t, out)
	if len(m) != 2 || m[1].Text != "branch point" {
		t.Fatalf("history must stop at the branch point: %s", out)
	}
	if p := api.lastReplies; p.Timestamp != "1.0" || p.Latest != "1.5" {
		t.Fatalf("params = %+v", p)
	}
	if !strings.Contains(logs.joined(), "(the origin thread)") {
		t.Fatalf("logs = %s", logs.joined())
	}
}

func TestRelayHistoryKeepsTheNewest(t *testing.T) {
	api := &fakeAPI{replies: []slack.Message{mkMsg("1.0", "", "a"), mkMsg("1.1", "", "b"), mkMsg("1.2", "", "c")}}
	r, _ := newRelay(t, api, RelayConfig{})
	out, _ := r.History(t.Context(), "C1/1.0", false, 2)
	if m := msgs(t, out); len(m) != 2 || m[0].Text != "b" || m[1].Text != "c" {
		t.Fatalf("out = %s", out)
	}
}

func TestReadThreadKeepsTheNewest(t *testing.T) {
	api := &fakeAPI{replies: []slack.Message{mkMsg("1.0", "", "a"), mkMsg("1.1", "", "b")}}
	r, _ := newRelay(t, api, RelayConfig{})
	out, _ := r.ReadThread(t.Context(), "k", "C1", "1.0", 1)
	if m := msgs(t, out); len(m) != 1 || m[0].Text != "b" {
		t.Fatalf("out = %s", out)
	}
}

func TestRelayHistoryFailures(t *testing.T) {
	ctx := t.Context()
	r, _ := newRelay(t, &fakeAPI{}, RelayConfig{})
	if _, err := r.History(ctx, "nokey", false, 0); err == nil {
		t.Fatal("a non-thread key must fail")
	}
	if _, err := r.History(ctx, "C1/9.9", true, 0); err == nil || !strings.Contains(err.Error(), "no origin") {
		t.Fatalf("err = %v", err)
	}
	r, _ = newRelay(t, &fakeAPI{}, RelayConfig{Origin: func(string) (Origin, bool) { return Origin{}, false }})
	if _, err := r.History(ctx, "C1/9.9", true, 0); err == nil {
		t.Fatal("unbranched thread has no origin")
	}
	r, _ = newRelay(t, &fakeAPI{repliesErr: errors.New("boom")}, RelayConfig{})
	if _, err := r.History(ctx, "C1/9.9", false, 0); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("err = %v", err)
	}
	r, _ = newRelay(t, &fakeAPI{}, RelayConfig{ReadsPerMinute: 1})
	_, _ = r.History(ctx, "C1/9.9", false, 0)
	if _, err := r.History(ctx, "C1/9.9", false, 0); err == nil || !strings.Contains(err.Error(), "rate cap") {
		t.Fatalf("err = %v", err)
	}
}
