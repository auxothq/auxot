# PLAN_REDUCE_CONTEXT.md
# Context Bloat & Truncation Fixes

## Problem Statement

Long-running agent sessions accumulate context that exceeds the `historyBlob` byte cap
(`80_000` bytes). The current truncation strategy drops whole turns from the front of
the history array without regard to exchange boundaries, producing two symptoms:

1. **Orphaned tool_results** — a `[tool_result (id: X)]` entry survives in the blob
   after its corresponding `[assistant]` `<tool_use>` turn was dropped. Claude sees
   results he has no record of requesting and concludes the _user_ must have performed
   the action ("It seems Jeremy ran some command…").

2. **Redundant large payloads** — `bash` is capped at 100 KB; `read` has no output
   cap at all. A single tool result can consume the entire blob budget, forcing
   aggressive truncation of surrounding turns.

Additionally there is a suspected issue with whether tool_use blocks are reaching
Claude at all in certain paths, which needs explicit verification.

### Why truncation fires at all

History compaction is supposed to fire a summary every 32K bytes, so the summary
plus recent history should normally be small. When the 80K cap is hit it means roughly
48K of raw turns accumulated since the last summary — almost always caused by a single
large tool result (e.g. a 100 KB `bash` output). Fixing the tool output limits
(Steps 2 and 3) will prevent most truncation events. The truncation logic still needs
to be correct for the cases it does handle.

---

## Step 1 — Rewrite `buildPrompt` truncation: token-aware, boundary-safe

**File:** `internal/cliworker/claude.go` · `buildPrompt` (lines 854–877)

### Current behavior

```go
history = history[1:] // drop oldest turn — blind, one string at a time
```

Each element of `history` is one serialized turn (user, assistant, or tool_result).
Dropping `history[0]` when it is an `[assistant]` turn that contained tool calls
leaves the immediately following `[tool_result ...]` turns as orphans.

### Token estimation

Replace byte counting with estimated token counting. Use the standard approximation:
**1 token ≈ 4 bytes**:

```go
func estimateTokens(s string) int { return (len(s) + 3) / 4 }
```

Budget threshold: **128 000 tokens** — well inside Claude's 200K input limit, leaving
headroom for the system prompt, tool definitions, and the current turn.

```go
const maxHistoryTokens = 128_000
```

### Protected zone: current agentic turn

Everything from the last `[user]` turn onward is the current agentic turn (the
message Claude is responding to, plus any in-flight assistant/tool_result turns). This
zone must never be touched by truncation.

```go
// Identify the protected boundary before any modification.
lastUserIdx := -1
for i := len(history) - 1; i >= 0; i-- {
    if strings.HasPrefix(history[i], "[user]") {
        lastUserIdx = i
        break
    }
}
compactable := history[:lastUserIdx]  // everything before the current user turn
protected   := history[lastUserIdx:]  // current user message onward — never touched
```

Only `compactable` is modified. `protected` is always appended as-is when
reconstructing the final blob.

### Pass 1 — strip tool_result content from oldest turns, one at a time

While `estimateTokens(blob) > maxHistoryTokens`:

1. Walk `compactable` from index 0 forward.
2. Find the first `[tool_result ...]` entry whose body has not yet been redacted.
3. Replace its body with:
   ```
   [output redacted — call tool_recall("<call_id>") to retrieve]
   ```
   Keep the `[tool_result (id: X)]` header so the structural pairing is intact and
   Claude knows the call completed.
4. Recompute estimated tokens.
5. If no unredacted tool_result body remains in `compactable` and blob is still over
   budget, exit Pass 1 and proceed to Pass 2.

Rationale: tool_result bodies are the cheapest thing to remove. The assistant's
`<tool_use>` block and the result header stay, so Claude knows what happened. He can
call `tool_recall` if he needs the actual output.

### Pass 2 — drop complete turns from oldest, one at a time

Only reached if Pass 1 exhausted all tool_result content and the blob is still over
128K tokens. This is the last-resort path.

While `estimateTokens(blob) > maxHistoryTokens` and `len(compactable) > 0`:

1. Drop `compactable[0]` (oldest turn, regardless of role).
2. Recompute.

After the loop, if any turns were dropped, insert a synthetic `[user]` turn at the
front of the surviving history so Claude knows context was cut:

```
[user]
[N turns omitted — history was too large to include in full]
```

