package transcriptrender

import (
	"strings"
	"testing"
)

func TestCapTail(t *testing.T) {
	// Under the cap: unchanged.
	if got := CapTail("hello"); got != "hello" {
		t.Fatalf("small input changed: %q", got)
	}
	// Over the cap: trimmed to <= MaxRenderBytes, ends preserved, starts at a line boundary.
	var b strings.Builder
	for i := 0; i < 20000; i++ {
		b.WriteString("line ")
		b.WriteString(strings.Repeat("x", 10))
		b.WriteByte('\n')
	}
	big := b.String() + "FINAL-TAIL-LINE\n"
	got := CapTail(big)
	if len(got) > MaxRenderBytes {
		t.Fatalf("capTail returned %d bytes, want <= %d", len(got), MaxRenderBytes)
	}
	if !strings.HasSuffix(got, "FINAL-TAIL-LINE\n") {
		t.Fatal("capTail dropped the most-recent content")
	}
	if strings.HasPrefix(got, "ine ") || got[0] == 'x' {
		t.Fatalf("capTail did not start at a line boundary: %q", got[:20])
	}
}

// The shared renderer keeps every agent's transcript looking the same, so its
// layout rules are pinned here rather than only through one agent's package.
func TestRenderLaysOutTurns(t *testing.T) {
	got := Render([]Message{
		{IsUser: true, Text: "run the tests"},
		{Text: "On it."},
		{Text: ToolLine("bash", map[string]any{"command": "go   test\n./..."})},
		{Text: ToolLine("read", map[string]any{"filePath": "/tmp/x.go"})},
		{IsUser: true, Text: "thanks"},
	}, "opencode", 100)

	if n := strings.Count(got, "\u23fa opencode"); n != 1 {
		t.Errorf("want one agent header for a run of agent messages, got %d:\n%s", n, got)
	}
	if n := strings.Count(got, "\u203a You"); n != 2 {
		t.Errorf("want a header for each user turn, got %d:\n%s", n, got)
	}
	if !strings.Contains(got, "bash: go test ./...") {
		t.Errorf("tool command not summarized on one line:\n%s", got)
	}
	if !strings.Contains(got, "read: /tmp/x.go") {
		t.Errorf("opencode's filePath spelling not recognized:\n%s", got)
	}
	if strings.Contains(got, "On it.\n\n") {
		t.Errorf("blank line inserted inside a single turn:\n%s", got)
	}
}

func TestRenderSkipsEmptyAndBounds(t *testing.T) {
	got := Render([]Message{
		{IsUser: true, Text: "  "},
		{Text: ""},
		{IsUser: true, Text: "real"},
	}, "opencode", 100)
	if strings.Count(got, "\u203a You") != 1 {
		t.Errorf("an empty message produced a header:\n%s", got)
	}

	many := make([]Message, 10)
	for i := range many {
		many[i] = Message{Text: "m"}
	}
	if n := strings.Count(Render(many, "opencode", 3), "m\n"); n != 3 {
		t.Errorf("maxMessages not applied: got %d", n)
	}
}

func TestToolLineWithoutUsefulInput(t *testing.T) {
	if got := ToolLine("todowrite", map[string]any{"todos": []any{}}); got != "\u2699 todowrite" {
		t.Errorf("want a bare tool name, got %q", got)
	}
	if got := ToolLine("read", nil); got != "\u2699 read" {
		t.Errorf("nil input should not panic or annotate, got %q", got)
	}
	if got := ToolLine("read", map[string]any{"path": "/a", "offset": float64(12)}); got != "\u2699 read: /a" {
		t.Errorf("want the path preferred, got %q", got)
	}
}
