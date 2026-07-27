// Package slack implements the ag-share backend for Slack.
//
// Threading needs the parent message's ts, which Incoming Webhooks do not
// return, so this backend uses the Web API (chat.postMessage + bot token,
// scope chat:write only). The bot must be invited to the target channel.
package slack

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/neguse/ag-share/internal/backend"
)

const (
	postMessageURL = "https://slack.com/api/chat.postMessage"
	chatUpdateURL  = "https://slack.com/api/chat.update"
)

// maxMessageLen is the truncation budget per posted message. Slack rejects
// messages over ~40k characters and collapses long ones in the UI; 3800 keeps
// replies readable while fitting well under the hard limit.
const maxMessageLen = 3800

// Backend posts to one Slack channel with one bot token.
type Backend struct {
	// HTTPClient is used for API calls; tests inject a fake. Nil means
	// http.DefaultClient with a sane timeout.
	HTTPClient *http.Client

	token   string
	channel string
}

// New validates token/channel presence and returns a Slack backend.
func New(token, channel string) (*Backend, error) {
	if token == "" {
		return nil, fmt.Errorf("Slack bot token must not be empty")
	}
	if channel == "" {
		return nil, fmt.Errorf("Slack channel must not be empty")
	}

	client := *http.DefaultClient
	client.Timeout = 15 * time.Second
	return &Backend{
		HTTPClient: &client,
		token:      token,
		channel:    channel,
	}, nil
}

// parentText renders the single-line, channel-visible parent message:
//
//	:claude: {repo} — {topic} (by {user}@{host})
//
// or, with no topic, ":claude: {repo} — session by {user}@{host}".
func parentText(info backend.SessionInfo) string {
	if info.Topic == "" {
		return fmt.Sprintf("%s %s — session by %s@%s", info.AgentEmoji(), info.Repo, info.User, info.Host)
	}
	return fmt.Sprintf("%s %s — %s (by %s@%s)", info.AgentEmoji(), info.Repo, info.Topic, info.User, info.Host)
}

// CreateThread posts the parent message to the channel and returns its ts as
// the thread ref.
func (b *Backend) CreateThread(info backend.SessionInfo) (string, error) {
	response, err := b.postMessage(parentText(info), nil)
	if err != nil {
		return "", err
	}
	if response.TS == "" {
		return "", fmt.Errorf("Slack chat.postMessage response is missing ts")
	}
	return response.TS, nil
}

// UpdateThread rewrites the parent message via chat.update (chat:write covers
// updating the bot's own messages; no extra scope).
func (b *Backend) UpdateThread(threadRef string, info backend.SessionInfo) error {
	_, err := b.call(chatUpdateURL, chatUpdateRequest{
		Channel: b.channel,
		TS:      threadRef,
		Text:    parentText(info),
	})
	return err
}

// PostTurn renders the turn as one mrkdwn message and posts it with
// thread_ts=threadRef:
//
//   - each user prompt as a "> " quoted block
//   - Claude's text blocks joined by blank lines, code fences preserved
//   - "_(N tool calls)_" appended when N > 0
//   - head-first truncation to maxMessageLen, cuts marked "…(truncated)"
//
// API errors (ok=false payloads, HTTP/network failures) are returned to the
// caller; this package does not retry (turn-level catch-up handles loss).
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

	text := truncate(strings.Join(parts, "\n\n"), maxMessageLen)
	_, err := b.postMessage(text, &threadRef)
	return err
}

type postMessageRequest struct {
	Channel  string  `json:"channel"`
	Text     string  `json:"text"`
	ThreadTS *string `json:"thread_ts,omitempty"`
}

type chatUpdateRequest struct {
	Channel string `json:"channel"`
	TS      string `json:"ts"`
	Text    string `json:"text"`
}

type postMessageResponse struct {
	OK    bool   `json:"ok"`
	TS    string `json:"ts"`
	Error string `json:"error"`
}

func (b *Backend) postMessage(text string, threadRef *string) (postMessageResponse, error) {
	return b.call(postMessageURL, postMessageRequest{
		Channel:  b.channel,
		Text:     text,
		ThreadTS: threadRef,
	})
}

func (b *Backend) call(apiURL string, request any) (postMessageResponse, error) {
	payload, err := json.Marshal(request)
	if err != nil {
		return postMessageResponse{}, err
	}

	req, err := http.NewRequest(http.MethodPost, apiURL, bytes.NewReader(payload))
	if err != nil {
		return postMessageResponse{}, err
	}
	req.Header.Set("Authorization", "Bearer "+b.token)
	req.Header.Set("Content-Type", "application/json")

	response, err := b.httpClient().Do(req)
	if err != nil {
		return postMessageResponse{}, fmt.Errorf("Slack %s request: %w", apiMethod(apiURL), err)
	}
	defer response.Body.Close()

	var result postMessageResponse
	decodeErr := json.NewDecoder(response.Body).Decode(&result)
	if decodeErr == nil && !result.OK && result.Error != "" {
		return postMessageResponse{}, fmt.Errorf("Slack %s failed: %s", apiMethod(apiURL), result.Error)
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return postMessageResponse{}, fmt.Errorf("Slack %s returned HTTP %s", apiMethod(apiURL), response.Status)
	}
	if decodeErr != nil {
		return postMessageResponse{}, fmt.Errorf("decode Slack %s response: %w", apiMethod(apiURL), decodeErr)
	}
	if !result.OK {
		return postMessageResponse{}, fmt.Errorf("Slack %s failed", apiMethod(apiURL))
	}
	return result, nil
}

// apiMethod names the API in error messages: the URL's last path segment.
func apiMethod(apiURL string) string {
	if i := strings.LastIndexByte(apiURL, '/'); i >= 0 {
		return apiURL[i+1:]
	}
	return apiURL
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
