package slackproto

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The Slack app manifest is data, not code, so nothing compiled it and
// nothing caught it drifting away from the live installed app. It did
// drift: the file subscribed only to public channels while the deployed
// app had been granted groups:history by hand, so private-channel
// ambient mode was inert for anyone installing from this file.
//
// These tests pin the manifest against what the relay actually needs.
// Add to them whenever a new Slack API call or event source is
// introduced — an operator has to REINSTALL the app for changed
// subscriptions or scopes to take effect, so silent drift here surfaces
// in production as missing events or missing_scope errors.

type appManifest struct {
	Features struct {
		AppHome struct {
			MessagesTabEnabled bool `json:"messages_tab_enabled"`
		} `json:"app_home"`
	} `json:"features"`
	OAuthConfig struct {
		Scopes struct {
			Bot  []string `json:"bot"`
			User []string `json:"user"`
		} `json:"scopes"`
	} `json:"oauth_config"`
	Settings struct {
		EventSubscriptions struct {
			BotEvents []string `json:"bot_events"`
		} `json:"event_subscriptions"`
		SocketModeEnabled bool `json:"socket_mode_enabled"`
	} `json:"settings"`
}

func loadManifest(t *testing.T) appManifest {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "docs", "slack-app-manifest.json"))
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	var m appManifest
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("manifest is not valid JSON: %v", err)
	}
	return m
}

func TestManifestSubscribesEveryMessageSource(t *testing.T) {
	m := loadManifest(t)
	// message.groups is the private-channel counterpart of
	// message.channels. Without it the relay receives nothing
	// un-mentioned in a private channel and ambient mode is silently
	// inert there.
	want := []string{"app_mention", "message.channels", "message.groups", "message.im"}
	for _, ev := range want {
		if !contains(m.Settings.EventSubscriptions.BotEvents, ev) {
			t.Errorf("manifest bot_events missing %q", ev)
		}
	}
}

func TestManifestGrantsHistoryScopeForEveryChannelType(t *testing.T) {
	m := loadManifest(t)
	// Each message.* subscription needs its matching history scope, or
	// the events never arrive (and conversations.replies backfill fails
	// with missing_scope).
	want := []string{
		"app_mentions:read",
		"channels:history", // public channels
		"channels:read",    // slack_list_channels (users.conversations), public
		"groups:history",   // private channels
		"groups:read",      // slack_list_channels (users.conversations), private
		"im:history",       // DMs
		"im:read",
		"im:write",
		"chat:write", // posting and streaming edits
		"users:read", // display names in backfill
	}
	for _, scope := range want {
		if !contains(m.OAuthConfig.Scopes.Bot, scope) {
			t.Errorf("manifest bot scopes missing %q", scope)
		}
	}
}

// TestManifestGrantsUserScopeForSelfVerification pins the user-token
// scope. `slack-acp verify` posts its human-authored checks with an
// xoxp- user token, because that is the ONLY way to produce a message
// Slack considers human — and therefore the only way to exercise the
// app_mention guard, which drops every bot-authored mention with no
// exception. Without this scope the app_mention checks cannot run at
// all and the harness reports SKIP.
//
// Adding a user scope requires the operator to REINSTALL the app,
// authenticated as the identity the harness will post as.
func TestManifestGrantsUserScopeForSelfVerification(t *testing.T) {
	m := loadManifest(t)
	if !contains(m.OAuthConfig.Scopes.User, "chat:write") {
		t.Error("manifest user scopes missing \"chat:write\" — slack-acp verify cannot post as a human without it")
	}
}

// The relay-hosted `slack` MCP server (internal/slackmcp) lets the agent
// post as the bot in read_write mode. The only thing stopping it posting
// into a channel nobody invited the bot to is the ABSENCE of
// chat:write.public — Slack rejects the call. Adding that scope would
// silently widen the agent's reach to the whole workspace, so pin it out.
func TestManifestWithholdsChatWritePublic(t *testing.T) {
	m := loadManifest(t)
	if contains(m.OAuthConfig.Scopes.Bot, "chat:write.public") {
		t.Error("chat:write.public must stay OFF — bot-membership is what bounds agent-initiated posts")
	}
	// Same reasoning for search: search:read is user-token-only, and the
	// BOT token — the only credential internal/slackmcp's Slack client
	// ever holds — cannot carry it. (`slack-acp verify` does use an
	// xoxp- user token, but it is read straight from the environment by
	// the verify subcommand and is never handed to the relay's client,
	// nor to the agent: see internal/config/agentenv.go, which scrubs
	// SLACK_USER_TOKEN from the agent's environment by name and by
	// value.)
	for _, s := range m.OAuthConfig.Scopes.Bot {
		if strings.HasPrefix(s, "search:") {
			t.Errorf("search scope %q requires a user token (xoxp-); the bot token must not carry one", s)
		}
	}
}

func TestManifestKeepsSocketModeAndDMComposeBox(t *testing.T) {
	m := loadManifest(t)
	if !m.Settings.SocketModeEnabled {
		t.Error("socket_mode_enabled must stay true — the relay has no HTTP endpoint")
	}
	// Without the messages tab, users land on a DM with no input field.
	if !m.Features.AppHome.MessagesTabEnabled {
		t.Error("messages_tab_enabled must stay true or DMs have no compose box")
	}
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
