// This file is the branch feature: open a new Slack thread that
// continues this conversation.
//
// A Slack conversation is a thread. A branch posts a NEW top-level
// message in the same channel — which opens a new thread — linking back
// to the branch point by permalink, and posts the new thread's link in
// the origin thread. The new thread's session is a fork of the origin's
// agent session (ACP session/fork) when the agent supports it, else a
// fresh one, and the child can read the origin up to the branch point
// with history(origin=true).
//
// Three entry points share performBranch so they cannot drift:
// `!branch <text>`, a :fork_and_knife: reaction on a message, and the
// agent's `branch` MCP tool.
package handler

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"

	"github.com/slack-go/slack"

	"github.com/kfet/acp-kit/client"
	"github.com/kfet/acp-kit/convo"
	kitlog "github.com/kfet/acp-kit/log"
	"github.com/kfet/slack-acp/internal/journal"
	"github.com/kfet/slack-acp/internal/router"
	"github.com/kfet/slack-acp/internal/slackmcp"
	"github.com/kfet/slack-acp/internal/slackproto"
)

// maxDerivedTitle bounds a title derived from the seed; maxQuotedSeed
// bounds the seed quoted in the new thread's opening message.
const (
	maxDerivedTitle = 60
	maxQuotedSeed   = 500
	// latestScan is how far back from a thread's end the tool looks for
	// the latest message from a person.
	latestScan = 200
	// maxBranchedMarks bounds the reaction dedup memory.
	maxBranchedMarks = 4096
)

// branchUsage answers a bare `!branch`.
const branchUsage = "Usage: `!branch <text>` — open a new thread in this channel that continues this conversation, starting with <text>."

// branchPlan is one branch to make.
type branchPlan struct {
	origin router.ConvKey
	// at is the branch point: the origin message the branch is cut at.
	at    string
	seed  string
	title string // empty: derived from seed
	// actor is the Slack user who asked, or "" for the agent's tool.
	actor string
	// announce posts the new thread's link in the origin thread. The
	// tool batches its links into one post instead.
	announce bool
}

// branchMade describes a branch that was opened.
type branchMade struct {
	child router.ConvKey
	title string
	link  string // permalink of the new thread, or "" if Slack refused one
}

// branchCommand is the `!branch` relay command.
func (h *Handler) branchCommand() convo.Command {
	return convo.Command{
		Match: isBranchCommand,
		Run: func(ctx context.Context, in *convo.In, arg string) {
			if arg == "" {
				_ = h.reply(ctx, in, branchUsage)
				return
			}
			ev := in.Meta.(slackproto.Event)
			h.background(func() {
				_, err := h.performBranch(ctx, branchPlan{
					origin:   router.ConvKey{ChannelID: ev.ChannelID, ThreadTS: ev.ThreadTS},
					at:       ev.TS,
					seed:     arg,
					actor:    ev.UserID,
					announce: true,
				})
				if err != nil {
					_ = h.reply(ctx, in, fmt.Sprintf("Could not branch: %v", err))
				}
			})
		},
		Help: []string{"`!branch <text>` — open a new thread in this channel that continues this conversation (its agent session is a fork of this one), starting with <text>. Reacting :fork_and_knife: to a message does the same with that message's text."},
	}
}

// isBranchCommand matches `!branch` and `!branch <text>`.
func isBranchCommand(text string) (string, bool) {
	rest, ok := strings.CutPrefix(strings.TrimSpace(text), "!branch")
	if !ok {
		return "", false
	}
	if rest != "" && rest[0] != ' ' && rest[0] != '\n' && rest[0] != '\t' {
		return "", false // `!branches`, `!branchy` …
	}
	return strings.TrimSpace(rest), true
}

// HandleReaction implements slackproto.ReactionHandler: a
// :fork_and_knife: on a message branches a new thread seeded with that
// message's text.
func (h *Handler) HandleReaction(ctx context.Context, r slackproto.Reaction) {
	h.background(func() { h.branchReaction(ctx, r) })
}

// background runs branch work off the Slack event loop — it makes
// several Web API calls and an ACP fork — while keeping it visible to
// WaitIdle.
func (h *Handler) background(fn func()) {
	h.bg.Add(1)
	go func() {
		defer h.bg.Done()
		fn()
	}()
}

