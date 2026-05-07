// covcheck filters a Go coverage profile through a .covignore file
// and enforces a minimum statement-weighted coverage percentage.
//
// Usage:
//
//	go tool covcheck -profile=bin/coverage.tmp.out \
//	                 -out=bin/coverage.out         \
//	                 -ignore=.covignore            \
//	                 -min=100
//
// See package internal/covcheck for the filter and gate logic.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/kfet/slack-acp/internal/covcheck"
)

func main() {
	var cfg covcheck.Config
	flag.StringVar(&cfg.ProfilePath, "profile", "", "input coverage profile (required)")
	flag.StringVar(&cfg.OutPath, "out", "", "filtered profile output path (required)")
	flag.StringVar(&cfg.IgnorePath, "ignore", "", "path to .covignore (line-oriented regexes; # comments)")
	flag.Float64Var(&cfg.Min, "min", 100.0, "minimum coverage percent (statement-weighted)")
	flag.Parse()
	cfg.Stdout = os.Stdout
	cfg.Stderr = os.Stderr
	if err := covcheck.Run(cfg); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