Note: Pass 2 can in theory orphan a `[tool_result]` whose `[assistant]` was dropped.
By the time Pass 2 runs, Pass 1 has already nullified the body of every tool_result
in `compactable`, so the orphan contains only a redaction marker, not real data.
This is acceptable.

### Logging

```go
slog.Warn("cliworker: history compacted",
    "est_tokens_before", tokensBefore,
    "est_tokens_after",  tokensAfter,
    "budget",            maxHistoryTokens,
    "pass1_redacted",    redactedCount,
    "pass2_dropped",     droppedCount,
)
```

Suppress the existing per-turn `Warn` in session mode (where the blob is computed but
never sent). Add `"session_mode": true` to the log entry or skip the compaction
entirely when `sessionFileExists` is true — the blob is unused in that path.

**Acceptance criteria:**

- [ ] A blob containing interleaved `<tool_use>` and `[tool_result]` turns: after
  Pass 1, every tool_result body is replaced by the redact marker; no orphaned
  content remains.
- [ ] The current agentic turn (from `lastUserIdx` onward) is identical before and
  after compaction.
- [ ] Pass 2 never fires in normal operation once Steps 2 and 3 are implemented.
- [ ] Existing `TestBuildPrompt` tests pass; add a fixture with 60 turns including
  large tool results and assert Pass 1 brings it under budget without entering Pass 2.
- [ ] Session-mode runs no longer emit spurious compaction warnings.

---

## Step 2 — Reduce `bash` tool output: 1 KB inline to LLM, full result to server

**File:** `pkg/codingtools/bash.go`

### Current behavior

Output is capped at 100 KB (`maxBashOutputBytes = 100 * 1024`). The full 100 KB can
land in the history blob every turn, consuming the entire 80K budget in one shot.

### Two separate output concerns

| Destination | Limit | Content |
|---|---|---|
| Server (audit log, DB) | 100 KB (unchanged) | Full result — always |
| LLM (inline in tool_result) | 1 KB | Tail of output |

The tool `Execute` function returns the string that goes to the LLM. The server
receives that same string for audit logging. Therefore the truncation to 1 KB must
happen in the tool itself before returning, but the **full output (up to 100 KB) must
be stored by the server** before the inline string is returned. This means the call
ID — assigned by the server when it dispatches the tool call — must be available at
execution time so the server can correlate the full result with the row.

The full-result storage is the server's responsibility (it already receives every tool
result for audit). The tool's job is only to return the correct inline string.

### Required changes

```go
const (
    bashInlineBytes    = 1_024         // returned inline to the LLM
    maxBashOutputBytes = 100 * 1024    // collected from subprocess, sent to server
)
```

After collecting `result` (already capped at 100 KB):

1. If `len(result) <= bashInlineBytes` → return as-is.
2. If `len(result) > bashInlineBytes`:
   - Return the **tail** (`result[len(result)-bashInlineBytes:]`) with a header:
     ```
     [output truncated — showing last 1024 bytes; use tool_recall("<call_id>", offset=0) to read from the beginning]
     <last 1024 bytes>
     ```
   - The call ID in the message comes from `toolEnv["_call_id"]` if available,
     otherwise `"<call_id>"` as a literal placeholder.

The model sees the tail because for bash commands the most recent output (exit status,
final error message, last lines of a long listing) is almost always the most
actionable.

**Acceptance criteria:**

- [ ] A command emitting 10 KB: server-side receives the full 10 KB; LLM receives ≤
  1 KB tail with the truncation header.
- [ ] A command emitting 500 bytes: LLM receives the full 500 bytes; no truncation
  header.
- [ ] The inline portion is the **tail**, not the head.

---

## Step 3 — Enforce output limit on `read` tool: line and byte offset modes

**File:** `pkg/codingtools/read.go`

### Current behavior

`offset` (line) and `limit` (line count) parameters exist but are optional, and there
is no byte-based mode. A file that is one long line — e.g. a minified JSON, an XLSX
unpacked to XML, a CSV with no newlines — returns everything regardless of size.

### Required changes

#### New parameter schema

```json
{
  "path":        { "type": "string", "required": true },
  "offset_line": { "type": "integer", "description": "1-indexed line to start from (line mode)" },
  "limit_lines": { "type": "integer", "description": "Max lines to return (line mode)" },
  "offset_byte": { "type": "integer", "description": "Byte offset to start from (byte mode)" },
  "limit_bytes": { "type": "integer", "description": "Max bytes to return (byte mode)" }
}
```

