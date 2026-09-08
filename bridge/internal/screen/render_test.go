package screen

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/itsmaleen/cmux-companion/bridge/internal/backend"
)

// fixtureFrame is one NDJSON line of the phone's screenshot-mode fixtures
// (testdata/frames-*.ndjson): a single full `terminal.frame` record.
type fixtureFrame struct {
	Type     string `json:"type"`
	Seq      int    `json:"seq"`
	Width    int    `json:"width"`
	Height   int    `json:"height"`
	Encoding string `json:"encoding"`
	Full     bool   `json:"full"`
	Bytes    string `json:"bytes"`
}

// loadFixture reads the first NDJSON line of a testdata frame recording
// and returns it as a backend.Frame ready to Feed.
func loadFixture(t *testing.T, name string) backend.Frame {
	t.Helper()
	f, err := os.Open("testdata/" + name)
	if err != nil {
		t.Fatalf("open fixture %s: %v", name, err)
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 4<<20)
	if !scanner.Scan() {
		t.Fatalf("fixture %s has no lines", name)
	}
	var rec fixtureFrame
	if err := json.Unmarshal(scanner.Bytes(), &rec); err != nil {
		t.Fatalf("decode fixture %s: %v", name, err)
	}
	return backend.Frame{
		Seq: rec.Seq, Full: rec.Full,
		Width: rec.Width, Height: rec.Height,
		Encoding: rec.Encoding, Bytes: rec.Bytes,
	}
}

// findLine returns the Line whose runs, concatenated, contain want.
func findLine(t *testing.T, u Update, want string) (Line, bool) {
	t.Helper()
	for _, l := range u.Lines {
		var sb strings.Builder
		for _, r := range l.Runs {
			sb.WriteString(r.T)
		}
		if strings.Contains(sb.String(), want) {
			return l, true
		}
	}
	return Line{}, false
}

func TestRendererShellFixtureGreenPass(t *testing.T) {
	f := loadFixture(t, "frames-shell.ndjson")
	if !f.Full || f.Width != 85 || f.Height != 19 {
		t.Fatalf("fixture = %+v, want a full 85x19 frame", f)
	}
	r := New(f.Width, f.Height)
	if err := r.Feed(f); err != nil {
		t.Fatalf("Feed: %v", err)
	}
	u := r.Update(true)
	if !u.Full || u.Cols != 85 || u.Rows != 19 {
		t.Fatalf("update = %+v", u)
	}

	line, ok := findLine(t, u, "11 pass")
	if !ok {
		t.Fatalf("no line contains %q; lines = %+v", "11 pass", u.Lines)
	}
	var found bool
	for _, run := range line.Runs {
		if strings.Contains(run.T, "pass") {
			if run.FG != "#008000" {
				t.Fatalf("run %+v, want green fg #008000", run)
			}
			found = true
		}
	}
	if !found {
		t.Fatalf("no run in line %+v carries the pass text", line)
	}
}

func TestRendererClaudeFixtureBoldAndSize(t *testing.T) {
	f := loadFixture(t, "frames-claude.ndjson")
	if !f.Full || f.Width != 120 || f.Height != 40 {
		t.Fatalf("fixture = %+v, want a full 120x40 frame", f)
	}
	r := New(f.Width, f.Height)
	if err := r.Feed(f); err != nil {
		t.Fatalf("Feed: %v", err)
	}
	u := r.Update(true)
	if u.Rows != 40 || u.Cols != 120 {
		t.Fatalf("update grid = %dx%d, want 120x40", u.Cols, u.Rows)
	}
	if len(u.Lines) != 40 {
		t.Fatalf("full update carried %d lines, want all 40 rows (incl. empty ones)", len(u.Lines))
	}

	line, ok := findLine(t, u, "Kept")
	if !ok {
		t.Fatalf("no line contains %q", "Kept")
	}
	var sawBold bool
	for _, run := range line.Runs {
		if strings.Contains(run.T, "Kept") && run.A&1 != 0 {
			sawBold = true
		}
	}
	if !sawBold {
		t.Fatalf("expected a bold run containing Kept in %+v", line.Runs)
	}
}

