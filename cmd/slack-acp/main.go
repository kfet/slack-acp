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
	"path/filepath"
	"strings"
	"syscall"

	"github.com/kfet/slack-acp/internal/acpclient"
	"github.com/kfet/slack-acp/internal/config"
	"github.com/kfet/slack-acp/internal/handler"
	"github.com/kfet/slack-acp/internal/initcmd"
	"github.com/kfet/slack-acp/internal/policy"
	"github.com/kfet/slack-acp/internal/router"
	"github.com/kfet/slack-acp/internal/skills"
	"github.com/kfet/slack-acp/internal/slackproto"
	"github.com/kfet/slack-acp/internal/sysprompt"
)

var version = "dev"

func main() {
	// Subcommand dispatch (must happen before flag.Parse on the main
	// flagset, since each subcommand has its own flags).
	if len(os.Args) > 1 && os.Args[1] == "init" {
		if err := runInit(os.Args[2:]); err != nil {
			log.Fatalf("init: %v", err)
		}
		return
	}

	configPath := flag.String("config", "", "path to JSON config file")
	agentCmd := flag.String("agent-cmd", "", "agent argv (default: fir --mode acp); space-separated; overrides config")
	policyName := flag.String("policy", "", "permission policy: allow-all|read-only|deny-all (default allow-all)")
	stateDir := flag.String("state-dir", "", "root directory for per-thread state (default: $XDG_STATE_HOME/slack-acp)")
	showVersion := flag.Bool("version", false, "print version and exit")
	printPaths := flag.Bool("print-paths", false, "print resolved config, state dir, and agent command then exit")
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

	if *printPaths {
		cp := *configPath
		if cp == "" {
			cp = "(none; using env + defaults)"
		}
		fmt.Printf("config:     %s\n", cp)
		fmt.Printf("state-dir:  %s\n", cfg.StateDir)
		fmt.Printf("agent-cmd:  %s\n", strings.Join(cfg.AgentCmd, " "))
		fmt.Printf("policy:     %s\n", policyOrDefault(cfg.Policy))
		return
	}

	// Validate tokens before any disk/network work so operators see a
	// targeted error (with hints) rather than an opaque Slack auth
	// failure later on.
	if err := config.ValidateTokens(cfg.BotToken, cfg.AppToken); err != nil {
		log.Fatalf("slack-acp: %v", err)
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
		SystemPrompt: sysprompt.Resolve(cfg.SystemPrompt, cfg.DisableSystemPrompt, buildSkillsCatalog(*configPath)),
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
		if strings.Contains(err.Error(), "invalid_auth") || strings.Contains(err.Error(), "not_authed") || strings.Contains(err.Error(), "account_inactive") {
			log.Fatalf("slack: %v\n  → Slack rejected the bot token. Re-check SLACK_BOT_TOKEN / bot_token (xoxb-…) at api.slack.com/apps → your app → Install App.", err)
		}
		log.Fatalf("slack run: %v", err)
	}
}

func policyOrDefault(p string) string {
	if p == "" {
		return "allow-all (default)"
	}
	return p
}

// buildSkillsCatalog merges embedded built-in skills with optional
// host-supplied skills from <dirname(cfgPath)>/skills/ and returns a
// fir-style <available_skills> block ready for injection. Best-effort:
// extraction failures degrade to whatever layers succeeded (the bot is
// still usable without a catalog). Host skills with the same name as
// a built-in override the built-in.
func buildSkillsCatalog(cfgPath string) string {
	builtin, err := skills.LoadBuiltin()
	if err != nil {
		log.Printf("skills: builtin load failed (continuing): %v", err)
	}
	var host []skills.Skill
	if cfgPath != "" {
		hostDir := filepath.Join(filepath.Dir(cfgPath), "skills")
		host, err = skills.LoadDir(hostDir)
		if err != nil {
			log.Printf("skills: host dir %s: %v (continuing)", hostDir, err)
		}
	}
	merged := skills.Merge([][]skills.Skill{builtin, host}, nil)
	if len(merged) == 0 {
		return ""
	}
	names := make([]string, 0, len(merged))
	for _, s := range merged {
		names = append(names, s.Name)
	}
	log.Printf("skills: %d builtin + %d host → injected %d (%s)",
		len(builtin), len(host), len(merged), strings.Join(names, ","))
	return skills.FormatCatalog(merged)
}

// runInit drives the `slack-acp init` subcommand. Kept as a thin
// flag-parsing shim around internal/initcmd so the wizard logic stays
// testable in isolation (main is exempt from the coverage gate).
func runInit(args []string) error {
	fs := flag.NewFlagSet("init", flag.ExitOnError)
	bot := fs.String("bot-token", "", "Slack bot token (xoxb-…); empty = prompt")
	app := fs.String("app-token", "", "Slack app-level token (xapp-…); empty = prompt")
	cfgPath := fs.String("config", "", "where to write config.json (default $XDG_CONFIG_HOME/slack-acp/config.json)")
	envPath := fs.String("env", "", "where to write the env file (default $XDG_CONFIG_HOME/slack-acp/env)")
	nonInt := fs.Bool("non-interactive", false, "fail instead of prompting for missing tokens")
	skipVerify := fs.Bool("skip-verify", false, "skip the auth.test verification call")
	force := fs.Bool("force", false, "overwrite existing config / env files")
	if err := fs.Parse(args); err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return initcmd.Run(ctx, initcmd.Options{
		BotToken:       *bot,
		AppToken:       *app,
		ConfigPath:     *cfgPath,
		EnvPath:        *envPath,
		NonInteractive: *nonInt,
		SkipVerify:     *skipVerify,
		Force:          *force,
	})
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