func (h *Handler) branchReaction(ctx context.Context, r slackproto.Reaction) {
	rec := journal.Record{
		Stage:   journal.StageHandler,
		Path:    journal.PathReaction,
		Channel: r.ChannelID,
		TS:      r.TS,
		User:    r.UserID,
	}
	drop := func(reason string) {
		rec.Decision, rec.Reason = journal.DecisionDrop, reason
		journal.Log(rec)
	}
	if !h.allowed(slackproto.Event{UserID: r.UserID, ChannelID: r.ChannelID}) {
		drop(journal.ReasonAllowlist)
		return
	}
	mark := r.ChannelID + "/" + r.TS
	if !h.branched.add(mark) {
		drop(journal.ReasonBranchDuplicate)
		return
	}
	m, err := h.findMessage(ctx, r.ChannelID, r.TS)
	if err != nil {
		h.branched.remove(mark)
		kitlog.Debugf("handler: branch reaction on %s: %v", mark, err)
		drop(journal.ReasonBranchUnreadable)
		return
	}
	rec.ThreadTS = threadOf(m)
	if _, err := h.performBranch(ctx, branchPlan{
		origin:   router.ConvKey{ChannelID: r.ChannelID, ThreadTS: rec.ThreadTS},
		at:       r.TS,
		seed:     strings.TrimSpace(m.Text),
		actor:    r.UserID,
		announce: true,
	}); err != nil {
		h.branched.remove(mark)
		kitlog.Debugf("handler: branch reaction on %s: %v", mark, err)
		drop(journal.ReasonBranchFailed)
		return
	}
	rec.Decision, rec.Reason = journal.DecisionRun, journal.ReasonBranch
	journal.Log(rec)
}

// BranchTasks implements slackmcp.Brancher: the agent's `branch` tool.
func (h *Handler) BranchTasks(ctx context.Context, sessionKey string, tasks []slackmcp.BranchTask) ([]slackmcp.BranchResult, error) {
	key := parseKey(sessionKey)
	if key.ChannelID == "" || key.ThreadTS == "" {
		return nil, fmt.Errorf("session %q is not a Slack thread", sessionKey)
	}
	out := make([]slackmcp.BranchResult, len(tasks))
	var made []branchMade
	for i, t := range tasks {
		if !h.branchRate.Allow() {
			out[i].Error = "branch rate cap exceeded (agent_posts_per_minute); try again shortly"
			continue
		}
		b, err := h.branchTask(ctx, key, t)
		if err != nil {
			out[i].Error = err.Error()
			continue
		}
		made = append(made, b)
		out[i] = slackmcp.BranchResult{Channel: b.child.ChannelID, ThreadTS: b.child.ThreadTS, Link: b.link}
	}
	if len(made) > 0 {
		lines := make([]string, len(made))
		for i, b := range made {
			lines[i] = "• " + slackLink(b.link, b.title)
		}
		h.post(ctx, key.ChannelID, key.ThreadTS, ":fork_and_knife: Branched:\n"+strings.Join(lines, "\n"))
	}
	kitlog.Debugf("handler: branch tool made %d of %d in %s", len(made), len(tasks), sessionKey)
	return out, nil
}

// branchTask resolves one tool task's branch point and seed, then
// branches.
func (h *Handler) branchTask(ctx context.Context, key router.ConvKey, t slackmcp.BranchTask) (branchMade, error) {
	var m slack.Message
	var err error
	if t.FromMsg == "" {
		m, err = h.latestHumanMessage(ctx, key)
	} else {
		m, err = h.findMessage(ctx, key.ChannelID, t.FromMsg)
		if err == nil && threadOf(m) != key.ThreadTS {
			err = fmt.Errorf("from_msg %s is not a message in this thread", t.FromMsg)
		}
	}
	if err != nil {
		return branchMade{}, err
	}
	seed := t.Seed
	if seed == "" {
		seed = strings.TrimSpace(m.Text)
	}
	return h.performBranch(ctx, branchPlan{origin: key, at: m.Timestamp, seed: seed, title: t.Title})
}

