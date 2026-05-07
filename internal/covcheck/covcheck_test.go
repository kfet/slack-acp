package covcheck

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeWriter is an io.Writer that always returns an error. Used to
// exercise the write-failure branches inside filter() without relying
// on filesystem-level errors that aren't portable.
type fakeWriter struct{ err error }

func (f fakeWriter) Write(_ []byte) (int, error) { return 0, f.err }

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestRun_RequiredFlags(t *testing.T) {
	if err := Run(Config{}); err == nil {
		t.Fatal("expected error when ProfilePath/OutPath are empty")
	}
}

func TestRun_HappyPath_NoIgnore(t *testing.T) {
	dir := t.TempDir()
	prof := filepath.Join(dir, "in.out")
	out := filepath.Join(dir, "out.out")
	writeFile(t, prof, strings.Join([]string{
		"mode: set",
		"pkg/a.go:1.1,2.2 3 1",
		"pkg/a.go:3.1,4.2 2 1",
	}, "\n")+"\n")

	var stdout, stderr bytes.Buffer
	err := Run(Config{
		ProfilePath: prof, OutPath: out, Min: 100,
		Stdout: &stdout, Stderr: &stderr,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(stdout.String(), "100.0%") {
		t.Fatalf("want 100%% in stdout, got %q", stdout.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("unexpected stderr: %s", stderr.String())
	}
	got, _ := os.ReadFile(out)
	if !strings.HasPrefix(string(got), "mode: set\n") {
		t.Fatalf("filtered profile missing mode line: %q", got)
	}
}

func TestRun_AppliesIgnoreAndDropsLines(t *testing.T) {
	dir := t.TempDir()
	prof := filepath.Join(dir, "in.out")
	out := filepath.Join(dir, "out.out")
	ign := filepath.Join(dir, ".covignore")
	writeFile(t, prof, strings.Join([]string{
		"mode: set",
		"pkg/a.go:1.1,2.2 3 1", // covered, kept
		"pkg/b.go:1.1,2.2 2 0", // dropped by ignore
		"pkg/c.go:1.1,2.2 1 1",
	}, "\n")+"\n")
	writeFile(t, ign, "# comment\n\n^pkg/b\\.go:\n")

	var stdout bytes.Buffer
	err := Run(Config{
		ProfilePath: prof, OutPath: out, IgnorePath: ign, Min: 100,
		Stdout: &stdout, Stderr: io.Discard,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	got, _ := os.ReadFile(out)
	if strings.Contains(string(got), "pkg/b.go") {
		t.Fatalf("ignored line leaked into output: %s", got)
	}
}

func TestRun_FailsBelowThreshold_ListsUncovered(t *testing.T) {
	dir := t.TempDir()
	prof := filepath.Join(dir, "in.out")
	out := filepath.Join(dir, "out.out")
	writeFile(t, prof, strings.Join([]string{
		"mode: set",
		"pkg/a.go:1.1,2.2 5 1",
		"pkg/a.go:3.1,4.2 5 0",
	}, "\n")+"\n")

	var stdout, stderr bytes.Buffer
	err := Run(Config{
		ProfilePath: prof, OutPath: out, Min: 100,
		Stdout: &stdout, Stderr: &stderr,
	})
	if err == nil {
		t.Fatal("expected gate failure")
	}
	if !strings.Contains(stderr.String(), "uncovered:") {
		t.Fatalf("expected uncovered listing, got %q", stderr.String())
	}
	if !strings.Contains(stdout.String(), "50.0%") {
		t.Fatalf("expected 50%% in stdout, got %q", stdout.String())
	}
}

func TestRun_NilWritersAreSafe(t *testing.T) {
	dir := t.TempDir()
	prof := filepath.Join(dir, "in.out")
	out := filepath.Join(dir, "out.out")
	writeFile(t, prof, "mode: set\npkg/a.go:1.1,2.2 1 1\n")
	if err := Run(Config{ProfilePath: prof, OutPath: out, Min: 100}); err != nil {
		t.Fatalf("Run: %v", err)
	}
}

func TestRun_NoStatementsReportsHundredPercent(t *testing.T) {
	dir := t.TempDir()
	prof := filepath.Join(dir, "in.out")
	out := filepath.Join(dir, "out.out")
	writeFile(t, prof, "mode: set\n")

	var stdout bytes.Buffer
	err := Run(Config{
		ProfilePath: prof, OutPath: out, Min: 100, Stdout: &stdout,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(stdout.String(), "100.0%") {
		t.Fatalf("want 100%% with no statements, got %q", stdout.String())
	}
}

func TestRun_MissingProfile(t *testing.T) {
	dir := t.TempDir()
	err := Run(Config{
		ProfilePath: filepath.Join(dir, "missing"),
		OutPath:     filepath.Join(dir, "out"),
		Min:         100,
	})
	if err == nil {
		t.Fatal("expected error for missing profile")
	}
}

func TestRun_OutPathUnwritable(t *testing.T) {
	dir := t.TempDir()
	prof := filepath.Join(dir, "in.out")
	writeFile(t, prof, "mode: set\n")
	// Use a path under a non-existent directory to force os.Create error.
	err := Run(Config{
		ProfilePath: prof,
		OutPath:     filepath.Join(dir, "nope", "out"),
		Min:         100,
	})
	if err == nil {
		t.Fatal("expected error for unwritable out path")
	}
}

func TestRun_BadIgnoreRegex(t *testing.T) {
	dir := t.TempDir()
	prof := filepath.Join(dir, "in.out")
	ign := filepath.Join(dir, ".covignore")
	writeFile(t, prof, "mode: set\n")
	writeFile(t, ign, "[unterminated\n")
	err := Run(Config{
		ProfilePath: prof, OutPath: filepath.Join(dir, "out"),
		IgnorePath: ign, Min: 100,
	})
	if err == nil || !strings.Contains(err.Error(), "covignore") {
		t.Fatalf("want covignore parse error, got %v", err)
	}
}

func TestRun_FilterPropagatesScannerError(t *testing.T) {
	// A profile line longer than bufio.Scanner's max buffer (1 MiB) causes
	// scanner.Err() to return bufio.ErrTooLong, which Run must surface.
	dir := t.TempDir()
	prof := filepath.Join(dir, "in.out")
	out := filepath.Join(dir, "out.out")
	huge := strings.Repeat("a", 2*1024*1024)
	writeFile(t, prof, "mode: set\n"+huge+"\n")
	err := Run(Config{ProfilePath: prof, OutPath: out, Min: 100})
	if err == nil {
		t.Fatal("expected scanner error from oversize line")
	}
}

func TestLoadIgnore_ScannerError(t *testing.T) {
	dir := t.TempDir()
	ign := filepath.Join(dir, ".covignore")
	// Default bufio.Scanner buffer maxes at 64 KiB; exceed it to force
	// bufio.ErrTooLong from s.Err() after Scan returns false.
	writeFile(t, ign, strings.Repeat("a", 200_000)+"\n")
	if _, err := loadIgnore(ign); err == nil {
		t.Fatal("expected scanner error from oversize line")
	}
}

func TestLoadIgnore_MissingFileIsNoError(t *testing.T) {
	pats, err := loadIgnore(filepath.Join(t.TempDir(), "nope"))
	if err != nil || pats != nil {
		t.Fatalf("missing file should yield (nil,nil), got (%v,%v)", pats, err)
	}
}

func TestLoadIgnore_EmptyPathIsNoError(t *testing.T) {
	pats, err := loadIgnore("")
	if err != nil || pats != nil {
		t.Fatalf("empty path should yield (nil,nil), got (%v,%v)", pats, err)
	}
}

func TestLoadIgnore_OpenError(t *testing.T) {
	// A directory passed where a file is expected: os.Open succeeds but
	// the subsequent Scan returns a read error on most platforms.
	// To deterministically force the open-error branch, point at an
	// unreadable path. Use a path whose parent component is a regular
	// file so os.Open returns ENOTDIR.
	dir := t.TempDir()
	notDir := filepath.Join(dir, "file")
	writeFile(t, notDir, "x")
	_, err := loadIgnore(filepath.Join(notDir, "child"))
	if err == nil || errors.Is(err, os.ErrNotExist) {
		t.Fatalf("want non-NotExist open error, got %v", err)
	}
}

func TestParseProfileLine(t *testing.T) {
	cases := []struct {
		in           string
		stmts, count int
		ok           bool
	}{
		{"pkg/a.go:1.1,2.2 3 4", 3, 4, true},
		{"too few fields", 0, 0, false},
		{"a b c d", 0, 0, false}, // 4 fields → not 3
		{"pkg/a.go:1.1,2.2 X 4", 0, 0, false},
		{"pkg/a.go:1.1,2.2 3 X", 0, 0, false},
	}
	for _, c := range cases {
		s, n, ok := parseProfileLine(c.in)
		if s != c.stmts || n != c.count || ok != c.ok {
			t.Errorf("parseProfileLine(%q) = (%d,%d,%v), want (%d,%d,%v)",
				c.in, s, n, ok, c.stmts, c.count, c.ok)
		}
	}
}

func TestFilter_PassesNonProfileLinesThrough(t *testing.T) {
	in := strings.NewReader("mode: set\nnot a profile line\npkg/a.go:1.1,2.2 1 1\n")
	var out bytes.Buffer
	tot, cov, unc, err := filter(in, &out, nil)
	if err != nil {
		t.Fatalf("filter: %v", err)
	}
	if tot != 1 || cov != 1 || len(unc) != 0 {
		t.Fatalf("got tot=%d cov=%d unc=%d", tot, cov, len(unc))
	}
	if !strings.Contains(out.String(), "not a profile line") {
		t.Fatalf("non-profile line should pass through: %q", out.String())
	}
}

func TestFilter_WriteErrorOnFirstLine(t *testing.T) {
	in := strings.NewReader("mode: set\n")
	_, _, _, err := filter(in, fakeWriter{err: io.ErrShortWrite}, nil)
	if err == nil {
		t.Fatal("expected write error on first line")
	}
}

func TestFilter_WriteErrorOnSubsequentLine(t *testing.T) {
	// First Fprintln (mode:) succeeds via a tee; second one fails.
	in := strings.NewReader("mode: set\npkg/a.go:1.1,2.2 1 1\n")
	w := &failAfterN{n: 1}
	_, _, _, err := filter(in, w, nil)
	if err == nil {
		t.Fatal("expected write error on subsequent line")
	}
}

func TestFilter_ScannerError(t *testing.T) {
	// Force a scanner error by providing a Reader that returns an error.
	r := errReader{err: io.ErrUnexpectedEOF}
	_, _, _, err := filter(r, io.Discard, nil)
	if err == nil {
		t.Fatal("expected scanner error")
	}
}

// failAfterN is an io.Writer that succeeds for the first n writes and
// fails thereafter. Used to hit the second-line write-error branch.
type failAfterN struct {
	n     int
	calls int
}

func (f *failAfterN) Write(p []byte) (int, error) {
	f.calls++
	if f.calls > f.n {
		return 0, io.ErrShortWrite
	}
	return len(p), nil
}

type errReader struct{ err error }

func (e errReader) Read(_ []byte) (int, error) { return 0, e.err }
