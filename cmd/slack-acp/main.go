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
)

var version = "dev"

func main() {
	configPath := flag.String("config", "", "path to JSON config file")
	agentCmd := flag.String("agent-cmd", "", "agent argv (default: fir --mode acp); space-separated; overrides config")
	policyName := flag.String("policy", "", "permission policy: allow-all|read-only|deny-all (default allow-all)")
	cwdRoot := flag.String("cwd-root", "", "root directory for per-conversation cwds")
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
	if *cwdRoot != "" {
		cfg.CwdRoot = *cwdRoot
	}

	if cfg.BotToken == "" || cfg.AppToken == "" {
		log.Fatalf("slack-acp: bot_token and app_token required (set in config or via SLACK_BOT_TOKEN / SLACK_APP_TOKEN env)")
	}

	pol, err := policy.Parse(cfg.Policy)
	if err != nil {
		log.Fatalf("policy: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	agent, err := acpclient.Start(ctx, acpclient.Config{
		Command: cfg.AgentCmd,
		Cwd:     os.TempDir(),
		Policy:  pol,
		Stderr:  os.Stderr,
	})
	if err != nil {
		log.Fatalf("agent start: %v", err)
	}
	defer agent.Close()
	log.Printf("slack-acp %s: agent up (caps=%+v)", version, agent.Caps())

	r, err := router.New(router.Config{
		Agent:   agent,
		CwdRoot: cfg.CwdRoot,
	})
	if err != nil {
		log.Fatalf("router: %v", err)
	}
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