// performBranch opens the branch: the new top-level message, the
// origin record, the session fork, the link back in the origin thread,
// and the child's first turn.
func (h *Handler) performBranch(ctx context.Context, p branchPlan) (branchMade, error) {
	// The opening message is the one post outside a reply that seed and
	// title — agent-controlled for the tool — reach: no channel pings.
	seed := slackproto.StripBroadcastPings(p.seed)
	title := slackproto.StripBroadcastPings(p.title)
	if title == "" {
		title = deriveTitle(seed)
	}
	if seed == "" || title == "" {
		return branchMade{}, errors.New("there is no text to start the new thread with")
	}
	originLink := h.permalink(ctx, p.origin.ChannelID, p.at)
	head := fmt.Sprintf(":fork_and_knife: *%s*\n_Branched from %s", title, slackLink(originLink, "this message"))
	if p.actor != "" {
		head += fmt.Sprintf(" by <@%s>", p.actor)
	}
	head += "._\n" + quote(truncateRunes(seed, maxQuotedSeed))
	ts, err := h.postErr(ctx, p.origin.ChannelID, "", head)
	if err != nil {
		return branchMade{}, fmt.Errorf("posting the new thread: %w", err)
	}
	// The thread exists now: finish wiring it even if the caller's
	// deadline (the tool's) runs out, or it is left without a session.
	ctx = context.WithoutCancel(ctx)
	child := router.ConvKey{ChannelID: p.origin.ChannelID, ThreadTS: ts}
	if err := h.cfg.Router.SetOrigin(child, router.Origin{ChannelID: p.origin.ChannelID, ThreadTS: p.origin.ThreadTS, BranchTS: p.at}); err != nil {
		kitlog.Debugf("handler: branch %s: recording origin: %v", child, err)
	}
	forked := h.forkBranch(ctx, p.origin, child, p.at)
	made := branchMade{child: child, title: title, link: h.permalink(ctx, child.ChannelID, child.ThreadTS)}
	if p.announce {
		h.post(ctx, p.origin.ChannelID, p.origin.ThreadTS, ":fork_and_knife: Branched to "+slackLink(made.link, title))
	}
	ev := slackproto.Event{UserID: p.actor, ChannelID: child.ChannelID, ThreadTS: ts, TS: ts, Text: p.seed, IsMention: true}
	prompt := branchPrompt(p.seed, originLink, forked)
	h.convo.Start(ctx, convo.Job{Conv: child.String(), Run: func(ctx context.Context, _ *convo.Turn) error {
		return h.run(ctx, ev, child, prompt)
	}})
	kitlog.Debugf("handler: branched %s from %s at %s (forked=%v)", child, p.origin, p.at, forked)
	return made, nil
}

// forkBranch forks the origin's session into child, reporting whether
// it did. Any failure is logged and the child opens a fresh session on
// its first turn.
func (h *Handler) forkBranch(ctx context.Context, origin, child router.ConvKey, at string) bool {
	sid, err := h.cfg.Router.Fork(ctx, origin, child, at)
	switch {
	case errors.Is(err, client.ErrForkUnsupported):
		log.Printf("handler: branch %s: agent cannot fork sessions (%v); starting a fresh session", child, err)
		return false
	case err != nil:
		log.Printf("handler: branch %s: fork failed (%v); starting a fresh session", child, err)
		return false
	}
	kitlog.Debugf("handler: branch %s: forked session %s from %s", child, sid, origin)
	return true
}

// branchPrompt is the child's first prompt.
func branchPrompt(seed, originLink string, forked bool) string {
	where := slackLink(originLink, "another thread")
	if forked {
		return fmt.Sprintf("(This is a new Slack thread branched from %s. Your session is a fork of that conversation, so you already have its context. Continue from here.)\n\n%s", where, seed)
	}
	return fmt.Sprintf("(This is a new Slack thread branched from %s. You start without its context; read it up to the branch point with the `history` tool and origin=true if you need it.)\n\n%s", where, seed)
}

// findMessage reads one message by ts. It works for top-level messages
// and thread replies alike.
func (h *Handler) findMessage(ctx context.Context, channel, ts string) (slack.Message, error) {
	// Slack always puts the thread's parent first, whatever the bounds,
	// so a reply needs room for two.
	msgs, _, _, err := h.cfg.API.GetConversationRepliesContext(ctx, &slack.GetConversationRepliesParameters{
		ChannelID: channel, Timestamp: ts, Oldest: ts, Latest: ts, Inclusive: true, Limit: 2,
	})
	if err != nil {
		return slack.Message{}, fmt.Errorf("reading message %s: %w", ts, err)
	}
	for _, m := range msgs {
		if m.Timestamp == ts {
			return m, nil
		}
	}
	return slack.Message{}, fmt.Errorf("message %s not found", ts)
}

