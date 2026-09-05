// Package opencode reads opencode's session database so a surface running it
// can show its conversation.
//
// opencode is a full-screen TUI: it repaints a fixed viewport and keeps no
// terminal scrollback, so reading the surface's screen only ever yields the last
// frame. The conversation itself lives in ~/.local/share/opencode/opencode.db —
// sessions, messages, and message parts in SQLite, rather than Claude Code's
// append-only JSONL.
//
// Queries shell out to the sqlite3 CLI. The alternative is a SQLite driver:
// cgo (which this cross-compiled-nothing single binary avoids) or a pure-Go
// translation that is megabytes of generated code — a heavy dependency for a
// bridge whose only other needs are a WebSocket and a Unix socket. sqlite3(1)
// ships with macOS, which is the only platform this bridge runs on.
package opencode

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/itsmaleen/cmux-companion/bridge/internal/transcriptrender"
)

// sessionIDPattern matches an opencode session id (`ses_` + base62). Ids that
// reach a query are checked against it, so nothing shaped like SQL ever does.
var sessionIDPattern = regexp.MustCompile(`^ses_[A-Za-z0-9]+$`)

// queryTimeout bounds one sqlite3 invocation. The database is live — opencode
// writes to it continuously — so a query can meet a busy writer; failing the
// poll is better than hanging the client's request.
const queryTimeout = 5 * time.Second

// Session is one opencode conversation.
type Session struct {
	ID        string
	Title     string
	Directory string
	UpdatedAt int64
}

// Store queries the opencode database and the process table.
type Store struct {
	dbPath string
	sqlite string

	procMu   sync.Mutex
	procAt   time.Time
	procTTYs map[string]int
}

// NewStore locates opencode's database and the sqlite3 binary.
func NewStore() *Store {
	base := os.Getenv("XDG_DATA_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return &Store{}
		}
		base = filepath.Join(home, ".local", "share")
	}
	sqlite, err := exec.LookPath("sqlite3")
	if err != nil {
		sqlite = "/usr/bin/sqlite3"
	}
	return &Store{
		dbPath: filepath.Join(base, "opencode", "opencode.db"),
		sqlite: sqlite,
	}
}

// Available reports whether this machine has an opencode database to read.
func (s *Store) Available() bool {
	if s == nil || s.dbPath == "" {
		return false
	}
	info, err := os.Stat(s.dbPath)
	if err != nil || !info.Mode().IsRegular() {
		return false
	}
	_, err = os.Stat(s.sqlite)
	return err == nil
}

// ResolveSession maps a cmux surface to the opencode session it is running.
//
// opencode surfaces carry no cmux resume_binding — cmux has an opencode plugin
// that would record one, but it is opt-in and usually absent — so the binding is
// inferred from what a surface does expose: its working directory and its title,
// which opencode sets to the session's title (as "OC | <title>", truncated).
//
// The title is REQUIRED, not merely preferred. Several opencode surfaces
// routinely share one working directory, so "newest session in this directory"
// would confidently return a different conversation; a surface whose title
// carries no session name (a freshly started opencode, titled just "OpenCode")
// resolves to nothing instead. An empty transcript beats someone else's.
func (s *Store) ResolveSession(surfaceTitle, directory string) (Session, bool) {
	stem := SessionTitleFromSurfaceTitle(surfaceTitle)
	if stem == "" || directory == "" || !s.Available() {
		return Session{}, false
	}

	rows, err := s.query(
		"select id, title, directory, time_updated from session" +
			" where directory = " + textLiteral(directory) +
			// Subagent sessions (parent_id set) are the agent talking to itself;
			// they share the directory and would outrank the real conversation.
			" and parent_id is null" +
			" order by time_updated desc limit 200",
	)
	if err != nil {
		return Session{}, false
	}

	for _, row := range rows {
		title := stringField(row, "title")
		// The surface title is truncated, so it is a PREFIX of the session
		// title, never the whole of it.
		if strings.HasPrefix(title, stem) {
			return Session{
				ID:        stringField(row, "id"),
				Title:     title,
				Directory: stringField(row, "directory"),
				UpdatedAt: intField(row, "time_updated"),
			}, true
		}
	}
	return Session{}, false
}

