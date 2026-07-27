// Codex rollout parsing. A Codex session transcript ("rollout",
// ~/.codex/sessions/YYYY/MM/DD/rollout-*.jsonl) is JSONL of
// {timestamp, type, payload} records. Like the Claude format it is internal
// and undocumented (the hooks doc says so explicitly); the rules here were
// derived from real rollouts and are pinned by testdata/codex-rollout.jsonl.
//
// Forwardable content lives in "event_msg" records:
//
//   - payload.type "user_message":  the user prompt (payload.message)
//   - payload.type "agent_message": one assistant text block (payload.message)
//   - payload.type "task_complete": end of a turn; payload.turn_id is the
//     cursor. Rollout lines have no per-line UUID, so turns are the cursor
//     unit — LastPostedUUID stores the last posted turn's ID.
//
// Tool calls are "response_item" records with payload.type "function_call".
// Everything else (session_meta, turn_context, reasoning, token_count, ...)
// is skipped.
//
// A turn's task_complete record is written only AFTER the turn's Stop hooks
// finish (verified empirically: the record's timestamp tracks the hook's
// exit, and a Stop hook that waited for it stalled Codex for the full wait).
// The Stop hook can therefore never observe its own turn as complete;
// instead, the content after the last task_complete IS the completing turn,
// and it is attributed to the Stop payload's turn_id (stopTurnID). Without a
// stopTurnID (UserPromptSubmit, format drift) trailing content is left alone
// — on ambiguity, never post.
package transcript

import (
	"encoding/json"

	"github.com/neguse/ag-share/internal/backend"
)

// codexEntry is one rollout record reduced to what extraction needs.
type codexEntry struct {
	kind   codexKind
	text   string // prompt or agent text
	turnID string // turnEnd only
}

type codexKind int

const (
	codexSkip codexKind = iota
	codexPrompt
	codexText
	codexToolCall
	codexTurnEnd
)

func parseCodexLine(line []byte) codexEntry {
	var record struct {
		Type    string `json:"type"`
		Payload struct {
			Type    string `json:"type"`
			Message string `json:"message"`
			TurnID  string `json:"turn_id"`
		} `json:"payload"`
	}
	if err := json.Unmarshal(line, &record); err != nil {
		return codexEntry{}
	}
	switch record.Type {
	case "event_msg":
		switch record.Payload.Type {
		case "user_message":
			return codexEntry{kind: codexPrompt, text: record.Payload.Message}
		case "agent_message":
			return codexEntry{kind: codexText, text: record.Payload.Message}
		case "task_complete":
			return codexEntry{kind: codexTurnEnd, turnID: record.Payload.TurnID}
		}
	case "response_item":
		if record.Payload.Type == "function_call" {
			return codexEntry{kind: codexToolCall}
		}
	}
	return codexEntry{}
}

func readCodexEntries(path string) ([]codexEntry, error) {
	var entries []codexEntry
	err := readLines(path, func(line []byte) {
		if entry := parseCodexLine(line); entry.kind != codexSkip {
			entries = append(entries, entry)
		}
	})
	return entries, err
}

// codexSource implements Source over parsed rollout entries. stopTurnID is
// the completing turn's ID from the Stop payload ("" outside Stop).
type codexSource struct {
	entries    []codexEntry
	stopTurnID string
}

// LatestCursor is the last completed turn's ID; "" if no turn has completed.
func (s codexSource) LatestCursor() string {
	for i := len(s.entries) - 1; i >= 0; i-- {
		if s.entries[i].kind == codexTurnEnd {
			return s.entries[i].turnID
		}
	}
	return ""
}

// Title returns "": rollouts carry no auto-generated title record (Codex
// keeps thread names in a separate index file, another unstable internal —
// the prompt-head fallback covers the topic instead).
func (s codexSource) Title() string { return "" }

// SplitAfter emits one Chunk per completed turn after the cursor, plus — when
// stopTurnID is set — one Chunk for the trailing in-flight content, cursored
// as stopTurnID (its task_complete appears only after this hook exits; the
// next SplitAfter finds it as a regular turn boundary). Empty complete turns
// advance the cursor of the preceding chunk (never posted).
func (s codexSource) SplitAfter(cursor string) (chunks []Chunk, latest string, cursorFound bool) {
	latest = s.LatestCursor()
	start := len(s.entries)
	// The cursor can be a stopTurnID posted by a previous Stop whose
	// task_complete had not landed yet at that Stop's read (or never landed:
	// crash, kill). Matching it directly — not only via a task_complete
	// record — keeps such a cursor valid instead of triggering a reset, and
	// a duplicate Stop for the same turn yields nothing new.
	cursorFound = cursor == "" || cursor == s.stopTurnID
	if cursor == "" {
		start = 0
	}
	for i, entry := range s.entries {
		if cursor != "" && entry.kind == codexTurnEnd && entry.turnID == cursor {
			start = i + 1
			cursorFound = true
			break
		}
	}
	if !cursorFound {
		return nil, latest, false
	}

	var current backend.Turn
	lastEnd := ""
	cut := func(turnID string) {
		if !current.Empty() || current.ToolCalls > 0 {
			chunks = append(chunks, Chunk{Turn: current, LastUUID: turnID})
			current = backend.Turn{}
		}
	}
	for _, entry := range s.entries[start:] {
		switch entry.kind {
		case codexPrompt:
			current.UserPrompts = append(current.UserPrompts, entry.text)
		case codexText:
			current.Texts = append(current.Texts, entry.text)
		case codexToolCall:
			current.ToolCalls++
		case codexTurnEnd:
			lastEnd = entry.turnID
			cut(entry.turnID)
		}
	}
	if s.stopTurnID != "" && s.stopTurnID != cursor {
		// The completing turn: its content is fully flushed, only its
		// task_complete is pending on this very hook's exit.
		before := len(chunks)
		cut(s.stopTurnID)
		if len(chunks) > before {
			lastEnd = s.stopTurnID
		}
	}
	// Trailing empty complete turns still advance the cursor.
	if n := len(chunks); n > 0 && lastEnd != "" {
		chunks[n-1].LastUUID = lastEnd
	}
	return chunks, latest, true
}