// latestHumanMessage is the newest message in key's thread not sent by
// an app (the relay included).
func (h *Handler) latestHumanMessage(ctx context.Context, key router.ConvKey) (slack.Message, error) {
	msgs, err := slackproto.ThreadTail(ctx, h.cfg.API, slack.GetConversationRepliesParameters{
		ChannelID: key.ChannelID, Timestamp: key.ThreadTS, Inclusive: true,
	}, latestScan)
	if err != nil {
		return slack.Message{}, fmt.Errorf("reading this thread: %w", err)
	}
	for i := len(msgs) - 1; i >= 0; i-- {
		if m := msgs[i]; m.BotID == "" && m.User != "" && strings.TrimSpace(m.Text) != "" {
			return m, nil
		}
	}
	return slack.Message{}, errors.New("this thread has no message from a person to branch at; pass from_msg")
}

// threadOf is the thread a message belongs to: its thread_ts, or its
// own ts for an unthreaded top-level message.
func threadOf(m slack.Message) string { return firstNonEmpty(m.ThreadTimestamp, m.Timestamp) }

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// permalink is a message's permalink, or "" (logged) when Slack refuses.
func (h *Handler) permalink(ctx context.Context, channel, ts string) string {
	link, err := h.cfg.API.GetPermalinkContext(ctx, &slack.PermalinkParameters{Channel: channel, Ts: ts})
	if err != nil {
		kitlog.Debugf("handler: permalink %s/%s: %v", channel, ts, err)
		return ""
	}
	return link
}

// post sends one relay message, logging a failure.
func (h *Handler) post(ctx context.Context, channel, threadTS, text string) {
	if _, err := h.postErr(ctx, channel, threadTS, text); err != nil {
		kitlog.Debugf("handler: post in %s/%s: %v", channel, threadTS, err)
	}
}

// postErr sends one relay message under the same outbound guards as
// every streamed reply: the self-drive scrub and the own-ts memory.
func (h *Handler) postErr(ctx context.Context, channel, threadTS, text string) (string, error) {
	opts := []slack.MsgOption{slack.MsgOptionText(h.cfg.SelfDrive.Scrub(text), false)}
	if threadTS != "" {
		opts = append(opts, slack.MsgOptionTS(threadTS))
	}
	_, ts, err := h.cfg.API.PostMessageContext(ctx, channel, opts...)
	if err != nil {
		return "", err
	}
	h.cfg.SelfDrive.RecordTS(ts)
	return ts, nil
}

// slackLink renders <url|label>, or the bare label without a url.
// Angle brackets in the label are escaped so it cannot close the link
// early or open another one.
func slackLink(url, label string) string {
	if url == "" {
		return label
	}
	return "<" + url + "|" + labelEscaper.Replace(label) + ">"
}

var labelEscaper = strings.NewReplacer("<", "&lt;", ">", "&gt;")

// deriveTitle is the seed's first line, bounded.
func deriveTitle(seed string) string {
	line, _, _ := strings.Cut(seed, "\n")
	return truncateRunes(strings.Join(strings.Fields(line), " "), maxDerivedTitle)
}

// truncateRunes bounds s to n runes, ending a cut string with "…".
func truncateRunes(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}

// quote renders text as a Slack blockquote.
func quote(text string) string {
	return "> " + strings.ReplaceAll(text, "\n", "\n> ")
}

// Origin resolves the thread sessionKey was branched out of, for the
// `history` tool's origin=true.
func (h *Handler) Origin(sessionKey string) (slackmcp.Origin, bool) {
	o, ok := h.cfg.Router.OriginOf(parseKey(sessionKey))
	return slackmcp.Origin{ChannelID: o.ChannelID, ThreadTS: o.ThreadTS, BranchTS: o.BranchTS}, ok
}

// markSet remembers the messages a reaction already branched, so a
// re-added reaction (or a second person's) does not branch twice. It
// forgets the oldest past maxBranchedMarks; a message that old being
// re-reacted is a deliberate second branch.
type markSet struct {
	mu    sync.Mutex
	set   map[string]struct{}
	order []string
}

// add reports whether mark was new.
func (s *markSet) add(mark string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.set[mark]; ok {
		return false
	}
	if s.set == nil {
		s.set = map[string]struct{}{}
	}
	if len(s.order) >= maxBranchedMarks {
		delete(s.set, s.order[0])
		s.order = s.order[1:]
	}
	s.set[mark] = struct{}{}
	s.order = append(s.order, mark)
	return true
}

// remove forgets mark, so a failed branch can be retried.
func (s *markSet) remove(mark string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.set, mark)
	for i, m := range s.order {
		if m == mark {
			s.order = append(s.order[:i], s.order[i+1:]...)
			break
		}
	}
}
