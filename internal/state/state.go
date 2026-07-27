// Package state persists per-session forwarding state under
// BaseDir()/sessions/<session_id>.json. Files are ephemeral and machine-local.
package state

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/neguse/ag-share/internal/config"
)

// Session is the forwarding state of one Claude Code session.
//
// Disabling keeps the file (Enabled=false) so a later re-enable continues the
// same thread via ThreadRef. ThreadRef is an opaque, backend-defined string
// (Slack: the parent message ts). LastPostedUUID is the transcript cursor.
type Session struct {
	Enabled        bool   `json:"enabled"`
	Repo           string `json:"repo"`
	Service        string `json:"service"`
	Channel        string `json:"channel,omitempty"`
	ThreadRef      string `json:"thread_ref,omitempty"`
	PostedTopic    string `json:"posted_topic,omitempty"` // topic currently shown on the parent message
	LastPostedUUID string `json:"last_posted_uuid,omitempty"`
}

// Load reads the state for sessionID. Returns (nil, nil) when no state file
// exists; an error only for a present-but-unreadable file.
func Load(sessionID string) (*Session, error) {
	data, err := os.ReadFile(sessionPath(sessionID))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	var session Session
	if err := json.Unmarshal(data, &session); err != nil {
		return nil, err
	}
	return &session, nil
}

// Save writes the state atomically (temp file in the same directory, then
// rename) — the Stop hook and the next prompt's UserPromptSubmit hook can
// overlap. Creates the sessions directory as needed.
func Save(sessionID string, s *Session) error {
	dir := sessionsDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}

	data, err := json.Marshal(s)
	if err != nil {
		return err
	}
	data = append(data, '\n')

	temp, err := os.CreateTemp(dir, ".session-*.tmp")
	if err != nil {
		return err
	}
	tempName := temp.Name()
	defer os.Remove(tempName)

	if _, err := temp.Write(data); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	return os.Rename(tempName, sessionPath(sessionID))
}

// CleanupStale best-effort deletes state files whose mtime is older than
// maxAge (design default: 7 days). Never fails; call it from the Stop hook.
func CleanupStale(maxAge time.Duration) {
	entries, err := os.ReadDir(sessionsDir())
	if err != nil {
		return
	}

	cutoff := time.Now().Add(-maxAge)
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() || !info.ModTime().Before(cutoff) {
			continue
		}
		_ = os.Remove(filepath.Join(sessionsDir(), entry.Name()))
	}
}

func sessionsDir() string {
	return filepath.Join(config.BaseDir(), "sessions")
}

func sessionPath(sessionID string) string {
	return filepath.Join(sessionsDir(), sessionID+".json")
}
