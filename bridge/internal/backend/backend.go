// Package backend defines the seam between the phone-facing WebSocket server
// and whatever terminal runtime owns the terminals on this Mac (cmux, herdr).
//
// The phone speaks one vocabulary — the command set documented in
// shared/protocol.md (`workspace.list`, `surface.read_text`, …). A Backend
// accepts that vocabulary and either proxies it (cmux, whose socket API *is*
// that vocabulary) or translates it (herdr). Push events flow the other way
// through a Hub so the WebSocket handler never knows which runtime it fronts.
package backend

import (
	"context"
	"encoding/json"
	"sync"
)

// Event is a push message to every connected phone. Type is the wire `type`
// (`notification.created`, `backend.connected`, …); Data is the wire `data`.
type Event struct {
	Type string
	Data any
}

// Capabilities tells the phone which affordances this runtime can honour, so
// it hides the ones that would only ever error.
type Capabilities struct {
	// Browser: the runtime has browser surfaces (`surface.create {type:
	// "browser"}`, `browser.url.get`).
	Browser bool `json:"browser"`
	// AgentStatus: surfaces carry a runtime-detected `agent_status`
	// (idle/working/blocked/done/unknown) the phone can show instead of
	// inferring activity from successive reads.
	AgentStatus bool `json:"agent_status"`
	// Notifications: "polled" (the runtime keeps a list the bridge polls) or
	// "push" (the bridge synthesises them from runtime events).
	Notifications string `json:"notifications"`
	// Frames: the runtime can stream a surface's live terminal frames
	// (`surface.frames.subscribe`), so the phone can render it with a real
	// terminal emulator instead of polling `surface.read_text`.
	Frames bool `json:"frames"`
	// Screen: the runtime can stream a surface's terminal as bridge-rendered
	// styled rows (`surface.screen.subscribe`) instead of raw frames — true
	// whenever Frames is, since Screen is implemented generically over any
	// FrameSource (see package screen).
	Screen bool `json:"screen"`
}

// Info identifies the runtime behind the bridge.
type Info struct {
	// Kind is "cmux" or "herdr".
	Kind         string
	Capabilities Capabilities
}

// Error is a command failure with a machine-readable code, surfaced to the
// phone as the `error` object of the command response.
type Error struct {
	Code    string
	Message string
}

func (e *Error) Error() string { return "[" + e.Code + "] " + e.Message }

// Errorf builds an Error.
func Errorf(code, message string) *Error { return &Error{Code: code, Message: message} }

// Backend is one terminal runtime.
type Backend interface {
	Info() Info
	// Run maintains the runtime connection until ctx is done, broadcasting
	// backend.connected / backend.disconnected through the Hub as it goes.
	Run(ctx context.Context)
	// Connected reports whether the runtime is reachable right now.
	Connected() bool
	// Ping verifies a request round-trip with the runtime, for pairing-time
	// diagnostics.
	Ping() error
	// Handle executes one phone command. The error is an *Error when the
	// backend classified the failure; anything else is reported as a generic
	// proxy_error.
	Handle(method string, params map[string]any) (json.RawMessage, error)
	// Hub is where this backend publishes push events.
	Hub() *Hub
}

// Frame is one terminal repaint, relayed to the phone as a `surface.frame`
// push. The first frame of a stream is always Full; later ones are deltas in
// the source runtime's own encoding (herdr: raw PTY bytes it crops/pads to
// the requested grid).
type Frame struct {
	SurfaceID string
	Seq       int
	Full      bool
	Width     int
	Height    int
	Encoding  string
	Bytes     string // still base64; passed through to the phone untouched
}

// FrameInfo is the grid a frame stream actually settled on, reported back as
// the surface.frames.subscribe result.
type FrameInfo struct {
	Width, Height int
}

// FrameEvent is one item off a frame stream: a Frame to relay, or a terminal
// Ended reason ("closed", "error", "backpressure") — exactly one is set. A
// stream that is torn down by the caller (ctx cancelled) sends nothing
// further; the caller already knows why.
type FrameEvent struct {
	Frame *Frame
	Ended string
}

// FrameSource is implemented by backends that can stream a surface's live
// terminal frames (herdr; cmux has no such stream). The WebSocket handler
// type-asserts for it and answers `surface.frames.subscribe` with an
// `unsupported` error when a backend doesn't implement it.
type FrameSource interface {
	// Frames starts a frame stream for surfaceID at cols x rows (0 for either
	// means the surface's native size). The returned channel is closed when
	// the stream ends; cancel ctx to stop it early. A synchronous error
	// (`not_found`, `frames_error`) means no stream was started at all.
	Frames(ctx context.Context, surfaceID string, cols, rows int) (<-chan FrameEvent, FrameInfo, error)
}

// FitHandle is one surface's live PTY resize, held open for as long as the
// phone wants the runtime's full-screen TUI laid out for its screen.
type FitHandle interface {
	// Resize changes the fit's PTY size in place, without tearing down and
	// restarting the underlying control session.
	Resize(cols, rows int) error
	// Done is closed once the fit's underlying process has fully torn down,
	// whether that's because it ended on its own (the runtime closed it, or
	// it died) or because Release was called. Reason distinguishes the two:
	// it is only non-empty when the fit ended on its own, which is what a
	// caller should check before pushing `surface.fit.ended` — an explicit
	// release is reported to whoever called Release, not through this
	// channel, so a connection releasing its own fits never pushes an ended
	// event for them.
	Done() <-chan struct{}
	// Reason explains why the fit ended: "closed" (the runtime ended it) or
	// "error" (the controlling process/stream ended unexpectedly, with no
	// explanation from the runtime). Meaningful only after Done has closed,
	// and left empty when Done closed because of a Release.
	Reason() string
	// Release ends the fit, restoring the runtime's own layout size. Safe to
	// call more than once and safe to call after the fit already ended on its
	// own.
	Release()
}

