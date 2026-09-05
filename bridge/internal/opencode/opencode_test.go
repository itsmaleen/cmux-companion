package opencode

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// newTestStore builds a database shaped like opencode's own and returns a Store
// pointed at it. Real sqlite3 against a real file: the queries under test are
// SQL, and a fake would only prove the fake agrees with itself.
func newTestStore(t *testing.T) *Store {
	t.Helper()
	sqlite, err := exec.LookPath("sqlite3")
	if err != nil {
		t.Skip("sqlite3 not installed")
	}
	dir := t.TempDir()
	db := filepath.Join(dir, "opencode.db")
	schema := `
		create table session (id text primary key, project_id text, parent_id text,
			slug text, directory text not null, title text not null, version text,
			time_created integer not null, time_updated integer not null);
		create table message (id text primary key, session_id text not null,
			time_created integer not null, time_updated integer not null, data text not null);
		create table part (id text primary key, message_id text not null, session_id text not null,
			time_created integer not null, time_updated integer not null, data text not null);
	`
	run(t, sqlite, db, schema)
	return &Store{dbPath: db, sqlite: sqlite}
}

func run(t *testing.T, sqlite, db, sql string) {
	t.Helper()
	cmd := exec.Command(sqlite, db, sql)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("sqlite3: %v: %s", err, out)
	}
}

func quote(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

func addSession(t *testing.T, s *Store, id, directory, title, parent string, updated int) {
	t.Helper()
	parentSQL := "null"
	if parent != "" {
		parentSQL = quote(parent)
	}
	run(t, s.sqlite, s.dbPath, fmt.Sprintf(
		"insert into session (id, directory, title, parent_id, time_created, time_updated)"+
			" values (%s, %s, %s, %s, %d, %d);",
		quote(id), quote(directory), quote(title), parentSQL, updated, updated))
}

// addMessage inserts a message and its parts. Part data mirrors opencode's
// shape, including the huge `state.output` a tool part really carries.
func addMessage(t *testing.T, s *Store, sessionID, msgID, role string, at int, parts ...string) {
	t.Helper()
	run(t, s.sqlite, s.dbPath, fmt.Sprintf(
		"insert into message (id, session_id, time_created, time_updated, data)"+
			" values (%s, %s, %d, %d, %s);",
		quote(msgID), quote(sessionID), at, at, quote(`{"role":"`+role+`"}`)))
	for i, part := range parts {
		run(t, s.sqlite, s.dbPath, fmt.Sprintf(
			"insert into part (id, message_id, session_id, time_created, time_updated, data)"+
				" values (%s, %s, %s, %d, %d, %s);",
			quote(fmt.Sprintf("%s-p%d", msgID, i)), quote(msgID), quote(sessionID),
			at+i, at+i, quote(part)))
	}
}

func textPart(text string) string {
	return `{"type":"text","text":` + jsonString(text) + `}`
}

func toolPart(name, input, output string) string {
	return `{"type":"tool","tool":"` + name + `","state":{"status":"completed","input":` +
		input + `,"output":` + jsonString(output) + `}}`
}

func jsonString(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	s = strings.ReplaceAll(s, "\n", `\n`)
	return `"` + s + `"`
}

func TestRenderConversation(t *testing.T) {
	s := newTestStore(t)
	addSession(t, s, "ses_abc123", "/work", "Fix the parser", "", 100)
	addMessage(t, s, "ses_abc123", "msg_1", "user", 10, textPart("fix the parser"))
	addMessage(t, s, "ses_abc123", "msg_2", "assistant", 20,
		textPart("Looking at it."),
		toolPart("read", `{"filePath":"/work/parser.go"}`, strings.Repeat("FILE CONTENT ", 500)),
		toolPart("bash", `{"command":"go test ./..."}`, "ok"))
	addMessage(t, s, "ses_abc123", "msg_3", "assistant", 30, textPart("Fixed."))

	got, err := s.Render("ses_abc123", 100)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	for _, want := range []string{
		"› You", "fix the parser",
		"⏺ opencode", "Looking at it.",
		"⚙ read: /work/parser.go", "⚙ bash: go test ./...", "Fixed.",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	// A tool's OUTPUT is not the conversation — it's the bulk of the database
	// and would bury the reply it belongs to.
	if strings.Contains(got, "FILE CONTENT") {
		t.Errorf("tool output leaked into the transcript:\n%s", got)
	}
	// One header per turn, not per message.
	if n := strings.Count(got, "⏺ opencode"); n != 1 {
		t.Errorf("want a single agent header for consecutive agent messages, got %d:\n%s", n, got)
	}
}

// opencode records its own reasoning and step bookkeeping as parts; a reader
// wants neither.
func TestRenderSkipsNonConversationParts(t *testing.T) {
	s := newTestStore(t)
	addSession(t, s, "ses_skip1", "/work", "Session", "", 100)
	addMessage(t, s, "ses_skip1", "msg_1", "assistant", 10,
		`{"type":"reasoning","text":"INTERNAL SCRATCHPAD"}`,
		`{"type":"step-start"}`,
		`{"type":"step-finish"}`,
		`{"type":"compaction"}`,
		textPart("The answer."))

	got, err := s.Render("ses_skip1", 100)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got, "INTERNAL SCRATCHPAD") {
		t.Errorf("reasoning leaked into the transcript:\n%s", got)
	}
	if !strings.Contains(got, "The answer.") {
		t.Errorf("dropped the actual reply:\n%s", got)
	}
}

func TestRenderKeepsTheMostRecentMessages(t *testing.T) {
	s := newTestStore(t)
	addSession(t, s, "ses_long1", "/work", "Long", "", 100)
	for i := 0; i < 20; i++ {
		addMessage(t, s, "ses_long1", fmt.Sprintf("msg_%02d", i), "user", 10+i,
			textPart(fmt.Sprintf("message-%02d", i)))
	}
	got, err := s.Render("ses_long1", 3)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "message-19") {
		t.Errorf("the newest message is missing — the limit kept the wrong end:\n%s", got)
	}
	if strings.Contains(got, "message-00") {
		t.Errorf("the limit did not apply:\n%s", got)
	}
}