The existing `offset` and `limit` fields map to `offset_line` / `limit_lines` for
backwards compatibility — keep them as aliases but document the new names.

Byte mode takes precedence if `offset_byte` or `limit_bytes` is set.

#### Default limits

```go
const (
    defaultReadLines = 100
    defaultReadBytes = 4_096   // 4 KB inline default in byte mode
    maxReadBytes     = 100 * 1024
)
```

#### Line mode (default when no byte params set)

- If `limit_lines` is 0 (unset), default to `defaultReadLines`.
- If the default was applied, append:
  ```
  [output capped at 100 lines — file has <N> total lines; use offset_line/limit_lines or offset_byte/limit_bytes to read further]
  ```

#### Byte mode (when `offset_byte` or `limit_bytes` is set)

- Read raw bytes from the file starting at `offset_byte`.
- Cap at `min(limit_bytes, maxReadBytes)`. If `limit_bytes` is 0, use `defaultReadBytes`.
- Append:
  ```
  [showing bytes <start>–<end> of <total>; use offset_byte=<end> to continue]
  ```
- Do not split on newlines; return the raw byte slice as a string.

#### Both modes: inline to LLM vs. full result to server

Same principle as Step 2: the server always receives the full result (up to 100 KB).
The LLM receives the capped inline version. The `read` tool returns the capped
version; the server stores it for `tool_recall` access.

#### Head, not tail

For `read`, the inline portion shows the **head** (beginning) because you're typically
reading a file to understand its structure, and the beginning is the most useful part.
The trailer tells the model exactly what byte offset to use to continue.

#### Update `Description`

```
Read a file. Line mode (default): returns up to 100 lines from offset_line.
Byte mode: set offset_byte/limit_bytes for binary-safe or large-file reading.
Output is capped inline — use tool_recall or repeat with offset to read further.
```

**Acceptance criteria:**

- [ ] `read({path: "big.txt"})` returns 100 lines plus the trailer showing total line count.
- [ ] `read({path: "big.txt", limit_lines: 200})` returns 200 lines, no trailer.
- [ ] `read({path: "one_line_100kb.json"})` in line mode returns 1 line (the whole
  file) plus a trailer noting it was capped by bytes at `maxReadBytes`.
- [ ] `read({path: "file.bin", offset_byte: 0, limit_bytes: 512})` returns exactly
  512 bytes of raw content.
- [ ] `read({path: "file.txt", offset_byte: 4096})` starts at byte 4096, returns up
  to `defaultReadBytes` bytes, with a continue-offset hint.

---

## Step 4 — `tool_recall` server-side tool

Pass 1 above emits markers referencing `tool_recall("<call_id>")`. The tool must
exist for those markers to be actionable.

### Architecture: server-side, not agent-side

The server already receives the full tool result (up to 100 KB) for audit logging and
stores it in the database. `tool_recall` is therefore a **server-side MCP tool** — it
queries the database for the stored result and returns a paginated slice. It does not
run in the agent/worker process and does not use an in-memory map.

The same inline limits that apply to the original tool call apply here: `tool_recall`
never returns more than `bashInlineBytes` (1 KB) in a single call. The model must
paginate using offsets to retrieve a full large result.

### Tool definition (server-side MCP)

```json
{
  "name": "tool_recall",
  "description": "Retrieve the full output of a previous tool call by its call ID. Output is paginated — use offset_byte and limit_bytes to navigate large results. When the response is truncated a continue hint is included.",
  "parameters": {
    "call_id":    { "type": "string",  "required": true,  "description": "The tool call ID from the redacted tool_result marker" },
    "offset_byte":{ "type": "integer", "required": false, "description": "Byte offset into the stored output (default 0)" },
    "limit_bytes":{ "type": "integer", "required": false, "description": "Max bytes to return (default and max: 1024)" }
  }
}
```

### Server implementation

1. Look up the tool call row by `call_id` in the database.
2. If not found: return `[tool_recall: no result found for call_id "<id>"]`.
3. Read `output[offset_byte : offset_byte + limit_bytes]` (capped at 1 024 bytes).
4. If there is more content beyond the returned slice, append:
   ```
   [showing bytes <start>–<end> of <total>; call tool_recall("<id>", offset_byte=<end>) to continue]
   ```
