package claude

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ---- extractContent tests ---------------------------------------------------

func TestExtractContentString(t *testing.T) {
	raw := json.RawMessage(`"hello world"`)
	got := extractContent(raw)
	if got != "hello world" {
		t.Fatalf("got %q, want %q", got, "hello world")
	}
}

func TestExtractContentArray(t *testing.T) {
	raw := json.RawMessage(`[
		{"type":"text","text":"Here is the answer"},
		{"type":"tool_use","name":"Bash"},
		{"type":"thinking","thinking":"internal reasoning"},
		{"type":"tool_result","content":"output"}
	]`)
	got := extractContent(raw)
	want := "Here is the answer\n⚙ Bash"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestExtractContentArrayOnlyThinking(t *testing.T) {
	raw := json.RawMessage(`[{"type":"thinking","thinking":"skip me"}]`)
	got := extractContent(raw)
	if got != "" {
		t.Fatalf("got %q, want empty string", got)
	}
}

func TestExtractContentEmpty(t *testing.T) {
	if got := extractContent(nil); got != "" {
		t.Fatalf("got %q, want empty", got)
	}
	if got := extractContent(json.RawMessage("")); got != "" {
		t.Fatalf("got %q, want empty", got)
	}
}

func TestExtractContentUnknownBlock(t *testing.T) {
	raw := json.RawMessage(`[{"type":"mystery","data":"ignored"}]`)
	got := extractContent(raw)
	if got != "" {
		t.Fatalf("got %q, want empty string for unknown block type", got)
	}
}

// ---- renderJSONL tests ------------------------------------------------------

// buildJSONL creates JSONL bytes from lines for test convenience.
func buildJSONL(lines []string) []byte {
	return []byte(strings.Join(lines, "\n"))
}

// A snapshot of synthetic events representing a short conversation.
var syntheticLines = []string{
	// non-message line: should be skipped
	`{"type":"system","message":{"role":"system","content":"You are helpful."}}`,
	// user message with string content
	`{"type":"user","message":{"role":"user","content":"What is 2+2?"}}`,
	// assistant message with array content (text + tool_use + thinking skipped)
	`{"type":"assistant","message":{"role":"assistant","content":[{"type":"thinking","thinking":"let me think"},{"type":"text","text":"The answer is 4."},{"type":"tool_use","name":"Calculator"}]}}`,
	// another user message
	`{"type":"user","message":{"role":"user","content":"Thanks!"}}`,
	// empty line should be skipped silently
	``,
}

func TestRenderJSONLBasic(t *testing.T) {
	data := buildJSONL(syntheticLines)
	got := renderJSONL(data, 200)

	// Should have 3 messages: user, assistant, user
	// Check headers appear in order
	if !strings.Contains(got, "› You") {
		t.Error("missing '› You' header")
	}
	if !strings.Contains(got, "⏺ Claude") {
		t.Error("missing '⏺ Claude' header")
	}

	// Check content
	if !strings.Contains(got, "What is 2+2?") {
		t.Error("missing first user message")
	}
	if !strings.Contains(got, "The answer is 4.") {
		t.Error("missing assistant text block")
	}
	if !strings.Contains(got, "⚙ Calculator") {
		t.Error("missing tool_use rendered as ⚙")
	}
	if !strings.Contains(got, "Thanks!") {
		t.Error("missing second user message")
	}

	// thinking block must be absent
	if strings.Contains(got, "let me think") {
		t.Error("thinking block leaked into output")
	}

	// system line must be absent
	if strings.Contains(got, "You are helpful") {
		t.Error("system line leaked into output")
	}
}

func TestRenderJSONLOrdering(t *testing.T) {
	data := buildJSONL(syntheticLines)
	got := renderJSONL(data, 200)

	userIdx := strings.Index(got, "What is 2+2?")
	assistantIdx := strings.Index(got, "The answer is 4.")
	thanks := strings.Index(got, "Thanks!")

	if userIdx < 0 || assistantIdx < 0 || thanks < 0 {
		t.Fatal("one or more expected messages missing from output")
	}
	if !(userIdx < assistantIdx && assistantIdx < thanks) {
		t.Errorf("messages out of order: user=%d assistant=%d thanks=%d", userIdx, assistantIdx, thanks)
	}
}

func TestRenderJSONLMaxMessages(t *testing.T) {
	// 3 real messages in syntheticLines (user, assistant, user).
	// maxMessages=1 should keep only the last one ("Thanks!").
	data := buildJSONL(syntheticLines)
	got := renderJSONL(data, 1)

	if strings.Contains(got, "What is 2+2?") {
		t.Error("first user message should have been truncated")
	}
	if strings.Contains(got, "The answer is 4.") {
		t.Error("assistant message should have been truncated")
	}
	if !strings.Contains(got, "Thanks!") {
		t.Error("last message should be present")
	}
}

func TestRenderJSONLMaxMessagesTwo(t *testing.T) {
	data := buildJSONL(syntheticLines)
	got := renderJSONL(data, 2)

	// Should keep last 2: assistant + user("Thanks!")
	if strings.Contains(got, "What is 2+2?") {
		t.Error("oldest message should be dropped with maxMessages=2")
	}
	if !strings.Contains(got, "The answer is 4.") {
		t.Error("second-to-last message (assistant) should be present")
	}
	if !strings.Contains(got, "Thanks!") {
		t.Error("last message should be present")
	}
}

func TestRenderJSONLHeaders(t *testing.T) {
	lines := []string{
		`{"type":"user","message":{"role":"user","content":"Hi"}}`,
		`{"type":"assistant","message":{"role":"assistant","content":"Hello"}}`,
	}
	got := renderJSONL(buildJSONL(lines), 200)

	// First header should be › You
	firstHeader := strings.Index(got, "› You")
	claudeHeader := strings.Index(got, "⏺ Claude")
	if firstHeader < 0 {
		t.Fatal("'› You' not found")
	}
	if claudeHeader < 0 {
		t.Fatal("'⏺ Claude' not found")
	}
	if firstHeader > claudeHeader {
		t.Error("user header should come before assistant header")
	}
}

func TestRenderJSONLSkipsEmptyContent(t *testing.T) {
	lines := []string{
		// message with only whitespace text — should be skipped
		`{"type":"user","message":{"role":"user","content":"   "}}`,
		`{"type":"user","message":{"role":"user","content":"Real message"}}`,
	}
	got := renderJSONL(buildJSONL(lines), 200)

	// Only one › You header for the real message
	count := strings.Count(got, "› You")
	if count != 1 {
		t.Errorf("expected 1 user header, got %d", count)
	}
}

func TestRenderJSONLInvalidLinesSkipped(t *testing.T) {
	lines := []string{
		`not valid json {{{`,
		`{"type":"user","message":{"role":"user","content":"Valid"}}`,
	}
	got := renderJSONL(buildJSONL(lines), 200)
	if !strings.Contains(got, "Valid") {
		t.Error("valid line after invalid JSON should still appear")
	}
}

// ---- encodeCWD tests --------------------------------------------------------

func TestEncodeCWD(t *testing.T) {
	cases := []struct {
		cwd  string
		want string
	}{
		{"/Users/marlin/wrinkles", "-Users-marlin-wrinkles"},
		{"/Users/marlin/my.project", "-Users-marlin-my-project"},
		{"/Users/marlin/my_project", "-Users-marlin-my-project"},
	}
	for _, tc := range cases {
		got := encodeCWD(tc.cwd)
		if got != tc.want {
			t.Errorf("encodeCWD(%q) = %q, want %q", tc.cwd, got, tc.want)
		}
	}
}

func TestResolveRejectsMaliciousCheckpointID(t *testing.T) {
	home := t.TempDir()
	projects := filepath.Join(home, ".claude", "projects", "-proj")
	if err := os.MkdirAll(projects, 0o755); err != nil {
		t.Fatal(err)
	}
	validID := "dc47b9c2-97b1-4709-8aad-9396b2d7c37c"
	if err := os.WriteFile(filepath.Join(projects, validID+".jsonl"),
		[]byte(`{"type":"user","message":{"role":"user","content":"hi"}}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A secret file outside ~/.claude/projects that traversal might target.
	secret := filepath.Join(home, "secret.jsonl")
	if err := os.WriteFile(secret, []byte(`{"type":"user","message":{"role":"user","content":"SECRET"}}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := newResolver(home)

	// Valid UUID resolves.
	if res, err := r.Render(Request{ResumeBinding: map[string]any{"kind": "claude", "checkpoint_id": validID}, MaxMessages: 100}); err != nil || !res.Supported || res.SessionID != validID {
		t.Fatalf("valid id: supported=%v sid=%q err=%v", res.Supported, res.SessionID, err)
	}

	// Malicious values must NOT resolve to any file (fall through to "" → text empty).
	for _, bad := range []string{
		"../../secret",
		"../../../../../../etc/passwd",
		"*",
		"?",
		"[a-z]",
		"..",
		validID + "/../../secret",
	} {
		res, err := r.Render(Request{ResumeBinding: map[string]any{"kind": "claude", "checkpoint_id": bad}, MaxMessages: 100})
		if err != nil {
			t.Fatalf("bad id %q returned error %v (should quietly not resolve)", bad, err)
		}
		if strings.Contains(res.Text, "SECRET") {
			t.Fatalf("bad id %q LEAKED the out-of-tree file", bad)
		}
	}
}

// A surface whose checkpoint_id names a session with no file on disk must
// render nothing — NOT the newest session in its project dir. Many surfaces
// share one cwd, so that fallback served a different surface's conversation.
func TestResolveMissingCheckpointFileDoesNotFallBackToCWD(t *testing.T) {
	home := t.TempDir()
	cwd := "/Users/someone/proj"
	projects := filepath.Join(home, ".claude", "projects", encodeCWD(cwd))
	if err := os.MkdirAll(projects, 0o755); err != nil {
		t.Fatal(err)
	}
	// A different surface's session, sitting in the same project dir.
	otherID := "aaaaaaaa-1111-2222-3333-444444444444"
	if err := os.WriteFile(filepath.Join(projects, otherID+".jsonl"),
		[]byte(`{"type":"user","message":{"role":"user","content":"OTHER SURFACE"}}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := newResolver(home)

	// This surface points at a session whose file is gone.
	goneID := "bbbbbbbb-1111-2222-3333-444444444444"
	res, err := r.Render(Request{
		ResumeBinding: map[string]any{
			"kind":          "claude",
			"checkpoint_id": goneID,
			"cwd":           cwd,
		},
		MaxMessages: 100,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(res.Text, "OTHER SURFACE") {
		t.Fatalf("served another surface's transcript: %q", res.Text)
	}
	if res.Text != "" {
		t.Fatalf("want empty text for a missing session file, got %q", res.Text)
	}
	if !res.SessionMissing {
		t.Fatal("want SessionMissing=true so the client can say why it's empty")
	}
	if res.SessionID != goneID {
		t.Fatalf("want the bound session echoed back, got %q", res.SessionID)
	}
	if !res.Supported {
		t.Fatal("want Supported=true — it is still a claude surface")
	}
}

// With no checkpoint_id at all, the cwd fallback is still the best guess.
func TestResolveWithoutCheckpointFallsBackToLatestInCWD(t *testing.T) {
	home := t.TempDir()
	cwd := "/Users/someone/proj"
	projects := filepath.Join(home, ".claude", "projects", encodeCWD(cwd))
	if err := os.MkdirAll(projects, 0o755); err != nil {
		t.Fatal(err)
	}
	onlyID := "aaaaaaaa-1111-2222-3333-444444444444"
	if err := os.WriteFile(filepath.Join(projects, onlyID+".jsonl"),
		[]byte(`{"type":"user","message":{"role":"user","content":"HELLO"}}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := newResolver(home)

	res, err := r.Render(Request{ResumeBinding: map[string]any{"kind": "claude", "cwd": cwd}, MaxMessages: 100})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(res.Text, "HELLO") {
		t.Fatalf("want the cwd fallback to resolve, got %q", res.Text)
	}
	if res.SessionID != onlyID {
		t.Fatalf("want session %q, got %q", onlyID, res.SessionID)
	}
	if res.SessionMissing {
		t.Fatal("want SessionMissing=false — nothing was bound to begin with")
	}
}

func TestReadTail(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	content := []byte(strings.Repeat("A", 100) + "TAILMARKER")
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatal(err)
	}
	// size <= max → full read
	full, err := readTail(path, int64(len(content)), 1<<20)
	if err != nil || string(full) != string(content) {
		t.Fatalf("full read: %v", err)
	}
	// size > max → only the tail
	tail, err := readTail(path, int64(len(content)), 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(tail) != 20 || !strings.HasSuffix(string(tail), "TAILMARKER") {
		t.Fatalf("tail read = %q, want last 20 bytes ending TAILMARKER", string(tail))
	}
}
