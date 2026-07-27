// Package discord implements the ag-share backend for Discord.
//
// It uses the REST API with a bot token (no Gateway connection needed):
// the parent message is posted to a text channel, a public thread is started
// from it (the thread ID equals the parent message ID), and turns are posted
// into the thread. The bot is the thread creator, so it can rename the thread
// (topic refresh) without MANAGE_THREADS. Required permissions: VIEW_CHANNEL,
// SEND_MESSAGES, CREATE_PUBLIC_THREADS, SEND_MESSAGES_IN_THREADS
// (invite URL permissions=309237648384).
package discord

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/neguse/ag-share/internal/backend"
)

const defaultBaseURL = "https://discord.com/api/v10"

// maxMessageLen is the truncation budget per posted message; Discord rejects
// content over 2000 characters, and the budget leaves headroom for markup.
const maxMessageLen = 1950

// maxThreadNameRunes is Discord's thread name limit (1-100 characters).
const maxThreadNameRunes = 100

// Backend posts to one Discord channel with one bot token.
type Backend struct {
	// HTTPClient is used for API calls; tests inject a fake. Nil means a
	// default client with a sane timeout.
	HTTPClient *http.Client
	// BaseURL overrides the API base (tests). Empty means the real API.
	BaseURL string

	token   string
	channel string
}

// New validates token/channel presence and returns a Discord backend.
func New(token, channel string) (*Backend, error) {
	if token == "" {
		return nil, fmt.Errorf("Discord bot token must not be empty")
	}
	if channel == "" {
		return nil, fmt.Errorf("Discord channel must not be empty")
	}
	return &Backend{token: token, channel: channel}, nil
}

// CreateThread posts the parent message, starts a public thread from it named
// after the topic, and returns the thread ID (== parent message ID).
func (b *Backend) CreateThread(info backend.SessionInfo) (string, error) {
	parent := struct {
		Content string `json:"content"`
	}{Content: parentText(info)}
	var msg struct {
		ID string `json:"id"`
	}
	path := fmt.Sprintf("/channels/%s/messages", b.channel)
	if err := b.call(http.MethodPost, path, parent, &msg); err != nil {
		return "", err
	}
	if msg.ID == "" {
		return "", fmt.Errorf("Discord create message response is missing id")
	}

	start := struct {
		Name string `json:"name"`
	}{Name: threadName(info)}
	path = fmt.Sprintf("/channels/%s/messages/%s/threads", b.channel, msg.ID)
	if err := b.call(http.MethodPost, path, start, nil); err != nil {
		return "", err
	}
	return msg.ID, nil
}

// PostTurn renders the turn as one Discord-markdown message and posts it into
// the thread. Same shape as the Slack rendering: quoted prompts, text blocks,
// tool-call count, head-first truncation.
func (b *Backend) PostTurn(threadRef string, turn backend.Turn) error {
	parts := make([]string, 0, len(turn.UserPrompts)+len(turn.Texts)+1)
	for _, prompt := range turn.UserPrompts {
		lines := strings.Split(prompt, "\n")
		for i := range lines {
			lines[i] = "> " + lines[i]
		}
		parts = append(parts, strings.Join(lines, "\n"))
	}
	parts = append(parts, turn.Texts...)
	if turn.ToolCalls > 0 {
		parts = append(parts, fmt.Sprintf("_(%d tool calls)_", turn.ToolCalls))
	}

	body := struct {
		Content string `json:"content"`
	}{Content: truncate(strings.Join(parts, "\n\n"), maxMessageLen)}
	return b.call(http.MethodPost, fmt.Sprintf("/channels/%s/messages", threadRef), body, nil)
}

// UpdateThread renames the thread to the fresh topic. The bot created the
// thread, so no MANAGE_THREADS is needed. (Note: channel-name edits are
// rate-limited server-side; failures are the caller's to log and ignore.)
func (b *Backend) UpdateThread(threadRef string, info backend.SessionInfo) error {
	body := struct {
		Name string `json:"name"`
	}{Name: threadName(info)}
	return b.call(http.MethodPatch, "/channels/"+threadRef, body, nil)
}

// parentText is the channel-visible parent message body. The topic lives in
// the thread name, so the body stays constant. Discord has no per-agent
// custom emoji to lean on, so the agent name is spelled out.
func parentText(info backend.SessionInfo) string {
	agent := info.Agent
	if agent == "" {
		agent = "claude"
	}
	return fmt.Sprintf("🤖 [%s] %s — session by %s@%s", agent, info.Repo, info.User, info.Host)
}

// threadName renders the topic as the thread name (1-100 chars required).
func threadName(info backend.SessionInfo) string {
	name := info.Topic
	if name == "" {
		name = "session"
	}
	runes := []rune(name)
	if len(runes) > maxThreadNameRunes {
		name = string(runes[:maxThreadNameRunes-1]) + "…"
	}
	return name
}

type apiError struct {
	Message string `json:"message"`
	Code    int    `json:"code"`
}

func (b *Backend) call(method, path string, request, response any) error {
	payload, err := json.Marshal(request)
	if err != nil {
		return err
	}
	base := b.BaseURL
	if base == "" {
		base = defaultBaseURL
	}
	req, err := http.NewRequest(method, base+path, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bot "+b.token)
	req.Header.Set("Content-Type", "application/json")

	res, err := b.httpClient().Do(req)
	if err != nil {
		return fmt.Errorf("Discord %s %s request: %w", method, path, err)
	}
	defer res.Body.Close()

	if res.StatusCode < http.StatusOK || res.StatusCode >= http.StatusMultipleChoices {
		var apiErr apiError
		detail := ""
		if body, err := io.ReadAll(io.LimitReader(res.Body, 4096)); err == nil {
			if json.Unmarshal(body, &apiErr) == nil && apiErr.Message != "" {
				detail = fmt.Sprintf(": %s (code %d)", apiErr.Message, apiErr.Code)
			}
		}
		return fmt.Errorf("Discord %s %s returned HTTP %s%s", method, path, res.Status, detail)
	}
	if response != nil {
		if err := json.NewDecoder(res.Body).Decode(response); err != nil {
			return fmt.Errorf("decode Discord %s %s response: %w", method, path, err)
		}
	}
	return nil
}

func (b *Backend) httpClient() *http.Client {
	if b.HTTPClient != nil {
		return b.HTTPClient
	}
	client := *http.DefaultClient
	client.Timeout = 15 * time.Second
	return &client
}

func truncate(text string, limit int) string {
	if len(text) <= limit {
		return text
	}

	const marker = "…(truncated)"
	budget := limit - len(marker)
	if budget <= 0 {
		return marker[:limit]
	}
	for budget > 0 && !utf8.RuneStart(text[budget]) {
		budget--
	}
	return text[:budget] + marker
}
