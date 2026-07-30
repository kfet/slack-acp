package slackproto

import (
	"encoding/json"
	"os"
	"path/filepath"
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
			Bot []string `json:"bot"`
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
		"groups:history",   // private channels
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
