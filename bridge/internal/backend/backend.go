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
