package initcmd

import (
	"context"
	"fmt"
	"os"

	"github.com/slack-go/slack"
)

// DefaultVerifier calls Slack's auth.test with the bot token and
// returns a human-readable identity string on success. The app-level
// token is currently only shape-checked (a full Socket Mode handshake
// would add complexity for little gain — operators who fat-finger the
// app token find out within seconds of `slack-acp` starting).
//
// Honours SLACK_API_BASE for parity with internal/slackproto so the
// e2e harness's mock Slack works with `init` too.
func DefaultVerifier(ctx context.Context, botToken, appToken string) (string, error) {
	opts := []slack.Option{slack.OptionAppLevelToken(appToken)}
	if base := os.Getenv("SLACK_API_BASE"); base != "" {
		opts = append(opts, slack.OptionAPIURL(base))
	}
	api := slack.New(botToken, opts...)
	resp, err := api.AuthTestContext(ctx)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("team %s user %s", resp.TeamID, resp.UserID), nil
}
