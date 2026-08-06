package verify

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"

	"github.com/kfet/slack-acp/internal/journal"
)

// CommandSource reads the relay's ingest journal by running a command
// and parsing its output — in production `journalctl --user -u
// slack-acp`, but any command that emits the relay's log lines works
// (a file cat, an ssh into the deployment host, a container log).
//
// It re-runs the command on every call rather than tailing: the
// harness polls, the window is bounded by --since, and a re-read is
// far simpler to reason about than a long-lived pipe that can stall.
type CommandSource struct {
	Argv []string
}

// DefaultJournalArgv reads the systemd user unit's log from the point
// the harness started. `-o cat` strips journald's own prefix, though
// journal.Parse tolerates it either way.
func DefaultJournalArgv(unit, since string) []string {
	return []string{"journalctl", "--user", "-u", unit, "--since", since, "-o", "cat", "--no-pager"}
}

// NewCommandSource builds a Source from an argv.
func NewCommandSource(argv []string) (*CommandSource, error) {
	if len(argv) == 0 {
		return nil, fmt.Errorf("verify: journal command is empty")
	}
	return &CommandSource{Argv: argv}, nil
}

// NewShellSource builds a Source from a shell command line. Splitting
// an operator-supplied string on whitespace would mangle the quoting
// that any realistic override needs (`ssh host journalctl --since "10
// min ago"`), so the string is handed to the shell verbatim.
func NewShellSource(cmdline string) (*CommandSource, error) {
	if strings.TrimSpace(cmdline) == "" {
		return nil, fmt.Errorf("verify: journal command is empty")
	}
	return &CommandSource{Argv: []string{"sh", "-c", cmdline}}, nil
}

// Records runs the command and parses every ingest record from its
// output. Non-record lines are ignored, so the command may emit the
// relay's ordinary logging too.
func (c *CommandSource) Records(ctx context.Context) ([]journal.Record, error) {
	cmd := exec.CommandContext(ctx, c.Argv[0], c.Argv[1:]...) //nolint:gosec // argv is operator-supplied by construction
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("%s: %w: %s", c.Argv[0], err, strings.TrimSpace(stderr.String()))
	}
	return ParseRecords(out), nil
}

// ParseRecords extracts every ingest record from a log stream.
func ParseRecords(b []byte) []journal.Record {
	var recs []journal.Record
	sc := bufio.NewScanner(bytes.NewReader(b))
	// Journal lines are short, but a stray long log line in the same
	// stream must not abort the scan.
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		if rec, ok := journal.Parse(sc.Text()); ok {
			recs = append(recs, rec)
		}
	}
	return recs
}
