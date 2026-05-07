// Package acpclient — unreachable.go isolates structurally-unreachable
// defensive branches so the project-wide coverage gate (.covignore)
// can ignore them by file rather than by line number, which is brittle
// against any edit above the line.
//
// See AGENTS.md "Coverage" section for the convention. Add code here
// ONLY if it cannot be reached from a test without contorting
// production code, and pair every helper with a comment justifying the
// unreachability claim.
package acpclient

import (
	"fmt"
	"io"
	"os/exec"
)

// setupCmdPipes returns the child's stdin and stdout. The error
// branches of cmd.{Stdin,Stdout}Pipe only fire when the corresponding
// stream was already set on cmd, which Start never does (it constructs
// cmd inline via exec.CommandContext and only writes .Dir / .Env /
// .Stderr). Both branches are therefore structurally unreachable in
// production, so we panic rather than propagating them — that way the
// caller has no impossible "if err != nil" branch left over to cover.
func setupCmdPipes(cmd *exec.Cmd) (io.WriteCloser, io.Reader) {
	stdin, err := cmd.StdinPipe()
	if err != nil {
		panic(fmt.Errorf("acpclient: stdin pipe (unreachable): %w", err))
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		panic(fmt.Errorf("acpclient: stdout pipe (unreachable): %w", err))
	}
	return stdin, stdout
}
