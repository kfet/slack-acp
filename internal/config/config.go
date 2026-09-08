// Package config loads the slack-acp JSON config file.
package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
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

	// AllowedUserIDs, if non-empty, restricts who can talk to the bot.
	AllowedUserIDs []string `json:"allowed_user_ids,omitempty"`
	// AllowedChannelIDs, if non-empty, restricts where the bot will respond.
	AllowedChannelIDs []string `json:"allowed_channel_ids,omitempty"`

	// SessionIdleTimeoutSeconds: GC sessions idle this long. 0 = default 30m.
	SessionIdleTimeoutSeconds int `json:"session_idle_timeout_seconds,omitempty"`

	// NoProgressTimeoutSeconds cuts a turn that has gone silent: no
	// agent output and no tool activity for this long. Tool calls count
	// as progress, so a legitimately long tool is never cut. 0 = 2
	// minutes.
	NoProgressTimeoutSeconds int `json:"no_progress_timeout_seconds,omitempty"`
	// PromptTimeoutSeconds is an OPT-IN absolute ceiling on one agent
	// turn, enforced regardless of progress. 0 = NO ceiling.
	//
	// The turn bound used to be a fixed, unconfigurable 10-minute
	// wall-clock cap, which punished exactly the turns working hardest.
	// The guard that fires now is NoProgressTimeoutSeconds; this key
	// exists for operators who deliberately want a hard upper bound.
	PromptTimeoutSeconds int `json:"prompt_timeout_seconds,omitempty"`

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

	// Ambient enables forwarding of non-DM thread replies to threads
	// the bot is already part of (summoned via @-mention). When false,
	// only @-mentions and DMs trigger responses. Default: false.
	Ambient bool `json:"ambient,omitempty"`

	// Backfill enables catching up on missed messages via
	// conversations.replies when a gap is detected (bot was offline or
	// restarted). Requires Ambient to be enabled. Default: false.
	Backfill bool `json:"backfill,omitempty"`

	// BackfillMaxMessages caps how many historical messages to fetch
	// when backfilling a detected gap. Default: 50.
	BackfillMaxMessages int `json:"backfill_max_messages,omitempty"`

	// SilentSentinel is the exact output string that signals the agent
	// has chosen not to reply. The relay suppresses posting when the
	// full streamed response equals this sentinel. Default: "<<SILENT>>"
	SilentSentinel string `json:"silent_sentinel,omitempty"`

	// HideThinking suppresses agent_thought_chunk output (the italic
	// one-liners) from the posted Slack message, mirroring poe-acp's
	// hide_thinking. Default false (thoughts are shown).
	HideThinking bool `json:"hide_thinking,omitempty"`

	// SelfDriveSentinel opts into the self-drive escape hatch: a
	// bot-authored message whose text *begins* with this exact token is
	// accepted (with the token stripped) instead of being dropped as
	// the relay's own output.
	//
	// This deliberately reopens the bot-message boundary. Leave it
	// empty in production — empty is the default and means OFF.
	SelfDriveSentinel string `json:"self_drive_sentinel,omitempty"`

	// SelfDrivePerMinute caps how many hatch-accepted messages the
	// relay will act on per minute. Only meaningful when
	// SelfDriveSentinel is set. Default 4.
	SelfDrivePerMinute int `json:"self_drive_per_minute,omitempty"`

	// ModelProbeBudgetSeconds bounds the total time the startup model
	// probe may spend retrying a not-yet-ready agent. Agents that block
	// on external readiness (e.g. `fir --mode acp --wait-mcp` waiting
	// for every MCP server) can take minutes to answer, so the probe
	// retries with backoff inside this budget instead of sampling once.
	// The probe is best-effort — exhausting the budget costs only the
	// provider emoji. 0 = default 300s.
	ModelProbeBudgetSeconds int `json:"model_probe_budget_seconds,omitempty"`

	// HumanAuthorUserIDs names Slack users whose messages count as
	// human-authored even though Slack marks them with a bot_id.
	//
	// Why this exists: Slack stamps the posting app's identity
	// (app_id, bot_id, bot_profile) onto EVERY message sent through
	// chat.postMessage — including one sent with a user (xoxp-) token
	// on behalf of a real person. There is no way for an API caller to
	// produce a message Slack presents as "typed in a client". The
	// ingest guards use bot_id as a proxy for "not a human", so
	// without this list an API-authored message from a real person is
	// indistinguishable from a bot post and is dropped.
	//
	// This does NOT open the self-loop: a message whose author is our
	// own bot user (or has no author at all) is refused
	// unconditionally, before this list is consulted, with no
	// override. The list only narrows the bot_id *proxy*, and only for
	// user ids an operator has explicitly written down.
	//
	// Empty (the default) is byte-for-byte the previous behaviour, and
	// is the correct production setting. Its only intended use is
	// `slack-acp verify`, which posts as a named human. See
	// docs/self-verification.md.
	HumanAuthorUserIDs []string `json:"human_author_user_ids,omitempty"`

	// HumanAuthorPerMinute caps how often the reclassification above
	// may fire — the loop backstop of last resort, mirroring
	// self_drive_per_minute. Ignored unless HumanAuthorUserIDs is set.
	// 0 = default 12.
	HumanAuthorPerMinute int `json:"human_author_per_minute,omitempty"`
}

