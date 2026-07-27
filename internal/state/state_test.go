package state

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestLoadAndSave(t *testing.T) {
	tests := []struct {
		name    string
		session *Session
	}{
		{
			name: "absent",
		},
		{
			name: "round trip",
			session: &Session{
				Enabled:        true,
				Repo:           "github.com/acme/product",
				Service:        "slack",
				Channel:        "C1",
				ThreadRef:      "123.456",
				LastPostedUUID: "a6",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("AG_SHARE_HOME", t.TempDir())
			const sessionID = "session-1"

			if tt.session != nil {
				if err := Save(sessionID, tt.session); err != nil {
					t.Fatalf("Save() error = %v", err)
				}
				updated := *tt.session
				updated.ThreadRef = "789.012"
				if err := Save(sessionID, &updated); err != nil {
					t.Fatalf("second Save() error = %v", err)
				}
				tt.session = &updated
			}

			got, err := Load(sessionID)
			if err != nil {
				t.Fatalf("Load() error = %v", err)
			}
			if !reflect.DeepEqual(got, tt.session) {
				t.Fatalf("Load() = %#v, want %#v", got, tt.session)
			}
		})
	}
}

func TestCleanupStale(t *testing.T) {
	t.Setenv("AG_SHARE_HOME", t.TempDir())

	tests := []struct {
		name    string
		modTime time.Time
		removed bool
	}{
		{
			name:    "old",
			modTime: time.Now().Add(-8 * 24 * time.Hour),
			removed: true,
		},
		{
			name:    "fresh",
			modTime: time.Now(),
			removed: false,
		},
	}

	for _, tt := range tests {
		if err := Save(tt.name, &Session{Repo: tt.name}); err != nil {
			t.Fatalf("Save(%q) error = %v", tt.name, err)
		}
		path := filepath.Join(sessionsDir(), tt.name+".json")
		if err := os.Chtimes(path, tt.modTime, tt.modTime); err != nil {
			t.Fatal(err)
		}
	}

	CleanupStale(7 * 24 * time.Hour)

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := os.Stat(filepath.Join(sessionsDir(), tt.name+".json"))
			if tt.removed && !os.IsNotExist(err) {
				t.Fatalf("old state Stat() error = %v, want not exist", err)
			}
			if !tt.removed && err != nil {
				t.Fatalf("fresh state Stat() error = %v", err)
			}
		})
	}
}
