package debuglog

import (
	"bytes"
	"log"
	"sync"
	"testing"
)

func TestActiveDefault(t *testing.T) {
	// Reset package-level once+enabled so each subtest evaluates active()
	// fresh against the current env. We can't reach the un-exported once,
	// so guard the test with a parallel-safe lock and replace state.
	resetOnce(t)
	t.Setenv("SLACK_ACP_DEBUG", "")
	if active() {
		t.Fatal("default should be off")
	}
}

func TestActiveExplicitOff(t *testing.T) {
	for _, v := range []string{"0", "false"} {
		v := v
		t.Run(v, func(t *testing.T) {
			resetOnce(t)
			t.Setenv("SLACK_ACP_DEBUG", v)
			if active() {
				t.Fatalf("%q should disable", v)
			}
		})
	}
}

func TestActiveOnAndLogf(t *testing.T) {
	resetOnce(t)
	t.Setenv("SLACK_ACP_DEBUG", "1")
	if !active() {
		t.Fatal("debug should be on")
	}
	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(prev) })
	Logf("hello %s", "world")
	if !bytes.Contains(buf.Bytes(), []byte("[debug] hello world")) {
		t.Fatalf("missing log line: %q", buf.String())
	}
}

func TestLogfDisabledNoOp(t *testing.T) {
	resetOnce(t)
	t.Setenv("SLACK_ACP_DEBUG", "")
	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(prev) })
	Logf("ignored")
	if buf.Len() != 0 {
		t.Fatalf("expected no output, got %q", buf.String())
	}
}

// resetOnce reinitialises the package-level sync.Once so each test sees
// the current env. The package guards env evaluation behind sync.Once,
// which is the right contract for production but hostile to tests; we
// reach in via the test-only setter below.
func resetOnce(t *testing.T) {
	t.Helper()
	resetMu.Lock()
	defer resetMu.Unlock()
	once = sync.Once{}
	enabled = false
}

var resetMu sync.Mutex
