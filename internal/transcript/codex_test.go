package transcript

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

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

// Codex flushes task_complete after firing Stop; OpenWait must pick up the
// record when it lands mid-wait.
func TestOpenWaitPicksUpLateTaskComplete(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rollout.jsonl")
	turn1 := `{"type":"event_msg","payload":{"type":"user_message","message":"p"}}` + "\n" +
		`{"type":"event_msg","payload":{"type":"agent_message","message":"a"}}` + "\n"
	if err := os.WriteFile(path, []byte(turn1), 0o600); err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(50 * time.Millisecond)
		f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			return
		}
		defer f.Close()
		_, _ = f.WriteString(`{"type":"event_msg","payload":{"type":"task_complete","turn_id":"t1"}}` + "\n")
	}()

	src, err := OpenWait("codex", path, "t1", 2*time.Second, 10*time.Millisecond)
	if err != nil {
		t.Fatalf("OpenWait: %v", err)
	}
	chunks, _, found := src.SplitAfter("")
	if !found || len(chunks) != 1 || chunks[0].LastUUID != "t1" {
		t.Fatalf("chunks = %+v found=%v, want the completed turn t1", chunks, found)
	}
}

// A turn that never lands (abort, format drift) must not block past the
// bound; the transcript is returned as-is.
func TestOpenWaitTimesOutAndReturns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rollout.jsonl")
	if err := os.WriteFile(path, []byte(`{"type":"event_msg","payload":{"type":"user_message","message":"p"}}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	src, err := OpenWait("codex", path, "never", 150*time.Millisecond, 20*time.Millisecond)
	if err != nil {
		t.Fatalf("OpenWait: %v", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("OpenWait blocked %v; want bounded wait", elapsed)
	}
	if _, _, found := src.SplitAfter("never"); found {
		t.Fatal("turn must still be absent after timeout")
	}
}

// Claude Code sends no turn_id; OpenWait must not wait at all.
func TestOpenWaitEmptyTurnIDSkipsWait(t *testing.T) {
	start := time.Now()
	if _, err := OpenWait("claude", filepath.Join(t.TempDir(), "absent.jsonl"), "", 2*time.Second, 100*time.Millisecond); err != nil {
		t.Fatalf("OpenWait: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("OpenWait waited %v with empty turn ID", elapsed)
	}
}
