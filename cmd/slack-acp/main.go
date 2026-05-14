// slack-acp is a standalone Slack bot that runs an ACP-compatible agent
// (e.g. fir --mode acp, claude-code) and relays each Slack thread to a
// dedicated ACP session.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/kfet/slack-acp/internal/acpclient"
	"github.com/kfet/slack-acp/internal/config"
	"github.com/kfet/slack-acp/internal/handler"
	"github.com/kfet/slack-acp/internal/policy"
	"github.com/kfet/slack-acp/internal/router"
	"github.com/kfet/slack-acp/internal/slackproto"
	"github.com/kfet/slack-acp/internal/sysprompt"
)

var version = "dev"

func main() {
	configPath := flag.String("config", "", "path to JSON config file")
	agentCmd := flag.String("agent-cmd", "", "agent argv (default: fir --mode acp); space-separated; overrides config")
	policyName := flag.String("policy", "", "permission policy: allow-all|read-only|deny-all (default allow-all)")
	stateDir := flag.String("state-dir", "", "root directory for per-thread state (default: $XDG_STATE_HOME/slack-acp)")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println(version)
		return
	}

	cfg := &config.Config{}
	if *configPath != "" {
		c, err := config.Load(*configPath)
		if err != nil {
			log.Fatalf("config: %v", err)
		}
		cfg = c
	}
	// CLI/env overrides.
	if v := os.Getenv("SLACK_BOT_TOKEN"); v != "" {
		cfg.BotToken = v
	}
	if v := os.Getenv("SLACK_APP_TOKEN"); v != "" {
		cfg.AppToken = v
	}
	if *agentCmd != "" {
		cfg.AgentCmd = strings.Fields(*agentCmd)
	}
	if len(cfg.AgentCmd) == 0 {
		cfg.AgentCmd = []string{"fir", "--mode", "acp"}
	}
	if *policyName != "" {
		cfg.Policy = *policyName
	}
	if *stateDir != "" {
		cfg.StateDir = *stateDir
	}
	if cfg.StateDir == "" {
		cfg.StateDir = router.DefaultStateDir()
	}

	// Validate required tokens before doing any disk or network work,
	// so an operator with missing tokens fails fast rather than seeing
	// "state dir created" before the real error.
	if cfg.BotToken == "" || cfg.AppToken == "" {
		log.Fatalf("slack-acp: bot_token and app_token required (set in config or via SLACK_BOT_TOKEN / SLACK_APP_TOKEN env)")
	}

	if err := os.MkdirAll(cfg.StateDir, 0o755); err != nil {
		log.Fatalf("state dir: %v", err)
	}
	log.Printf("slack-acp: state dir %s", cfg.StateDir)

	pol, err := policy.Parse(cfg.Policy)
	if err != nil {
		log.Fatalf("policy: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	agent, err := acpclient.Start(ctx, acpclient.Config{
		Command: cfg.AgentCmd,
		Cwd:     cfg.StateDir,
		Policy:  pol,
		Stderr:  os.Stderr,
	})
	if err != nil {
		log.Fatalf("agent start: %v", err)
	}
	defer agent.Close()
	log.Printf("slack-acp %s: agent up (caps=%+v)", version, agent.Caps())

	r, err := router.New(router.Config{
		Agent:        agent,
		StateDir:     cfg.StateDir,
		SystemPrompt: sysprompt.Resolve(cfg.SystemPrompt, cfg.DisableSystemPrompt),
	})
	if err != nil {
		log.Fatalf("router: %v", err)
	}
	defer r.Close()
	go r.Run(ctx)

	allowedUsers := toSet(cfg.AllowedUserIDs)
	allowedChannels := toSet(cfg.AllowedChannelIDs)

	h := handler.New(handler.Config{
		Router:            r,
		AllowedUserIDs:    allowedUsers,
		AllowedChannelIDs: allowedChannels,
	})

	sc, err := slackproto.New(cfg.BotToken, cfg.AppToken, h)
	if err != nil {
		log.Fatalf("slack: %v", err)
	}
	// API client is needed by the handler for posting; wire it now that we have it.
	h.SetAPI(sc.API())

	log.Printf("slack-acp: connecting to Slack…")
	if err := sc.Run(ctx); err != nil && ctx.Err() == nil {
		log.Fatalf("slack run: %v", err)
	}
}

func toSet(ss []string) map[string]struct{} {
	if len(ss) == 0 {
		return nil
	}
	m := make(map[string]struct{}, len(ss))
	for _, s := range ss {
		m[s] = struct{}{}
	}
	return m
}
