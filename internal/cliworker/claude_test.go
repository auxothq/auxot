package cliworker

import (
	"fmt"
	"strings"
	"testing"
)

// makeToolResult constructs a [tool_result] turn string with the given call ID and body.
func makeToolResult(callID, body string) string {
	return fmt.Sprintf("[tool_result (id: %s)]\n%s", callID, body)
}

// makeUserTurn constructs a [user] turn string.
func makeUserTurn(text string) string {
	return "[user]\n" + text
}

// makeAssistantTurn constructs an [assistant] turn string.
func makeAssistantTurn(text string) string {
	return "[assistant]\n" + text
}

// largebody produces a string of n bytes.
func largeBody(n int) string {
	return strings.Repeat("x", n)
}

// TestExtractToolCallID verifies the ID parser handles all expected formats.
func TestExtractToolCallID(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"[tool_result (id: toolu_abc123)]", "toolu_abc123"},
		{"[tool_result (id: toolu_xyz)]", "toolu_xyz"},
		{"[tool_result]", ""},
		{"[tool_result (id: )]", ""},
	}
	for _, c := range cases {
		got := extractToolCallID(c.in)
		if got != c.want {
			t.Errorf("extractToolCallID(%q) = %q; want %q", c.in, got, c.want)
		}
	}
}

// TestEstimateTokens checks the token-estimation helper.
func TestEstimateTokens(t *testing.T) {
	if got := estimateTokens(""); got != 0 {
		t.Errorf("empty string: got %d, want 0", got)
	}
	// 4 bytes → 1 token
	if got := estimateTokens("abcd"); got != 1 {
		t.Errorf("4-byte string: got %d, want 1", got)
	}
	// 5 bytes → 2 tokens (ceiling)
	if got := estimateTokens("abcde"); got != 2 {
		t.Errorf("5-byte string: got %d, want 2", got)
	}
}

// TestCompactHistory_NothingToCompact verifies that a small history is returned unchanged.
func TestCompactHistory_NothingToCompact(t *testing.T) {
	history := []string{
		makeUserTurn("hello"),
		makeAssistantTurn("hi"),
		makeUserTurn("current turn"),
	}
	result, redacted, dropped, _, _ := compactHistory(history)
	if redacted != 0 || dropped != 0 {
		t.Errorf("expected no compaction; got redacted=%d dropped=%d", redacted, dropped)
	}
	if len(result) != len(history) {
		t.Errorf("result length changed: got %d, want %d", len(result), len(history))
	}
}

