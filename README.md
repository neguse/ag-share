# ag-share

Forward coding-agent sessions — Claude Code and Codex — to a team chat thread,
turn by turn, so teammates can follow work in near real time. Slack and
Discord are the available backends. One session = one thread; only a single
parent message appears in the channel. Forwarding is **opt-in per session** —
by default nothing is sent.

Design details: [docs/design.md](docs/design.md). Planned runtime migration:
[docs/rust-migration.md](docs/rust-migration.md).

## Install

Claude Code:

```
/plugin marketplace add neguse/ag-share
/plugin install ag-share
```

Codex:

```
codex plugin marketplace add neguse/ag-share
codex plugin add ag-share@ag-share
```

Codex requires you to review and trust the plugin's hooks before they run:
run `/hooks` inside a Codex session and both **trust and enable** each of the
two ag-share hooks (they are separate states — a trusted-but-not-enabled hook
is skipped silently). Trust is recorded per hook hash, so plugin updates that
change a hook ask again. If forwarding silently does nothing, check the
`[hooks.state]` entries in `~/.codex/config.toml` for a missing
`enabled = true`.

On Windows, [Git for Windows](https://git-scm.com/downloads/win) is required
(hooks run under Git Bash).

## Slack setup

1. Create a Slack app in your workspace, add the **`chat:write`** bot scope
   (nothing else), and install it to the workspace
2. Invite the bot to the target channel (`/invite @your-bot`)
3. Write `~/.config/ag-share/config.json` (owner-only permissions):

```json
{
  "repos": {
    "github.com/acme/product": {
      "service": "slack",
      "bot_token": "xoxb-...",
      "channel": "C0XXXXXXX",
      "default": "off"
    }
  }
}
```

The key is the repo's `origin` remote normalized to `host/owner/repo` (an
absolute path for repos without a remote). A key ending in `/*` matches by
prefix — `"github.com/neguse/*"` covers every repo under that owner; an exact
key beats a wildcard. `default` is optional and defaults to `"off"`; set it to
`"on"` to share every session in that repo without toggling — be deliberate
about combining it with a wildcard key, since that auto-shares all matching
repos.

The config is agent-neutral: one entry covers Claude Code and Codex sessions
alike, and both agents' sessions in a repo land in the same channel (each
session still gets its own thread, labeled with the agent).

## Discord setup

1. Create an application in the [Developer Portal](https://discord.com/developers/applications),
   open its **Bot** tab, and copy the bot token
2. Invite the bot to your server:
   `https://discord.com/oauth2/authorize?client_id=<APP_ID>&scope=bot&permissions=309237648384`
   (view channel, send messages, create public threads, send messages in threads)
3. Use `"service": "discord"` with the bot token and the target **text
   channel** ID (enable Developer Mode in Discord to right-click-copy IDs):

```json
{
  "repos": {
    "github.com/acme/product": {
      "service": "discord",
      "bot_token": "...",
      "channel": "123456789012345678",
      "default": "off"
    }
  }
}
```

Each session becomes a thread on that channel; the thread name carries the
session topic.

## Usage

| Prompt | Effect |
| --- | --- |
| `share-on` (or `/ag-share:on`) | Start forwarding this session, from the next turn |
| `share-on-from-begin` (or `/ag-share:on-from-begin`) | Start forwarding AND replay the whole session so far into the thread, one reply per turn |
| `share-off` (or `/ag-share:off`) | Stop forwarding; a later `share-on` continues the same thread (turns while off are never posted) |

Type the toggle as a prompt by itself, in either agent. It is intercepted by
a hook — it never reaches the model and is never forwarded.

Each turn posts one thread reply: your prompt, the agent's text (including
commentary between tool calls), and a tool-call count. Tool inputs/outputs and
thinking are never forwarded.

## Security

- Transcripts contain code fragments, file paths, and command output. Not
  enabling forwarding for sessions that touch secrets is your responsibility
- No repo can enable forwarding for someone else: destinations and defaults
  live only in your machine-local config
- The bot token needs `chat:write` only — the tool can post, never read

## Troubleshooting

Failures never interfere with the agent session; they are logged to
`~/.config/ag-share/error.log`. A turn that fails to post is included in the
next turn's message. For Codex, hooks that were never trusted (`/hooks`) are
skipped silently — approve them once per install.

## Development

```
go test ./...
go build -o ag-share ./cmd/ag-share
AG_SHARE_BIN=$PWD/ag-share claude --plugin-dir .
```

`AG_SHARE_BIN` bypasses the release-binary download in `bin/run.sh`.
Releases: push a `v*` tag; CI builds the platform binaries, publishes them as
release assets, and pins `bin/VERSION` + `bin/checksums.txt` on master.
