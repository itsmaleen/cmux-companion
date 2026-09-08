// Package screen renders a backend's raw terminal frames (herdr's own
// encoding: a full repaint then deltas of raw PTY bytes) into the phone's
// `surface.screen` wire shape — styled text runs for the rows that changed —
// using a headless VT100 emulator (github.com/charmbracelet/x/vt) instead of
// shipping raw ANSI bytes for the phone to interpret itself.
//
// The types on the wire (Update/Line/Run/Cursor) are defined once in package
// backend (ScreenUpdate/ScreenLine/ScreenRun/ScreenCursor) to avoid an import
// cycle — backend can't import screen, since screen needs backend.Frame and
// backend.FrameEvent — and aliased here under the shorter names this package
// uses internally.
package screen

import (
	"encoding/base64"
	"fmt"
	"image/color"
	"strings"

	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/vt"

	"github.com/itsmaleen/cmux-companion/bridge/internal/backend"
)

// Update, Line, Run and Cursor are the wire types this package produces; see
// their doc comments on the backend.Screen* originals.
type (
	Update = backend.ScreenUpdate
	Line   = backend.ScreenLine
	Run    = backend.ScreenRun
	Cursor = backend.ScreenCursor
)

// Renderer wraps a headless terminal emulator, turning the raw PTY bytes fed
// to it into diffs of styled text runs. Not safe for concurrent use — a
// Renderer belongs to one stream.
type Renderer struct {
	emu  *vt.Emulator
	cols int
	rows int

	// cursorVisible tracks the DECTCEM mode via the emulator's callback:
	// vt.Emulator exposes CursorPosition() but no cursor-visibility accessor,
	// so visibility is observed the only way the library offers it — see the
	// deviation note in the top-level report.
	cursorVisible bool

	seq int
	// sent holds a hash of each row's rendered runs as of the last Update
	// this Renderer RETURNED (not the last Feed), so a caller that coalesced
	// several fed frames into one tick still gets every row that changed
	// since what it last sent.
	sent []uint64
	// haveSent is false until the first Update ever returned (or after a
	// resize/full reset invalidates every row), forcing that Update to be
	// full even if the caller didn't ask for one.
	haveSent bool
}

// New builds a Renderer at cols x rows.
func New(cols, rows int) *Renderer {
	r := &Renderer{}
	r.reset(cols, rows)
	return r
}

func (r *Renderer) reset(cols, rows int) {
	if cols <= 0 {
		cols = 1
	}
	if rows <= 0 {
		rows = 1
	}
	r.cols, r.rows = cols, rows
	r.cursorVisible = true
	e := vt.NewEmulator(cols, rows)
	e.SetCallbacks(vt.Callbacks{
		CursorVisibility: func(visible bool) { r.cursorVisible = visible },
	})
	r.emu = e
	r.sent = nil
	r.haveSent = false
}

// Feed applies one backend frame to the emulator. A Full frame rebuilds the
// emulator fresh at the frame's own width/height — the first frame of a
// stream, or a repaint following a resize — discarding whatever the emulator
// held before; a delta is written into the existing one. A delta that
// carries a different width/height than the emulator's current size (a
// mid-stream resize without a fresh Full) resizes the emulator in place and
// invalidates every row's sent-hash, so the next Update reports full
// regardless of what the caller asks for.
func (r *Renderer) Feed(f backend.Frame) error {
	data, err := base64.StdEncoding.DecodeString(f.Bytes)
	if err != nil {
		return fmt.Errorf("screen: decode frame bytes: %w", err)
	}
	if f.Full {
		cols, rows := f.Width, f.Height
		if cols <= 0 {
			cols = r.cols
		}
		if rows <= 0 {
			rows = r.rows
		}
		r.reset(cols, rows)
	} else if f.Width > 0 && f.Height > 0 && (f.Width != r.cols || f.Height != r.rows) {
		r.cols, r.rows = f.Width, f.Height
		r.emu.Resize(f.Width, f.Height)
		r.sent = nil
		r.haveSent = false
	}
	if _, err := r.emu.Write(data); err != nil {
		return fmt.Errorf("screen: write frame bytes: %w", err)
	}
	return nil
}

// Update computes the diff against the last Update THIS RENDERER RETURNED —
// full forces every row (including empty ones) regardless of what changed;
// it is also forced automatically the first time Update is ever called and
// right after any reset (a Full Feed, or a mid-stream resize).
func (r *Renderer) Update(full bool) Update {
	r.seq++
	wireFull := full || !r.haveSent

	hashes := make([]uint64, r.rows)
	var lines []Line
	for y := 0; y < r.rows; y++ {
		runs := r.renderRow(y)
		h := hashRuns(runs)
		hashes[y] = h
		if wireFull || y >= len(r.sent) || r.sent[y] != h {
			lines = append(lines, Line{I: y, Runs: runs})
		}
	}
	r.sent = hashes
	r.haveSent = true

	pos := r.emu.CursorPosition()
	return Update{
		Seq:  r.seq,
		Cols: r.cols,
		Rows: r.rows,
		Full: wireFull,
		Cursor: Cursor{
			X:       pos.X,
			Y:       pos.Y,
			Visible: r.cursorVisible,
		},
		Lines: lines,
	}
}