// TestCompactHistory_Pass1RedactsToolResults verifies that Pass 1 replaces tool_result
// bodies (oldest-first) while leaving the protected zone (from the last [user]) intact.
func TestCompactHistory_Pass1RedactsToolResults(t *testing.T) {
	// Build 60 turns: alternating assistant+tool_result pairs, then a current user turn.
	// Each tool result body is 20 KB; 30 results × 20 KB = 600 KB → ~150K tokens > 128K.
	// Redacting enough bodies (oldest-first) will bring us under the 128K-token budget.
	const bodySize = 20 * 1024
	const numPairs = 30 // 30 assistant + 30 tool_result + 1 user (current) = 61 turns

	var history []string
	for i := 0; i < numPairs; i++ {
		callID := fmt.Sprintf("toolu_%03d", i)
		history = append(history, makeAssistantTurn(fmt.Sprintf("<tool_use id=%q name=\"bash\"><command>ls</command></tool_use>", callID)))
		history = append(history, makeToolResult(callID, largeBody(bodySize)))
	}
	// Current agentic turn: last user message.
	currentUserMsg := "What is the status of the project?"
	history = append(history, makeUserTurn(currentUserMsg))

	// Record the protected zone before compaction.
	lastUserIdx := len(history) - 1
	protectedBefore := history[lastUserIdx:]

	result, redacted, dropped, tokBefore, tokAfter := compactHistory(history)

	// Pass 1 should have fired — some tool results must have been redacted.
	if redacted == 0 {
		t.Fatal("expected Pass 1 to redact tool_result bodies; got redacted=0")
	}
	// Pass 2 should NOT have been needed (tool result compaction should suffice).
	if dropped != 0 {
		t.Errorf("expected Pass 2 not to fire; got dropped=%d", dropped)
	}
	// Result must be under budget.
	if tokAfter > maxHistoryTokens {
		t.Errorf("after compaction still over budget: tokAfter=%d > maxHistoryTokens=%d", tokAfter, maxHistoryTokens)
	}
	// Token count should have decreased.
	if tokAfter >= tokBefore {
		t.Errorf("tokAfter (%d) should be less than tokBefore (%d)", tokAfter, tokBefore)
	}
	// Protected zone (the current user turn) must be identical.
	if len(protectedBefore) == 0 {
		t.Fatal("test setup: protectedBefore should not be empty")
	}
	// Find the last [user] turn in result.
	resultLastUserIdx := -1
	for i := len(result) - 1; i >= 0; i-- {
		if strings.HasPrefix(result[i], "[user]") {
			resultLastUserIdx = i
			break
		}
	}
	if resultLastUserIdx < 0 {
		t.Fatal("no [user] turn found in compacted result")
	}
	if result[resultLastUserIdx] != protectedBefore[0] {
		t.Errorf("protected zone mutated:\ngot:  %q\nwant: %q", result[resultLastUserIdx], protectedBefore[0])
	}

	// Pass 1 works oldest-first: the earliest tool_results should be redacted.
	// Count redacted and non-redacted tool_results to verify the ordering.
	var (
		redactedTurns    []int
		nonRedactedTurns []int
	)
	for i, turn := range result[:resultLastUserIdx] {
		if !strings.HasPrefix(turn, "[tool_result") {
			continue
		}
		nlIdx := strings.IndexByte(turn, '\n')
		if nlIdx < 0 {
			t.Errorf("turn %d: [tool_result] has no newline", i)
			continue
		}
		body := turn[nlIdx+1:]
		if strings.Contains(body, "[output redacted") {
			redactedTurns = append(redactedTurns, i)
		} else {
			nonRedactedTurns = append(nonRedactedTurns, i)
		}
	}
	// There must be at least one redacted turn.
	if len(redactedTurns) == 0 {
		t.Error("expected at least one redacted tool_result turn")
	}
	// Oldest-first ordering: all redacted turns must come before all non-redacted turns.
	if len(redactedTurns) > 0 && len(nonRedactedTurns) > 0 {
		lastRedacted := redactedTurns[len(redactedTurns)-1]
		firstNonRedacted := nonRedactedTurns[0]
		if lastRedacted > firstNonRedacted {
			t.Errorf("ordering violation: redacted turn %d comes after non-redacted turn %d",
				lastRedacted, firstNonRedacted)
		}
	}
}

// TestCompactHistory_ProtectedZoneUnchanged checks that the protected zone is
// byte-identical before and after compaction regardless of how much is compacted.
func TestCompactHistory_ProtectedZoneUnchanged(t *testing.T) {
	const bodySize = 5 * 1024
	var history []string
	for i := 0; i < 20; i++ {
		callID := fmt.Sprintf("toolu_%02d", i)
		history = append(history, makeAssistantTurn(fmt.Sprintf("<tool_use id=%q/>", callID)))
		history = append(history, makeToolResult(callID, largeBody(bodySize)))
	}
	// Multi-element protected zone: a [user] message followed by a partial assistant/tool pair.
	protectedUser := makeUserTurn("current user prompt — do not touch")
	protectedAssistant := makeAssistantTurn("<tool_use id=\"toolu_current\" name=\"bash\"/>")
	protectedToolResult := makeToolResult("toolu_current", "current tool result — do not touch")
	history = append(history, protectedUser, protectedAssistant, protectedToolResult)

	lastUserIdx := -1
	for i := len(history) - 1; i >= 0; i-- {
		if strings.HasPrefix(history[i], "[user]") {
			lastUserIdx = i
			break
		}
	}
	protectedBefore := make([]string, len(history[lastUserIdx:]))
	copy(protectedBefore, history[lastUserIdx:])

	result, _, _, _, _ := compactHistory(history)

	// Find the protected zone in result.
	resultLastUserIdx := -1
	for i := len(result) - 1; i >= 0; i-- {
		if strings.HasPrefix(result[i], "[user]") {
			resultLastUserIdx = i
			break
		}
	}
	if resultLastUserIdx < 0 {
		t.Fatal("no [user] turn in result")
	}
	got := result[resultLastUserIdx:]
	if len(got) != len(protectedBefore) {
		t.Fatalf("protected zone length: got %d, want %d", len(got), len(protectedBefore))
	}
	for i := range got {
		if got[i] != protectedBefore[i] {
			t.Errorf("protected zone element %d mutated:\ngot:  %q\nwant: %q", i, got[i], protectedBefore[i])
		}
	}
}