// Load reads and validates the config file.
func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(b))
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
	if c.SessionIdleTimeoutSeconds < 0 {
		return fmt.Errorf("session_idle_timeout_seconds must be >= 0")
	}
	if c.NoProgressTimeoutSeconds < 0 {
		return fmt.Errorf("no_progress_timeout_seconds must be >= 0")
	}
	if c.PromptTimeoutSeconds < 0 {
		return fmt.Errorf("prompt_timeout_seconds must be >= 0")
	}
	if c.BackfillMaxMessages < 0 {
		return fmt.Errorf("backfill_max_messages must be >= 0")
	}
	if c.ModelProbeBudgetSeconds < 0 {
		return fmt.Errorf("model_probe_budget_seconds must be >= 0")
	}
	if c.SelfDriveSentinel != "" {
		// Short tokens are the dangerous case: the hatch reopens the
		// bot-message boundary, so an accidental or guessable prefix
		// is a live self-trigger.
		if len(c.SelfDriveSentinel) < minSelfDriveSentinel {
			return fmt.Errorf("self_drive_sentinel must be at least %d characters (got %d) — short tokens are too easy to trigger by accident", minSelfDriveSentinel, len(c.SelfDriveSentinel))
		}
		// Slack HTML-escapes <, > and & in inbound message text, so a
		// sentinel containing any of them arrives escaped
		// (<<DRIVE-TEST>> becomes &lt;&lt;DRIVE-TEST&gt;&gt;) and the
		// prefix match can never fire. The hatch would silently do
		// nothing, with no error at any layer — so refuse the token
		// here rather than let an operator debug a no-op.
		//
		// Note this rule is specific to self_drive_sentinel, which is
		// matched against *inbound* text. silent_sentinel is compared
		// against agent *output* and is unaffected; its default
		// (<<SILENT>>) stays valid.
		if i := strings.IndexAny(c.SelfDriveSentinel, "<>&"); i >= 0 {
			return fmt.Errorf("self_drive_sentinel must not contain < > or & (found %q) — Slack HTML-escapes those in inbound message text, so the token would arrive escaped (e.g. \"<<X>>\" as \"&lt;&lt;X&gt;&gt;\") and could never match; use a token like \"drive-me-9f3a\"", c.SelfDriveSentinel[i])
		}
		if c.SelfDriveSentinel == c.GetSilentSentinel() {
			return fmt.Errorf("self_drive_sentinel must differ from silent_sentinel (both %q) — one means 'drive me', the other 'stay quiet'", c.SelfDriveSentinel)
		}
		if c.SelfDrivePerMinute < 0 {
			return fmt.Errorf("self_drive_per_minute must be >= 1 when self_drive_sentinel is set (0 or omitted uses the default of 4)")
		}
	}
	if c.HumanAuthorPerMinute < 0 {
		return fmt.Errorf("human_author_per_minute must be >= 1 (0 or omitted uses the default of 12)")
	}
	for _, id := range c.HumanAuthorUserIDs {
		if strings.TrimSpace(id) == "" {
			return fmt.Errorf("human_author_user_ids must not contain empty entries — an empty id would match every author-less message (webhooks, classic bots), which is exactly what the guard exists to refuse")
		}
	}
	return nil
}

