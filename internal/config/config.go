// Package config loads the slack-acp JSON config file.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// Config is the operator-facing JSON config.
type Config struct {
	// Slack tokens. Bot token starts with xoxb-, app token (Socket Mode) with xapp-.
	BotToken string `json:"bot_token,omitempty"`
	AppToken string `json:"app_token,omitempty"`

	// AgentCmd is the argv used to spawn the ACP agent (default: ["fir","--mode","acp"]).
	AgentCmd []string `json:"agent_cmd,omitempty"`

	// StateDir is the root under which per-thread state lives. Each Slack
	// thread gets a stable cwd at <StateDir>/threads/<channel>/<thread_ts>
	// so agent state (e.g. .fir/) persists across restarts and idle GC,
	// allowing future session resumption. Defaults to
	// $XDG_STATE_HOME/slack-acp (or ~/.local/state/slack-acp).
	StateDir string `json:"state_dir,omitempty"`

	// Policy is one of allow-all|read-only|deny-all (default allow-all).
	Policy string `json:"policy,omitempty"`

	// AllowedUserIDs, if non-empty, restricts who can talk to the bot.
	AllowedUserIDs []string `json:"allowed_user_ids,omitempty"`
	// AllowedChannelIDs, if non-empty, restricts where the bot will respond.
	AllowedChannelIDs []string `json:"allowed_channel_ids,omitempty"`

	// SessionIdleTimeoutSeconds: GC sessions idle this long. 0 = default 30m.
	SessionIdleTimeoutSeconds int `json:"session_idle_timeout_seconds,omitempty"`

	// SystemPrompt, if non-empty, is appended to the built-in Slack-
	// formatting instructions and injected into every ACP session as a
	// durable system prompt. Use for operator-specific guidance ("you
	// are the @ops bot, …"). Leave empty to use only the built-in
	// Slack-formatting block.
	SystemPrompt string `json:"system_prompt,omitempty"`
	// DisableSystemPrompt skips system-prompt injection entirely
	// (including the built-in Slack-formatting block). Use only if you
	// have a reason to want raw, unguided agent output in Slack.
	DisableSystemPrompt bool `json:"disable_system_prompt,omitempty"`
}

// Load reads and validates the config file.
func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	var c Config
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	return &c, c.Validate()
}

// Validate checks required fields. Tokens may be supplied via env at runtime
// instead of config, so they're not required here.
func (c *Config) Validate() error {
	if c.Policy != "" {
		switch strings.ToLower(c.Policy) {
		case "allow-all", "allow", "read-only", "readonly", "deny-all", "deny":
		default:
			return fmt.Errorf("invalid policy %q", c.Policy)
		}
	}
	return nil
}

// ValidateTokens returns a multi-line, operator-friendly error when bot
// or app tokens are missing or have the wrong shape. Slack bot tokens
// start with "xoxb-" (issued on Install App → Install to Workspace);
// app-level tokens start with "xapp-" (Basic Information → App-Level
// Tokens → Generate with the connections:write scope). The shape
// check is cheap and lets operators catch a swapped-pair mistake
// before a real network round-trip.
func ValidateTokens(botToken, appToken string) error {
	switch {
	case botToken == "" && appToken == "":
		return fmt.Errorf("missing Slack tokens.\n" +
			"  • Bot token (xoxb-…): api.slack.com/apps → your app → Install App → Install to Workspace.\n" +
			"  • App-level token (xapp-…): same app → Basic Information → App-Level Tokens → Generate with scope connections:write.\n" +
			"  Set them in config (bot_token / app_token) or via env (SLACK_BOT_TOKEN / SLACK_APP_TOKEN).")
	case botToken == "":
		return fmt.Errorf("missing bot token (xoxb-…). Install App → Install to Workspace; set bot_token or SLACK_BOT_TOKEN")
	case appToken == "":
		return fmt.Errorf("missing app-level token (xapp-…). Basic Information → App-Level Tokens → Generate with connections:write; set app_token or SLACK_APP_TOKEN")
	case !strings.HasPrefix(botToken, "xoxb-"):
		return fmt.Errorf("bot token must start with %q (got %q…); make sure you didn't swap it with the xapp- app-level token", "xoxb-", truncatePrefix(botToken))
	case !strings.HasPrefix(appToken, "xapp-"):
		return fmt.Errorf("app token must start with %q (got %q…); make sure you didn't swap it with the xoxb- bot token", "xapp-", truncatePrefix(appToken))
	}
	return nil
}

// truncatePrefix returns the first few non-empty chars of a token so
// error messages can hint at what was actually supplied without
// leaking the whole secret to logs.
func truncatePrefix(tok string) string {
	const n = 6
	if len(tok) <= n {
		return tok
	}
	return tok[:n]
}