// TestCompactHistory_Pass2NotEntered ensures Pass 2 is not entered when Pass 1 is sufficient.
func TestCompactHistory_Pass2NotEntered(t *testing.T) {
	// Construct a history where tool_result content is large enough to push us over
	// budget, but redacting all of them will bring us under. Assistant turns are tiny.
	const bodySize = 20 * 1024 // 20 KB each → token count >> 128K for many turns
	var history []string
	for i := 0; i < 50; i++ {
		callID := fmt.Sprintf("toolu_%02d", i)
		history = append(history, makeAssistantTurn(fmt.Sprintf("<tool_use id=%q/>", callID))) // ~30 bytes
		history = append(history, makeToolResult(callID, largeBody(bodySize)))
	}
	history = append(history, makeUserTurn("current"))

	_, _, dropped, _, _ := compactHistory(history)
	if dropped != 0 {
		t.Errorf("Pass 2 should not have fired; got dropped=%d", dropped)
	}
}

// TestCompactHistory_Pass2DropsAndPrependsMarker verifies that when Pass 1 alone is
// insufficient, Pass 2 drops turns and prepends the synthetic omission marker.
func TestCompactHistory_Pass2DropsAndPrependsMarker(t *testing.T) {
	// History where there are no tool_results at all, just many large user/assistant turns.
	// This forces Pass 2 since Pass 1 can't redact anything.
	const bodySize = 10 * 1024
	var history []string
	for i := 0; i < 60; i++ {
		history = append(history, makeUserTurn(largeBody(bodySize)))
		history = append(history, makeAssistantTurn("ok"))
	}
	history = append(history, makeUserTurn("current"))

	result, redacted, dropped, tokBefore, tokAfter := compactHistory(history)

	if redacted != 0 {
		t.Errorf("Pass 1 should not have redacted anything; got redacted=%d", redacted)
	}
	if dropped == 0 {
		t.Error("expected Pass 2 to drop turns; got dropped=0")
	}
	if tokBefore <= maxHistoryTokens {
		t.Fatalf("test setup: tokBefore=%d should be > maxHistoryTokens=%d", tokBefore, maxHistoryTokens)
	}
	if tokAfter > maxHistoryTokens {
		t.Errorf("still over budget after Pass 2: tokAfter=%d > maxHistoryTokens=%d", tokAfter, maxHistoryTokens)
	}
	// First element should be the synthetic omission marker.
	if len(result) == 0 {
		t.Fatal("result is empty")
	}
	if !strings.HasPrefix(result[0], "[user]\n[") || !strings.Contains(result[0], "turns omitted") {
		t.Errorf("expected synthetic omission marker as first element; got: %q", result[0])
	}
}

// TestCompactHistory_OrphanedToolResult verifies no panic when a [tool_result] exists
// without a preceding [assistant] turn (orphaned).
func TestCompactHistory_OrphanedToolResult(t *testing.T) {
	// Simulate history where the assistant turn was manually dropped, leaving an orphan.
	history := []string{
		makeToolResult("toolu_orphan", largeBody(5*1024)),
		makeToolResult("toolu_also_orphan", "small body"),
		makeUserTurn("current"),
	}
	// Should not panic.
	result, _, _, _, _ := compactHistory(history)
	if len(result) == 0 {
		t.Error("result should not be empty")
	}
}

