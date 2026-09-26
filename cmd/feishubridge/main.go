// Command feishubridge is the Feishu <-> PowerShell <-> Claude Code bridge.
//
// Usage:
//
//	feishubridge [-config path/to/config.json] [-loglevel debug]
//
// Requires:
//   - a Feishu bot with "机器人接收消息" and "卡片回调" enabled
//   - PowerShell Core (pwsh) on PATH
//   - Claude Code (claude) on PATH
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"feishubridge/internal/bridge"
	"feishubridge/internal/config"
	"feishubridge/internal/feishu"
)

// Build metadata, stamped at link time via
// -ldflags "-X main.Version=... -X main.Commit=... -X main.BuildTime=...".
// The Makefile supplies them; a hand build leaves them empty and prints
// "(not stamped)".
var (
	Version   = ""
	Commit    = ""
	BuildTime = ""
)

func main() {
	configPath := flag.String("config", "", "config file path (default: $FEISHU_BRIDGE_CONFIG or ~/.config/feishu-bridge/config.json)")
	logLevel := flag.String("loglevel", "info", "log level: debug or info")
	flag.Parse()

	logger := log.New(os.Stdout, "", log.LstdFlags|log.Lmsgprefix)
	if *logLevel == "debug" {
		log.SetFlags(log.LstdFlags | log.Lmsgprefix | log.Lshortfile)
		logger.SetFlags(log.LstdFlags | log.Lmsgprefix | log.Lshortfile)
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "配置错误: %v\n\n提示: 复制 config.example.json 并按注释填写。\n", err)
		os.Exit(2)
	}

	// Expand ${VAR} in credentials via environment.
	domain := feishu.DomainFeishu
	if cfg.Domain == "lark" {
		domain = feishu.DomainLark
	}

	cli := feishu.New(cfg.AppID, cfg.AppSecr, domain, feishu.Options{})

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	b := bridge.New(cfg, cli, logger)

	logger.Printf("feishubridge %s (commit %s, built %s)",
		orDev(Version), orDev(Commit), orDev(BuildTime))

	logger.Printf("starting feishubridge: %d bot(s), domain=%s, agent=%s, workspace=%s",
		len(cfg.Bots), cfg.Domain, cfg.Agent.Command, cfg.WorkDir)

	// Print a short summary of what we're about to do so operators can see
	// at a glance whether the wrong bot/workspace was picked up.
	for i := range cfg.Bots {
		boot := &cfg.Bots[i]
		logger.Printf("  bot %s: workspace=%s shell=%s group_mode=%s users=%v",
			boot.ID, boot.Workspace, boot.Shell, boot.GroupMode, boot.AllowedUsers)
	}

	if err := b.Run(ctx); err != nil {
		if ctx.Err() == context.Canceled {
			logger.Printf("shutting down")
			return
		}
		logger.Printf("fatal: %v", err)
		os.Exit(1)
	}

	// Give outbound calls a moment to drain.
	time.Sleep(200 * time.Millisecond)
}

// orDev renders a build-metadata field, falling back to a placeholder so an
// unstamped local build is recognisable in the logs.
func orDev(s string) string {
	if s == "" {
		return "(not stamped)"
	}
	return s
}