func TestRenderRejectsMaliciousSessionID(t *testing.T) {
	s := newTestStore(t)
	addSession(t, s, "ses_real01", "/work", "Real", "", 100)
	addMessage(t, s, "ses_real01", "msg_1", "user", 10, textPart("SECRET"))

	for _, bad := range []string{
		"ses_real01'; drop table session; --",
		"' or '1'='1",
		"",
		"../ses_real01",
		"ses_real01 union select 1",
	} {
		if _, err := s.Render(bad, 100); err == nil {
			t.Fatalf("accepted %q as a session id", bad)
		}
	}
	// The table is still there and still readable.
	if got, err := s.Render("ses_real01", 100); err != nil || !strings.Contains(got, "SECRET") {
		t.Fatalf("database damaged or unreadable after the attempts: %v %q", err, got)
	}
}

func TestResolveSessionByTitleAndDirectory(t *testing.T) {
	s := newTestStore(t)
	// Two surfaces working in one directory is the ordinary case, and the
	// reason a directory alone can't identify a conversation.
	addSession(t, s, "ses_mine01", "/work", "Stripe vs Autumn billing for pepo-marketing", "", 100)
	addSession(t, s, "ses_other1", "/work", "Something else entirely", "", 999)
	// A subagent session shares the directory and is more recent still.
	addSession(t, s, "ses_sub001", "/work", "Stripe vs Autumn billing (subagent)", "ses_mine01", 1000)

	// The surface title is truncated by the terminal, so it's a prefix.
	got, ok := s.ResolveSession("OC | Stripe vs Autumn billing for pepo-mar...", "/work")
	if !ok {
		t.Fatal("did not resolve a titled surface")
	}
	if got.ID != "ses_mine01" {
		t.Fatalf("resolved to %q (%q), want ses_mine01", got.ID, got.Title)
	}
}

func TestResolveSessionRefusesToGuess(t *testing.T) {
	s := newTestStore(t)
	addSession(t, s, "ses_someth", "/work", "Someone else's conversation", "", 999)

	// A surface with no session in its title must not be handed the newest
	// session in its directory.
	for _, title := range []string{"OpenCode", "", "zsh", "~/work", "OC | ", "OC | ab"} {
		if got, ok := s.ResolveSession(title, "/work"); ok {
			t.Errorf("title %q resolved to %q — it names no session", title, got.ID)
		}
	}
	// Nor may a titled surface match across directories.
	if got, ok := s.ResolveSession("OC | Someone else's conversation", "/elsewhere"); ok {
		t.Errorf("matched %q in the wrong directory", got.ID)
	}
}

// A directory or title arriving from a terminal must not be able to alter the
// query it lands in.
func TestResolveSessionQuotingIsInert(t *testing.T) {
	s := newTestStore(t)
	addSession(t, s, "ses_quote1", "/work'; drop table session; --", "Odd directory", "", 100)

	got, ok := s.ResolveSession("OC | Odd directory", "/work'; drop table session; --")
	if !ok || got.ID != "ses_quote1" {
		t.Fatalf("a quote in the directory broke the lookup: ok=%v id=%q", ok, got.ID)
	}
	if _, ok := s.ResolveSession("OC | Odd' or '1'='1", "/work"); ok {
		t.Error("a quoted title matched something it should not have")
	}
	// Still intact.
	if _, ok := s.ResolveSession("OC | Odd directory", "/work'; drop table session; --"); !ok {
		t.Fatal("session table damaged")
	}
}

