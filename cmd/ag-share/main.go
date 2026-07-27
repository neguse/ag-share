// Command ag-share is the hook binary behind the ag-share plugin (Claude Code
// and Codex). One subcommand per hook, both reading the hook input JSON from
// stdin (the two agents share the same hook stdin contract):
//
//	ag-share hook-prompt --agent claude|codex   UserPromptSubmit: toggle detection (may exit 2)
//	ag-share hook-stop   --agent claude|codex   Stop: transcript extraction and forwarding
//
// The agent is a registration-time fact, not a runtime guess: each agent's
// plugin manifest registers its own hooks file, which passes the matching
// --agent. It selects the transcript parser (formats differ completely).
//
// Exit discipline: hook-stop and non-toggle hook-prompt always exit 0, even on
// fatal errors (failures are logged to error.log). The single exception is a
// recognized toggle prompt, where hook-prompt exits 2 to block the prompt and
// surface feedback via stderr.
package main

import (
	"fmt"
	"os"
	"os/user"
	"strings"
	"time"

	"github.com/neguse/ag-share/internal/backend"
	"github.com/neguse/ag-share/internal/backend/discord"
	"github.com/neguse/ag-share/internal/backend/slack"
	"github.com/neguse/ag-share/internal/config"
	"github.com/neguse/ag-share/internal/hookio"
	"github.com/neguse/ag-share/internal/state"
	"github.com/neguse/ag-share/internal/transcript"
)

const (
	staleStateAge = 7 * 24 * time.Hour
	postInterval  = 300 * time.Millisecond
	finalTextWait = 500 * time.Millisecond
)

func main() {
	if len(os.Args) < 2 {
		hookio.Logf("missing subcommand")
		os.Exit(0)
	}
	agent, ok := agentFlag(os.Args[2:])
	if !ok {
		hookio.Logf("bad --agent in %v", os.Args[1:])
		os.Exit(0)
	}
	switch os.Args[1] {
	case "hook-prompt":
		os.Exit(hookPrompt(agent))
	case "hook-stop":
		hookStop(agent)
		os.Exit(0)
	default:
		hookio.Logf("unknown subcommand: %s", os.Args[1])
		os.Exit(0)
	}
}

// agentFlag parses the subcommand arguments, which are exactly
// ["--agent", <name>] or empty ("claude", for hook registrations predating the
// flag). Unknown agents are rejected so a typo cannot silently select the
// wrong transcript parser.
func agentFlag(args []string) (agent string, ok bool) {
	switch {
	case len(args) == 0:
		return "claude", true
	case len(args) == 2 && args[0] == "--agent":
		if args[1] == "claude" || args[1] == "codex" {
			return args[1], true
		}
	}
	return "", false
}

// toggleOp is one of the three toggle commands. Each is self-contained —
// mistake resistance beats orthogonality here: the bare share-on can never
// post retroactively; replaying history takes the long, explicit command.
type toggleOp int

const (
	opOn toggleOp = iota
	opOnFromBegin
	opOff
)

// toggle recognizes toggle prompts. Matching is exact (whole prompt),
// covering both the bare-word and the raw slash-command spellings (verified:
// slash commands reach the hook untouched, as typed).
func toggle(prompt string) (op toggleOp, ok bool) {
	switch prompt {
	case "share-on", "/ag-share:on":
		return opOn, true
	case "share-on-from-begin", "/ag-share:on-from-begin":
		return opOnFromBegin, true
	case "share-off", "/ag-share:off":
		return opOff, true
	}
	return 0, false
}