// Cursor reports the emulator's current cursor position and visibility
// without computing a row diff.
func (r *Renderer) Cursor() Cursor {
	pos := r.emu.CursorPosition()
	return Cursor{X: pos.X, Y: pos.Y, Visible: r.cursorVisible}
}

// renderRow builds row y's runs: iterate cells left to right, merging
// adjacent cells of identical style into one run and skipping the padding
// cell(s) that follow a wide character, then trim trailing whitespace.
func (r *Renderer) renderRow(y int) []Run {
	var runs []Run
	var cur *Run
	flush := func() {
		if cur != nil {
			runs = append(runs, *cur)
			cur = nil
		}
	}
	for x := 0; x < r.cols; {
		cell := r.emu.CellAt(x, y)
		content := " "
		var style uv.Style
		width := 1
		if cell != nil {
			style = cell.Style
			if cell.Width > 0 {
				content = cell.Content
				width = cell.Width
			}
			// cell.Width == 0 is a wide character's padding cell (or an
			// otherwise-empty placeholder); treat it as a single blank
			// column rather than skipping it outright, since renderRow
			// should never advance x by less than 1.
		}
		fg, hasFg := r.hexColor(style.Fg)
		bg, hasBg := r.hexColor(style.Bg)
		attrs := wireAttrs(style)
		run := Run{T: content, A: attrs}
		if hasFg {
			run.FG = fg
		}
		if hasBg {
			run.BG = bg
		}
		if cur != nil && cur.FG == run.FG && cur.BG == run.BG && cur.A == run.A {
			cur.T += content
		} else {
			flush()
			next := run
			cur = &next
		}
		x += width
	}
	flush()
	trimTrailingWhitespace(&runs)
	return runs
}

// hexColor resolves a cell's style color to `#rrggbb` through the emulator's
// palette. Indexed/basic ANSI colors are looked up via the emulator's own
// IndexedColor (which reflects any OSC 4 palette redefinition); a truecolor
// value is used as-is. nil (the terminal's default color) reports ok=false.
func (r *Renderer) hexColor(c color.Color) (hex string, ok bool) {
	if c == nil {
		return "", false
	}
	switch v := c.(type) {
	case ansi.BasicColor:
		c = r.emu.IndexedColor(int(v))
	case ansi.IndexedColor:
		c = r.emu.IndexedColor(int(v))
	}
	if c == nil {
		return "", false
	}
	rr, gg, bb, _ := c.RGBA()
	return fmt.Sprintf("#%02x%02x%02x", rr>>8, gg>>8, bb>>8), true
}

// wireAttrs maps ultraviolet's style bits onto the wire's attribute bitset
// (1 bold, 2 italic, 4 underline, 8 dim, 16 inverse, 32 strikethrough) —
// deliberately not the same bit positions as uv.Attrs, which the wire
// contract doesn't expose.
func wireAttrs(style uv.Style) int {
	var a int
	if style.Attrs&uv.AttrBold != 0 {
		a |= 1
	}
	if style.Attrs&uv.AttrItalic != 0 {
		a |= 2
	}
	if style.Underline != uv.UnderlineNone {
		a |= 4
	}
	if style.Attrs&uv.AttrFaint != 0 {
		a |= 8
	}
	if style.Attrs&uv.AttrReverse != 0 {
		a |= 16
	}
	if style.Attrs&uv.AttrStrikethrough != 0 {
		a |= 32
	}
	return a
}

// trimTrailingWhitespace drops a row's trailing run(s) of plain spaces, per
// the wire contract ("trailing whitespace of a line trimmed away, so a blank
// line has no runs") — including a styled trailing run whose content is
// nothing but spaces, since a run with no visible text carries nothing worth
// a client re-rendering.
func trimTrailingWhitespace(runs *[]Run) {
	rs := *runs
	for len(rs) > 0 {
		last := &rs[len(rs)-1]
		trimmed := strings.TrimRight(last.T, " ")
		if trimmed == last.T {
			break
		}
		if trimmed == "" {
			rs = rs[:len(rs)-1]
			continue
		}
		last.T = trimmed
		break
	}
	*runs = rs
}

// hashRuns hashes a row's rendered runs (FNV-1a over T/FG/BG/A) so Update can
// tell whether a row changed without keeping the previous run slice around.
func hashRuns(runs []Run) uint64 {
	const (
		offset64 = 14695981039346656037
		prime64  = 1099511628211
	)
	h := uint64(offset64)
	mix := func(s string) {
		for i := 0; i < len(s); i++ {
			h ^= uint64(s[i])
			h *= prime64
		}
		h ^= 0xff // separator between fields
		h *= prime64
	}
	for _, run := range runs {
		mix(run.T)
		mix(run.FG)
		mix(run.BG)
		h ^= uint64(run.A)
		h *= prime64
	}
	return h
}
