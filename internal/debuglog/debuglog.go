// Package debuglog provides a tiny opt-in debug logger gated by SLACK_ACP_DEBUG.
package debuglog

import (
	"log"
	"os"
	"sync"
)

var (
	once    sync.Once
	enabled bool
)

func active() bool {
	once.Do(func() {
		v := os.Getenv("SLACK_ACP_DEBUG")
		enabled = v != "" && v != "0" && v != "false"
	})
	return enabled
}

// Logf logs to stderr when debug is enabled.
func Logf(format string, args ...any) {
	if active() {
		log.Printf("[debug] "+format, args...)
	}
}
