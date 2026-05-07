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