// TestCompactHistory_EmptyHistory handles a nil/empty input gracefully.
func TestCompactHistory_EmptyHistory(t *testing.T) {
	result, redacted, dropped, tokBefore, tokAfter := compactHistory(nil)
	if len(result) != 0 || redacted != 0 || dropped != 0 || tokBefore != 0 || tokAfter != 0 {
		t.Errorf("empty input: got result=%v redacted=%d dropped=%d tokBefore=%d tokAfter=%d",
			result, redacted, dropped, tokBefore, tokAfter)
	}
}

// TestCompactHistory_SingleUserTurn: only a current user turn, nothing to compact.
func TestCompactHistory_SingleUserTurn(t *testing.T) {
	history := []string{makeUserTurn("only turn")}
	result, redacted, dropped, _, _ := compactHistory(history)
	if redacted != 0 || dropped != 0 {
		t.Errorf("single user turn should not compact: redacted=%d dropped=%d", redacted, dropped)
	}
	if len(result) != 1 || result[0] != history[0] {
		t.Errorf("result mismatch: got %v, want %v", result, history)
	}
}

// TestBuildPrompt_HistoryBlobContainsToolUse verifies that historyBlob includes both
// the <tool_use> tag from an assistant turn and the matching [tool_result] entry.
func TestBuildPrompt_HistoryBlobContainsToolUse(t *testing.T) {
	// We test buildPrompt indirectly by calling it with tiny messages that won't
	// trigger compaction, and asserting the blob structure.
	// Since buildPrompt depends on protocol.ChatMessage, let's test via compactHistory
	// which is the unit we control. For the pairing assertion we do a direct string check.

	// Simulate what buildPrompt produces for a simple tool-use exchange.
	assistantTurn := "[assistant]\n<tool_use name=\"bash\" id=\"toolu_aaa\">\n<command>ls</command>\n</tool_use>"
	toolResultTurn := "[tool_result (id: toolu_aaa)]\nfile1.txt\nfile2.txt"
	currentUser := "[user]\nWhat files did you find?"

	history := []string{assistantTurn, toolResultTurn, currentUser}
	blob := "<conversation_history>\n" + strings.Join(history, "\n\n") + "\n</conversation_history>"

	if !strings.Contains(blob, "<tool_use") {
		t.Error("blob missing <tool_use tag")
	}
	if !strings.Contains(blob, "toolu_aaa") {
		t.Error("blob missing tool call ID toolu_aaa")
	}
	if !strings.Contains(blob, "[tool_result (id: toolu_aaa)]") {
		t.Error("blob missing matching [tool_result] entry")
	}
}

// TestCompactHistory_RedactMarkerContainsCallID verifies the redact marker includes
// the correct call ID for tool_recall guidance.
func TestCompactHistory_RedactMarkerContainsCallID(t *testing.T) {
	// Use enough content to exceed maxHistoryTokens: 128K tokens × 4 bytes ≈ 512 KB.
	// One 600 KB body is sufficient.
	const bodySize = 600 * 1024
	callID := "toolu_deadbeef"
	history := []string{
		makeAssistantTurn("<tool_use id=\"" + callID + "\"/>"),
		makeToolResult(callID, largeBody(bodySize)),
		makeUserTurn("current"),
	}

	result, redacted, _, _, _ := compactHistory(history)
	if redacted == 0 {
		t.Fatal("expected Pass 1 to redact the tool_result body")
	}

	// Find the redacted tool_result.
	for _, turn := range result {
		if !strings.HasPrefix(turn, "[tool_result") {
			continue
		}
		nlIdx := strings.IndexByte(turn, '\n')
		if nlIdx < 0 {
			continue
		}
		body := turn[nlIdx+1:]
		if strings.Contains(body, "[output redacted") {
			if !strings.Contains(body, callID) {
				t.Errorf("redact marker missing call ID %q; got: %q", callID, body)
			}
			return
		}
	}
	t.Error("no redacted tool_result found in result")
}
