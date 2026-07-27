package transcript

import (
	"fmt"
	"time"
)

// Source is one parsed session transcript. Each supported agent (Claude Code,
// Codex) has its own on-disk format; behind this interface the hook logic is
// agent-agnostic. Cursor strings are opaque to callers and format-specific:
// Claude uses transcript entry UUIDs, Codex uses task_complete turn IDs.
type Source interface {
	// LatestCursor returns the cursor for "everything up to now has been
	// handled" — set on enablement so history is never posted retroactively.
	// "" means the transcript has no complete content yet.
	LatestCursor() string

	// Title returns the session's auto-generated title, or "" if the format
	// has none (yet).
	Title() string

	// SplitAfter collects forwardable content strictly after cursor, split
	// into one Chunk per turn. latest is the end-of-transcript cursor.
	// cursorFound=false means the cursor no longer exists in the transcript
	// (unknown rewrite); the caller resets to latest and skips the range.
	SplitAfter(cursor string) (chunks []Chunk, latest string, cursorFound bool)
}

// OpenWait is Open, but first waits (bounded by wait, polling every poll)
// until the transcript contains the completed turn turnID. Codex fires Stop
// before flushing the turn's task_complete record to the rollout (verified
// empirically: the cursor trailed the thread by one turn without this), so
// extraction must wait for the record to land. turnID "" skips the wait
// (Claude Code sends no turn_id, and it flushes before Stop). On timeout the
// transcript is returned as-is: the turn simply rides along at the next Stop.
func OpenWait(agent, path, turnID string, wait, poll time.Duration) (Source, error) {
	deadline := time.Now().Add(wait)
	for {
		src, err := Open(agent, path)
		if err != nil || turnID == "" {
			return src, err
		}
		if _, _, found := src.SplitAfter(turnID); found {
			return src, nil
		}
		if time.Now().After(deadline) {
			return src, nil
		}
		time.Sleep(poll)
	}
}

// Open reads the transcript at path in the named agent's format. A missing
// file is a valid empty transcript.
func Open(agent, path string) (Source, error) {
	switch agent {
	case "claude":
		entries, err := ReadEntries(path)
		if err != nil {
			return nil, err
		}
		return claudeSource{entries}, nil
	case "codex":
		entries, err := readCodexEntries(path)
		if err != nil {
			return nil, err
		}
		return codexSource{entries}, nil
	default:
		return nil, fmt.Errorf("unknown agent %q", agent)
	}
}

// claudeSource adapts the Claude Code entry functions to Source.
type claudeSource struct{ entries []Entry }

func (s claudeSource) LatestCursor() string { return LatestUUID(s.entries) }
func (s claudeSource) Title() string        { return LatestTitle(s.entries) }
func (s claudeSource) SplitAfter(cursor string) ([]Chunk, string, bool) {
	return SplitAfter(s.entries, cursor)
}