// surfaceTitlePrefix is what opencode puts before the session title in the
// terminal title.
const surfaceTitlePrefix = "OC | "

// truncationSuffix matches the ellipsis (ASCII or single-glyph) a truncated
// title ends with, plus any trailing whitespace.
var truncationSuffix = regexp.MustCompile(`[.…]+\s*$`)

// SessionTitleFromSurfaceTitle extracts the session-title stem from a surface
// title, or "" when the title names no session.
func SessionTitleFromSurfaceTitle(surfaceTitle string) string {
	title := strings.TrimSpace(surfaceTitle)
	if !strings.HasPrefix(title, surfaceTitlePrefix) {
		// "OpenCode" on its own is the TUI before a session has a title. It
		// identifies the agent, not the conversation.
		return ""
	}
	stem := strings.TrimSpace(strings.TrimPrefix(title, surfaceTitlePrefix))
	stem = strings.TrimSpace(truncationSuffix.ReplaceAllString(stem, ""))
	// A stem of one or two characters matches far too much to be evidence.
	if len([]rune(stem)) < 3 {
		return ""
	}
	return stem
}

// Fingerprint identifies the current state of a session's transcript, so an
// unchanged one costs a tiny query instead of a full render. Parts are counted
// as well as messages: a streaming reply grows parts without adding messages.
func (s *Store) Fingerprint(sessionID string, maxMessages int) (string, error) {
	if !sessionIDPattern.MatchString(sessionID) {
		return "", errors.New("invalid session id")
	}
	rows, err := s.query(
		"select coalesce(max(time_updated),0) as updated, count(*) as parts from part" +
			" where session_id = " + textLiteral(sessionID),
	)
	if err != nil {
		return "", err
	}
	if len(rows) == 0 {
		return "", nil
	}
	return fmt.Sprintf("%d-%d-%d", intField(rows[0], "updated"), intField(rows[0], "parts"), maxMessages), nil
}

// Render renders a session's conversation.
func (s *Store) Render(sessionID string, maxMessages int) (string, error) {
	if !sessionIDPattern.MatchString(sessionID) {
		return "", errors.New("invalid session id")
	}
	if maxMessages <= 0 {
		maxMessages = 200
	}

	// Newest messages first so the limit keeps the RECENT end of a long
	// session, then flipped back into reading order.
	msgRows, err := s.query(
		"select id, json_extract(data,'$.role') as role from message" +
			" where session_id = " + textLiteral(sessionID) +
			" order by time_created desc limit " + fmt.Sprint(maxMessages),
	)
	if err != nil {
		return "", err
	}
	if len(msgRows) == 0 {
		return "", nil
	}

	order := make([]string, 0, len(msgRows))
	isUser := make(map[string]bool, len(msgRows))
	for i := len(msgRows) - 1; i >= 0; i-- {
		id := stringField(msgRows[i], "id")
		if id == "" {
			continue
		}
		order = append(order, id)
		isUser[id] = stringField(msgRows[i], "role") == "user"
	}

	ids := make([]string, 0, len(order))
	for _, id := range order {
		ids = append(ids, textLiteral(id))
	}
	// Extract the fields in SQL rather than selecting part.data whole: a tool
	// part embeds its entire OUTPUT, so the raw column drags megabytes of file
	// contents and command output across the process boundary on every poll,
	// only to be discarded here. Naming the fields — and dropping the
	// reasoning/step parts in the query — took one real session's read from
	// 1.7s to under 0.1s.
	partRows, err := s.query(
		"select p.message_id as message_id," +
			" json_extract(p.data,'$.type') as type," +
			" substr(coalesce(json_extract(p.data,'$.text'),''),1," + fmt.Sprint(maxPartTextBytes) + ") as text," +
			" json_extract(p.data,'$.tool') as tool," +
			" json_extract(p.data,'$.state.input') as input" +
			" from part p" +
			" where p.message_id in (" + strings.Join(ids, ",") + ")" +
			" and json_extract(p.data,'$.type') in ('text','tool')" +
			" order by p.time_created asc",
	)
	if err != nil {
		return "", err
	}

	bodies := make(map[string][]string, len(order))
	for _, row := range partRows {
		id := stringField(row, "message_id")
		if line := renderPart(row); line != "" {
			bodies[id] = append(bodies[id], line)
		}
	}

	messages := make([]transcriptrender.Message, 0, len(order))
	for _, id := range order {
		lines := bodies[id]
		if len(lines) == 0 {
			continue
		}
		messages = append(messages, transcriptrender.Message{
			IsUser: isUser[id],
			Text:   strings.Join(lines, "\n"),
		})
	}
	return transcriptrender.Render(messages, "opencode", maxMessages), nil
}

