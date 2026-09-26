// Package config loads and validates the bridge configuration.
//
// Resolution order:
//  1. explicit path passed via -config
//  2. $FEISHU_BRIDGE_CONFIG
//  3. ~/.config/feishu-bridge/config.json (Linux/macOS)
//     %USERPROFILE%\.config\feishu-bridge\config.json (Windows)
package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Config is the root document.
type Config struct {
	AppID   string      `json:"app_id"`
	AppSecr string      `json:"app_secret"`
	Domain  string      `json:"domain"` // "feishu" or "lark"
	WorkDir string      `json:"default_workspace"`
	Agent   AgentConfig `json:"agent"`
	Bots    []BotConfig `json:"bots"`
	// BotChats maps chat_id -> bot id. Only needed with more than one bot.
	BotChats map[string]string `json:"bot_chats"`
	Stream   StreamCfg         `json:"streaming"`
}

// AgentConfig describes how a Claude Code session is launched.
type AgentConfig struct {
	Command    string `json:"command"`
	Model      string `json:"model"`
	TimeoutSec int    `json:"timeout_seconds"`
	SilentSec  int    `json:"silent_timeout_seconds"`
	AppendSys  string `json:"append_system_prompt"`
	SettingSrc string `json:"setting_sources"`
}

// BotConfig is one named bot: its own workspace, shell, and ACL.
type BotConfig struct {
	ID           string   `json:"id"`
	DisplayName  string   `json:"display_name"`
	Workspace    string   `json:"workspace"`
	Shell        string   `json:"shell"`
	AllowedUsers []string `json:"allowed_users"`
	GroupMode    string   `json:"group_mode"`
	OwnerOpenID  string   `json:"owner_open_id"`
	ExtraCLIArgs []string `json:"extra_cli_args"`
}

// StreamCfg controls card streaming and shell lifecycle behaviour.
type StreamCfg struct {
	ThrottleMs   int `json:"throttle_ms"`
	MaxCardBytes int `json:"max_card_bytes"`
	MaxDivChars  int `json:"max_div_chars"`
	// ShellIdleSec stops a chat's pwsh after this much silence. 0 disables.
	ShellIdleSec int `json:"shell_idle_seconds"`
	// SessionIdleSec drops a Claude session record after this much silence.
	// Longer than shell idle: a session is cheap while a pwsh process is not.
	SessionIdleSec int `json:"session_idle_seconds"`
}

