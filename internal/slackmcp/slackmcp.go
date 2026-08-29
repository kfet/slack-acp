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
// Deliberately absent: message search. Slack's search.messages (and
// search.all / search.files) accept only USER tokens (xoxp-) via the
// legacy search:read scope; a bot token cannot search a workspace. We do
// not want a user token anywhere near this process, so there is no
// search tool.
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

// Controller is the relay-side implementation the tools drive. Every
// method receives the sessionKey resolved server-side from the
// connection token (never client-supplied), so implementations can log
// and attribute calls to the originating Slack thread. Each returns the
// text shown to the agent, or an error surfaced as an MCP tool error.
type Controller interface {
	ReadThread(ctx context.Context, sessionKey, channel, threadTS string, limit int) (string, error)
	ReadChannel(ctx context.Context, sessionKey, channel string, limit int, oldest string) (string, error)
	ListChannels(ctx context.Context, sessionKey string) (string, error)
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