func TestFingerprintTracksStreamingParts(t *testing.T) {
	s := newTestStore(t)
	addSession(t, s, "ses_fp0001", "/work", "Session", "", 100)
	addMessage(t, s, "ses_fp0001", "msg_1", "assistant", 10, textPart("first"))

	first, err := s.Fingerprint("ses_fp0001", 100)
	if err != nil || first == "" {
		t.Fatalf("fingerprint: %q %v", first, err)
	}
	if again, _ := s.Fingerprint("ses_fp0001", 100); again != first {
		t.Errorf("fingerprint changed with no new content: %q -> %q", first, again)
	}
	// A message bound is part of the rendering, so it must change the print.
	if other, _ := s.Fingerprint("ses_fp0001", 5); other == first {
		t.Error("fingerprint ignored the message bound")
	}
	// opencode appends PARTS to an existing message while a reply streams; a
	// fingerprint that only counted messages would call that unchanged.
	run(t, s.sqlite, s.dbPath, fmt.Sprintf(
		"insert into part (id, message_id, session_id, time_created, time_updated, data)"+
			" values ('p_new', 'msg_1', 'ses_fp0001', 20, 20, %s);", quote(textPart("second"))))
	after, _ := s.Fingerprint("ses_fp0001", 100)
	if after == first {
		t.Error("fingerprint did not notice a part appended to an existing message")
	}
}

func TestSessionTitleFromSurfaceTitle(t *testing.T) {
	cases := map[string]string{
		"OC | Fix the parser":      "Fix the parser",
		"OC | Fix the parser...":   "Fix the parser",
		"OC | Fix the parser…":     "Fix the parser",
		"OC |  Padded  ":           "Padded",
		"OpenCode":                 "",
		"":                         "",
		"claude session":           "",
		"OC | ab":                  "", // too short to be evidence
		"Fix the parser":           "", // no prefix: not an opencode title
		"OC | Node v22.1.0 ready…": "Node v22.1.0 ready",
	}
	for input, want := range cases {
		if got := SessionTitleFromSurfaceTitle(input); got != want {
			t.Errorf("SessionTitleFromSurfaceTitle(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestAvailableWithoutDatabase(t *testing.T) {
	s := &Store{dbPath: filepath.Join(t.TempDir(), "missing.db"), sqlite: "/usr/bin/sqlite3"}
	if s.Available() {
		t.Error("reported available with no database")
	}
	// Every entry point must be safe on a machine with no opencode at all.
	if _, ok := s.ResolveSession("OC | Anything", "/work"); ok {
		t.Error("resolved a session with no database")
	}
	if _, err := s.Render("ses_abc123", 10); err == nil {
		t.Error("rendered with no database")
	}
	if _, err := s.Fingerprint("ses_abc123", 10); err == nil {
		t.Error("fingerprinted with no database")
	}
}

func TestIsOpencodeCommand(t *testing.T) {
	for _, yes := range []string{"opencode", "/Users/x/.opencode/bin/opencode", "opencode-tui"} {
		if !isOpencodeCommand(yes) {
			t.Errorf("%q should be recognised as opencode", yes)
		}
	}
	// Matching the whole command line instead of argv[0] would call each of
	// these an agent surface.
	for _, no := range []string{"vim opencode.go", "rg opencode", "bash", "/bin/zsh", "opencodex", ""} {
		if isOpencodeCommand(no) {
			t.Errorf("%q should NOT be recognised as opencode", no)
		}
	}
}

func TestNormalizeTTY(t *testing.T) {
	for input, want := range map[string]string{
		"/dev/ttys007": "ttys007",
		"ttys007":      "ttys007",
		" ttys007 ":    "ttys007",
		"":             "",
	} {
		if got := NormalizeTTY(input); got != want {
			t.Errorf("NormalizeTTY(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestStoreIsReadOnly(t *testing.T) {
	s := newTestStore(t)
	addSession(t, s, "ses_ro0001", "/work", "Session", "", 100)
	addMessage(t, s, "ses_ro0001", "msg_1", "user", 10, textPart("hello"))

	before, err := os.Stat(s.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Render("ses_ro0001", 100); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(s.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	// opencode owns this file and is writing to it live; reading must not so
	// much as create a journal beside it.
	if before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
		t.Error("reading the transcript modified the database")
	}
	entries, err := os.ReadDir(filepath.Dir(s.dbPath))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), "-journal") || strings.HasSuffix(e.Name(), "-wal") {
			t.Errorf("reading left %s beside the database", e.Name())
		}
	}
}
