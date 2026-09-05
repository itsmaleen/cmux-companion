// Package transcriptrender turns an agent's conversation into the text a
// surface card shows.
//
// Every supported agent stores its session differently — Claude Code appends
// JSONL per session, opencode writes rows to SQLite — but what the phone renders
// must look the same for all of them, and must keep looking the same as agents
// are added. This package owns that shared shape so the per-agent packages only
// have to answer "what were the messages".
package transcriptrender

import (
	"fmt"
	"strings"
)

// Message is one rendered turn fragment: a text block, or a tool call.
type Message struct {
	// IsUser selects the header. Everything not from the user renders as the
	// agent, including tool calls it made.
	IsUser bool
	// Text is the already-extracted body: prose, or a tool summary line.
	Text string
}

// MaxRenderBytes bounds a rendered transcript so a long session can't produce a
// payload that blows past WebSocket message-size limits or bogs down the
// client's text view. The tail (most recent conversation) is kept.
const MaxRenderBytes = 128 * 1024

// Render lays messages out as a conversation: one header per turn, blank lines
// between turns but not within one, oldest first, capped to the most recent
// maxMessages and MaxRenderBytes.
//
// AgentLabel names the non-user side ("Claude", "opencode").
func Render(messages []Message, agentLabel string, maxMessages int) string {
	// Drop anything with no body — an empty bubble is noise, and it would also
	// take a header with it.
	kept := messages[:0:0]
	for _, m := range messages {
		if strings.TrimSpace(m.Text) != "" {
			kept = append(kept, m)
		}
	}
	if maxMessages > 0 && len(kept) > maxMessages {
		kept = kept[len(kept)-maxMessages:]
	}

	agentHeader := "⏺ " + agentLabel
	var sb strings.Builder
	for i, m := range kept {
		// One header per turn, not per message: an agent turn is a run of lines
		// (text, then a line per tool call), and repeating the header between
		// them turns a readable exchange into a ladder of banners.
		if i == 0 || kept[i-1].IsUser != m.IsUser {
			if i > 0 {
				sb.WriteByte('\n')
			}
			if m.IsUser {
				sb.WriteString("› You")
			} else {
				sb.WriteString(agentHeader)
			}
			sb.WriteByte('\n')
		}
		sb.WriteString(m.Text)
		sb.WriteByte('\n')
	}
	return CapTail(sb.String())
}

// CapTail trims s to at most MaxRenderBytes, keeping the end and starting at a
// line boundary so a message isn't cut mid-line.
func CapTail(s string) string {
	if len(s) <= MaxRenderBytes {
		return s
	}
	s = s[len(s)-MaxRenderBytes:]
	if i := strings.IndexByte(s, '\n'); i >= 0 && i+1 < len(s) {
		s = s[i+1:]
	}
	return s
}

// ToolLine renders one tool call as a single line: the tool's name plus its most
// identifying argument.
//
// A bare tool name says nothing about what a turn did; "⚙ Bash: go test ./..."
// is the difference between a readable conversation and a wall of tool names.
func ToolLine(name string, input map[string]any) string {
	return "⚙ " + name + toolArgumentSummary(input)
}

// toolArgumentKeys are the inputs worth showing, most identifying first. Agents
// disagree on spelling for the same idea (Claude's `file_path` is opencode's
// `filePath`), so both conventions are listed.
var toolArgumentKeys = []string{
	"command", "file_path", "filePath", "notebook_path", "notebookPath",
	"path", "pattern", "query", "url", "skill", "subagent_type", "description",
	"prompt",
}

// maxToolArgumentRunes bounds the summary: it shares a line with the tool name
// on a phone-width card, and some inputs (a prompt, a heredoc) are enormous.
const maxToolArgumentRunes = 80

func toolArgumentSummary(input map[string]any) string {
	if input == nil {
		return ""
	}
	for _, key := range toolArgumentKeys {
		value := stringValue(input[key])
		value = strings.TrimSpace(strings.Join(strings.Fields(value), " "))
		if value == "" {
			continue
		}
		runes := []rune(value)
		if len(runes) > maxToolArgumentRunes {
			value = string(runes[:maxToolArgumentRunes]) + "…"
		}
		return ": " + value
	}
	return ""
}

// stringValue renders a tool argument that may not have arrived as a string —
// a line number, a boolean flag — rather than dropping the summary entirely.
func stringValue(raw any) string {
	switch v := raw.(type) {
	case string:
		return v
	case nil:
		return ""
	case float64:
		if v == float64(int64(v)) {
			return fmt.Sprintf("%d", int64(v))
		}
		return fmt.Sprintf("%g", v)
	case bool:
		return fmt.Sprintf("%t", v)
	default:
		return ""
	}
}