5. Apply the same `maxBashOutputBytes` (100 KB) cap to what is stored — no more than
   what the server already holds.

### Notes

- `tool_recall` applies the same limits as the original tool — it does not provide
  a bypass to retrieve content beyond the 100 KB server-side cap.
- The server should expose this as a named MCP tool so it appears in the model's tool
  list automatically for any session that has tool history.
- Location: implement in `auxot-server`, not in `auxothq/auxot/pkg/codingtools`.

**Acceptance criteria:**

- [ ] After a `bash` call producing 10 KB output, `tool_recall("<id>")` returns the
  first 1 KB with a continue hint showing `offset_byte=1024`.
- [ ] `tool_recall("<id>", offset_byte=1024)` returns bytes 1024–2048 with a
  continue hint, and so on through the full result.
- [ ] `tool_recall("nonexistent_id")` returns a clear error message.
- [ ] The tool appears in the model's tool list for sessions that have tool history.

---

## Step 5 — Audit and confirm tool_use reaches Claude in both paths

The confusion symptom ("Jeremy uploaded the file") has been observed even in short
sessions that should not have triggered truncation. This step confirms the root cause
before closing the investigation.

### Path A: historyBlob (no session file, or after reseed)

`reconstructAssistantTurn` at line 951 emits `<tool_use name="..." id="...">` as text
inside the blob. This is XML-like text, not Anthropic API objects, but Claude should
read it as context.

**Action:** Add a unit test that:

1. Builds a `[]protocol.ChatMessage` with one assistant turn containing a `ToolCall`
   and a following `tool`-role message.
2. Calls `buildPrompt`.
3. Asserts that `historyBlob` contains a `<tool_use` tag with the matching ID **and**
   the `[tool_result (id: X)]` entry with the matching ID.

This confirms the pairing is present before truncation fires.

### Path B: session resume (`--resume`, delta-only)

In session mode the Claude Code CLI manages the JSONL. The worker sends only
`currentPrompt` via stdin. The session JSONL should contain proper `tool_use` /
`tool_result` Anthropic API blocks.

**Action:** In a dev environment, after a tool call turn, inspect the session JSONL at
`/tmp/auxot-worker/.claude/projects/-tmp-auxot-worker/<session-id>.jsonl` and confirm:

- `tool_use` blocks with IDs are present in assistant message entries.
- Corresponding `tool_result` blocks with matching IDs appear in user message entries.

If `tool_use` blocks are absent, the issue is in how Claude Code CLI writes its
session file — not in the worker code — and should be escalated as a claude CLI bug.

**Note:** The historyBlob truncation warning currently fires even in session mode
(the blob is computed but never sent). After Step 1, suppress this log or skip the
compaction pass entirely when `sessionFileExists` is true.

---

---

## Step 0 — Pre-compaction in the thread coordinator (primary fix, all backends)

**Files:** `auxot-server/internal/coordinator/types.go`,
`auxot-server/internal/server/chat_preparer.go`

### Rationale

The cliworker compaction in Step 1 is a safety net for the historyBlob path (claude
CLI without a session file). But the root fix belongs upstream: the coordinator
assembles `PreparedJob.Messages` for **every** backend — Qwen on GPU, Claude, OpenAI.
Pre-compacting there means Qwen (32K limit) gets the same protection as Claude (200K).

### Step 0a — Add `ContextWindowTokens` to `Route`

`Route` in `coordinator/types.go` already has `Provider`, `Model`. Add:

```go
// ContextWindowTokens is the input token limit for this model/provider.
// 0 means unknown; callers treat 0 as a conservative default (32 000 tokens).
// Used by the chat preparer to pre-compact tool results before dispatch.
ContextWindowTokens int
```

Whoever resolves or constructs a `Route` (the route resolver, `RouteResolver`
implementations) should populate this field from the model registry or provider
config. Fallback when 0: use 32 000 (safe floor for Qwen; Claude/OpenAI will
have explicit values set).

This field has already been added to `coordinator/types.go`.

### Step 0b — Pre-compact `llmMsgs` in `chat_preparer.go`

In `PrepareJob` (server/chat_preparer.go), after the line:

```go
llmMsgs := jobChatMsgsToLLM(result.JobMessage.Messages)
```

and before building `PreparedJob`, call a new function:

```go
llmMsgs = preCompactToolResults(llmMsgs, j.Route.ContextWindowTokens)
```

### `preCompactToolResults` algorithm

```go
// preCompactToolResults redacts tool_result content that is older than
// toolRecallRedactAfterUserMsgs user messages from the end of the slice,
// replacing it with a tool_recall marker. This reduces context size for
// all backends while preserving structural pairing.
//
// The current agentic turn (everything from the last user message onward)
// is never touched.
func preCompactToolResults(msgs []llm.Message, contextWindowTokens int) []llm.Message
```

Constants:

```go
const (
    toolRecallRedactAfterUserMsgs = 3    // redact results older than this many user turns
    defaultContextWindowTokens    = 32_000
    compactBudgetFraction         = 0.75 // target 75% of the context window
)
```

Algorithm:

1. Compute `budget = contextWindowTokens * compactBudgetFraction`. If
   `contextWindowTokens == 0`, use `defaultContextWindowTokens`.
2. Find the last user message index — everything from there onward is the
   protected current agentic turn.
3. Walk backwards through `msgs[:lastUserIdx]`. Count user messages seen.
4. For any `tool` role message (tool_result) that is more than
   `toolRecallRedactAfterUserMsgs` user-messages ago:
   - Replace its `Content` string with:
     ```
     [output redacted — call tool_recall("<tool_call_id>", offset_byte=0) to retrieve]
     ```
   - Keep `ToolCallID` intact so structural pairing is preserved.
5. After age-based redaction, estimate total tokens. If still over budget,
   do a second pass: find the oldest unredacted tool_result in `compactable`
   and redact it, then recheck. Repeat until under budget.
6. Log a single summary line if any redaction occurred:
   ```go
   slog.Info("chat_preparer: pre-compacted tool results",
       "thread_id", j.ReferenceID,
       "redacted_count", n,
       "est_tokens_before", before,
       "est_tokens_after", after,
       "budget", budget,
   )
   ```

Token estimation: `(len(s) + 3) / 4` (same as Step 1).

### Interaction with Step 1 (cliworker compaction)

Step 0 is the primary pass — it runs for all backends. The cliworker's Step 1
compaction is a safety net for the historyBlob path specifically (no session file,
history injected as flat text). Both can coexist: Step 0 reduces the messages in
`PreparedJob`; Step 1 further reduces the serialized text blob when the cliworker
builds it. In practice, Step 0 should prevent Step 1 from ever needing to fire.

**Acceptance criteria:**

- [ ] A Qwen job with a large bash tool result from 4+ user turns ago: the result
  is redacted in `PreparedJob.Messages` before the job is dispatched.
- [ ] A Claude job with the same history: the recent 3 turns of tool results are
  intact; older ones are redacted.
- [ ] The current agentic turn (from the last user message onward) is always intact.
- [ ] `Route.ContextWindowTokens = 0` falls back to the 32K default.
- [ ] A unit test in `server/chat_preparer_test.go` exercises all branches.

---

---

## Step 6 — Thread the pre-compaction step through the debug context viewer

### How the context viewer actually works

`GET /api/jobs/{jobID}/context` (`server/job_context.go`) does NOT read stored
message content. The `dispatch_snapshot` stored in `job.data` is only metadata —
`LatestMsgID`, token counts, compaction flags, `ContextSize`. The handler uses
`LatestMsgID` to load exactly the messages that were visible at dispatch time from
the DB, then re-runs `ws.BuildChatJobMessage` live to reconstruct the windowed
message list. Message content is never duplicated in job data.

### The gap

After Step 0b ships, `chat_preparer.PrepareJob` calls `preCompactToolResults` on
`llmMsgs` after `BuildChatJobMessage`. The context viewer re-runs `BuildChatJobMessage`
but does not call `preCompactToolResults`. So the viewer shows tool results the LLM
never actually saw.

### The fix — one function call in `handleJobContext`

`DispatchSnapshot` already has a `ContextSize` field (populated by `ComputeWindow`).
No new data needs to be stored. Add the compaction step after the existing
`BuildChatJobMessage` call:

```go
// existing
result := ws.BuildChatJobMessage(j, msgs, systemPrompt, nil, 0)

// add — apply the same pre-compaction that PrepareJob ran at dispatch time
llmMsgs := jobChatMsgsToLLM(result.JobMessage.Messages)
llmMsgs = preCompactToolResults(llmMsgs, snap.ContextSize)

// return llmMsgs instead of result.JobMessage.Messages
```

`snap.ContextSize` is already stored in the snapshot, so the re-run uses the same
budget as the original dispatch even if the route config has changed since.

### Optional: expose compaction metadata in the response

Add two fields to `DispatchSnapshot` (populated by `preCompactToolResults`):

```go
ToolResultsCompacted int `json:"tool_results_compacted,omitempty"` // count redacted
EstTokensSent        int `json:"est_tokens_sent,omitempty"`        // after compaction
```

The debug UI can show a banner:
> "2 tool results were compacted before dispatch."

**Acceptance criteria:**

- [ ] The context viewer for a job where tool results were compacted shows the
  redaction markers, not the full DB content.
- [ ] The context viewer for a job with no compaction is identical to today.
- [ ] `snap.ContextSize` (already stored) is used as the budget — no new fields
  required in job data.

---

## Step 7 — Large user message handling

### The problem

User messages can be large in two ways:
- A user pastes a large block of text into the chat input (code, a document, a log)
- A user message injected by a platform integration (Slack thread, email body) that
  is large from the start

Unlike tool results, user messages cannot be silently discarded — they are intentional
input. But once they are old enough to be covered by the running summary, their raw
content in the live window is redundant and expensive.

Threshold: **1 024 bytes** (same as the tool inline limit — consistent model for
what "large" means across the system).

---

### Step 7a — Frontend: large paste → file attachment

**Location:** chat input component (web app)

#### Behavior

When the user pastes text into the chat input box and the pasted content exceeds
1 024 bytes:

1. Intercept the paste event before it is inserted into the input field.
2. Store the pasted content as a named attachment (e.g. `pasted-text.txt`) — either
   upload immediately or hold in local state until send.
3. Insert a pill/chip in the input field: `📎 pasted-text.txt (N KB)` instead of
   the raw text.
4. The user can optionally add a short description in the text field alongside the
   attachment chip.

#### What gets sent to the server

The message is sent as a multipart payload:
- `content` (text field): the user's short description, if any, plus a reference:
  ```
  [attached: pasted-text.txt — N bytes]
  ```
- `attachments`: the full pasted content, stored server-side and associated with the
  message row.

The server stores the full content in the DB. When building the LLM context for the
current turn, the full attachment content is injected inline (the LLM sees the full
paste for the turn it was sent and the agentic loop that follows). After that turn
ages out, Step 7b handles truncation.

#### Why this is better than inline

- The chat UI doesn't freeze rendering a 50KB paste in a text bubble.
- The LLM sees the full content when it matters (current turn) and a compact
  reference thereafter.
- The user gets visual confirmation that a large paste was captured as a file.

**Acceptance criteria:**

- [ ] Pasting ≤ 1 024 bytes: no change from current behavior — text appears inline.
- [ ] Pasting > 1 024 bytes: content is converted to a named attachment chip; the
  text field shows only the chip plus any additional user text.
- [ ] The attachment is visible as a file reference in the chat history after send.
- [ ] The LLM receives the full pasted content on the turn it was sent.

---

### Step 7b — Server: age-based truncation of large user messages + `message_recall`

**Location:** `server/chat_preparer.go` (`preCompactToolResults`, extended) and a
new `message_recall` server-side MCP tool.

#### Extension to `preCompactToolResults`

Rename (or accompany) `preCompactToolResults` with a pass that also handles large
user messages. Combined function: `preCompactMessages`.

The same protected zone applies: the last user message and everything after it is
never touched.

For each `user`-role message in `compactable` that is more than
`toolRecallRedactAfterUserMsgs` (3) user turns old and whose content exceeds
1 024 bytes, replace its `Content` with the first 1 024 bytes followed by a
continuation marker:

```
<first 1024 bytes of message content>
[message truncated — call message_recall("<message_id>", offset_byte=1024) to retrieve the rest]
```

The `message_id` is the DB row ID of the message, available on `llm.Message` (needs
to be threaded through from the message assembly step — add an `ID int64` field to
`llm.Message` if not already present, or carry it as metadata alongside the slice).

