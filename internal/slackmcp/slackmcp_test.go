package slackmcp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"reflect"
	"strings"
	"testing"

	"github.com/kfet/acp-kit/mcphost"
)

// fakeCtrl records what the tool layer decoded and hands back a canned
// result, so these tests pin argument decoding and validation without
// touching Slack.
type fakeCtrl struct {
	sessionKey string
	channel    string
	threadTS   string
	limit      int
	oldest     string
	text       string
	search     SearchParams
	called     string

	result string
	err    error
}

func (f *fakeCtrl) ReadThread(_ context.Context, sessionKey, channel, threadTS string, limit int) (string, error) {
	f.called, f.sessionKey, f.channel, f.threadTS, f.limit = ToolReadThread, sessionKey, channel, threadTS, limit
	return f.result, f.err
}

func (f *fakeCtrl) ReadChannel(_ context.Context, sessionKey, channel string, limit int, oldest string) (string, error) {
	f.called, f.sessionKey, f.channel, f.limit, f.oldest = ToolReadChannel, sessionKey, channel, limit, oldest
	return f.result, f.err
}

func (f *fakeCtrl) ListChannels(_ context.Context, sessionKey string) (string, error) {
	f.called, f.sessionKey = ToolListChannels, sessionKey
	return f.result, f.err
}

func (f *fakeCtrl) Search(_ context.Context, sessionKey string, p SearchParams) (string, error) {
	f.called, f.sessionKey, f.search, f.channel = ToolSearch, sessionKey, p, p.Channel
	return f.result, f.err
}

func (f *fakeCtrl) Post(_ context.Context, sessionKey, channel, threadTS, text string) (string, error) {
	f.called, f.sessionKey, f.channel, f.threadTS, f.text = ToolPost, sessionKey, channel, threadTS, text
	return f.result, f.err
}

func TestConfigGetters(t *testing.T) {
	hc := HostConfig()
	if hc.ServerName != "slack" || hc.ServerInfoName != "slack-acp" || hc.SocketName != "mcp.sock" {
		t.Fatalf("HostConfig = %+v", hc)
	}
	if hc.EnvSocket != EnvSocket || hc.EnvToken != EnvToken || hc.RedirSubcommand != Subcommand {
		t.Fatalf("HostConfig env/sub = %+v", hc)
	}
	if hc.DirPrefix != DirPrefix {
		t.Fatalf("DirPrefix = %q", hc.DirPrefix)
	}
	rc := RedirConfig()
	if rc.Subcommand != Subcommand || rc.EnvSocket != EnvSocket || rc.EnvToken != EnvToken {
		t.Fatalf("RedirConfig = %+v", rc)
	}
	// The env var names must not collide with the Slack credentials the
	// agent is deliberately denied.
	for _, n := range []string{EnvSocket, EnvToken} {
		if strings.HasPrefix(n, "SLACK_BOT") || strings.HasPrefix(n, "SLACK_APP") {
			t.Fatalf("env name %q shadows a Slack credential variable", n)
		}
	}
}

// liveHost registers the tools against a real host bound to a socket and
// returns the host plus a fresh token for session key "C1/9.9".
func liveHost(t *testing.T, ctrl Controller, allowPost bool) (*mcphost.Host, string) {
	t.Helper()
	cfg := HostConfig()
	cfg.BaseDir = t.TempDir()
	cfg.RedirCommand = "/bin/true"
	h, err := mcphost.New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { h.Close() })
	Register(h, ctrl, allowPost)
	tok := ""
	for _, e := range h.ServerConfigForSession("C1/9.9")[0].Stdio.Env {
		if e.Name == EnvToken {
			tok = e.Value
		}
	}
	if err := h.Listen(); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	return h, tok
}

