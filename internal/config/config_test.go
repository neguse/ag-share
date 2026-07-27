package config

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

func TestLoad(t *testing.T) {
	tests := []struct {
		name      string
		content   *string
		wantRepos map[string]RepoEntry
		wantErr   bool
	}{
		{
			name:      "missing",
			wantRepos: map[string]RepoEntry{},
		},
		{
			name:    "valid",
			content: stringPointer(`{"repos":{"github.com/acme/product":{"service":"slack","bot_token":"token","channel":"C1"}}}`),
			wantRepos: map[string]RepoEntry{
				"github.com/acme/product": {
					Service:  "slack",
					BotToken: "token",
					Channel:  "C1",
				},
			},
		},
		{
			name:    "malformed",
			content: stringPointer(`{"repos":`),
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("AG_SHARE_HOME", dir)
			if tt.content != nil {
				if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(*tt.content), 0o600); err != nil {
					t.Fatal(err)
				}
			}

			cfg, err := Load()
			if (err != nil) != tt.wantErr {
				t.Fatalf("Load() error = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr && !reflect.DeepEqual(cfg.Repos, tt.wantRepos) {
				t.Fatalf("Load().Repos = %#v, want %#v", cfg.Repos, tt.wantRepos)
			}
		})
	}
}

func TestLoadWarnings(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("AG_SHARE_HOME", dir)
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"repos":{}}`), 0o644); err != nil {
		t.Fatal(err)
	}

	warnings := LoadWarnings()
	if runtime.GOOS == "windows" {
		if warnings != nil {
			t.Fatalf("LoadWarnings() = %#v, want nil on Windows", warnings)
		}
		return
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "owner-only") {
		t.Fatalf("LoadWarnings() = %#v, want one owner-only warning", warnings)
	}

	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if warnings := LoadWarnings(); len(warnings) != 0 {
		t.Fatalf("LoadWarnings() = %#v after chmod 0600, want none", warnings)
	}
}

func TestLookup(t *testing.T) {
	const key = "github.com/acme/product"
	tests := []struct {
		name      string
		cfg       Config
		key       string
		want      RepoEntry
		wantIs    error
		wantError string
	}{
		{
			name: "happy",
			cfg: Config{Repos: map[string]RepoEntry{
				key: {Service: "slack", BotToken: "xoxb-token", Channel: "C1"},
			}},
			key:  key,
			want: RepoEntry{Service: "slack", BotToken: "xoxb-token", Channel: "C1"},
		},
		{
			name:   "missing",
			cfg:    Config{Repos: map[string]RepoEntry{}},
			key:    key,
			wantIs: ErrNotConfigured,
		},
		{
			name: "unknown service",
			cfg: Config{Repos: map[string]RepoEntry{
				key: {Service: "teams", BotToken: "token", Channel: "C1"},
			}},
			key:       key,
			wantError: "unsupported service",
		},
		{
			name: "valid discord entry",
			cfg: Config{Repos: map[string]RepoEntry{
				key: {Service: "discord", BotToken: "token", Channel: "C1"},
			}},
			key:  key,
			want: RepoEntry{Service: "discord", BotToken: "token", Channel: "C1"},
		},
		{
			name: "missing bot token",
			cfg: Config{Repos: map[string]RepoEntry{
				key: {Service: "slack", Channel: "C1"},
			}},
			key:       key,
			wantError: "bot_token",
		},
		{
			name: "missing channel",
			cfg: Config{Repos: map[string]RepoEntry{
				key: {Service: "slack", BotToken: "token"},
			}},
			key:       key,
			wantError: "channel",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.cfg.Lookup(tt.key)
			if tt.wantIs != nil {
				if !errors.Is(err, tt.wantIs) {
					t.Fatalf("Lookup() error = %v, want errors.Is(_, %v)", err, tt.wantIs)
				}
				return
			}
			if tt.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantError) {
					t.Fatalf("Lookup() error = %v, want containing %q", err, tt.wantError)
				}
				return
			}
			if err != nil {
				t.Fatalf("Lookup() error = %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("Lookup() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestRepoIdentity(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not available")
	}

	remoteTests := []struct {
		name   string
		remote string
		want   string
	}{
		{
			name:   "scp syntax",
			remote: "git@github.com:acme/product.git",
			want:   "github.com/acme/product",
		},
		{
			name:   "https",
			remote: "https://github.com/acme/product.git",
			want:   "github.com/acme/product",
		},
		{
			name:   "ssh URL",
			remote: "ssh://git@github.com/acme/product",
			want:   "github.com/acme/product",
		},
	}

	for _, tt := range remoteTests {
		t.Run(tt.name, func(t *testing.T) {
			repo := t.TempDir()
			runGit(t, repo, "init", "-q")
			runGit(t, repo, "remote", "add", "origin", tt.remote)
			if got := RepoIdentity(repo); got != tt.want {
				t.Fatalf("RepoIdentity() = %q, want %q", got, tt.want)
			}
		})
	}

	t.Run("git repo without origin uses top level", func(t *testing.T) {
		repo := t.TempDir()
		runGit(t, repo, "init", "-q")
		nested := filepath.Join(repo, "nested", "deeper")
		if err := os.MkdirAll(nested, 0o755); err != nil {
			t.Fatal(err)
		}
		want := filepath.ToSlash(filepath.Clean(repo))
		if got := RepoIdentity(nested); got != want {
			t.Fatalf("RepoIdentity() = %q, want %q", got, want)
		}
	})

	t.Run("non git directory uses cwd", func(t *testing.T) {
		cwd := t.TempDir()
		want := filepath.ToSlash(filepath.Clean(cwd))
		if got := RepoIdentity(cwd); got != want {
			t.Fatalf("RepoIdentity() = %q, want %q", got, want)
		}
	})
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	commandArgs := append([]string{"-C", dir}, args...)
	if output, err := exec.Command("git", commandArgs...).CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, output)
	}
}

func stringPointer(value string) *string {
	return &value
}

func TestLookupWildcard(t *testing.T) {
	t.Parallel()

	cfg := Config{Repos: map[string]RepoEntry{
		"github.com/neguse/*":       {Service: "slack", BotToken: "owner", Channel: "C1"},
		"github.com/*":              {Service: "slack", BotToken: "host", Channel: "C2"},
		"github.com/neguse/special": {Service: "slack", BotToken: "exact", Channel: "C3"},
	}}

	got, err := cfg.Lookup("github.com/neguse/ag-share")
	if err != nil || got.BotToken != "owner" {
		t.Errorf("owner wildcard: entry=%v err=%v, want owner match", got.BotToken, err)
	}
	got, err = cfg.Lookup("github.com/neguse/special")
	if err != nil || got.BotToken != "exact" {
		t.Errorf("exact beats wildcard: entry=%v err=%v, want exact", got.BotToken, err)
	}
	got, err = cfg.Lookup("github.com/acme/product")
	if err != nil || got.BotToken != "host" {
		t.Errorf("host wildcard: entry=%v err=%v, want host match", got.BotToken, err)
	}
	if _, err = cfg.Lookup("gitlab.com/neguse/foo"); !errors.Is(err, ErrNotConfigured) {
		t.Errorf("no match: err=%v, want ErrNotConfigured", err)
	}

	ownerOnly := Config{Repos: map[string]RepoEntry{
		"github.com/neguse/*": {Service: "slack", BotToken: "owner", Channel: "C1"},
	}}
	if _, err = ownerOnly.Lookup("github.com/neguse"); !errors.Is(err, ErrNotConfigured) {
		t.Errorf("wildcard must not match its own prefix: err=%v, want ErrNotConfigured", err)
	}
	if _, err = ownerOnly.Lookup("github.com/neguse-fork/x"); !errors.Is(err, ErrNotConfigured) {
		t.Errorf("prefix boundary must be a path separator: err=%v, want ErrNotConfigured", err)
	}
}
