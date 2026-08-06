// Package verify is the relay's self-verification harness: it drives
// real messages through real Slack and asserts that each inbound path
// behaved as designed.
//
// # Why this exists
//
// Every inbound path had a verification story except one. `app_mention`
// carries an absolute guard — a bot-authored mention is dropped, no
// exceptions, because the relay replies as the same bot and a
// bot-triggerable mention is an unbounded reply→trigger→reply loop. The
// consequence was that no automated check could ever exercise
// `app_mention`, and a real bug (the mention flag lost across the
// slackproto→handler boundary) survived to release because only a human
// typing in Slack could find it.
//
// The way out is not to weaken the guard, and not to add a test-only
// door past it. It is to post as a *human*: `chat.postMessage` with a
// user token (`xoxp-`) produces a message with no `bot_id` and a real
// human `user`, so it clears the guard the same way a person does, over
// the same websocket, through Slack's own servers. Nothing here bypasses
// anything — that is the entire point, and it is why the checks are
// worth trusting.
//
// # How a check asserts
//
// Both halves, always:
//
//   - the ingest journal (internal/journal) record for the exact ts it
//     posted, which says what the relay decided and why; and
//   - the actual Slack thread state via conversations.replies.
//
// The journal half is load-bearing for the negative checks. "The bot
// must NOT answer this" asserted by silence alone is unfalsifiable — it
// passes just as happily when the relay is down, misconfigured, or
// still booting. Waiting for the *drop record* for that specific ts
// turns it into positive evidence that the relay saw the message and
// deliberately declined it.
package verify

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/kfet/slack-acp/internal/journal"
)

// Status is the outcome of one named check.
type Status string

// Check outcomes. Skip is a first-class result, not a soft pass: a
// check that could not run (no user token, no sentinel configured)
// must be visibly absent from the evidence, never quietly green.
const (
	StatusPass Status = "PASS"
	StatusFail Status = "FAIL"
	StatusSkip Status = "SKIP"
)

// Result is one check's outcome plus the evidence behind it.
type Result struct {
	Name    string
	Status  Status
	Detail  string
	Records []journal.Record
}

// Message is the subset of a Slack message the harness reasons about.
type Message struct {
	TS      string
	User    string
	BotID   string
	SubType string
	Text    string
}

// authored reports whether this message came from the relay bot.
func (m Message) authored(botUserID string) bool {
	return m.BotID != "" || (botUserID != "" && m.User == botUserID)
}

// Slack is the Web API surface the harness needs. Two implementations
// are wired at runtime — one bound to the bot token, one to the user
// token — because *which token posts* is the whole experiment.
type Slack interface {
	// AuthTest returns the user id the token authenticates as.
	AuthTest(ctx context.Context) (string, error)
	// Post publishes a message, returning its ts. threadTS may be empty
	// for a top-level post.
	Post(ctx context.Context, channel, threadTS, text string) (string, error)
	// Delete removes a message. Slack only permits deleting messages
	// authored by the calling token.
	Delete(ctx context.Context, channel, ts string) error
	// Replies returns the messages of a thread, parent first.
	Replies(ctx context.Context, channel, threadTS string) ([]Message, error)
	// OpenDM returns the IM channel id for a conversation with userID.
	OpenDM(ctx context.Context, userID string) (string, error)
}

// Source yields the relay's ingest-journal records. The live
// implementation shells out to journalctl; tests supply a fixture.
type Source interface {
	Records(ctx context.Context) ([]journal.Record, error)
}

