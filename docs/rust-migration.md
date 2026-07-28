# Rust migration plan

Status: proposed

This document plans a behavior-preserving rewrite of the `ag-share` runtime
from Go to Rust. It deliberately separates the language migration from the
later delivery redesign (tool-boundary streaming, message splitting, and a
durable SQLite outbox). Combining those changes would remove the working Go
implementation as an oracle at the same time that externally visible behavior
changes.

The migration is complete only when the Rust binary can replace the Go binary
without changing hook definitions, user configuration, existing session
state, release asset names, or posted Slack/Discord content.

## Why migrate

The current synchronous, turn-at-a-time implementation is small and works well
in Go. The planned delivery model adds:

- agent-specific hook producers that can overlap or lack a total event order
- a durable queue with transactional enqueue and acknowledgement
- short-lived worker processes
- leases, retries, pacing, and per-part progress
- explicit state transitions across process crashes

Rust is a better long-term fit for that state machine while still producing a
single native executable. SQLite can be compiled into the binary with
`rusqlite`'s `bundled` feature instead of depending on a system SQLite or a
mechanically C-to-Go-translated implementation.

The rewrite is not intended to change the product on its own. The current Go
behavior remains the specification until the parity release is accepted.

## Non-goals

The parity migration must not:

- change `~/.config/ag-share/config.json` or introduce named destinations
- change the `AG_SHARE_HOME` / `XDG_CONFIG_HOME` directory rules
- change the existing per-session JSON schema
- add `PostToolUse`, background workers, SQLite, or message splitting
- change transcript filtering, including the exclusion of thinking and tool
  results
- change retry, truncation, rate-limit, or cursor behavior
- require users to reinstall or edit plugin settings
- change `bin/run.sh`, release asset names, or checksum semantics except where
  development commands refer to the compiler

These changes belong to the post-parity delivery milestone described near the
end of this document.

## Compatibility contract

The following surfaces are compatibility gates, not implementation details.

### CLI and hook behavior

- The executable remains named `ag-share` (`ag-share.exe` on Windows).
- Existing commands remain valid:
  - `ag-share hook-prompt --agent claude|codex`
  - `ag-share hook-stop --agent claude|codex`
- Omitting `--agent` continues to mean `claude`.
- Unknown arguments and unknown agents are logged and must not break the agent
  session.
- A recognized toggle exits `2` and writes the current feedback text to
  stderr.
- `hook-stop` and non-toggle `hook-prompt` paths exit `0`, including on
  configuration, transcript, state, and backend failures.
- Each command continues to read one hook JSON object from stdin.
- `--agent` selects the agent-specific payload decoder before event-specific
  fields are interpreted. The overlapping common fields are not treated as a
  complete shared hook schema.

### Configuration

- The config remains `BaseDir()/config.json` with the current `repos` map.
- Exact repo matches continue to beat wildcard matches; the longest matching
  wildcard wins.
- Git remote and path normalization must return byte-for-byte equivalent repo
  identities for existing test cases.
- Missing config remains a valid empty config.
- Unknown services and missing credentials remain lookup errors.
- Permission warnings remain non-fatal and never print credentials.

### Session state

- Existing files under `BaseDir()/sessions/<session_id>.json` must load
  unchanged.
- The JSON field names and `omitempty` behavior remain compatible.
- Writes remain atomic from the caller's perspective.
- Files and directories remain owner-only where the platform supports Unix
  permissions.
- Seven-day stale cleanup keeps the current behavior.
- Rust and Go binaries must be able to alternate against the same state files
  during development and rollback testing.

Atomic replacement needs an explicit Windows test. Rust's generic rename APIs
must not be assumed to have the same replace-existing behavior as Go's
`os.Rename`; use a tested platform-specific replacement when necessary.

### Transcript extraction

The existing JSONL fixtures are the source of truth. The Rust implementation
must preserve these rules:

- malformed JSONL records are skipped; file-level errors are reported
- unknown JSON fields and record types are ignored
- Claude user prompts are only plain-string, non-meta, non-compact-summary
  user entries
- Claude assistant `text` blocks are forwarded, `thinking` is excluded, and
  `tool_use` blocks only increment the count
- Claude compaction records and summaries never become forwarded prompts
- Claude's Stop path retains the bounded wait for
  `last_assistant_message`
- Codex forwards `user_message` and `agent_message`, counts
  `function_call`, and ignores tool output/reasoning records
