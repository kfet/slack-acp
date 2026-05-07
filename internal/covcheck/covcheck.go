// Package covcheck filters a Go coverage profile through a list of
// regex patterns (a ".covignore" file) and enforces a minimum
// statement-weighted coverage percentage on the result.
//
// Each non-blank, non-comment line in the ignore file is a regular
// expression. Profile lines whose text matches any pattern are dropped
// before the threshold check and omitted from the filtered output.
//
// The filtered profile is written to the configured output path so
// downstream tools (`go tool cover -html=…`, `-func=…`) see exactly
// the same view that the gate evaluated.
package covcheck

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"
)

// Config controls a single Run invocation.
type Config struct {
	// ProfilePath is the input coverage profile (required).
	ProfilePath string
	// OutPath is where the filtered profile is written (required).
	OutPath string
	// IgnorePath is the .covignore file. Empty or missing → no filtering.
	IgnorePath string
	// Min is the minimum statement-weighted coverage percentage required.
	Min float64
	// Stdout receives the one-line coverage summary on success.
	// Stderr receives the gate-failure detail (uncovered profile entries).
	Stdout, Stderr io.Writer
}

// Run executes the filter + gate. It returns nil on success and an
// error describing the failure otherwise. The filtered profile is
// always written when the input could be read, even if the gate fails,
// so the caller can inspect it.
func Run(cfg Config) error {
	if cfg.ProfilePath == "" || cfg.OutPath == "" {
		return errors.New("covcheck: -profile and -out are required")
	}
	if cfg.Stdout == nil {
		cfg.Stdout = io.Discard
	}
	if cfg.Stderr == nil {
		cfg.Stderr = io.Discard
	}

	patterns, err := loadIgnore(cfg.IgnorePath)
	if err != nil {
		return err
	}

	in, err := os.Open(cfg.ProfilePath)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.Create(cfg.OutPath)
	if err != nil {
		return err
	}
	defer out.Close()

	totalStmts, coveredStmts, uncovered, err := filter(in, out, patterns)
	if err != nil {
		return err
	}

	pct := 100.0
	if totalStmts > 0 {
		pct = 100.0 * float64(coveredStmts) / float64(totalStmts)
	}
	fmt.Fprintf(cfg.Stdout, "coverage: %.1f%% of statements (%d/%d)\n",
		pct, coveredStmts, totalStmts)

	// Fail if pct is strictly below Min (allow tiny float slack).
	if pct+1e-9 < cfg.Min {
		fmt.Fprintf(cfg.Stderr,
			"ERROR: coverage %.1f%% < required %.1f%% — see %s\n",
			pct, cfg.Min, cfg.OutPath)
		for _, u := range uncovered {
			fmt.Fprintln(cfg.Stderr, "  uncovered:", u)
		}
		return fmt.Errorf("coverage %.1f%% < %.1f%%", pct, cfg.Min)
	}
	return nil
}

// filter copies in→out skipping lines matching any pattern, and tallies
// statement-weighted coverage on the surviving lines. The first line
// (typically "mode: set") is preserved verbatim.
func filter(in io.Reader, out io.Writer, patterns []*regexp.Regexp) (
	totalStmts, coveredStmts int, uncovered []string, err error,
) {
	s := bufio.NewScanner(in)
	s.Buffer(make([]byte, 64*1024), 1<<20)
	first := true
	for s.Scan() {
		line := s.Text()
		if first {
			first = false
			if _, werr := fmt.Fprintln(out, line); werr != nil {
				return 0, 0, nil, werr
			}
			continue
		}
		if matchesAny(line, patterns) {
			continue
		}
		if _, werr := fmt.Fprintln(out, line); werr != nil {
			return 0, 0, nil, werr
		}
		stmts, count, ok := parseProfileLine(line)
		if !ok {
			continue
		}
		totalStmts += stmts
		if count > 0 {
			coveredStmts += stmts
		} else {
			uncovered = append(uncovered, line)
		}
	}
	if serr := s.Err(); serr != nil {
		return 0, 0, nil, serr
	}
	return totalStmts, coveredStmts, uncovered, nil
}

func matchesAny(line string, patterns []*regexp.Regexp) bool {
	for _, p := range patterns {
		if p.MatchString(line) {
			return true
		}
	}
	return false
}

// parseProfileLine parses "file:start.col,end.col numStmt count".
// Returns ok=false for unrecognised shapes (which the caller skips
// silently — they're treated as non-coverage records).
func parseProfileLine(line string) (stmts, count int, ok bool) {
	fields := strings.Fields(line)
	if len(fields) != 3 {
		return 0, 0, false
	}
	stmts, err := strconv.Atoi(fields[1])
	if err != nil {
		return 0, 0, false
	}
	count, err = strconv.Atoi(fields[2])
	if err != nil {
		return 0, 0, false
	}
	return stmts, count, true
}

// loadIgnore reads a .covignore file. Blank lines and lines starting
// with '#' (after trimming) are skipped. A non-existent path yields
// an empty pattern set, not an error — the gate is still applied.
func loadIgnore(path string) ([]*regexp.Regexp, error) {
	if path == "" {
		return nil, nil
	}
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	var out []*regexp.Regexp
	s := bufio.NewScanner(f)
	for s.Scan() {
		line := strings.TrimSpace(s.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		re, err := regexp.Compile(line)
		if err != nil {
			return nil, fmt.Errorf("covignore: bad regex %q: %w", line, err)
		}
		out = append(out, re)
	}
	if err := s.Err(); err != nil {
		return nil, err
	}
	return out, nil
}
