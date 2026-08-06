package verify

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/kfet/slack-acp/internal/journal"
)

func TestDefaultJournalArgv(t *testing.T) {
	argv := DefaultJournalArgv("slack-acp", "10 min ago")
	joined := strings.Join(argv, " ")
	for _, want := range []string{"journalctl", "--user", "-u slack-acp", "--since 10 min ago", "--no-pager"} {
		if !strings.Contains(joined, want) {
			t.Errorf("argv missing %q: %s", want, joined)
		}
	}
}

func TestNewCommandSourceRejectsEmptyArgv(t *testing.T) {
	if _, err := NewCommandSource(nil); err == nil {
		t.Fatal("an empty journal command must be rejected")
	}
}

// TestCommandSourceParsesRealCommandOutput runs an actual subprocess,
// so the exec path is exercised rather than mocked.
func TestCommandSourceParsesRealCommandOutput(t *testing.T) {
	line := `SLACK-ACP-INGEST {"stage":"slackproto","path":"app_mention","decision":"deliver","reason":"mention","channel":"C1","ts":"1.0"}`
	src, err := NewCommandSource([]string{"sh", "-c",
		"echo 'slack-acp: connecting to Slack…'; echo '" + line + "'; echo 'unrelated trailing line'"})
	if err != nil {
		t.Fatal(err)
	}
	recs, err := src.Records(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 {
		t.Fatalf("want 1 record among the noise, got %+v", recs)
	}
	want := journal.Record{Stage: journal.StageProto, Path: journal.PathAppMention,
		Decision: journal.DecisionDeliver, Reason: journal.ReasonMention, Channel: "C1", TS: "1.0"}
	if recs[0] != want {
		t.Fatalf("got %+v want %+v", recs[0], want)
	}
}

// TestCommandSourceSurfacesCommandFailure matters operationally: a
// wrong unit name or missing journal access must be a loud error, not
// an empty record set that silently fails every check as "the relay
// never saw it".
func TestCommandSourceSurfacesCommandFailure(t *testing.T) {
	src, err := NewCommandSource([]string{"sh", "-c", "echo 'no such unit' >&2; exit 1"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = src.Records(context.Background())
	if err == nil || !strings.Contains(err.Error(), "no such unit") {
		t.Fatalf("got %v", err)
	}
}

// TestParseRecordsToleratesAVeryLongLine pins the scanner buffer: one
// oversized log line in the stream must not truncate the scan.
func TestParseRecordsToleratesAVeryLongLine(t *testing.T) {
	long := strings.Repeat("x", 200*1024)
	line := `SLACK-ACP-INGEST {"stage":"handler","decision":"run","reason":"prompt","channel":"C1","ts":"2.0"}`
	recs := ParseRecords([]byte(long + "\n" + line + "\n"))
	if len(recs) != 1 || recs[0].TS != "2.0" {
		t.Fatalf("got %+v", recs)
	}
}

// TestNewShellSourcePreservesQuoting is why the override is a shell
// command line and not a whitespace split: every realistic override
// (`--since "10 min ago"`, an ssh into the deployment host) contains
// quoted arguments that Fields() would shred.
func TestNewShellSourcePreservesQuoting(t *testing.T) {
	line := `SLACK-ACP-INGEST {"stage":"handler","decision":"run","reason":"prompt","channel":"C1","ts":"9.0"}`
	// Quoted arguments containing spaces AND the JSON's own double
	// quotes — exactly what a Fields() split would destroy.
	src, err := NewShellSource(`printf '%s\n' '` + line + `' # --since "10 min ago"`)
	if err != nil {
		t.Fatal(err)
	}
	recs, err := src.Records(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 || recs[0].TS != "9.0" {
		t.Fatalf("got %+v", recs)
	}
	if _, err := NewShellSource("   "); err == nil {
		t.Error("a blank command line must be rejected")
	}
}

// TestJournalReadIsNotBoundedByTheCallersDeadline is the regression
// test for a misdiagnosis, not a crash. The harness polls this inside a
// wait budget; when that budget expired it used to kill the in-flight
// journalctl and report "context deadline exceeded", which reads as
// "journald is broken" when the truth was "the thing you waited for
// never happened". A read must succeed (or fail on its own terms)
// regardless of the caller's deadline.
func TestJournalReadIsNotBoundedByTheCallersDeadline(t *testing.T) {
	line := `SLACK-ACP-INGEST {"stage":"handler","decision":"run","reason":"prompt","channel":"C1","ts":"1.0"}`
	src, err := NewShellSource(`printf '%s\n' '` + line + `'`)
	if err != nil {
		t.Fatal(err)
	}
	// A context that is ALREADY past its deadline.
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Hour))
	defer cancel()

	recs, err := src.Records(ctx)
	if err != nil {
		t.Fatalf("an expired caller deadline must not break the read: %v", err)
	}
	if len(recs) != 1 || recs[0].TS != "1.0" {
		t.Fatalf("got %+v", recs)
	}
}

// TestCommandSourceAppliesDefaultTimeout covers the zero-value guard: a
// CommandSource built as a literal (not via the constructors) must
// still bound its read rather than hang forever.
func TestCommandSourceAppliesDefaultTimeout(t *testing.T) {
	src := &CommandSource{Argv: []string{"sh", "-c", "echo hi"}} // Timeout unset
	if _, err := src.Records(context.Background()); err != nil {
		t.Fatal(err)
	}
}