// EnvPattern matches ${VAR} placeholders.
var EnvPattern = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// Load reads, expands env placeholders and validates the config.
func Load(path string) (*Config, error) {
	if path == "" {
		path = ResolvePath()
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %q: %w", path, err)
	}

	var cfg Config
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("parse config %q: %w", path, err)
	}

	cfg.expandEnv()
	if err := cfg.normalize(); err != nil {
		return nil, err
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// ResolvePath walks the discovery chain.
func ResolvePath() string {
	if p := os.Getenv("FEISHU_BRIDGE_CONFIG"); p != "" {
		return p
	}
	home, err := os.UserHomeDir()
	if err == nil {
		return filepath.Join(home, ".config", "feishu-bridge", "config.json")
	}
	return "config.json"
}

func (c *Config) expandEnv() {
	expand := func(s string) string {
		if s == "" {
			return s
		}
		return EnvPattern.ReplaceAllStringFunc(s, func(m string) string {
			key := EnvPattern.FindStringSubmatch(m)[1]
			return os.Getenv(key)
		})
	}
	c.AppID = expand(c.AppID)
	c.AppSecr = expand(c.AppSecr)
	c.WorkDir = expand(c.WorkDir)
	for i := range c.Bots {
		c.Bots[i].Workspace = expand(c.Bots[i].Workspace)
		c.Bots[i].OwnerOpenID = expand(c.Bots[i].OwnerOpenID)
	}
}

// normalize fills in defaults for anything the operator left empty.
func (c *Config) normalize() error {
	if len(c.Bots) == 0 {
		return fmt.Errorf("config: at least one entry in \"bots\" is required")
	}

	if c.Domain == "" {
		c.Domain = "feishu"
	}
	if c.Agent.Command == "" {
		c.Agent.Command = "claude"
	}
	if c.Agent.TimeoutSec <= 0 {
		c.Agent.TimeoutSec = 300
	}
	if c.Agent.SilentSec <= 0 {
		c.Agent.SilentSec = 480
	}
	if c.Stream.ThrottleMs <= 0 {
		c.Stream.ThrottleMs = 800
	}
	if c.Stream.MaxCardBytes <= 0 {
		c.Stream.MaxCardBytes = 28 * 1024
	}
	if c.Stream.MaxDivChars <= 0 {
		c.Stream.MaxDivChars = 10000
	}
	if c.Stream.ShellIdleSec < 0 {
		c.Stream.ShellIdleSec = 0
	}
	if c.Stream.SessionIdleSec <= 0 {
		// One week. A session is cheap while a pwsh process is not, so this
		// is much longer than the shell threshold.
		c.Stream.SessionIdleSec = 7 * 24 * 3600
	}

	seen := make(map[string]bool, len(c.Bots))
	for i := range c.Bots {
		b := &c.Bots[i]
		if b.ID == "" {
			b.ID = fmt.Sprintf("bot%d", i+1)
		}
		if seen[b.ID] {
			return fmt.Errorf("config: duplicate bot id %q", b.ID)
		}
		seen[b.ID] = true

		if b.DisplayName == "" {
			b.DisplayName = b.ID
		}
		if b.Shell == "" {
			b.Shell = "pwsh"
		}
		if b.Workspace == "" {
			b.Workspace = c.WorkDir
		}
		if b.GroupMode == "" {
			b.GroupMode = "mention-all"
		}
		if !validGroupMode(b.GroupMode) {
			return fmt.Errorf("config: bot %q has invalid group_mode %q (want disabled, mention-all, or owner-only)", b.ID, b.GroupMode)
		}
		if len(b.AllowedUsers) == 0 {
			b.AllowedUsers = []string{"*"}
		}
	}
	return nil
}

func validGroupMode(m string) bool {
	switch m {
	case "disabled", "mention-all", "owner-only":
		return true
	}
	return false
}

// Validate enforces constraints that cannot be defaulted.
func (c *Config) Validate() error {
	if strings.TrimSpace(c.AppID) == "" {
		return fmt.Errorf("config: app_id is empty (set FEISHU_APP_ID or config.app_id)")
	}
	if strings.TrimSpace(c.AppSecr) == "" {
		return fmt.Errorf("config: app_secret is empty (set FEISHU_APP_SECRET or config.app_secret)")
	}
	if strings.ContainsAny(c.AppSecr, "\n\r\t") {
		return fmt.Errorf("config: app_secret must not contain whitespace")
	}
	for i := range c.Bots {
		b := &c.Bots[i]
		if !filepath.IsAbs(b.Workspace) {
			abs, err := filepath.Abs(b.Workspace)
			if err != nil {
				return fmt.Errorf("config: bot %q workspace %q: %w", b.ID, b.Workspace, err)
			}
			b.Workspace = abs
		}
	}

	// bot_chats points every mapped chat at a real bot; a typo here would
	// silently route all traffic to the fallback, so fail loudly instead.
	byID := make(map[string]bool, len(c.Bots))
	for i := range c.Bots {
		byID[c.Bots[i].ID] = true
	}
	for chatID, botID := range c.BotChats {
		if !byID[botID] {
			return fmt.Errorf("config: bot_chats[%q] references unknown bot %q", chatID, botID)
		}
	}
	return nil
}

// FindBot returns the BotConfig for id, or nil.
func (c *Config) FindBot(id string) *BotConfig {
	for i := range c.Bots {
		if c.Bots[i].ID == id {
			return &c.Bots[i]
		}
	}
	return nil
}

// AllowedUser reports whether openID may drive this bot.
func (b *BotConfig) AllowedUser(openID string) bool {
	for _, u := range b.AllowedUsers {
		if u == "*" {
			return true
		}
		if u == openID {
			return true
		}
	}
	return false
}
