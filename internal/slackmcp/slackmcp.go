// Package slackmcp wires slack-acp's relay-hosted Slack tools onto the
// generic acp-kit/mcphost Host, giving the spawned ACP agent
// cross-thread and cross-channel Slack reach WITHOUT ever handing it a
// Slack token.
//
// The shape mirrors poe-acp's poemcp: mcphost owns the transport (unix
// socket, per-session token auth, MCP JSON-RPC loop, stdio redirector);
// this package owns everything Slack-specific — the `slack` server
// identity, the tool names/descriptions/schemas, the env var names and
// redirector subcommand, and the glue to a Controller.
//
// Why mediated tools rather than the bot token: the agent is a
// general-purpose tool-using process driven, in ambient threads, by text
// from people who are not the operator. A token would let it post as the
// bot anywhere, read any channel the app can see, and re-scope its own
// reach, with no way to take that back. A tool call goes through the
// relay, which checks the channel allowlist, clamps the arguments, and
// logs the outcome. See internal/config/agentenv.go and
// docs/agent-slack-access.md.
//
// Search is deliberately NOT Slack search. Slack's search.messages (and
// search.all / search.files) accept only USER tokens (xoxp-) via the
// legacy search:read scope; a bot token cannot search a workspace, and
// we do not want a user token anywhere near this process. slack_search
// is therefore a bounded LOCAL fanout: conversations.history over the
// channels this session may already read, filtered relay-side. It calls
// no search.* method and adds no scope. See BACKLOG.md for the
// user-token variant and the allowlist-on-results problem that blocks
// it.
package slackmcp

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/kfet/acp-kit/mcphost"
)

// Tool names exposed to the agent by the relay-hosted `slack` MCP
// server.
const (
	ToolReadThread   = "slack_read_thread"
	ToolReadChannel  = "slack_read_channel"
	ToolListChannels = "slack_list_channels"
	ToolSearch       = "slack_search"
	ToolPost         = "slack_post"
)

// Env var names the relay sets on the spawned redirector (via the ACP
// McpServerStdio.Env), so no secrets land on the command line. The
// session key is intentionally NOT passed: mcphost derives it
// server-side from the token, so the agent cannot spoof which thread it
// is calling as.
const (
	EnvToken  = "SLACK_ACP_MCP_TOKEN"
	EnvSocket = "SLACK_ACP_MCP_SOCKET"
)

// Redirector subcommand and the server identity advertised to the agent.
const (
	Subcommand     = "mcp-serve"
	ServerName     = "slack"
	ServerInfoName = "slack-acp"
	SocketName     = "mcp.sock"
	DirPrefix      = "slack-acp-mcp-"
)

// CallTimeout bounds a single tool call's Slack API work. mcphost
// handlers carry no context of their own (the MCP connection outlives
// any one call), so each call gets a fresh bounded one rather than
// inheriting a request context that does not exist.
const CallTimeout = 30 * time.Second

// SearchParams carries slack_search's arguments. A struct rather than
// six positional parameters, because the tool is the one call with
// enough knobs that positional order would be a bug waiting to happen.
type SearchParams struct {
	// Query is the case-insensitive substring to look for.
	Query string
	// Channel, when set, restricts the scan to that one channel; empty
	// means every channel the session may read.
	Channel string
	// Limit caps returned matches (clamped relay-side).
	Limit int
	// Days bounds how far back the scan reaches (clamped relay-side).
	Days int
	// IncludeThreads additionally scans replies of threaded messages in
	// the scanned window, at one extra Slack call per thread.
	IncludeThreads bool
}

// Controller is the relay-side implementation the tools drive. Every
// method receives the sessionKey resolved server-side from the
// connection token (never client-supplied), so implementations can log
// and attribute calls to the originating Slack thread. Each returns the
// text shown to the agent, or an error surfaced as an MCP tool error.
type Controller interface {
	ReadThread(ctx context.Context, sessionKey, channel, threadTS string, limit int) (string, error)
	ReadChannel(ctx context.Context, sessionKey, channel string, limit int, oldest string) (string, error)
	ListChannels(ctx context.Context, sessionKey string) (string, error)
	Search(ctx context.Context, sessionKey string, p SearchParams) (string, error)
	Post(ctx context.Context, sessionKey, channel, threadTS, text string) (string, error)
}

