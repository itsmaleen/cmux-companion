package transcripts

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestRenderClaudeBinding exercises the dispatcher's claude.transcript path:
// a resume_binding naming Claude routes to claude.Resolver and the result
// comes back tagged agent_kind "claude".
func TestRenderClaudeBinding(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	projectDir := filepath.Join(home, ".claude", "projects", "-work")
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatal(err)
	}
	sessionID := "11111111-2222-3333-4444-555555555555"
	transcriptPath := filepath.Join(projectDir, sessionID+".jsonl")
	line := `{"type":"user","message":{"role":"user","content":"hello from claude"}}` + "\n"
	if err := os.WriteFile(transcriptPath, []byte(line), 0o644); err != nil {
		t.Fatal(err)
	}

	d := New()
	res, err := d.Render(Request{
		SurfaceID:     "surface-1",
		ResumeBinding: map[string]any{"kind": "claude", "checkpoint_id": sessionID, "cwd": "/work"},
		MaxMessages:   100,
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !res.Supported || res.AgentKind != "claude" {
		t.Fatalf("want supported claude result, got %+v", res)
	}
	if !strings.Contains(res.Text, "hello from claude") {
		t.Errorf("missing message text in %q", res.Text)
	}
	if res.SessionID != sessionID {
		t.Errorf("session id = %q, want %q", res.SessionID, sessionID)
	}
}

// newOpencodeDB builds a database shaped like opencode's own under dataHome,
// and points XDG_DATA_HOME there for the duration of the test.
func newOpencodeDB(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("sqlite3"); err != nil {
		t.Skip("sqlite3 not installed")
	}
	dataHome := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dataHome)
	dbDir := filepath.Join(dataHome, "opencode")
	if err := os.MkdirAll(dbDir, 0o755); err != nil {
		t.Fatal(err)
	}
	db := filepath.Join(dbDir, "opencode.db")
	schema := `
		create table session (id text primary key, project_id text, parent_id text,
			slug text, directory text not null, title text not null, version text,
			time_created integer not null, time_updated integer not null);
		create table message (id text primary key, session_id text not null,
			time_created integer not null, time_updated integer not null, data text not null);
		create table part (id text primary key, message_id text not null, session_id text not null,
			time_created integer not null, time_updated integer not null, data text not null);
		insert into session (id, directory, title, time_created, time_updated)
			values ('ses_disp001', '/work', 'Dispatcher test session', 100, 100);
		insert into message (id, session_id, time_created, time_updated, data)
			values ('msg_1', 'ses_disp001', 10, 10, '{"role":"user"}');
		insert into part (id, message_id, session_id, time_created, time_updated, data)
			values ('msg_1-p0', 'msg_1', 'ses_disp001', 10, 10, '{"type":"text","text":"hello from opencode"}');
	`
	cmd := exec.Command("sqlite3", db, schema)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("sqlite3 setup: %v: %s", err, out)
	}
}

