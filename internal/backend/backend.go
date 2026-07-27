// Package backend defines the seam between ag-share's core (hooks, transcript
// extraction, state) and chat services. A backend is anything that can host
// "one parent message + a thread of replies"; a service that cannot thread
// cannot satisfy this interface, by design.
//
// Core code never sees service-specific types: thread references are opaque
// strings, and all service markup, message length limits, and truncation live
// inside the backend implementation.
package backend

// SessionInfo describes a session for the channel-visible parent message.
type SessionInfo struct {
	Agent string // agent name ("claude", "codex"); shown as the parent message's emoji/prefix
	Repo  string // normalized repo identity, e.g. "github.com/acme/product"
	User  string // OS user name
	Host  string // hostname
	Topic string // short session topic; "" falls back to a topic-less parent
}

// AgentEmoji is the parent-message prefix for the session's agent, e.g.
// ":claude:" — a workspace custom emoji name; services render it literally
// when the emoji does not exist, which still reads fine as a text tag.
func (i SessionInfo) AgentEmoji() string {
	if i.Agent == "" {
		return ":claude:"
	}
	return ":" + i.Agent + ":"
}

// Turn is the service-neutral content of one forwarded thread reply.
// A "turn" may span multiple prompts (queued prompts, catch-up after a failed
// post or an interrupted turn).
type Turn struct {
	UserPrompts []string // user prompts in the range, in order
	Texts       []string // Claude's text blocks in the range, in order
	ToolCalls   int      // number of tool calls in the range
}

// Empty reports whether the turn carries nothing worth posting.
func (t Turn) Empty() bool {
	return len(t.UserPrompts) == 0 && len(t.Texts) == 0
}

// Backend posts session content to one destination in one chat service.
// Implementations are constructed per hook invocation (no shared state) and
// must be safe to call sequentially: CreateThread once, then PostTurn per turn.
type Backend interface {
	// CreateThread posts the channel-visible parent message for a session and
	// returns an opaque reference to its thread, persisted in session state
	// and passed back to PostTurn. The parent message is a single line built
	// from info; everything else goes into the thread.
	CreateThread(info SessionInfo) (threadRef string, err error)

	// PostTurn renders turn into the service's markup (quoting the prompts,
	// preserving code fences, appending the tool-call count) and posts it as
	// one reply in the thread. Rendering truncates head-first to the service's
	// message limit, marking cuts with "…(truncated)".
	PostTurn(threadRef string, turn Turn) error

	// UpdateThread rewrites the parent message with fresh info — used when
	// the session topic becomes available or changes after thread creation.
	// Best-effort from the caller's perspective: a failure must not affect
	// turn posting or the cursor.
	UpdateThread(threadRef string, info SessionInfo) error
}
