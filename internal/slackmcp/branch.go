// This file holds the two conversation-scoped tools: `history` and
// `branch`.
//
// Unlike the slack_* tools they take no channel or thread argument:
// the conversation is the caller's, resolved from the connection token,
// so they can never address another thread. `history` reads it (or,
// with origin=true, the thread it was branched out of, up to the branch
// point); `branch` spins new threads out of it. Branching itself is the
// handler's — the same code path as `!branch` and the :fork_and_knife:
// reaction — so the three entry points cannot drift.
package slackmcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/kfet/acp-kit/mcphost"
)

// Tool names of the conversation-scoped tools.
const (
	ToolHistory = "history"
	ToolBranch  = "branch"
)

// MaxBranchTasks caps one `branch` call. A sanity guard against a
// runaway loop, not a quota.
const MaxBranchTasks = 10

// maxTitleRunes bounds an agent-supplied branch title.
const maxTitleRunes = 80

// BranchTask is one branch the agent asks for.
type BranchTask struct {
	// Title heads the new thread's opening message. Empty means
	// "derive it from the seed", as `!branch` does.
	Title string `json:"title,omitempty"`
	// Seed is the new session's first message. Empty means "the text
	// of FromMsg", as the :fork_and_knife: reaction does.
	Seed string `json:"seed,omitempty"`
	// FromMsg is the branch point: the ts of a message in the CALLER's
	// thread. The child's history(origin=true) stops there. Empty means
	// the latest message not sent by the relay.
	FromMsg string `json:"from_msg,omitempty"`
}

// BranchResult is what one task produced. Error is set, and the rest
// empty, when that task failed; the other tasks of the call still ran.
type BranchResult struct {
	Channel  string `json:"channel,omitempty"`
	ThreadTS string `json:"thread_ts,omitempty"`
	Link     string `json:"link,omitempty"`
	Error    string `json:"error,omitempty"`
}

// Brancher performs branches for the `branch` tool. sessionKey is the
// caller's conversation, resolved server-side from the token.
type Brancher interface {
	BranchTasks(ctx context.Context, sessionKey string, tasks []BranchTask) ([]BranchResult, error)
}

// registerConversation registers `history` (always) and `branch` (when
// br is non-nil).
func registerConversation(h *mcphost.Host, ctrl Controller, br Brancher) {
	h.Tool(ToolHistory,
		"Read THIS conversation's Slack thread, oldest first. With origin=true it instead reads the thread "+
			"this one was branched out of, up to and including the branch point — never what was said there "+
			"afterwards. There is no way to address any other thread with this tool. Each message carries its "+
			"`ts`, which is what `branch` takes as `from_msg`.",
		map[string]any{
			"type": "object",
			"properties": map[string]any{
				"origin": map[string]any{"type": "boolean", "description": "Read the origin thread (up to the branch point) instead of this one. Fails when this thread was not branched."},
				"limit":  map[string]any{"type": "integer", "description": "Maximum messages to return (default 50, capped at 100)."},
			},
		},
		func(sessionKey string, args json.RawMessage) (string, error) {
			var a struct {
				Origin bool `json:"origin"`
				Limit  int  `json:"limit"`
			}
			if err := decode(args, &a); err != nil {
				return "", err
			}
			ctx, cancel := context.WithTimeout(context.Background(), CallTimeout)
			defer cancel()
			return ctrl.History(ctx, sessionKey, a.Origin, a.Limit)
		},
	)
	if br == nil {
		return
	}
	h.Tool(ToolBranch,
		"Spin work out of THIS conversation into new Slack threads, one per task, in the same channel. The relay "+
			"posts a new top-level message for each (linking back here), and each new thread gets its own agent "+
			"session — a fork of this one when the agent supports it — which starts working on its seed at once "+
			"and can read this thread up to the branch point with history(origin=true). Use it to fan out "+
			"independent pieces of work. Per task: `seed` is the new session's first message (omit it to use the "+
			"text of `from_msg`); `title` heads the new thread (omit it to derive one from the seed); `from_msg` "+
			"is the ts of a message in THIS thread that is the branch point (omit it for the latest message not "+
			"sent by the relay). Get ts values from `"+ToolHistory+"`. A `from_msg` from another thread is refused. "+
			fmt.Sprintf("At most %d tasks per call. ", MaxBranchTasks)+
			"The relay posts the links in this thread itself, so do not repeat them. Returns, per task, the new "+
			"thread's channel, thread_ts and permalink, or an error.",
		map[string]any{
			"type": "object",
			"properties": map[string]any{
				"tasks": map[string]any{
					"type":     "array",
					"minItems": 1,
					"maxItems": MaxBranchTasks,
					"items": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"title":    map[string]any{"type": "string", "description": "Heading of the new thread."},
							"seed":     map[string]any{"type": "string", "description": "The new session's first message."},
							"from_msg": map[string]any{"type": "string", "description": "Branch point: the ts of a message in this thread."},
						},
					},
				},
			},
			"required": []string{"tasks"},
		},
		func(sessionKey string, args json.RawMessage) (string, error) {
			var a struct {
				Tasks []BranchTask `json:"tasks"`
			}
			if err := decode(args, &a); err != nil {
				return "", err
			}
			if len(a.Tasks) == 0 {
				return "", errors.New("tasks must hold at least one task")
			}
			if len(a.Tasks) > MaxBranchTasks {
				return "", fmt.Errorf("%d tasks is over the maximum of %d per call — split them over several calls", len(a.Tasks), MaxBranchTasks)
			}
			for i := range a.Tasks {
				t := &a.Tasks[i]
				t.Title = strings.Join(strings.Fields(t.Title), " ")
				if len([]rune(t.Title)) > maxTitleRunes {
					return "", fmt.Errorf("task %d: title is over %d characters", i+1, maxTitleRunes)
				}
				t.Seed = strings.TrimSpace(t.Seed)
				t.FromMsg = strings.TrimSpace(t.FromMsg)
			}
			// Branching posts and starts turns; give it longer than a read.
			ctx, cancel := context.WithTimeout(context.Background(), 2*CallTimeout)
			defer cancel()
			res, err := br.BranchTasks(ctx, sessionKey, a.Tasks)
			if err != nil {
				return "", err
			}
			out, _ := json.Marshal(res) //nolint:errchkjson // struct of strings; cannot fail
			return string(out), nil
		},
	)
}
