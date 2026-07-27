// Package transcript parses agent session transcripts (JSONL) and extracts
// forwardable turn content incrementally. Each supported agent has its own
// format behind the Source interface: this file and the Entry filter handle
// Claude Code, codex.go handles Codex rollouts.
//
// Both formats are internal to their agents and undocumented; the rules here
// were derived from real transcripts (see docs/design.md) and are pinned by
// the fixtures in testdata/. When an agent update changes its format, those
// fixtures are the tripwire.
package transcript

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"os"

	"github.com/neguse/ag-share/internal/backend"
)

const maxTranscriptLineSize = 16 * 1024 * 1024

// Entry is one transcript line. Only fields the filter needs are declared;
// unknown fields are ignored. Lines that fail to parse are skipped, not fatal.
type Entry struct {
	Type             string          `json:"type"` // "user", "assistant", "system", ...
	UUID             string          `json:"uuid"`
	IsMeta           bool            `json:"isMeta"`           // skill/command expansions
	IsCompactSummary bool            `json:"isCompactSummary"` // /compact summary entry
	AiTitle          string          `json:"aiTitle"`          // "ai-title" records only
	Message          json.RawMessage `json:"message"`
}

// UserPromptText returns the entry's content when it is a forwardable user
// prompt: Type "user", message.content a plain JSON string, and neither
// IsMeta nor IsCompactSummary. Tool results and skill expansions have array
// content and therefore do not match.
func (e Entry) UserPromptText() (string, bool) {
	if e.Type != "user" || e.IsMeta || e.IsCompactSummary {
		return "", false
	}

	var message struct {
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(e.Message, &message); err != nil {
		return "", false
	}
	content := bytes.TrimSpace(message.Content)
	if len(content) == 0 || content[0] != '"' {
		return "", false
	}

	var text string
	if err := json.Unmarshal(content, &text); err != nil {
		return "", false
	}
	return text, true
}

// AssistantContent returns (texts, toolCalls) for an assistant entry:
// the "text" blocks of message.content (in practice one block per entry) and
// the number of "tool_use" blocks. "thinking" blocks are never returned.
// Non-assistant entries yield (nil, 0).
func (e Entry) AssistantContent() (texts []string, toolCalls int) {
	if e.Type != "assistant" {
		return nil, 0
	}

	var message struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(e.Message, &message); err != nil {
		return nil, 0
	}
	for _, block := range message.Content {
		switch block.Type {
		case "text":
			texts = append(texts, block.Text)
		case "tool_use":
			toolCalls++
		}
	}
	return texts, toolCalls
}

// ReadEntries reads a Claude Code transcript file. Unparsable lines are
// skipped silently (format drift must degrade, not break). An error is
// returned only for file-level failures (open/read). A missing file is a
// valid empty transcript: (nil, nil) — enablement can happen before the first
// entry is written.
func ReadEntries(path string) ([]Entry, error) {
	var entries []Entry
	err := readLines(path, func(line []byte) {
		var entry Entry
		if err := json.Unmarshal(line, &entry); err == nil {
			entries = append(entries, entry)
		}
	})
	return entries, err
}

// readLines streams a JSONL file line by line. A missing file is a valid
// empty transcript (no callback, nil error); errors are file-level only.
func readLines(path string, handle func(line []byte)) error {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), maxTranscriptLineSize)
	for scanner.Scan() {
		handle(scanner.Bytes())
	}
	return scanner.Err()
}

// LatestTitle returns the newest auto-generated session title ("ai-title"
// records, written by Claude Code after turns in interactive sessions; print
// mode never writes them), or "" if none exists yet.
func LatestTitle(entries []Entry) string {
	for i := len(entries) - 1; i >= 0; i-- {
		if entries[i].Type == "ai-title" && entries[i].AiTitle != "" {
			return entries[i].AiTitle
		}
	}
	return ""
}

// LatestUUID returns the UUID of the last entry that has one, or "" for an
// empty transcript. Used to set the cursor on enablement.
func LatestUUID(entries []Entry) string {
	for i := len(entries) - 1; i >= 0; i-- {
		if entries[i].UUID != "" {
			return entries[i].UUID
		}
	}
	return ""
}

// Chunk is one turn-sized unit of forwardable content: it becomes one thread
// reply. LastUUID is the cursor position after this chunk — the UUID of the
// last entry consumed before the next chunk starts (including non-forwardable
// entries in between), so posting chunk-by-chunk advances the cursor safely.
type Chunk struct {
	Turn     backend.Turn
	LastUUID string
}

// SplitAfter collects forwardable content strictly after the entry with UUID
// afterUUID, split at turn boundaries: a forwardable user prompt starts a new
// chunk once the current chunk already carries assistant content (queued
// prompts with no response in between stay grouped).
//
//   - afterUUID == "": the range is the whole transcript (default-on repos,
//     share-from-begin backlog replay).
//   - afterUUID not found: returns cursorFound=false and no chunks — the
//     caller resets the cursor to LatestUUID and skips the range (never post
//     retroactively on ambiguity).
//   - lastUUID is the UUID of the last entry in the file that has one; the
//     final chunk's LastUUID equals it.
func SplitAfter(entries []Entry, afterUUID string) (chunks []Chunk, lastUUID string, cursorFound bool) {
	lastUUID = LatestUUID(entries)
	start := 0
	cursorFound = afterUUID == ""
	if !cursorFound {
		for i, entry := range entries {
			if entry.UUID == afterUUID {
				start = i + 1
				cursorFound = true
				break
			}
		}
	}
	if !cursorFound {
		return nil, lastUUID, false
	}

	var current backend.Turn
	cut := func(uuid string) {
		if !current.Empty() || current.ToolCalls > 0 {
			chunks = append(chunks, Chunk{Turn: current, LastUUID: uuid})
			current = backend.Turn{}
		}
	}
	prevUUID := afterUUID
	for _, entry := range entries[start:] {
		if prompt, ok := entry.UserPromptText(); ok {
			if len(current.Texts) > 0 || current.ToolCalls > 0 {
				cut(prevUUID)
			}
			current.UserPrompts = append(current.UserPrompts, prompt)
		} else {
			texts, toolCalls := entry.AssistantContent()
			current.Texts = append(current.Texts, texts...)
			current.ToolCalls += toolCalls
		}
		if entry.UUID != "" {
			prevUUID = entry.UUID
		}
	}
	cut(lastUUID)
	// Trailing entries with no forwardable content still advance the cursor.
	if n := len(chunks); n > 0 {
		chunks[n-1].LastUUID = lastUUID
	}
	return chunks, lastUUID, true
}

// ExtractAfter is SplitAfter merged into a single Turn; kept for callers that
// do not need turn boundaries.
func ExtractAfter(entries []Entry, afterUUID string) (turn backend.Turn, lastUUID string, cursorFound bool) {
	chunks, lastUUID, cursorFound := SplitAfter(entries, afterUUID)
	if !cursorFound {
		return backend.Turn{}, lastUUID, false
	}
	for _, c := range chunks {
		turn.UserPrompts = append(turn.UserPrompts, c.Turn.UserPrompts...)
		turn.Texts = append(turn.Texts, c.Turn.Texts...)
		turn.ToolCalls += c.Turn.ToolCalls
	}
	return turn, lastUUID, true
}