func TestRendererDeltaMovesCursorAndChangesOneLine(t *testing.T) {
	r := New(10, 3)
	full := backend.Frame{Full: true, Width: 10, Height: 3,
		Bytes: b64("\x1b[2J\x1b[1;1Hrow0\x1b[2;1Hrow1")}
	if err := r.Feed(full); err != nil {
		t.Fatalf("Feed full: %v", err)
	}
	first := r.Update(true)
	if !first.Full || len(first.Lines) != 3 {
		t.Fatalf("first update = %+v, want full with all 3 rows", first)
	}

	// Move the cursor to row 1 (1-based), col 2, and rewrite one cell.
	delta := backend.Frame{Full: false, Width: 10, Height: 3, Bytes: b64("\x1b[1;2HX")}
	if err := r.Feed(delta); err != nil {
		t.Fatalf("Feed delta: %v", err)
	}
	second := r.Update(false)
	if second.Full {
		t.Fatalf("second update = %+v, want a non-full diff", second)
	}
	if len(second.Lines) != 1 || second.Lines[0].I != 0 {
		t.Fatalf("changed lines = %+v, want exactly row 0", second.Lines)
	}
	var text strings.Builder
	for _, run := range second.Lines[0].Runs {
		text.WriteString(run.T)
	}
	if got := text.String(); got != "rXw0" {
		t.Fatalf("row 0 text = %q, want rXw0", got)
	}
	if second.Cursor.X != 2 || second.Cursor.Y != 0 {
		t.Fatalf("cursor = %+v, want (2,0) after writing X at 1-based (1,2)", second.Cursor)
	}
}

func TestRendererTruecolorHex(t *testing.T) {
	r := New(10, 1)
	f := backend.Frame{Full: true, Width: 10, Height: 1,
		Bytes: b64("\x1b[2J\x1b[1;1H\x1b[38;2;10;20;30mX")}
	if err := r.Feed(f); err != nil {
		t.Fatalf("Feed: %v", err)
	}
	u := r.Update(true)
	line, ok := findLine(t, u, "X")
	if !ok {
		t.Fatalf("no line with X: %+v", u.Lines)
	}
	var run Run
	for _, rn := range line.Runs {
		if strings.Contains(rn.T, "X") {
			run = rn
		}
	}
	if run.FG != "#0a141e" {
		t.Fatalf("fg = %q, want truecolor #0a141e passed through", run.FG)
	}
}

func TestRendererTrailingWhitespaceTrimmed(t *testing.T) {
	r := New(10, 1)
	f := backend.Frame{Full: true, Width: 10, Height: 1,
		Bytes: b64("\x1b[2J\x1b[1;1Hhi   ")}
	if err := r.Feed(f); err != nil {
		t.Fatalf("Feed: %v", err)
	}
	u := r.Update(true)
	if len(u.Lines) != 1 {
		t.Fatalf("lines = %+v", u.Lines)
	}
	line := u.Lines[0]
	var text strings.Builder
	for _, run := range line.Runs {
		if run.T == "" {
			t.Fatalf("empty run in %+v", line.Runs)
		}
		text.WriteString(run.T)
	}
	if got := text.String(); got != "hi" {
		t.Fatalf("row text = %q, want trailing spaces trimmed to hi", got)
	}
}

func TestRendererBlankLineHasNoRuns(t *testing.T) {
	r := New(10, 2)
	f := backend.Frame{Full: true, Width: 10, Height: 2,
		Bytes: b64("\x1b[2J\x1b[1;1Hhi")}
	if err := r.Feed(f); err != nil {
		t.Fatalf("Feed: %v", err)
	}
	u := r.Update(true)
	if len(u.Lines) != 2 {
		t.Fatalf("full update lines = %d, want 2 (including the blank row)", len(u.Lines))
	}
	blank := u.Lines[1]
	if blank.I != 1 || len(blank.Runs) != 0 {
		t.Fatalf("blank row = %+v, want no runs", blank)
	}
}

func TestRendererWideCharConsumesPaddingCell(t *testing.T) {
	r := New(6, 1)
	// A full-width character (bold), then a plain ASCII cell right after it:
	// two distinct runs prove the wide char's padding cell was skipped
	// rather than rendered as a spurious extra column.
	f := backend.Frame{Full: true, Width: 6, Height: 1,
		Bytes: b64("\x1b[2J\x1b[1;1H\x1b[1m你\x1b[0mA")}
	if err := r.Feed(f); err != nil {
		t.Fatalf("Feed: %v", err)
	}
	u := r.Update(true)
	if len(u.Lines) == 0 {
		t.Fatal("no lines in full update")
	}
	line := u.Lines[0]
	if len(line.Runs) != 2 {
		t.Fatalf("runs = %+v, want 2 (the wide char, then A)", line.Runs)
	}
	if line.Runs[0].T != "你" || line.Runs[0].A&1 == 0 {
		t.Fatalf("run 0 = %+v, want the bold wide char alone", line.Runs[0])
	}
	if line.Runs[1].T != "A" {
		t.Fatalf("run 1 = %+v, want plain A immediately after (no leftover padding cell)", line.Runs[1])
	}
}

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