// rpc drives one JSON-RPC request over the socket and returns the
// decoded result object.
func rpc(t *testing.T, h *mcphost.Host, tok, line string) map[string]any {
	t.Helper()
	c, err := net.Dial("unix", h.SocketPath())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()
	if _, err := c.Write([]byte(`{"token":"` + tok + `"}` + "\n")); err != nil {
		t.Fatalf("preamble: %v", err)
	}
	if _, err := c.Write([]byte(line + "\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	respLine, err := bufio.NewReader(c).ReadBytes('\n')
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(respLine, &m); err != nil {
		t.Fatalf("decode %q: %v", respLine, err)
	}
	res, ok := m["result"].(map[string]any)
	if !ok {
		t.Fatalf("no result in %q", respLine)
	}
	return res
}

func callTool(t *testing.T, h *mcphost.Host, tok, name, args string) map[string]any {
	t.Helper()
	return rpc(t, h, tok, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"`+name+`","arguments":`+args+`}}`)
}

func isErr(res map[string]any) bool {
	_, ok := res["isError"]
	return ok
}

func text(res map[string]any) string {
	return res["content"].([]any)[0].(map[string]any)["text"].(string)
}

func TestReadThreadTool(t *testing.T) {
	ctrl := &fakeCtrl{result: "[]"}
	h, tok := liveHost(t, ctrl, false)
	res := callTool(t, h, tok, ToolReadThread, `{"channel":"C9","thread_ts":"1.2","limit":7}`)
	if isErr(res) {
		t.Fatalf("unexpected error: %v", res)
	}
	if ctrl.called != ToolReadThread || ctrl.channel != "C9" || ctrl.threadTS != "1.2" || ctrl.limit != 7 {
		t.Fatalf("ctrl = %+v", ctrl)
	}
	// The session key is bound server-side from the token, never sent by
	// the agent — that is the anti-spoofing property.
	if ctrl.sessionKey != "C1/9.9" {
		t.Fatalf("sessionKey = %q, want the token-bound key", ctrl.sessionKey)
	}
}

func TestReadThreadToolValidation(t *testing.T) {
	h, tok := liveHost(t, &fakeCtrl{}, false)
	for _, tc := range []struct{ args, want string }{
		{`{"thread_ts":"1.2"}`, "channel is required"},
		{`{"channel":"C9"}`, "thread_ts is required"},
	} {
		res := callTool(t, h, tok, ToolReadThread, tc.args)
		if !isErr(res) || text(res) != tc.want {
			t.Fatalf("args %s → %v, want %q", tc.args, res, tc.want)
		}
	}
	res := callTool(t, h, tok, ToolReadThread, `{"channel":123}`)
	if !isErr(res) || !strings.HasPrefix(text(res), "invalid params:") {
		t.Fatalf("res = %v", res)
	}
}

func TestReadThreadToolControllerError(t *testing.T) {
	h, tok := liveHost(t, &fakeCtrl{err: errors.New("boom")}, false)
	res := callTool(t, h, tok, ToolReadThread, `{"channel":"C9","thread_ts":"1.2"}`)
	if !isErr(res) || text(res) != "boom" {
		t.Fatalf("res = %v", res)
	}
}

func TestReadChannelTool(t *testing.T) {
	ctrl := &fakeCtrl{result: "[]"}
	h, tok := liveHost(t, ctrl, false)
	res := callTool(t, h, tok, ToolReadChannel, `{"channel":"C9","limit":3,"oldest":"1.0"}`)
	if isErr(res) {
		t.Fatalf("unexpected error: %v", res)
	}
	if ctrl.called != ToolReadChannel || ctrl.limit != 3 || ctrl.oldest != "1.0" {
		t.Fatalf("ctrl = %+v", ctrl)
	}
}

func TestReadChannelToolValidation(t *testing.T) {
	h, tok := liveHost(t, &fakeCtrl{}, false)
	if res := callTool(t, h, tok, ToolReadChannel, `{}`); !isErr(res) || text(res) != "channel is required" {
		t.Fatalf("res = %v", res)
	}
	if res := callTool(t, h, tok, ToolReadChannel, `{"channel":[]}`); !isErr(res) || !strings.HasPrefix(text(res), "invalid params:") {
		t.Fatalf("res = %v", res)
	}
}

func TestListChannelsTool(t *testing.T) {
	ctrl := &fakeCtrl{result: "[]"}
	h, tok := liveHost(t, ctrl, false)
	res := callTool(t, h, tok, ToolListChannels, `{}`)
	if isErr(res) || ctrl.called != ToolListChannels || ctrl.sessionKey != "C1/9.9" {
		t.Fatalf("res = %v ctrl = %+v", res, ctrl)
	}
}

// The read/write split is the whole point of the config knob: in "read"
// mode slack_post must not exist, not merely refuse.
func TestPostToolAbsentWithoutAllowPost(t *testing.T) {
	h, tok := liveHost(t, &fakeCtrl{}, false)
	res := rpc(t, h, tok, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	names := map[string]bool{}
	for _, tl := range res["tools"].([]any) {
		names[tl.(map[string]any)["name"].(string)] = true
	}
	for _, want := range []string{ToolReadThread, ToolReadChannel, ToolListChannels, ToolSearch} {
		if !names[want] {
			t.Errorf("read mode is missing %q", want)
		}
	}
	if names[ToolPost] {
		t.Error("slack_post exposed in read mode")
	}
	if len(names) != 4 {
		t.Errorf("read mode exposes %d tools, want exactly 4: %v", len(names), names)
	}
}

func TestPostTool(t *testing.T) {
	ctrl := &fakeCtrl{result: "Posted to C9 (ts 1.5)."}
	h, tok := liveHost(t, ctrl, true)
	res := callTool(t, h, tok, ToolPost, `{"channel":"C9","thread_ts":"1.2","text":"hi"}`)
	if isErr(res) || text(res) != "Posted to C9 (ts 1.5)." {
		t.Fatalf("res = %v", res)
	}
	if ctrl.called != ToolPost || ctrl.channel != "C9" || ctrl.threadTS != "1.2" || ctrl.text != "hi" {
		t.Fatalf("ctrl = %+v", ctrl)
	}
}

func TestPostToolValidation(t *testing.T) {
	h, tok := liveHost(t, &fakeCtrl{}, true)
	for _, tc := range []struct{ args, want string }{
		{`{"text":"hi"}`, "channel is required"},
		{`{"channel":"C9"}`, "text is required"},
	} {
		res := callTool(t, h, tok, ToolPost, tc.args)
		if !isErr(res) || text(res) != tc.want {
			t.Fatalf("args %s → %v, want %q", tc.args, res, tc.want)
		}
	}
	if res := callTool(t, h, tok, ToolPost, `{"channel":{}}`); !isErr(res) || !strings.HasPrefix(text(res), "invalid params:") {
		t.Fatalf("res = %v", res)
	}
}

// slack_search exists, but it must never become Slack search. The
// invariant is about the API SURFACE, not the tool name: the relay's
// Slack client interface carries no search method, so no code path in
// this package can reach search.messages / search.all / search.files —
// and the app therefore still needs no user token and no search:read
// scope. Adding such a method is the thing that must trip this test.
func TestSearchToolCallsNoSearchAPI(t *testing.T) {
	h, tok := liveHost(t, &fakeCtrl{result: "{}"}, true)
	res := rpc(t, h, tok, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	found := false
	for _, tl := range res["tools"].([]any) {
		if tl.(map[string]any)["name"].(string) == ToolSearch {
			found = true
		}
	}
	if !found {
		t.Fatalf("%s is not registered", ToolSearch)
	}
	api := reflect.TypeOf((*API)(nil)).Elem()
	for i := range api.NumMethod() {
		if name := api.Method(i).Name; strings.Contains(name, "Search") {
			t.Fatalf("API method %q would reach a search.* Slack method, which needs a user token", name)
		}
	}
}

func TestSearchTool(t *testing.T) {
	ctrl := &fakeCtrl{result: "{}"}
	h, tok := liveHost(t, ctrl, false)
	res := callTool(t, h, tok, ToolSearch,
		`{"query":"deploy","channel":"C9","limit":5,"days":3,"include_threads":true}`)
	if isErr(res) || ctrl.called != ToolSearch {
		t.Fatalf("res = %v ctrl = %+v", res, ctrl)
	}
	want := SearchParams{Query: "deploy", Channel: "C9", Limit: 5, Days: 3, IncludeThreads: true}
	if ctrl.search != want {
		t.Fatalf("params = %+v, want %+v", ctrl.search, want)
	}
}

func TestSearchToolValidation(t *testing.T) {
	h, tok := liveHost(t, &fakeCtrl{}, false)
	if res := callTool(t, h, tok, ToolSearch, `{}`); !isErr(res) || text(res) != "query is required" {
		t.Fatalf("res = %v", res)
	}
	if res := callTool(t, h, tok, ToolSearch, `{"query":[]}`); !isErr(res) || !strings.HasPrefix(text(res), "invalid params:") {
		t.Fatalf("res = %v", res)
	}
}

// The tool description has to be honest about being a bounded scan: an
// agent that reads it as "workspace search" will report absence of
// results as absence of the message.
func TestSearchToolDescriptionIsHonest(t *testing.T) {
	h, tok := liveHost(t, &fakeCtrl{}, false)
	res := rpc(t, h, tok, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	for _, tl := range res["tools"].([]any) {
		m := tl.(map[string]any)
		if m["name"].(string) != ToolSearch {
			continue
		}
		desc := m["description"].(string)
		for _, want := range []string{"not", "index", "Absence of results is NOT evidence of absence"} {
			if !strings.Contains(strings.ToLower(desc), strings.ToLower(want)) {
				t.Errorf("description does not mention %q: %s", want, desc)
			}
		}
		return
	}
	t.Fatalf("%s not listed", ToolSearch)
}