// HostConfig returns the mcphost.Config for slack-acp's `slack` server.
func HostConfig() mcphost.Config {
	return mcphost.Config{
		DirPrefix:         DirPrefix,
		SocketName:        SocketName,
		RedirSubcommand:   Subcommand,
		ServerName:        ServerName,
		ServerInfoName:    ServerInfoName,
		ServerInfoVersion: "1",
		EnvSocket:         EnvSocket,
		EnvToken:          EnvToken,
	}
}

// RedirConfig returns the mcphost.RedirConfig used by main to intercept
// the redirector subcommand before normal startup.
func RedirConfig() mcphost.RedirConfig {
	return mcphost.RedirConfig{
		Subcommand: Subcommand,
		EnvSocket:  EnvSocket,
		EnvToken:   EnvToken,
	}
}

// Register registers the Slack tools on h, wiring them to ctrl. The
// read tools are always registered; slack_post is registered only when
// allowPost is true (config agent_slack_access = "read_write"), because
// it lets ambient thread text steer the bot into posting elsewhere.
func Register(h *mcphost.Host, ctrl Controller, allowPost bool) {
	h.Tool(ToolReadThread,
		"Read the messages of a Slack thread the bot can see. Use to pull context from "+
			"another thread — including one in a different channel — without leaving this conversation.",
		map[string]any{
			"type": "object",
			"properties": map[string]any{
				"channel":   map[string]any{"type": "string", "description": "Channel ID (e.g. C0123ABCD)."},
				"thread_ts": map[string]any{"type": "string", "description": "Timestamp of the thread's parent message (e.g. 1700000000.000100)."},
				"limit":     map[string]any{"type": "integer", "description": "Maximum messages to return (default 50, capped at 100)."},
			},
			"required": []string{"channel", "thread_ts"},
		},
		func(sessionKey string, args json.RawMessage) (string, error) {
			var a struct {
				Channel  string `json:"channel"`
				ThreadTS string `json:"thread_ts"`
				Limit    int    `json:"limit"`
			}
			if err := decode(args, &a); err != nil {
				return "", err
			}
			if a.Channel == "" {
				return "", errors.New("channel is required")
			}
			if a.ThreadTS == "" {
				return "", errors.New("thread_ts is required")
			}
			ctx, cancel := context.WithTimeout(context.Background(), CallTimeout)
			defer cancel()
			return ctrl.ReadThread(ctx, sessionKey, a.Channel, a.ThreadTS, a.Limit)
		},
	)

	h.Tool(ToolReadChannel,
		"Read recent messages from a Slack channel the bot is a member of.",
		map[string]any{
			"type": "object",
			"properties": map[string]any{
				"channel": map[string]any{"type": "string", "description": "Channel ID (e.g. C0123ABCD)."},
				"limit":   map[string]any{"type": "integer", "description": "Maximum messages to return (default 50, capped at 100)."},
				"oldest":  map[string]any{"type": "string", "description": "Optional Slack timestamp; only return messages after it."},
			},
			"required": []string{"channel"},
		},
		func(sessionKey string, args json.RawMessage) (string, error) {
			var a struct {
				Channel string `json:"channel"`
				Limit   int    `json:"limit"`
				Oldest  string `json:"oldest"`
			}
			if err := decode(args, &a); err != nil {
				return "", err
			}
			if a.Channel == "" {
				return "", errors.New("channel is required")
			}
			ctx, cancel := context.WithTimeout(context.Background(), CallTimeout)
			defer cancel()
			return ctrl.ReadChannel(ctx, sessionKey, a.Channel, a.Limit, a.Oldest)
		},
	)

	h.Tool(ToolListChannels,
		"List the Slack channels the bot is a member of, with their IDs and names. "+
			"Use to resolve a channel name to the ID the other tools take.",
		map[string]any{
			"type":       "object",
			"properties": map[string]any{},
		},
		func(sessionKey string, _ json.RawMessage) (string, error) {
			ctx, cancel := context.WithTimeout(context.Background(), CallTimeout)
			defer cancel()
			return ctrl.ListChannels(ctx, sessionKey)
		},
	)

	h.Tool(ToolSearch,
		"Search Slack messages by substring. IMPORTANT — this is NOT Slack's workspace search "+
			"and there is no index behind it: the relay fetches recent history from the channels this "+
			"bot may read and matches the text locally. It sees only those channels, only the last "+
			"`days` days, and only a bounded number of messages per channel, so a message you do not "+
			"find here may still exist. Absence of results is NOT evidence of absence. Narrow with "+
			"`channel` when you can; results may come back marked `truncated`.",
		map[string]any{
			"type": "object",
			"properties": map[string]any{
				"query":   map[string]any{"type": "string", "description": "Case-insensitive substring to match in message text."},
				"channel": map[string]any{"type": "string", "description": "Optional channel ID (e.g. C0123ABCD) to restrict the scan to. Default: every channel the bot may read."},
				"limit":   map[string]any{"type": "integer", "description": "Maximum matches to return (default 20, capped at 50)."},
				"days":    map[string]any{"type": "integer", "description": "How many days back to scan (default 7, capped at 30)."},
				"include_threads": map[string]any{
					"type":        "boolean",
					"description": "Also scan replies of threaded messages in the window. Costs one extra Slack call per thread and burns the shared read budget faster.",
				},
			},
			"required": []string{"query"},
		},
		func(sessionKey string, args json.RawMessage) (string, error) {
			var a struct {
				Query          string `json:"query"`
				Channel        string `json:"channel"`
				Limit          int    `json:"limit"`
				Days           int    `json:"days"`
				IncludeThreads bool   `json:"include_threads"`
			}
			if err := decode(args, &a); err != nil {
				return "", err
			}
			if a.Query == "" {
				return "", errors.New("query is required")
			}
			ctx, cancel := context.WithTimeout(context.Background(), CallTimeout)
			defer cancel()
			return ctrl.Search(ctx, sessionKey, SearchParams{
				Query:          a.Query,
				Channel:        a.Channel,
				Limit:          a.Limit,
				Days:           a.Days,
				IncludeThreads: a.IncludeThreads,
			})
		},
	)

	if !allowPost {
		return
	}
	h.Tool(ToolPost,
		"Post a message to a Slack channel or thread as the bot. Only for messages that "+
			"belong somewhere other than the conversation you are already replying in — your normal "+
			"reply is delivered automatically.",
		map[string]any{
			"type": "object",
			"properties": map[string]any{
				"channel":   map[string]any{"type": "string", "description": "Channel ID (e.g. C0123ABCD)."},
				"thread_ts": map[string]any{"type": "string", "description": "Optional: post as a reply in this thread instead of at channel top level."},
				"text":      map[string]any{"type": "string", "description": "Message text, in Slack mrkdwn."},
			},
			"required": []string{"channel", "text"},
		},
		func(sessionKey string, args json.RawMessage) (string, error) {
			var a struct {
				Channel  string `json:"channel"`
				ThreadTS string `json:"thread_ts"`
				Text     string `json:"text"`
			}
			if err := decode(args, &a); err != nil {
				return "", err
			}
			if a.Channel == "" {
				return "", errors.New("channel is required")
			}
			if a.Text == "" {
				return "", errors.New("text is required")
			}
			ctx, cancel := context.WithTimeout(context.Background(), CallTimeout)
			defer cancel()
			return ctrl.Post(ctx, sessionKey, a.Channel, a.ThreadTS, a.Text)
		},
	)
}

// decode unmarshals tool arguments, normalising the error into the
// "invalid params" shape the agent sees.
func decode(args json.RawMessage, v any) error {
	if err := json.Unmarshal(args, v); err != nil {
		return errors.New("invalid params: " + err.Error())
	}
	return nil
}