- Codex retains the special trailing-turn attribution to the Stop payload's
  `turn_id`
- a missing cursor resets to the latest cursor and skips ambiguous history
- catch-up ranges retain their current turn boundaries and cursor values

Rust deserialization should model only fields used by the filter and allow
unknown fields by default. Do not replace the current best-effort parsers with
a schema-strict parser.

### Backend requests

For the same `SessionInfo` and `Turn`, the Rust backend must produce equivalent:

- Slack `chat.postMessage` and `chat.update` paths, headers, and JSON bodies
- Discord REST methods, paths, headers, and JSON bodies
- parent text, thread names, quoted prompts, blank lines, code fences, and
  tool-count suffixes
- current byte budgets and `…(truncated)` behavior
- API error classification and safe error messages
- 15-second HTTP client timeout

Request ordering is part of the contract. Tests compare captured requests, not
only successful return values.

### Distribution

- Release assets keep these exact names:
  - `ag-share-linux-amd64`
  - `ag-share-windows-amd64.exe`
  - `ag-share-darwin-amd64`
  - `ag-share-darwin-arm64`
- `bin/VERSION` and `bin/checksums.txt` retain their current roles.
- `bin/run.sh` continues to resolve, verify, cache, and execute the binary.
- `AG_SHARE_BIN` continues to override the downloaded binary in development.
- Claude and Codex plugin manifests and hook command strings remain unchanged.

## Agent event boundary

Claude Code and Codex have similarly named lifecycle events, but they are
separate protocols. The stable common surface is only a small envelope and a
few product-level effects:

- both identify a session and working directory
- both can notify ag-share that a prompt was submitted or a turn is stopping
- both can run a command that reads JSON from stdin
- both can consume an exact-match toggle prompt

Everything else remains agent-owned:

- Claude Code has no general Codex-style `turn_id`; its transcript UUIDs are
  the current cursor unit
- Codex includes `turn_id` on turn-scoped hooks and uses rollout turn IDs as
  transcript cursors
- Claude Code separates successful and failed tool events and provides
  `PostToolBatch`; Codex currently provides `PostToolUse` without an equivalent
  batch event
- tool names and coverage differ; an event missing from one agent must not be
  synthesized from the other agent's semantics
- Stop timing, interrupt recovery, transcript flush timing, compaction, and
  subagent payloads are agent-specific
- Claude Code supports background command hooks; Codex currently parses an
  async option but does not execute command hooks asynchronously

See the current [Claude Code hooks reference][claude-hooks] and
[Codex hooks reference][codex-hooks]. These external hook contracts are inputs
to the adapters, not the ag-share domain model.

The Rust implementation must decode raw payloads into separate types:

```text
ClaudeEvent -> ingress::claude --+
                                 +-> IngressAction -> transcript scan
CodexEvent  -> ingress::codex  --+                       |
                                                         v
                                                semantic segments
```

`IngressAction` is deliberately smaller than either hook API:

```text
Toggle(op)
Scan(reason)
Finalize(reason)
```

The action may carry an opaque source checkpoint and agent-specific metadata,
but downstream code must not require every agent to supply `turn_id`,
`tool_use_id`, or the same event sequence.

For the parity migration, each adapter supports only the already shipped
`UserPromptSubmit` and `Stop` registrations and reproduces the current Go
behavior. Adding tool or recovery events remains a post-parity change.

## Target Rust shape

The parity implementation should stay synchronous and avoid an async runtime.
Hook invocations perform one bounded sequence of filesystem and HTTP work, so
blocking I/O is simpler and matches the current behavior.

Proposed source layout:

```text
Cargo.toml
Cargo.lock
rust-toolchain.toml
src/
├── main.rs
├── lib.rs
├── cli.rs
├── ingress/
│   ├── mod.rs
│   ├── claude.rs
│   └── codex.rs
├── log.rs
├── config.rs
├── state.rs
├── transcript/
│   ├── mod.rs
│   ├── claude.rs
│   └── codex.rs
└── backend/
    ├── mod.rs
    ├── slack.rs
    └── discord.rs
tests/
├── cli_compat.rs
└── differential.rs
```

`main.rs` is a thin process boundary over the testable library. The ingress
modules may share helpers for common envelope fields, logging, and exit-code
translation, but they do not share one catch-all hook payload struct.

