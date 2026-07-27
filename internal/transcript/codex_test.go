package transcript

import (
	"path/filepath"
	"reflect"
	"testing"

	"github.com/neguse/ag-share/internal/backend"
)

// The fixture mirrors real rollout records (codex-cli 0.145.0): two complete
// turns and a third still in flight. It is the tripwire for Codex format
// changes, like session.jsonl is for Claude Code.
func openCodexFixture(t *testing.T) Source {
	t.Helper()
	src, err := Open("codex", filepath.Join("testdata", "codex-rollout.jsonl"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return src
}

func TestCodexLatestCursor(t *testing.T) {
	if got := openCodexFixture(t).LatestCursor(); got != "turn-2" {
		t.Errorf("LatestCursor() = %q, want %q (last completed turn)", got, "turn-2")
	}
}

func TestCodexTitleIsEmpty(t *testing.T) {
	if got := openCodexFixture(t).Title(); got != "" {
		t.Errorf("Title() = %q, want empty (rollouts carry no title)", got)
	}
}

func TestCodexSplitAfterFromStart(t *testing.T) {
	chunks, latest, found := openCodexFixture(t).SplitAfter("")
	if !found {
		t.Fatal("cursor \"\" must always be found")
	}
	if latest != "turn-2" {
		t.Errorf("latest = %q, want %q", latest, "turn-2")
	}
	want := []Chunk{
		{
			Turn: backend.Turn{
				UserPrompts: []string{"first prompt"},
				Texts:       []string{"looking at the repo first", "first answer"},
				ToolCalls:   1,
			},
			LastUUID: "turn-1",
		},
		{
			Turn: backend.Turn{
				UserPrompts: []string{"second prompt"},
				Texts:       []string{"second answer"},
				ToolCalls:   2,
			},
			LastUUID: "turn-2",
		},
	}
	if !reflect.DeepEqual(chunks, want) {
		t.Errorf("chunks = %+v, want %+v", chunks, want)
	}
}

// Content after the last task_complete belongs to an unfinished turn and must
// stay unposted: the second call picks up nothing, and the cursor stays put.
func TestCodexSplitAfterCursorExcludesInFlightTurn(t *testing.T) {
	chunks, latest, found := openCodexFixture(t).SplitAfter("turn-2")
	if !found {
		t.Fatal("cursor turn-2 must be found")
	}
	if len(chunks) != 0 {
		t.Errorf("chunks = %+v, want none (third turn has no task_complete)", chunks)
	}
	if latest != "turn-2" {
		t.Errorf("latest = %q, want %q", latest, "turn-2")
	}
}

func TestCodexSplitAfterMidCursor(t *testing.T) {
	chunks, _, found := openCodexFixture(t).SplitAfter("turn-1")
	if !found {
		t.Fatal("cursor turn-1 must be found")
	}
	if len(chunks) != 1 || chunks[0].LastUUID != "turn-2" {
		t.Fatalf("chunks = %+v, want exactly the second turn", chunks)
	}
}

// An unknown cursor means the rollout was rewritten in a way we do not
// understand: never post retroactively, signal the caller to reset.
func TestCodexSplitAfterUnknownCursor(t *testing.T) {
	chunks, latest, found := openCodexFixture(t).SplitAfter("no-such-turn")
	if found {
		t.Fatal("unknown cursor must report cursorFound=false")
	}
	if chunks != nil {
		t.Errorf("chunks = %+v, want nil", chunks)
	}
	if latest != "turn-2" {
		t.Errorf("latest = %q, want %q", latest, "turn-2")
	}
}

func TestCodexMissingFileIsEmptyTranscript(t *testing.T) {
	src, err := Open("codex", filepath.Join(t.TempDir(), "absent.jsonl"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if got := src.LatestCursor(); got != "" {
		t.Errorf("LatestCursor() = %q, want empty", got)
	}
	if chunks, _, found := src.SplitAfter(""); !found || len(chunks) != 0 {
		t.Errorf("SplitAfter(\"\") = %+v found=%v, want no chunks, found", chunks, found)
	}
}

func TestOpenRejectsUnknownAgent(t *testing.T) {
	if _, err := Open("gemini", "whatever.jsonl"); err == nil {
		t.Fatal("Open must reject unknown agents")
	}
}