// hookPrompt implements UserPromptSubmit. Returns the process exit code:
// 0 to let the prompt through, 2 to block it (toggle handled; stderr is shown
// to the user).
func hookPrompt(agent string) int {
	in, err := hookio.Read(os.Stdin)
	if err != nil {
		hookio.Logf("hook-prompt: bad input: %v", err)
		return 0
	}
	op, ok := toggle(in.Prompt)
	if !ok {
		return 0
	}

	// From here on the prompt is ours: always block (exit 2), with stderr
	// explaining what happened — including failure modes, so a toggle never
	// silently becomes a prompt to the model.
	repoKey := config.RepoIdentity(in.CWD)
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "ag-share: config error (%v) — toggle ignored\n", err)
		hookio.Logf("hook-prompt: config: %v", err)
		return 2
	}
	for _, w := range config.LoadWarnings() {
		hookio.Logf("hook-prompt: %s", w)
	}
	entry, err := cfg.Lookup(repoKey)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ag-share: %v (repo %s; edit %s/config.json)\n", err, repoKey, config.BaseDir())
		return 2
	}

	s, err := state.Load(in.SessionID)
	if err != nil {
		hookio.Logf("hook-prompt: state load: %v", err)
		s = nil
	}
	if s == nil {
		s = &state.Session{Repo: repoKey, Service: entry.Service, Channel: entry.Channel}
	}

	latestCursor := func() string {
		// A missing/empty transcript file means the session has no entries
		// yet; cursor "" (from the start) is correct then.
		src, err := transcript.Open(agent, in.TranscriptPath, "")
		if err != nil {
			hookio.Logf("hook-prompt: transcript: %v", err)
			return ""
		}
		return src.LatestCursor()
	}

	var feedback string
	switch op {
	case opOn:
		if s.Enabled {
			feedback = "already ON — nothing changed"
			break
		}
		// Transitioning to enabled places the cursor at latest: neither
		// pre-toggle history nor share-off periods are ever posted implicitly.
		s.Enabled = true
		s.LastPostedUUID = latestCursor()
		feedback = fmt.Sprintf("ON — forwarding to %s (%s) from the next turn", entry.Service, s.Repo)
	case opOnFromBegin:
		s.Enabled = true
		s.LastPostedUUID = ""
		feedback = fmt.Sprintf("ON — replaying the whole session to %s (%s), one reply per turn", entry.Service, s.Repo)
		if s.ThreadRef != "" {
			feedback += " (thread already has posts; the replay will repeat them)"
		}
	case opOff:
		s.Enabled = false
		feedback = "OFF — forwarding stopped for this session"
	}
	if err := state.Save(in.SessionID, s); err != nil {
		fmt.Fprintf(os.Stderr, "ag-share: cannot save state (%v) — toggle ignored\n", err)
		hookio.Logf("hook-prompt: state save: %v", err)
		return 2
	}
	fmt.Fprintf(os.Stderr, "ag-share: %s\n", feedback)
	return 2
}