// maxPartTextBytes bounds one text part. The transcript as a whole is capped
// again by the renderer; this only stops a single pathological part from
// crossing the process boundary in full.
const maxPartTextBytes = 20000

// renderPart turns one extracted part row into a line of transcript.
//
// Only text and tool parts are queried. The rest — reasoning (the agent's own
// scratchpad, which Claude's renderer also drops) and the step/compaction
// bookkeeping opencode records around every turn — are filtered out in SQL,
// where they cost nothing to skip.
func renderPart(row map[string]any) string {
	switch stringField(row, "type") {
	case "text":
		return strings.TrimRight(stringField(row, "text"), "\n")
	case "tool":
		name := stringField(row, "tool")
		if name == "" {
			return ""
		}
		// json_extract hands back a nested object as its JSON text.
		var input map[string]any
		if raw := stringField(row, "input"); raw != "" {
			_ = json.Unmarshal([]byte(raw), &input)
		}
		return transcriptrender.ToolLine(name, input)
	default:
		return ""
	}
}

// query runs one read-only statement and decodes sqlite3's JSON output.
func (s *Store) query(sql string) ([]map[string]any, error) {
	if !s.Available() {
		return nil, errors.New("opencode database not available")
	}
	cmd := exec.Command(s.sqlite, "-readonly", "-json", s.dbPath, sql)
	// -readonly keeps this from ever writing, including the journal; opencode
	// owns the database and may be mid-write.
	done := make(chan struct{})
	var out []byte
	var err error
	go func() {
		out, err = cmd.Output()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(queryTimeout):
		_ = cmd.Process.Kill()
		<-done
		return nil, errors.New("opencode query timed out")
	}
	if err != nil {
		return nil, err
	}
	trimmed := strings.TrimSpace(string(out))
	if trimmed == "" {
		return nil, nil
	}
	var rows []map[string]any
	if err := json.Unmarshal([]byte(trimmed), &rows); err != nil {
		return nil, err
	}
	return rows, nil
}

// textLiteral renders a Go string as a SQL expression.
//
// The sqlite3 CLI takes a statement, not bound parameters, and these values
// (a working directory, a terminal title) come from the terminal — so they are
// encoded as hex blobs cast back to text rather than quoted. There is no quote
// to escape and therefore no way to end the literal early.
func textLiteral(value string) string {
	if value == "" {
		return "''"
	}
	return "cast(x'" + hex.EncodeToString([]byte(value)) + "' as text)"
}

func stringField(row map[string]any, key string) string {
	if v, ok := row[key].(string); ok {
		return v
	}
	return ""
}

func intField(row map[string]any, key string) int64 {
	if v, ok := row[key].(float64); ok {
		return int64(v)
	}
	return 0
}