// ScreenCursor is the emulated cursor's rendered position and visibility.
type ScreenCursor struct {
	X       int  `json:"x"`
	Y       int  `json:"y"`
	Visible bool `json:"visible"`
}

// ScreenRun is one run of identically-styled text within a rendered row.
// Adjacent cells sharing a style are merged into one run; T is never empty
// (a wholly blank trailing stretch of a row is trimmed away rather than sent
// as a run of spaces). FG/BG are `#rrggbb`, omitted when the cell uses the
// terminal's default color. A is the OR of attribute bits (0 when the run
// carries no attributes): 1 bold, 2 italic, 4 underline, 8 dim, 16 inverse,
// 32 strikethrough.
type ScreenRun struct {
	T  string `json:"t"`
	FG string `json:"fg,omitempty"`
	BG string `json:"bg,omitempty"`
	A  int    `json:"a,omitempty"`
}

// ScreenLine is one rendered row, identified by its 0-based index from the
// top of the grid.
type ScreenLine struct {
	I    int         `json:"i"`
	Runs []ScreenRun `json:"runs"`
}

// ScreenUpdate is one `surface.screen` push: the rows that changed since the
// last ScreenUpdate actually SENT on this stream, or (Full) every row —
// including empty ones, as a line with no runs — such as the first update of
// a stream or one following a resize.
type ScreenUpdate struct {
	SurfaceID string       `json:"surface_id"`
	Seq       int          `json:"seq"`
	Cols      int          `json:"cols"`
	Rows      int          `json:"rows"`
	Full      bool         `json:"full"`
	Cursor    ScreenCursor `json:"cursor"`
	Lines     []ScreenLine `json:"lines"`
}

// ScreenEvent is one item off a screen stream: a ScreenUpdate to relay, or a
// terminal Ended reason ("closed", "error", "backpressure") — exactly one is
// set, mirroring FrameEvent.
type ScreenEvent struct {
	Update *ScreenUpdate
	Ended  string
}

// ScreenSource is implemented by backends that can stream a surface's
// terminal as bridge-rendered styled rows instead of raw frames. Any
// FrameSource gets this generically via screen.FromFrames; the WebSocket
// handler type-asserts for it and answers `surface.screen.subscribe` with an
// `unsupported` error when a backend doesn't implement it.
type ScreenSource interface {
	// Screens starts a screen stream for surfaceID at cols x rows (0 for
	// either means the surface's native size, matching FrameSource.Frames).
	// The returned channel is closed when the stream ends; cancel ctx to
	// stop it early. A synchronous error (`not_found`, `frames_error`) means
	// no stream was started at all.
	Screens(ctx context.Context, surfaceID string, cols, rows int) (<-chan ScreenEvent, FrameInfo, error)
}

// Fitter is implemented by backends that can temporarily resize a surface's
// real PTY to a size the phone requests (herdr; cmux has no such control), so
// a full-screen TUI (opencode, Claude Code) re-layouts for the phone's
// screen. The WebSocket handler type-asserts for it and answers
// `surface.fit` with an `unsupported` error when a backend doesn't implement
// it.
type Fitter interface {
	// Fit resizes surfaceID's terminal to cols x rows for as long as the
	// returned handle stays open (until Release, or the connection holding it
	// closes). A synchronous error (`not_found`, `fit_error`) means no fit was
	// established at all.
	Fit(ctx context.Context, surfaceID string, cols, rows int) (FitHandle, error)
}

// Hub fans push events out to WebSocket clients. Slow consumers drop events
// rather than blocking the publisher.
type Hub struct {
	mu          sync.Mutex
	subscribers []chan Event
}

func NewHub() *Hub { return &Hub{} }

// Subscribe returns a buffered channel that receives every subsequent event.
func (h *Hub) Subscribe() <-chan Event {
	ch := make(chan Event, 32)
	h.mu.Lock()
	h.subscribers = append(h.subscribers, ch)
	h.mu.Unlock()
	return ch
}

// Unsubscribe removes a subscriber. The channel is NOT closed — closing a
// channel a publisher might concurrently be sending to would panic. Handlers
// detect disconnection through their own context instead.
func (h *Hub) Unsubscribe(ch <-chan Event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for i, sub := range h.subscribers {
		if sub == ch {
			h.subscribers = append(h.subscribers[:i], h.subscribers[i+1:]...)
			return
		}
	}
}

// Broadcast delivers ev to every current subscriber.
func (h *Hub) Broadcast(ev Event) {
	h.mu.Lock()
	subs := make([]chan Event, len(h.subscribers))
	copy(subs, h.subscribers)
	h.mu.Unlock()

	for _, sub := range subs {
		select {
		case sub <- ev:
		default:
			// slow consumer; drop
		}
	}
}