// minSelfDriveSentinel is the shortest accepted self-drive token.
const minSelfDriveSentinel = 8

// GetSelfDrivePerMinute returns the configured hatch rate cap or the
// default.
func (c *Config) GetSelfDrivePerMinute() int {
	if c.SelfDrivePerMinute <= 0 {
		return 4
	}
	return c.SelfDrivePerMinute
}

// GetSilentSentinel returns the configured sentinel or the default.
func (c *Config) GetSilentSentinel() string {
	if c.SilentSentinel == "" {
		return "<<SILENT>>"
	}
	return c.SilentSentinel
}

// GetBackfillMaxMessages returns the configured limit or the default.
func (c *Config) GetBackfillMaxMessages() int {
	if c.BackfillMaxMessages <= 0 {
		return 50
	}
	return c.BackfillMaxMessages
}

// IdleTimeout returns the configured router idle timeout. Zero means use
// router.New's default.
func (c *Config) IdleTimeout() time.Duration {
	if c.SessionIdleTimeoutSeconds <= 0 {
		return 0
	}
	return time.Duration(c.SessionIdleTimeoutSeconds) * time.Second
}

// NoProgressTimeout returns the per-turn no-progress window. Zero means
// use handler.New's default.
func (c *Config) NoProgressTimeout() time.Duration {
	if c.NoProgressTimeoutSeconds <= 0 {
		return 0
	}
	return time.Duration(c.NoProgressTimeoutSeconds) * time.Second
}

// TurnCeiling returns the OPT-IN absolute per-turn cap. 0 means none.
func (c *Config) TurnCeiling() time.Duration {
	if c.PromptTimeoutSeconds <= 0 {
		return 0
	}
	return time.Duration(c.PromptTimeoutSeconds) * time.Second
}

// ModelProbeBudget returns the configured startup model-probe budget.
// Zero means use probe.Models' default.
func (c *Config) ModelProbeBudget() time.Duration {
	if c.ModelProbeBudgetSeconds <= 0 {
		return 0
	}
	return time.Duration(c.ModelProbeBudgetSeconds) * time.Second
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

// DefaultConfigDir is the operator's config root for slack-acp.
//
// Order: $XDG_CONFIG_HOME/slack-acp → $HOME/.config/slack-acp →
// $TMPDIR/slack-acp.
func DefaultConfigDir() string {
	if d := os.Getenv("XDG_CONFIG_HOME"); d != "" {
		return filepath.Join(d, "slack-acp")
	}
	if h, err := os.UserHomeDir(); err == nil && h != "" {
		return filepath.Join(h, ".config", "slack-acp")
	}
	return filepath.Join(os.TempDir(), "slack-acp")
}

// DefaultConfigPath is the conventional location for config.json.
func DefaultConfigPath() string { return filepath.Join(DefaultConfigDir(), "config.json") }

// DefaultEnvPath is the conventional location for the env file used
// by supervisor units (systemd EnvironmentFile, launchd wrapper).
func DefaultEnvPath() string { return filepath.Join(DefaultConfigDir(), "env") }