While Go and Rust coexist, both implementations should read the transcript
fixtures from `internal/transcript/testdata`. Move them to a Rust-owned
location only after the Go implementation is removed.

Initial dependency direction:

- `serde` and `serde_json` for hook/config/state/transcript JSON
- a blocking HTTP client using rustls and platform certificate verification;
  `ureq` is the leading candidate
- `thiserror` for library error types
- `tempfile` for atomic-write and filesystem tests

`rusqlite` is intentionally absent from the parity binary. Add it with the
`bundled` feature only in the delivery milestone. Keep dependency features
minimal, commit `Cargo.lock`, and record third-party licenses in the release
process.

The top-level CLI owns exit-code policy. Internal modules return typed errors;
they do not call `process::exit`. Error display must never include bot tokens
or forwarded message bodies.

## Migration phases

### Phase 0: toolchain and feasibility spike

Before porting production logic:

- pin a Rust toolchain that supports Edition 2024
- build a hello-world `ag-share` for all four release targets
- verify the blocking HTTPS client against Slack/Discord-compatible local test
  servers and one opt-in real endpoint
- verify system proxy behavior and platform certificate roots
- verify stdin JSON, stdout/stderr, and exit codes through `bin/run.sh`
- verify Windows UTF-8 paths, atomic replacement, and owner-only best effort
- record release binary size and cold-start time as comparison data

Exit criterion: all release targets build and run without installing a runtime
or shared library on the destination machine.

Do not begin the full port if HTTPS or the Windows filesystem contract requires
an unacceptable runtime dependency.

### Phase 1: add a parallel Rust implementation

Add the Rust project without deleting or moving Go code.

- keep `go.mod`, `cmd/`, and `internal/` as the reference implementation
- give the Rust development artifact a separate local path, not a separate
  production command name
- make Rust tests read the existing transcript fixtures in place; do not
  duplicate or rewrite their contents
- establish CI for:
  - `cargo fmt --check`
  - `cargo clippy --all-targets -- -D warnings`
  - `cargo test --locked`
  - the existing `go vet ./...` and `go test ./...`

Exit criterion: both implementations build in the same checkout and CI still
tests the Go oracle.

### Phase 2: port pure behavior

Port deterministic code before side effects:

1. agent flag parsing and dispatch
2. Claude hook payload decoding into Claude event types
3. Codex hook payload decoding into Codex event types
4. toggle recognition and feedback construction
5. config parsing, wildcard resolution, and repo normalization
6. `SessionInfo`, `Turn`, and backend rendering helpers
7. Claude transcript parsing and splitting
8. Codex transcript parsing and splitting
9. topic derivation and truncation helpers

For every Go table test, add an equivalent Rust test using the same fixture or
test vector. Avoid cleanup or redesign while porting; awkward behavior that is
externally observable remains until parity is complete.

Exit criterion: pure Rust tests cover every existing Go behavior test and
produce the same values.

### Phase 3: port side effects

Port the filesystem, HTTP, and orchestration layers:

1. BaseDir resolution and safe logging
2. session state load/save/cleanup
3. Slack backend with an injected HTTP transport
4. Discord backend with an injected HTTP transport
5. Claude `hook-prompt` and `hook-stop` orchestration
6. Codex `hook-prompt` and `hook-stop` orchestration
7. shared post-ingress session and backend orchestration

Use local HTTP servers in tests. Production tests must not call Slack or
Discord.

Exit criterion: Rust integration tests cover successful requests, service
errors, HTTP errors, malformed responses, state-save errors, missing
transcripts, missing cursors, and catch-up posting.

### Phase 4: differential compatibility suite

Build both implementations and run equivalent product scenarios against
isolated temporary homes and fake HTTP servers. The Claude and Codex scenarios
use their own payload fixtures; they must not obtain parity by feeding one
invented shared hook payload to both adapters.

Compare:

- exit code
- stdout and stderr
- normalized error-log lines
- resulting session JSON
- HTTP method, path, headers, and decoded JSON body
- number and order of requests
- extracted chunks and cursors

Time, temp-file names, and platform-specific error wording must be normalized;
message content and request payloads must not be normalized.

Required differential scenarios:

- all three toggles, including repeated toggles
- missing, wildcard, invalid, and permission-wide configs
- default-on and opt-in sessions
- first thread creation and topic update
- single-turn and multi-turn catch-up
- backend failure before and after thread creation
- Unicode prompts and responses
- oversized Slack and Discord replies
- Claude delayed final text and timeout
- Claude compacted transcript
- Codex trailing in-flight turn and duplicate Stop
- Codex `turn_id` present where Claude has no corresponding field
- missing, null, unknown, and additional fields in each agent's own payload
- malformed and unknown transcript records

