package slackproto

import (
	"context"

	"github.com/slack-go/slack"
)

// RepliesAPI is the conversations.replies call. *slack.Client has it.
type RepliesAPI interface {
	GetConversationRepliesContext(ctx context.Context, params *slack.GetConversationRepliesParameters) ([]slack.Message, bool, string, error)
}

// Paging bounds for ThreadTail: Slack's page cap, and how many pages a
// single read may walk.
const (
	threadPageSize = 200
	maxThreadPages = 10
)

// ThreadTail returns the LAST n messages of a thread, oldest first.
// conversations.replies pages oldest-first, so one page of a long
// thread is its beginning — the opposite of what "read this thread"
// needs. It walks the cursor (bounded by maxThreadPages) and keeps the
// tail. p's Cursor and Limit are set here.
func ThreadTail(ctx context.Context, api RepliesAPI, p slack.GetConversationRepliesParameters, n int) ([]slack.Message, error) {
	var all []slack.Message
	p.Limit = threadPageSize
	for range maxThreadPages {
		msgs, more, cursor, err := api.GetConversationRepliesContext(ctx, &p)
		if err != nil {
			return nil, err
		}
		all = append(all, msgs...)
		if !more || cursor == "" {
			break
		}
		p.Cursor = cursor
	}
	if len(all) > n {
		all = all[len(all)-n:]
	}
	return all, nil
}