// Config wires a Runner.
type Config struct {
	// Bot is authenticated with the xoxb- token: it posts the
	// bot-authored checks and performs cleanup of the relay's replies.
	Bot Slack
	// User is authenticated with an xoxp- user token and posts every
	// human-authored check. Nil disables those checks (they report
	// SKIP with the reason) — the harness never silently substitutes
	// the bot token, because a bot-authored post cannot exercise the
	// app_mention guard and pretending otherwise is the exact failure
	// mode this package exists to avoid.
	User Slack
	// Journal reads the relay's ingest decisions.
	Journal Source

	PublicChannel  string
	PrivateChannel string

	// SelfDriveSentinel enables the self-drive check when non-empty.
	SelfDriveSentinel string

	// Nonce disambiguates this run's messages from any other run's.
	// Generated per-run when empty.
	Nonce string

	// Wait blocks until cond returns true, or fails. Injected so tests
	// need no wall clock; nil installs PollWaiter(200ms, 90s).
	Wait func(ctx context.Context, cond func(context.Context) (bool, error)) error

	// Now supplies the nonce timestamp; nil uses time.Now.
	Now func() time.Time
}

// Runner executes the checks.
type Runner struct {
	cfg       Config
	botUserID string
	humanID   string

	// posted records everything this run wrote, so cleanup can delete
	// it with the token that authored it.
	posted []posted
}

type posted struct {
	slack   Slack
	channel string
	ts      string
	// thread is set when this post started a thread, so cleanup can
	// sweep it for relay replies that landed too late to be tracked
	// during the check itself.
	thread bool
}

// PollWaiter returns a Wait function that re-evaluates cond every
// interval until it holds or timeout elapses.
func PollWaiter(interval, timeout time.Duration) func(context.Context, func(context.Context) (bool, error)) error {
	return func(ctx context.Context, cond func(context.Context) (bool, error)) error {
		ctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			ok, err := cond(ctx)
			if err != nil {
				return err
			}
			if ok {
				return nil
			}
			select {
			case <-ctx.Done():
				return fmt.Errorf("timed out after %s waiting for the expected state", timeout)
			case <-t.C:
			}
		}
	}
}