Exit criterion: no unexplained difference remains.

### Phase 5: native release pipeline

Replace the single Go cross-compile job with a native Rust build matrix. Native
runners avoid depending on cross-linking behavior once bundled SQLite is added
later.

Each target job:

- installs the pinned Rust toolchain
- runs the applicable tests
- builds with `cargo build --release --locked`
- names the artifact exactly as `bin/run.sh` expects
- uploads the artifact to a final release job

The final release job:

- assembles all four artifacts
- computes `checksums.txt`
- creates the GitHub release
- pins `bin/VERSION`, `bin/checksums.txt`, and plugin manifest versions using
  the current flow

Do not alter `bin/run.sh` as part of this phase unless testing proves a
Rust-specific platform issue.

Exit criterion: an unpublished release candidate installs and executes through
the existing plugin path on all four targets.

### Phase 6: real-session acceptance

Run an opt-in smoke matrix:

- Claude Code on macOS, Linux, and Windows
- Codex on at least one Unix platform and Windows
- Slack and Discord thread creation, reply, and topic update
- fresh install and cached-binary invocation
- resume, compaction, toggle off/on, and from-begin replay
- backend outage followed by catch-up

Inspect team-chat payloads and local state; do not rely only on exit status.

Exit criterion: the Rust release candidate matches the current release in real
sessions and creates no new hook warnings.

### Phase 7: cutover and rollback window

Publish the Rust binary under the existing asset names in a release candidate,
then a normal release.

- keep state and config formats unchanged so rollback is a `bin/VERSION` /
  checksum pin change
- retain Go source and tests for at least one accepted Rust release
- document the exact last Go release tag
- fix parity regressions in Rust rather than adding divergent compatibility
  behavior to `run.sh`

Rollback trigger examples:

- a release target fails to start
- an existing session file cannot be read
- hook exit behavior interferes with an agent session
- posted content differs or is lost
- proxy/TLS behavior regresses for existing installations

Exit criterion: the Rust release has completed the chosen observation window
without a rollback-class defect.

### Phase 8: remove the Go implementation

After the rollback window:

- remove `cmd/`, `internal/`, and `go.mod`
- remove Go jobs from CI
- update README development commands
- update `docs/design.md` from Go to Rust
- keep cross-language differential fixtures as Rust golden tests
- preserve the last Go tag indefinitely

This is the first phase where deleting the Go implementation is allowed.

## Post-parity delivery milestone

The real-time delivery redesign starts only after Phase 7 succeeds. It requires
its own design review because it changes user-visible message granularity and
the durable state format.

The intended direction is:

```text
Claude hook payloads                 Codex hook payloads
        │                                    │
        ▼                                    ▼
 ingress::claude                      ingress::codex
        │                                    │
        └───────────┐            ┌───────────┘
                    ▼            ▼
              agent-specific transcript scan
                            │
                            ▼
                  semantic stream segments
                            │
                            ▼
               SQLite outbox under BaseDir
                            │
                            ▼
                  short-lived drain worker
                            │
                            ▼
               Slack / Discord message parts
```

The intended trigger matrix is agent-specific:

| Agent | Incremental trigger | Tail and recovery triggers | Notes |
| --- | --- | --- | --- |
| Claude Code | `PostToolBatch` preferred | `Stop`, then `SessionEnd` as recovery | If the supported-version floor lacks `PostToolBatch`, use both `PostToolUse` and `PostToolUseFailure` |
| Codex | `PostToolUse` | `Stop`, then `SessionEnd` as recovery | Tool hooks do not cover every hosted or specialized tool path |

`UserPromptSubmit` remains the toggle boundary for both agents and may also
scan for progress left by a previous interrupted turn before handling the new
prompt. The exact `SessionEnd` recovery behavior remains adapter-specific; the
table does not imply equal event timing.

Tool events are wake-up signals. The adapter scans from its last durable source
checkpoint, extracts newly visible semantic content, and enqueues that content.
It never forwards `tool_input`, `tool_response`, or a raw hook payload as the
chat message. Overlapping, duplicated, missing, and out-of-order wake-ups must
converge through the source checkpoint and SQLite transaction.