#### `message_recall` server-side MCP tool

Parallel to `tool_recall`. Queries the message row from the DB by ID and returns
paginated content.

```json
{
  "name": "message_recall",
  "description": "Retrieve the full content of a previous user message by its ID. Use offset_byte and limit_bytes to paginate large messages.",
  "parameters": {
    "message_id": { "type": "integer", "required": true },
    "offset_byte": { "type": "integer", "required": false, "description": "Byte offset (default 0)" },
    "limit_bytes": { "type": "integer", "required": false, "description": "Max bytes to return (default and max: 1024)" }
  }
}
```

Same pagination behavior as `tool_recall`: cap at 1 024 bytes per call, include a
continue hint when there is more.

#### What the LLM sees across turns

| Turn | User message content in context |
|------|-------------------------------|
| Current (N) | Full content inline |
| N−1, N−2 | Full content (within 3-turn window) |
| N−3 and older, if > 1 KB | First 1 KB + `message_recall` continue hint |
| N−3 and older, if ≤ 1 KB | Full content (small messages stay) |

#### Threading `message_id` through

`llm.Message` needs to carry the source DB row ID so `preCompactMessages` can emit
the correct `message_recall` marker. Options:

1. Add `SourceMsgID int64` to `llm.Message` — cleanest, minimal surface area.
2. Carry a parallel `[]int64` ID slice alongside `[]llm.Message` through the
   preparer — avoids modifying the shared `llm` package.

Prefer option 1. `llm.Message` does not currently have such a field. Add it with
`json:"-"` so it is never serialized into the wire request to the LLM provider:

```go
// SourceMsgID is the DB row ID of the originating message.Message row.
// Populated by the chat preparer for compaction purposes; not sent to the LLM.
SourceMsgID int64 `json:"-"`
```

**Acceptance criteria:**

- [ ] A user message > 1 KB from 4 turns ago: truncated to the marker in the LLM
  context; full content retrievable via `message_recall`.
- [ ] A user message ≤ 1 KB from 4 turns ago: full content retained.
- [ ] The current user message and the 3 most recent user turns: always full content.
- [ ] `message_recall(id, offset_byte=0)` returns the first 1 KB with a continue
  hint if there is more.
- [ ] `message_recall(id, offset_byte=1024)` returns bytes 1024–2048, etc.
- [ ] `message_recall` with an invalid or inaccessible ID returns a clear error.

---

## Implementation Order

| # | Step | Repo | File(s) | Risk |
|---|------|------|---------|------|
| 2 | bash 1KB inline tail + full to server | `auxot` | `codingtools/bash.go` | Low |
| 3 | read line+byte offset modes + default cap | `auxot` | `codingtools/read.go` | Low |
| 4 | `tool_recall` server-side MCP tool | `auxot-server` | server MCP handler | Low |
| 0a | `ContextWindowTokens` on `Route` + populate at route resolution | `auxot-server` | `coordinator/types.go`, route resolvers | Low |
| 0b | `preCompactToolResults` in `chat_preparer.go` | `auxot-server` | `server/chat_preparer.go` | Medium |
| 1 | Token-aware compaction in cliworker (safety net) | `auxot` | `cliworker/claude.go` | Medium |
| 5A | Unit test: historyBlob tool_use pairing | `auxot` | test file | None |
| 5B | Manual JSONL audit in dev | — | dev environment | None |
| 6 | Thread pre-compaction through context viewer | `auxot-server` | `server/job_context.go` | Low |
| 7a | Frontend: large paste → file attachment | `auxot` (web) | chat input component | Medium |
| 7b | Server: age-based truncation of large user messages + `message_recall` tool | `auxot-server` | `server/chat_preparer.go`, server MCP handler | Medium |

Steps 2 and 3 ship first — they reduce tool result sizes before compaction runs.
Step 4 ships before 0b and 1 so `tool_recall` markers are usable the moment they
appear. Step 0b (coordinator pre-compaction) is the belt; Step 1 (cliworker safety
net) is the suspenders — both should be implemented, but 0b is the higher-value fix.

Step 6 should ship alongside 0b — the moment compaction is live, the debug view
must reflect it or it becomes actively misleading.

Steps 7a and 7b are independent of each other and of the other steps. 7a is a
pure frontend change; 7b extends the same `preCompactToolResults` pass already
planned in Step 0b.
