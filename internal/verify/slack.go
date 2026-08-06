package verify

import (
	"context"
	"fmt"
	"os"

	"github.com/slack-go/slack"
)

// slackAPI adapts slack-go to the narrow Slack interface. It is a
// straight translation layer with no policy of its own — which token
// the client carries is decided by the caller, and is the only thing
// that distinguishes a "human" post from a "bot" post.
type slackAPI struct{ api *slack.Client }

// NewSlack wraps a token in the Slack interface. Pass an xoxb- token
// for the bot client and an xoxp- user token for the human client.
// The token is never logged, and never appears in argv — it is read
// from the environment by the caller.
//
// SLACK_API_BASE redirects the Web API, matching slackproto's
// behaviour, so the harness can be exercised against a fake server.
func NewSlack(token string) Slack {
	var opts []slack.Option
	if base := os.Getenv("SLACK_API_BASE"); base != "" {
		opts = append(opts, slack.OptionAPIURL(base))
	}
	return &slackAPI{api: slack.New(token, opts...)}
}

func (s *slackAPI) AuthTest(ctx context.Context) (string, error) {
	resp, err := s.api.AuthTestContext(ctx)
	if err != nil {
		return "", err
	}
	return resp.UserID, nil
}

func (s *slackAPI) Post(ctx context.Context, channel, threadTS, text string) (string, error) {
	opts := []slack.MsgOption{
		slack.MsgOptionText(text, false),
		slack.MsgOptionDisableLinkUnfurl(),
	}
	if threadTS != "" {
		opts = append(opts, slack.MsgOptionTS(threadTS))
	}
	_, ts, err := s.api.PostMessageContext(ctx, channel, opts...)
	if err != nil {
		return "", fmt.Errorf("chat.postMessage: %w", err)
	}
	return ts, nil
}

func (s *slackAPI) Update(ctx context.Context, channel, ts, text string) error {
	if _, _, _, err := s.api.UpdateMessageContext(ctx, channel, ts,
		slack.MsgOptionText(text, false),
		slack.MsgOptionDisableLinkUnfurl(),
	); err != nil {
		return fmt.Errorf("chat.update: %w", err)
	}
	return nil
}

func (s *slackAPI) Delete(ctx context.Context, channel, ts string) error {
	if _, _, err := s.api.DeleteMessageContext(ctx, channel, ts); err != nil {
		return fmt.Errorf("chat.delete: %w", err)
	}
	return nil
}

func (s *slackAPI) Replies(ctx context.Context, channel, threadTS string) ([]Message, error) {
	msgs, _, _, err := s.api.GetConversationRepliesContext(ctx, &slack.GetConversationRepliesParameters{
		ChannelID: channel,
		Timestamp: threadTS,
		Limit:     200,
	})
	if err != nil {
		return nil, fmt.Errorf("conversations.replies: %w", err)
	}
	out := make([]Message, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, Message{
			TS:      m.Timestamp,
			User:    m.User,
			BotID:   m.BotID,
			SubType: m.SubType,
			Text:    m.Text,
		})
	}
	return out, nil
}

func (s *slackAPI) OpenDM(ctx context.Context, userID string) (string, error) {
	ch, _, _, err := s.api.OpenConversationContext(ctx, &slack.OpenConversationParameters{
		Users: []string{userID},
	})
	if err != nil {
		return "", fmt.Errorf("conversations.open: %w", err)
	}
	return ch.ID, nil
}