Constraints already agreed for that milestone:

- `config.json` keeps its current structure
- SQLite stores queue/session delivery state, not configuration
- one database belongs to one resolved BaseDir
- raw hook payloads are decoded by agent-specific ingress types and do not
  cross into the shared delivery model
- source checkpoints and deduplication keys remain opaque and agent-specific
- hook processes synchronously scan and durably enqueue newly visible semantic
  segments before returning, without waiting for chat APIs
- Claude Code's native asynchronous hook mode is not the durability boundary;
  Codex does not currently support it, and neither adapter reports success
  before its local SQLite transaction commits
- after committing, a hook best-effort starts a detached, short-lived drainer;
  a spawn failure leaves the job durable for the next hook or a manual drain
- workers are disposable consumers, not owners of session or config state
- worker exclusion uses transactionally claimed, expiring SQLite leases, not a
  global PID file or a permanently assigned process per session
- overlapping workers must be safe; ordering and pacing are coordinated per
  destination rather than by global process exclusion
- workers reload config before posting; bot tokens are never stored in SQLite
- a queued delivery records its repo and resolved service/channel
- token rotation may use the new token for the same route
- a service/channel change blocks an old queued delivery instead of silently
  rerouting sensitive content
- Claude `PostToolBatch` or its supported-version fallback and Codex
  `PostToolUse` flush visible transcript progress
- each agent's Stop adapter flushes its tail; later prompt/session recovery
  covers interrupts and unsupported or missed tool events
- tool-boundary stream segments and service-limit message parts are distinct
  layers
- semantic content is split before final rendering, while packing uses the
  rendered size
- long segments are split without truncation and preserve Unicode boundaries,
  prompt quoting, and fenced-code readability
- delivery is at-least-once: a crash in the remote-success/local-ack window may
  duplicate a part but must not intentionally omit it
- each successful part advances durable progress
- queued content is owner-only, never written to `error.log`, and removed
  after acknowledgement
- `share-off` prevents unsent queued content from being posted

Before implementing this milestone, specify:

- the supported Claude Code and Codex version floor and final trigger matrix
- captured payload fixtures and ordering tests for every registered event
- per-agent interruption, compaction, subagent, and SessionEnd recovery rules
- the SQLite schema and migrations
- transcript checkpoints suitable for in-turn streaming
- worker spawn/detach behavior on Unix and Windows
- destination lease and rate-limit rules
- config-change and permanent-error recovery UX
- queue retention and secure deletion policy
- migration and rollback from JSON session state

Do not smuggle these decisions into the language port.

## Risks and mitigations

| Risk | Mitigation |
| --- | --- |
| Rewrite silently changes filtering | Reuse fixtures and require differential output |
| Rust parser becomes schema-strict | Deserialize only needed fields; ignore unknown fields and malformed lines as today |
| Windows state replacement differs | Add an overwrite-existing integration test and platform implementation before cutover |
| TLS roots or proxies differ from Go | Use platform verification, test proxy environment variables, and run real endpoint smoke tests |
| Error strings leak payloads or tokens | Use typed errors with explicitly safe context; test redaction |
| Native build artifacts differ from expected names | Test the complete `run.sh` download/cache/exec path against an RC release |
| Language and queue changes become entangled | Keep SQLite and new hooks out of the parity binary |
| Similar hook names are mistaken for one protocol | Decode separate Claude and Codex event types and normalize only ingress actions and semantic segments |
| Agent-native async behavior diverges | Commit the local SQLite transaction synchronously in both adapters; keep remote delivery in ag-share workers |
| Rust worker design forces an async runtime | Use blocking HTTP and process-level concurrency; add async Rust only with a demonstrated need |
| Old and new binaries corrupt shared state | Keep the JSON contract identical and alternate binaries in compatibility tests |

## Definition of done

The Rust migration is done when:

- all compatibility gates in this document pass
- the differential suite has no unexplained differences
- all four release artifacts install through the existing plugin path
- existing config and session files work without migration
- real Claude and Codex sessions pass the acceptance matrix
- rollback to the last Go release is tested
- the Rust release survives the observation window
- Go is removed only after those conditions are met

The SQLite outbox and tool-boundary streaming are not part of this definition
of done; they begin after the behavior-preserving migration is proven.

[claude-hooks]: https://code.claude.com/docs/en/hooks
[codex-hooks]: https://developers.openai.com/codex/hooks
