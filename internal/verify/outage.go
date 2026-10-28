package verify

import (
	"context"
	"fmt"
	"os/exec"
	"strings"

	"github.com/kfet/slack-acp/internal/journal"
)

// Outage takes the relay down and brings it back, so the catch-up can
// be exercised against real Slack: only a message posted while the
// relay is NOT connected proves the catch-up, and nothing short of
// stopping it makes Slack withhold an event.
type Outage interface {
	Stop(ctx context.Context) error
	Start(ctx context.Context) error
}

// ShellOutage runs operator-supplied shell command lines, e.g.
// `systemctl --user stop slack-acp` / `systemctl --user start
// slack-acp`, or the same over ssh.
type ShellOutage struct {
	StopCmd, StartCmd string
}

// NewShellOutage builds an Outage from two command lines. Both empty
// means none (the check SKIPs); exactly one is a configuration error,
// because a relay stopped and never started is worse than no check.
func NewShellOutage(stop, start string) (Outage, error) {
	stop, start = strings.TrimSpace(stop), strings.TrimSpace(start)
	switch {
	case stop == "" && start == "":
		return nil, nil
	case stop == "" || start == "":
		return nil, fmt.Errorf("verify: the outage check needs BOTH a stop and a start command")
	}
	return ShellOutage{StopCmd: stop, StartCmd: start}, nil
}

// Stop runs the stop command.
func (o ShellOutage) Stop(ctx context.Context) error { return runShell(ctx, o.StopCmd) }

// Start runs the start command.
func (o ShellOutage) Start(ctx context.Context) error { return runShell(ctx, o.StartCmd) }

func runShell(ctx context.Context, cmdline string) error {
	out, err := exec.CommandContext(ctx, "sh", "-c", cmdline).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%q: %w: %s", cmdline, err, strings.TrimSpace(string(out)))
	}
	return nil
}

const skipNoOutage = "no outage commands configured (--outage-stop-cmd / --outage-start-cmd) — the catch-up is only exercised by a message posted while the relay is down"

// checkCatchUp stops the relay, posts while it is down, starts it, and
// asserts what the catch-up must do with what it missed: answer an
// @-mention once, and NOT run a relay command typed for a relay that
// was not there. It runs last, because it restarts the relay.
func (r *Runner) checkCatchUp(ctx context.Context) []Result {
	const answered, skipped = "catch_up_mention_answered", "catch_up_command_skipped"
	skip := func(why string) []Result {
		return []Result{{Name: answered, Status: StatusSkip, Detail: why}, {Name: skipped, Status: StatusSkip, Detail: why}}
	}
	fail := func(format string, args ...any) []Result {
		return []Result{failf(answered, format, args...), failf(skipped, format, args...)}
	}
	switch {
	case r.cfg.User == nil:
		return skip(skipNoUserToken)
	case r.cfg.Outage == nil:
		return skip(skipNoOutage)
	}
	if err := r.cfg.Outage.Stop(ctx); err != nil {
		return fail("stop the relay: %v", err)
	}
	mention, merr := r.post(ctx, r.cfg.User, r.cfg.PublicChannel, "",
		r.label(fmt.Sprintf("<@%s> posted while the relay was DOWN — the catch-up must answer this exactly once", r.botUserID)))
	// The command must START with the sigil once the mention is
	// stripped, so the label goes after it.
	command, cerr := r.post(ctx, r.cfg.User, r.cfg.PublicChannel, "",
		fmt.Sprintf("<@%s> !status %s", r.botUserID, r.label("a relay command posted while the relay was DOWN — it must NOT run late")))
	// Bring the relay back whatever happened above.
	if err := r.cfg.Outage.Start(ctx); err != nil {
		return fail("start the relay again: %v — IT MAY STILL BE DOWN", err)
	}
	if merr != nil || cerr != nil {
		return fail("post as user while the relay was down: %v", firstErr(merr, cerr))
	}

	res := r.expectRun(ctx, answered, r.cfg.PublicChannel, mention, mention, journal.PathAppMention, journal.ReasonMention)
	if res.Status == StatusPass && !hasCatchUpRecord(res.Records) {
		res.Status = StatusFail
		res.Detail = fmt.Sprintf("ts=%s was answered, but not by the catch-up (no catch_up record) — was the relay really down?", mention)
	}
	return []Result{res, r.expectDrop(ctx, skipped, r.cfg.PublicChannel, command, command, []string{journal.ReasonCatchUpCommand})}
}

func hasCatchUpRecord(recs []journal.Record) bool {
	for _, rec := range recs {
		if rec.CatchUp && rec.Stage == journal.StageProto {
			return true
		}
	}
	return false
}

func firstErr(errs ...error) error {
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}