// hookStop implements Stop: extract everything after the cursor and post it
// as one thread reply. Every failure logs and returns (exit 0).
func hookStop(agent string) {
	in, err := hookio.Read(os.Stdin)
	if err != nil {
		hookio.Logf("hook-stop: bad input: %v", err)
		return
	}
	defer state.CleanupStale(staleStateAge)

	s, err := state.Load(in.SessionID)
	if err != nil {
		hookio.Logf("hook-stop: state load: %v", err)
		return
	}
	cfg, err := config.Load()
	if err != nil {
		hookio.Logf("hook-stop: config: %v", err)
		return
	}
	if s == nil {
		// No toggle happened this session; only default-on repos forward.
		repoKey := config.RepoIdentity(in.CWD)
		entry, err := cfg.Lookup(repoKey)
		if err != nil || !entry.DefaultOn() {
			return
		}
		// Cursor "" = from the session start, which is what default-on means.
		s = &state.Session{Enabled: true, Repo: repoKey, Service: entry.Service, Channel: entry.Channel}
		if err := state.Save(in.SessionID, s); err != nil {
			hookio.Logf("hook-stop: state save: %v", err)
			return
		}
	}
	if !s.Enabled {
		return
	}
	entry, err := cfg.Lookup(s.Repo)
	if err != nil {
		hookio.Logf("hook-stop: %s: %v", s.Repo, err)
		return
	}

	// Claude Code flushes the turn's final assistant entry asynchronously —
	// at Stop time the transcript may still end before it (measured ~100ms
	// late). The payload's last_assistant_message is the guaranteed copy of
	// that text, so wait until it is readable after the cursor.
	if agent == "claude" {
		transcript.AwaitFinalText(in.TranscriptPath, s.LastPostedUUID, in.LastAssistantMessage, finalTextWait)
	}
	// in.TurnID (Codex only) attributes the trailing in-flight content to the
	// completing turn — Codex writes its task_complete only after this hook
	// exits, so waiting for it would deadlock into the timeout instead.
	src, err := transcript.Open(agent, in.TranscriptPath, in.TurnID)
	if err != nil {
		hookio.Logf("hook-stop: transcript: %v", err)
		return
	}
	chunks, latest, cursorFound := src.SplitAfter(s.LastPostedUUID)
	if !cursorFound {
		// Unknown rewrite: never post retroactively on ambiguity. Reset the
		// cursor to the current end and skip this turn.
		hookio.Logf("hook-stop: cursor %s not found; resetting", s.LastPostedUUID)
		s.LastPostedUUID = latest
		if err := state.Save(in.SessionID, s); err != nil {
			hookio.Logf("hook-stop: state save: %v", err)
		}
		return
	}
	if len(chunks) == 0 {
		return
	}

	b, err := newBackend(entry)
	if err != nil {
		hookio.Logf("hook-stop: backend: %v", err)
		return
	}
	// Topic: the auto-generated session title once it exists, else (at thread
	// creation only) the head of the first forwarded prompt.
	title := src.Title()
	info := backend.SessionInfo{Agent: agent, Repo: s.Repo, User: userName(), Host: hostName(), Topic: title}
	if s.ThreadRef == "" {
		if info.Topic == "" && len(chunks[0].Turn.UserPrompts) > 0 {
			info.Topic = promptHead(chunks[0].Turn.UserPrompts[0])
		}
		ref, err := b.CreateThread(info)
		if err != nil {
			hookio.Logf("hook-stop: create thread: %v", err)
			return
		}
		s.ThreadRef = ref
		s.PostedTopic = info.Topic
		if err := state.Save(in.SessionID, s); err != nil {
			hookio.Logf("hook-stop: state save: %v", err)
			return
		}
	}
	// One reply per chunk, advancing the cursor after each success: a failure
	// or hook timeout mid-replay resumes from the last posted chunk at the
	// next Stop instead of duplicating or dropping content.
	for i, c := range chunks {
		if i > 0 {
			time.Sleep(postInterval) // stay friendly to per-channel rate limits
		}
		if err := b.PostTurn(s.ThreadRef, c.Turn); err != nil {
			hookio.Logf("hook-stop: post: %v", err)
			break
		}
		s.LastPostedUUID = c.LastUUID
		if err := state.Save(in.SessionID, s); err != nil {
			hookio.Logf("hook-stop: state save: %v", err)
			return
		}
	}
	// Best-effort topic refresh: never affects the cursor.
	if title != "" && title != s.PostedTopic {
		if err := b.UpdateThread(s.ThreadRef, info); err != nil {
			hookio.Logf("hook-stop: update thread: %v", err)
		} else {
			s.PostedTopic = title
			if err := state.Save(in.SessionID, s); err != nil {
				hookio.Logf("hook-stop: state save: %v", err)
			}
		}
	}
}

// promptHead condenses a prompt into a short single-line topic: first 60
// runes, newlines flattened, cut marked with "…".
func promptHead(prompt string) string {
	const maxRunes = 60
	flat := strings.Join(strings.Fields(prompt), " ")
	runes := []rune(flat)
	if len(runes) <= maxRunes {
		return flat
	}
	return string(runes[:maxRunes]) + "…"
}

// newBackend constructs the backend selected by the repo entry. New services
// plug in here (and only here).
func newBackend(entry config.RepoEntry) (backend.Backend, error) {
	switch entry.Service {
	case "slack":
		return slack.New(entry.BotToken, entry.Channel)
	case "discord":
		return discord.New(entry.BotToken, entry.Channel)
	default:
		return nil, fmt.Errorf("unknown service %q", entry.Service)
	}
}

func userName() string {
	if u, err := user.Current(); err == nil && u.Username != "" {
		return u.Username
	}
	return "unknown"
}

func hostName() string {
	if h, err := os.Hostname(); err == nil && h != "" {
		return h
	}
	return "unknown"
}