// TestRenderOpencodeBindingWithSessionID covers herdr's case: the
// resume_binding already names the exact opencode session, so no title/tty
// matching is needed.
func TestRenderOpencodeBindingWithSessionID(t *testing.T) {
	newOpencodeDB(t)

	d := New()
	res, err := d.Render(Request{
		SurfaceID:     "pane-1",
		ResumeBinding: map[string]any{"kind": "opencode", "checkpoint_id": "ses_disp001", "cwd": "/work"},
		MaxMessages:   100,
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !res.Supported || res.AgentKind != "opencode" {
		t.Fatalf("want supported opencode result, got %+v", res)
	}
	if res.SessionID != "ses_disp001" {
		t.Errorf("session id = %q, want ses_disp001", res.SessionID)
	}
	if !strings.Contains(res.Text, "hello from opencode") {
		t.Errorf("missing message text in %q", res.Text)
	}
	if res.Source != "resume_binding" {
		t.Errorf("source = %q, want resume_binding", res.Source)
	}
}

// TestRenderOpencodeBindingUnchanged covers the fingerprint short-circuit for
// a resume_binding-identified opencode session, matching Claude's behaviour.
func TestRenderOpencodeBindingUnchanged(t *testing.T) {
	newOpencodeDB(t)

	d := New()
	first, err := d.Render(Request{
		ResumeBinding: map[string]any{"kind": "opencode", "checkpoint_id": "ses_disp001"},
		MaxMessages:   100,
	})
	if err != nil || first.Fingerprint == "" {
		t.Fatalf("first render: %+v %v", first, err)
	}

	second, err := d.Render(Request{
		ResumeBinding:    map[string]any{"kind": "opencode", "checkpoint_id": "ses_disp001"},
		MaxMessages:      100,
		KnownFingerprint: first.Fingerprint,
	})
	if err != nil {
		t.Fatalf("second render: %v", err)
	}
	if !second.Unchanged {
		t.Errorf("want Unchanged for a matching fingerprint, got %+v", second)
	}
	if second.Text != "" {
		t.Errorf("want no text re-sent when unchanged, got %q", second.Text)
	}
}

// TestRenderOpencodeByTitleFallback covers a resume_binding that names
// opencode but no session (a cmux hook that only learned the agent, not the
// checkpoint) — the dispatcher must still resolve via title+directory.
func TestRenderOpencodeByTitleFallback(t *testing.T) {
	newOpencodeDB(t)

	d := New()
	res, err := d.Render(Request{
		ResumeBinding: map[string]any{"kind": "opencode"},
		SurfaceTitle:  "OC | Dispatcher test session",
		Directory:     "/work",
		MaxMessages:   100,
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if res.SessionID != "ses_disp001" || res.Source != "opencode_title_cwd" {
		t.Fatalf("want title/cwd resolution to ses_disp001, got %+v", res)
	}
	if res.SessionTitle != "Dispatcher test session" {
		t.Errorf("session title = %q", res.SessionTitle)
	}
}

// TestRenderOpencodeUnidentified covers an opencode surface whose title names
// no session: it must report opencode_unidentified rather than guessing.
func TestRenderOpencodeUnidentified(t *testing.T) {
	newOpencodeDB(t)

	d := New()
	res, err := d.Render(Request{
		ResumeBinding: map[string]any{"kind": "opencode"},
		SurfaceTitle:  "OpenCode",
		Directory:     "/work",
		MaxMessages:   100,
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !res.Supported || res.AgentKind != "opencode" || res.Source != "opencode_unidentified" {
		t.Fatalf("want opencode_unidentified, got %+v", res)
	}
	if res.Text != "" || res.SessionID != "" {
		t.Errorf("want no text/session for an unidentified surface, got %+v", res)
	}
}

// TestRenderNoBindingRequiresTTY covers a plain surface with no resume_binding
// at all: without a TTY there is nothing to prove an opencode process is
// actually running there, so the result is unsupported regardless of title.
func TestRenderNoBindingRequiresTTY(t *testing.T) {
	newOpencodeDB(t)

	d := New()
	res, err := d.Render(Request{
		SurfaceTitle: "OC | Dispatcher test session",
		Directory:    "/work",
		MaxMessages:  100,
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if res.Supported {
		t.Errorf("want unsupported with no tty evidence, got %+v", res)
	}
}

// TestRenderNoBindingNoOpencode covers the ordinary plain-terminal case: no
// binding, no opencode database on this machine at all.
func TestRenderNoBindingNoOpencode(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir()) // guaranteed empty: no opencode.db

	d := New()
	res, err := d.Render(Request{TTY: "ttys099", MaxMessages: 100})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if res.Supported {
		t.Errorf("want unsupported with no opencode database, got %+v", res)
	}
}

func TestMaxMessagesBounds(t *testing.T) {
	if got := MaxMessages(map[string]any{}); got != 200 {
		t.Errorf("default = %d, want 200", got)
	}
	if got := MaxMessages(map[string]any{"max_messages": float64(50)}); got != 50 {
		t.Errorf("got %d, want 50", got)
	}
	if got := MaxMessages(map[string]any{"max_messages": float64(999999)}); got != 2000 {
		t.Errorf("got %d, want bounded to 2000", got)
	}
}

func TestKnownFingerprint(t *testing.T) {
	if got := KnownFingerprint(map[string]any{"known_fingerprint": "abc"}); got != "abc" {
		t.Errorf("got %q, want abc", got)
	}
	if got := KnownFingerprint(map[string]any{}); got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

// TestEncodeKeepsExistingFieldNames pins the wire shape: every field name from
// before opencode support must still be present so an older phone build's
// parser keeps working, alongside the new agent_kind/session_title.
func TestEncodeKeepsExistingFieldNames(t *testing.T) {
	raw := Encode(Result{
		Supported:      true,
		AgentKind:      "opencode",
		Text:           "hi",
		SessionID:      "ses_x",
		SessionTitle:   "Title",
		SessionMissing: false,
		Fingerprint:    "fp",
		Unchanged:      false,
		Source:         "resume_binding",
	})
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{
		"supported", "text", "session_id", "session_missing", "fingerprint",
		"unchanged", "source", "agent_kind", "session_title",
	} {
		if _, ok := decoded[field]; !ok {
			t.Errorf("encoded result missing field %q: %v", field, decoded)
		}
	}
}
