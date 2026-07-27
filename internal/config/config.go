// Package config loads the machine-local user configuration and resolves the
// current repository's identity. Nothing here is ever committed into target
// repositories: destinations are each user's own decision.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// BaseDir is ag-share's per-user data directory. All state, logs, and cached
// binaries live under it. AG_SHARE_HOME overrides it (tests, development);
// otherwise it is $XDG_CONFIG_HOME/ag-share, defaulting to ~/.config/ag-share.
// The location is agent-neutral on purpose: one config serves every supported
// agent (Claude Code, Codex), so it cannot live under either agent's dotdir.
func BaseDir() string {
	if dir := os.Getenv("AG_SHARE_HOME"); dir != "" {
		return dir
	}
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, "ag-share")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".ag-share" // last resort; callers treat errors as "log and exit 0"
	}
	return filepath.Join(home, ".config", "ag-share")
}

// RepoEntry is one destination: which backend, its credentials/destination
// fields, and whether sessions in this repo share by default.
//
// Service selects the backend and which of the remaining fields it reads.
// Slack fields: BotToken (xoxb-, scope chat:write only), Channel (channel ID).
// Future backends add their own fields here; unknown JSON fields are ignored.
type RepoEntry struct {
	Service  string `json:"service"`
	Default  string `json:"default,omitempty"` // "on" | "off" (empty = "off")
	BotToken string `json:"bot_token,omitempty"`
	Channel  string `json:"channel,omitempty"`
}

// DefaultOn reports whether sessions in this repo are enabled from the start.
func (e RepoEntry) DefaultOn() bool { return e.Default == "on" }

// Config is the whole user config: one flat map keyed by repo identity
// (see RepoIdentity) or by absolute path for repos without a remote.
type Config struct {
	Repos map[string]RepoEntry `json:"repos"`
}

// ErrNotConfigured is returned by Lookup when no entry matches the repo.
var ErrNotConfigured = errors.New("no destination configured for this repo")

// Load reads BaseDir()/config.json. A missing file yields an empty Config and
// no error (the common case: ag-share installed but this machine/user has no
// destinations yet). If the file has permissions wider than owner-only, Load
// still succeeds but the caller should log a warning (LoadWarnings reports it).
func Load() (Config, error) {
	data, err := os.ReadFile(filepath.Join(BaseDir(), "config.json"))
	if errors.Is(err, os.ErrNotExist) {
		return Config{Repos: make(map[string]RepoEntry)}, nil
	}
	if err != nil {
		return Config{}, err
	}

	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return Config{}, err
	}
	if cfg.Repos == nil {
		cfg.Repos = make(map[string]RepoEntry)
	}
	return cfg, nil
}

// LoadWarnings returns human-readable, non-fatal findings about the config
// (currently: config file readable by others). Best-effort; never fails.
func LoadWarnings() []string {
	if runtime.GOOS == "windows" {
		return nil
	}

	path := filepath.Join(BaseDir(), "config.json")
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm()&0o077 == 0 {
		return nil
	}
	return []string{fmt.Sprintf(
		"config file %s has permissions %04o; expected owner-only permissions",
		path,
		info.Mode().Perm(),
	)}
}

// Lookup resolves the destination for a repo key. An exact key wins; failing
// that, keys ending in "/*" match by prefix ("github.com/neguse/*" matches
// every repo under that owner), longest prefix first. Returns ErrNotConfigured
// if nothing matches, or a validation error if the entry is malformed
// (unknown service, missing required backend fields).
func (c Config) Lookup(repoKey string) (RepoEntry, error) {
	entry, ok := c.Repos[repoKey]
	if !ok {
		bestLen := -1
		for k, e := range c.Repos {
			prefix, wild := strings.CutSuffix(k, "/*")
			if wild && strings.HasPrefix(repoKey, prefix+"/") && len(prefix) > bestLen {
				bestLen = len(prefix)
				entry = e
				ok = true
			}
		}
	}
	if !ok {
		return RepoEntry{}, ErrNotConfigured
	}
	switch entry.Service {
	case "slack", "discord":
	default:
		return RepoEntry{}, fmt.Errorf("repo %q has unsupported service %q", repoKey, entry.Service)
	}
	if entry.BotToken == "" {
		return RepoEntry{}, fmt.Errorf("repo %q %s destination is missing bot_token", repoKey, entry.Service)
	}
	if entry.Channel == "" {
		return RepoEntry{}, fmt.Errorf("repo %q %s destination is missing channel", repoKey, entry.Service)
	}
	return entry, nil
}

// RepoIdentity resolves cwd to the config key for the current repository:
//
//   - If cwd is inside a git work tree with an "origin" remote, the remote URL
//     normalized to "host/owner/repo": scheme, credentials, ssh "user@host:"
//     form, and a trailing ".git" are all stripped.
//     git@github.com:acme/product.git and https://github.com/acme/product
//     both become "github.com/acme/product".
//   - Otherwise (no git, no origin), the git top-level directory if available,
//     else cwd itself: absolute, cleaned, with forward slashes, no trailing
//     slash. Users put that path in config verbatim.
//
// Never fails: on any error it falls back to the path form.
func RepoIdentity(cwd string) string {
	remote, err := exec.Command("git", "-C", cwd, "remote", "get-url", "origin").Output()
	if err == nil {
		if identity, ok := normalizeRemote(string(remote)); ok {
			return identity
		}
	}

	topLevel, err := exec.Command("git", "-C", cwd, "rev-parse", "--show-toplevel").Output()
	if err == nil {
		return pathIdentity(strings.TrimSpace(string(topLevel)))
	}
	return pathIdentity(cwd)
}

func normalizeRemote(remote string) (string, bool) {
	remote = strings.TrimSpace(remote)
	if remote == "" {
		return "", false
	}

	if strings.Contains(remote, "://") {
		parsed, err := url.Parse(remote)
		if err != nil || parsed.Host == "" {
			return "", false
		}
		return joinRemoteIdentity(parsed.Host, parsed.Path)
	}

	// Git's scp-like syntax is [user@]host:path.
	if colon := strings.IndexByte(remote, ':'); colon > 0 {
		authority := remote[:colon]
		if !strings.ContainsAny(authority, `/\`) {
			if at := strings.LastIndexByte(authority, '@'); at >= 0 {
				authority = authority[at+1:]
			}
			return joinRemoteIdentity(authority, remote[colon+1:])
		}
	}

	return "", false
}

func joinRemoteIdentity(host, repoPath string) (string, bool) {
	host = strings.TrimSpace(host)
	repoPath = strings.ReplaceAll(repoPath, `\`, "/")
	repoPath = strings.Trim(repoPath, "/")
	repoPath = strings.TrimSuffix(repoPath, ".git")
	repoPath = strings.TrimSuffix(repoPath, "/")
	if host == "" || repoPath == "" {
		return "", false
	}
	return host + "/" + repoPath, true
}

func pathIdentity(path string) string {
	absolute, err := filepath.Abs(path)
	if err == nil {
		path = absolute
	}
	path = filepath.Clean(path)
	path = filepath.ToSlash(path)

	trimmed := strings.TrimRight(path, "/")
	if trimmed == "" || (len(trimmed) == 2 && trimmed[1] == ':') {
		return path
	}
	return trimmed
}