// New constructs a Runner, filling in defaults.
func New(cfg Config) (*Runner, error) {
	if cfg.Bot == nil {
		return nil, errors.New("verify: a bot-token Slack client is required")
	}
	if cfg.Journal == nil {
		return nil, errors.New("verify: a journal source is required")
	}
	if cfg.PublicChannel == "" {
		return nil, errors.New("verify: public channel id is required")
	}
	if cfg.Wait == nil {
		cfg.Wait = PollWaiter(200*time.Millisecond, 90*time.Second)
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Nonce == "" {
		cfg.Nonce = fmt.Sprintf("slack-acp-verify-%d", cfg.Now().UnixNano())
	}
	return &Runner{cfg: cfg}, nil
}

// Nonce returns the marker embedded in every message this run posts.
func (r *Runner) Nonce() string { return r.cfg.Nonce }

// label prefixes every message the harness posts. The human-authored
// checks post with a *user* token, so these appear in a real workspace
// under a real person's name — anyone who sees one before cleanup runs
// deserves to know immediately that a machine wrote it and that it is
// about to delete itself.
func (r *Runner) label(body string) string {
	return fmt.Sprintf(":robot_face: [automated slack-acp verify · %s · auto-deleted] %s", r.cfg.Nonce, body)
}

// Run executes every check in order and returns one Result each.
// Cleanup runs unconditionally before returning, including on error.
func (r *Runner) Run(ctx context.Context) ([]Result, error) {
	defer r.cleanup(ctx)

	var err error
	if r.botUserID, err = r.cfg.Bot.AuthTest(ctx); err != nil {
		return nil, fmt.Errorf("bot auth.test: %w", err)
	}
	if r.cfg.User != nil {
		if r.humanID, err = r.cfg.User.AuthTest(ctx); err != nil {
			return nil, fmt.Errorf("user auth.test: %w", err)
		}
		if r.humanID == r.botUserID {
			return nil, errors.New("verify: the user token authenticates as the bot itself — it cannot exercise the app_mention guard")
		}
	}

	var out []Result
	// The public-channel mention runs first and leaves a live thread
	// behind; the known-thread ambient check reuses it, which is the
	// only honest way to test "the bot is already in this thread".
	mention, threadTS := r.checkMention(ctx, "app_mention_public", r.cfg.PublicChannel)
	out = append(out, mention)
	out = append(out, r.checkAmbientKnownThread(ctx, threadTS))
	out = append(out, r.checkPrivateMention(ctx))
	out = append(out, r.checkDM(ctx))
	out = append(out, r.checkAmbientUnknownThread(ctx))
	out = append(out, r.checkBotEcho(ctx))
	out = append(out, r.checkSelfDrive(ctx))
	return out, nil
}

// ---- individual checks ----

const skipNoUserToken = "no user token configured — this path can only be exercised by a human-authored message; see docs/self-verification.md"

func (r *Runner) checkMention(ctx context.Context, name, channel string) (Result, string) {
	if r.cfg.User == nil {
		return Result{Name: name, Status: StatusSkip, Detail: skipNoUserToken}, ""
	}
	ts, err := r.post(ctx, r.cfg.User, channel, "", r.label(fmt.Sprintf("<@%s> ping", r.botUserID)))
	if err != nil {
		return failf(name, "post as user: %v", err), ""
	}
	res := r.expectRun(ctx, name, channel, ts, ts, journal.PathAppMention, journal.ReasonMention)
	return res, ts
}

func (r *Runner) checkPrivateMention(ctx context.Context) Result {
	const name = "app_mention_private"
	if r.cfg.PrivateChannel == "" {
		return Result{Name: name, Status: StatusSkip, Detail: "no private channel id configured"}
	}
	res, _ := r.checkMention(ctx, name, r.cfg.PrivateChannel)
	return res
}

func (r *Runner) checkAmbientKnownThread(ctx context.Context, threadTS string) Result {
	const name = "ambient_thread_reply_known"
	if r.cfg.User == nil {
		return Result{Name: name, Status: StatusSkip, Detail: skipNoUserToken}
	}
	if threadTS == "" {
		return Result{Name: name, Status: StatusSkip, Detail: "the public app_mention check did not establish a thread, so there is no known thread to reply into"}
	}
	// No mention: this is the un-tagged follow-up that only ambient
	// mode admits, and only because the bot is already in this thread.
	ts, err := r.post(ctx, r.cfg.User, r.cfg.PublicChannel, threadTS, r.label("and one more thing"))
	if err != nil {
		return failf(name, "post as user: %v", err)
	}
	return r.expectRun(ctx, name, r.cfg.PublicChannel, threadTS, ts, journal.PathMessage, journal.ReasonAmbientThreadReply)
}

func (r *Runner) checkDM(ctx context.Context) Result {
	const name = "dm"
	if r.cfg.User == nil {
		return Result{Name: name, Status: StatusSkip, Detail: skipNoUserToken}
	}
	dm, err := r.cfg.Bot.OpenDM(ctx, r.humanID)
	if err != nil {
		return failf(name, "conversations.open with the test user: %v (the bot token needs the im:write scope)", err)
	}
	ts, err := r.post(ctx, r.cfg.User, dm, "", r.label("ping"))
	if err != nil {
		return failf(name, "post as user: %v", err)
	}
	return r.expectRun(ctx, name, dm, ts, ts, journal.PathMessageIM, journal.ReasonDM)
}

func (r *Runner) checkAmbientUnknownThread(ctx context.Context) Result {
	const name = "ambient_thread_reply_unknown_dropped"
	if r.cfg.User == nil {
		return Result{Name: name, Status: StatusSkip, Detail: skipNoUserToken}
	}
	// A thread the bot was never summoned into: top-level post with no
	// mention, then an un-tagged reply under it.
	parent, err := r.post(ctx, r.cfg.User, r.cfg.PublicChannel, "", r.label("unrelated conversation, the bot was never summoned here"))
	if err != nil {
		return failf(name, "post thread parent as user: %v", err)
	}
	ts, err := r.post(ctx, r.cfg.User, r.cfg.PublicChannel, parent, r.label("still unrelated — the relay must NOT answer this"))
	if err != nil {
		return failf(name, "post reply as user: %v", err)
	}
	return r.expectDrop(ctx, name, r.cfg.PublicChannel, parent, ts,
		[]string{journal.ReasonAmbientUnknownThrd})
}

func (r *Runner) checkBotEcho(ctx context.Context) Result {
	const name = "bot_echo_dropped"
	// The relay posting into its own thread must never re-trigger it.
	// Slack may route this as app_mention, as message.channels, or
	// both; either drop reason is the guard working, so both are
	// accepted — pinning one would be asserting Slack's dispatch
	// choices rather than our guard.
	parent, err := r.post(ctx, r.cfg.Bot, r.cfg.PublicChannel, "", r.label("bot echo parent"))
	if err != nil {
		return failf(name, "post as bot: %v", err)
	}
	ts, err := r.post(ctx, r.cfg.Bot, r.cfg.PublicChannel, parent,
		r.label(fmt.Sprintf("<@%s> echo", r.botUserID)))
	if err != nil {
		return failf(name, "post as bot: %v", err)
	}
	return r.expectDrop(ctx, name, r.cfg.PublicChannel, parent, ts,
		[]string{journal.ReasonBotAuthored, journal.ReasonAPIAuthored,
			journal.ReasonSelfDriveNotAccept, journal.ReasonSelfPostedTS})
}

func (r *Runner) checkSelfDrive(ctx context.Context) Result {
	const name = "self_drive_hatch"
	if r.cfg.SelfDriveSentinel == "" {
		return Result{Name: name, Status: StatusSkip, Detail: "self_drive_sentinel is not configured — the hatch is off, which is the correct production setting"}
	}
	ts, err := r.post(ctx, r.cfg.Bot, r.cfg.PublicChannel, "",
		fmt.Sprintf("%s %s", r.cfg.SelfDriveSentinel, r.label("ping")))
	if err != nil {
		return failf(name, "post as bot: %v", err)
	}
	return r.expectRun(ctx, name, r.cfg.PublicChannel, ts, ts, journal.PathSelfDrive, journal.ReasonSelfDrive)
}

// ---- assertions ----

// expectRun asserts the full happy path for one message: the protocol
// layer delivered it on the expected surface, the handler ran a prompt
// for it, and a reply from the bot actually landed in the thread.
func (r *Runner) expectRun(ctx context.Context, name, channel, threadTS, ts string, path journal.Path, deliverReason string) Result {
	var recs []journal.Record
	err := r.cfg.Wait(ctx, func(ctx context.Context) (bool, error) {
		var err error
		if recs, err = r.recordsFor(ctx, channel, ts); err != nil {
			return false, err
		}
		return hasRecord(recs, journal.StageHandler, journal.DecisionRun, journal.ReasonPrompt), nil
	})
	if err != nil {
		return Result{Name: name, Status: StatusFail, Records: recs,
			Detail: fmt.Sprintf("no handler run/prompt record for ts=%s: %v", ts, err)}
	}
	if !hasRecordPath(recs, journal.StageProto, path, journal.DecisionDeliver, deliverReason) {
		return Result{Name: name, Status: StatusFail, Records: recs,
			Detail: fmt.Sprintf("expected slackproto deliver on path=%s reason=%s for ts=%s", path, deliverReason, ts)}
	}

	// The journal says the relay decided to answer; Slack must show it
	// actually did. Only the two together rule out a silent failure in
	// the agent or the outbound streamer.
	var replies []Message
	err = r.cfg.Wait(ctx, func(ctx context.Context) (bool, error) {
		var err error
		if replies, err = r.cfg.Bot.Replies(ctx, channel, threadTS); err != nil {
			return false, err
		}
		return r.botReplied(replies, ts), nil
	})
	if err != nil {
		return Result{Name: name, Status: StatusFail, Records: recs,
			Detail: fmt.Sprintf("the relay journalled a prompt for ts=%s but no bot reply appeared in the thread: %v", ts, err)}
	}
	r.trackBotReplies(channel, replies)
	return Result{Name: name, Status: StatusPass, Records: recs,
		Detail: fmt.Sprintf("delivered on %s (%s), prompt run, bot replied in thread %s", path, deliverReason, threadTS)}
}

// expectDrop asserts a message was seen and deliberately declined. It
// waits for the drop record first and only then inspects the thread:
// checking thread state alone would pass against a relay that is
// simply down.
func (r *Runner) expectDrop(ctx context.Context, name, channel, threadTS, ts string, reasons []string) Result {
	var recs []journal.Record
	err := r.cfg.Wait(ctx, func(ctx context.Context) (bool, error) {
		var err error
		if recs, err = r.recordsFor(ctx, channel, ts); err != nil {
			return false, err
		}
		for _, reason := range reasons {
			if hasRecord(recs, "", journal.DecisionDrop, reason) {
				return true, nil
			}
		}
		return false, nil
	})
	if err != nil {
		return Result{Name: name, Status: StatusFail, Records: recs,
			Detail: fmt.Sprintf("no drop record (%s) for ts=%s — the relay may not have seen the message at all, which is NOT the same as dropping it: %v",
				strings.Join(reasons, "|"), ts, err)}
	}
	// Slack delivers a tagged message as BOTH app_mention and
	// message.channels, as two independent envelopes. A drop on one
	// path says nothing about the other, and the second can land after
	// the snapshot above — so re-read before declaring the message
	// declined. (One extra read narrows the window; it cannot close it
	// entirely, which docs/self-verification.md states plainly.)
	if again, rerr := r.recordsFor(ctx, channel, ts); rerr == nil {
		recs = again
	}
	if hasRecord(recs, journal.StageHandler, journal.DecisionRun, journal.ReasonPrompt) {
		return Result{Name: name, Status: StatusFail, Records: recs,
			Detail: fmt.Sprintf("ts=%s was dropped AND run — a second delivery path let it through", ts)}
	}
	replies, err := r.cfg.Bot.Replies(ctx, channel, threadTS)
	if err != nil {
		return failf(name, "conversations.replies: %v", err)
	}
	if r.botReplied(replies, "") {
		r.trackBotReplies(channel, replies)
		return Result{Name: name, Status: StatusFail, Records: recs,
			Detail: fmt.Sprintf("the relay journalled a drop for ts=%s but still replied in thread %s", ts, threadTS)}
	}
	return Result{Name: name, Status: StatusPass, Records: recs,
		Detail: fmt.Sprintf("dropped as designed, and no reply landed in thread %s", threadTS)}
}

// botReplied reports whether the thread contains a bot-authored message
// other than the ones this run posted. after, when non-empty, restricts
// the search to messages strictly newer than that ts (Slack timestamps
// are fixed-width decimals, so string order is time order).
func (r *Runner) botReplied(msgs []Message, after string) bool {
	for _, m := range msgs {
		if !m.authored(r.botUserID) || r.wePosted(m.TS) {
			continue
		}
		if after != "" && m.TS <= after {
			continue
		}
		return true
	}
	return false
}

func (r *Runner) wePosted(ts string) bool {
	for _, p := range r.posted {
		if p.ts == ts {
			return true
		}
	}
	return false
}

// recordsFor returns this run's journal records for one message.
//
// Keyed on (channel, ts), never ts alone: Slack timestamps are unique
// only *within* a channel, and this harness posts into three channels
// within the same second while the --since window may still hold an
// earlier run. Matching on ts alone would let a record from one channel
// satisfy an assertion about another — a false PASS, the one failure
// mode that would make this whole harness worthless.
func (r *Runner) recordsFor(ctx context.Context, channel, ts string) ([]journal.Record, error) {
	all, err := r.cfg.Journal.Records(ctx)
	if err != nil {
		return nil, fmt.Errorf("read ingest journal: %w", err)
	}
	var out []journal.Record
	for _, rec := range all {
		if rec.TS == ts && rec.Channel == channel {
			out = append(out, rec)
		}
	}
	return out, nil
}

func hasRecord(recs []journal.Record, stage journal.Stage, d journal.Decision, reason string) bool {
	for _, rec := range recs {
		if (stage == "" || rec.Stage == stage) && rec.Decision == d && rec.Reason == reason {
			return true
		}
	}
	return false
}

func hasRecordPath(recs []journal.Record, stage journal.Stage, p journal.Path, d journal.Decision, reason string) bool {
	for _, rec := range recs {
		if rec.Stage == stage && rec.Path == p && rec.Decision == d && rec.Reason == reason {
			return true
		}
	}
	return false
}

func failf(name, format string, args ...any) Result {
	return Result{Name: name, Status: StatusFail, Detail: fmt.Sprintf(format, args...)}
}

// ---- bookkeeping and cleanup ----

func (r *Runner) post(ctx context.Context, api Slack, channel, threadTS, text string) (string, error) {
	ts, err := api.Post(ctx, channel, threadTS, text)
	if err != nil {
		return "", err
	}
	r.posted = append(r.posted, posted{slack: api, channel: channel, ts: ts, thread: threadTS == ""})
	return ts, nil
}

// trackBotReplies queues the relay's own replies for deletion. They are
// deleted with the bot token, which is the only token permitted to
// delete them.
func (r *Runner) trackBotReplies(channel string, msgs []Message) {
	for _, m := range msgs {
		if m.authored(r.botUserID) && !r.wePosted(m.TS) {
			r.posted = append(r.posted, posted{slack: r.cfg.Bot, channel: channel, ts: m.TS})
		}
	}
}

// cleanup deletes everything this run put into Slack, newest first so
// thread parents go last. Uses a fresh context: the run's context may
// already be cancelled or timed out, and leaving test debris in a
// shared channel is its own failure.
func (r *Runner) cleanup(ctx context.Context) {
	// Always detach: the run context may be cancelled or timed out, and
	// leaving debris in a shared channel is its own failure.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()

	// A check that FAILED waiting for a reply may still get one, just
	// too late to have been tracked. Sweep every thread this run
	// parented before deleting, so nothing of ours survives.
	for _, p := range r.posted {
		if !p.thread {
			continue
		}
		if msgs, err := r.cfg.Bot.Replies(ctx, p.channel, p.ts); err == nil {
			r.trackBotReplies(p.channel, msgs)
		}
	}
	for i := len(r.posted) - 1; i >= 0; i-- {
		p := r.posted[i]
		// Best-effort: a failed delete must not mask a check result,
		// and Slack refuses deletes of messages the token did not
		// author (which the accounting above already avoids).
		_ = p.slack.Delete(ctx, p.channel, p.ts)
	}
	r.posted = nil
}

// Summarise renders the results as one line per check plus a verdict,
// and reports whether every check that ran passed.
func Summarise(results []Result) (string, bool) {
	var b strings.Builder
	pass, fail, skip := 0, 0, 0
	for _, res := range results {
		switch res.Status {
		case StatusPass:
			pass++
		case StatusFail:
			fail++
		case StatusSkip:
			skip++
		}
		fmt.Fprintf(&b, "%-6s %-38s %s\n", res.Status, res.Name, res.Detail)
	}
	fmt.Fprintf(&b, "\n%d passed, %d failed, %d skipped\n", pass, fail, skip)
	if skip > 0 {
		b.WriteString("NOTE: skipped checks are UNVERIFIED. They are not passes.\n")
	}
	return b.String(), fail == 0
}
