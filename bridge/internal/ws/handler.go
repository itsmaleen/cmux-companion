package ws

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/itsmaleen/cmux-companion/bridge/internal/backend"
	"github.com/itsmaleen/cmux-companion/bridge/internal/imagepaste"
)

// framePushBuffer bounds how many surface.frame/surface.frames.ended pushes
// may queue for the connection's write loop before a forwarder goroutine
// blocks. Deliberately small: if the loop can't drain this promptly the
// bottleneck is real network backpressure, and blocking here is what starves
// the backend's own frame channel into its 2s backpressure timeout instead of
// silently buffering an unbounded amount of stale video.
const framePushBuffer = 16

// maxFitDimension bounds cols/rows a `surface.fit` may request — generous for
// any real phone screen, but small enough that a bogus value can't make the
// bridge spawn a PTY thousands of cells wide.
const maxFitDimension = 500

// maxIncomingMessageBytes bounds one client message. Sized from the largest
// image a paste may carry: base64 inflates by 4/3, plus room for the JSON
// envelope around it.
const maxIncomingMessageBytes = imagepaste.MaxImageBytes/3*4 + 64*1024

const bridgeVersion = "0.2.0"

// protocolVersion 2 adds `backend`, `backend_connected` and `capabilities` to
// the connected payload, renames cmux.connected/disconnected to
// backend.connected/disconnected, and introduces surface.updated.
const protocolVersion = 2

type pushMessage struct {
	Type string `json:"type"`
	Data any    `json:"data"`
}

type commandRequest struct {
	ID     string         `json:"id"`
	Method string         `json:"method"`
	Params map[string]any `json:"params"`
}

type commandResponse struct {
	ID     string          `json:"id"`
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// handleClient manages a single WebSocket connection for its lifetime.
// It pushes backend events and dispatches commands to the backend.
func handleClient(w http.ResponseWriter, r *http.Request, token string, be backend.Backend, images *imagepaste.Store) {
	if !validateBearer(r, token) {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		InsecureSkipVerify: true, // LAN only; origins vary
	})
	if err != nil {
		log.Printf("ws: accept error: %v", err)
		return
	}
	// The library's default read limit is 32 KiB, which is ample for every
	// command the phone sends EXCEPT a pasted image — and exceeding it doesn't
	// fail the message, it closes the connection (1009). Admit a base64 payload
	// of a full-size image plus its JSON envelope.
	conn.SetReadLimit(maxIncomingMessageBytes)
	defer conn.Close(websocket.StatusNormalClosure, "")

	ctx := r.Context()
	// connCtx is cancelled on every handleClient exit path (deferred below),
	// independent of exactly how r.Context() itself unwinds. Every frame
	// subscription is a child of it, so all of them tear down deterministically
	// the moment this function returns — "every subscription of a connection
	// is torn down when the connection closes."
	connCtx, connCancel := context.WithCancel(ctx)
	defer connCancel()

	// frameSubs maps a subscribed surface_id to the cancel func for its
	// stream. Touched only from this goroutine (the command-dispatch loop
	// below), so it needs no lock.
	frameSubs := make(map[string]context.CancelFunc)
	// fits maps a surface_id with an active `surface.fit` to its handle.
	// Touched only from this goroutine, same as frameSubs. A surface may have
	// at most one fit per connection: a second surface.fit for the same
	// surface resizes the existing fit rather than starting another.
	fits := make(map[string]backend.FitHandle)
	// Every fit still open when the connection ends is released here — herdr
	// then restores the pane's own layout size. This doesn't itself push
	// surface.fit.ended (see FitHandle.Release's doc).
	defer func() {
		for _, h := range fits {
			h.Release()
		}
	}()
	// framePushes carries surface.frame / surface.frames.ended /
	// surface.fit.ended pushes from the per-subscription forwarder goroutines
	// (started below) into the single writer goroutine's select loop;
	// websocket connections aren't safe for concurrent writes.
	framePushes := make(chan pushMessage, framePushBuffer)

	// Subscribe to backend events BEFORE reading the connection snapshot, so a
	// transition between the two can't be missed: the snapshot says "up", the
	// backend drops, and the subscription started too late to hear it. The
	// channel is never closed by Unsubscribe — the handler exits via ctx
	// cancellation or incoming closing.
	hub := be.Hub()
	events := hub.Subscribe()
	defer hub.Unsubscribe(events)

	info := be.Info()
	connected := be.Connected()
	// Send initial connected event. cmux_connected is kept for phones that
	// predate protocol 2.
	_ = wsjson.Write(ctx, conn, pushMessage{
		Type: "connected",
		Data: map[string]any{
			"bridge_version":    bridgeVersion,
			"protocol_version":  protocolVersion,
			"backend":           info.Kind,
			"backend_connected": connected,
			"cmux_connected":    connected,
			"capabilities":      info.Capabilities,
		},
	})

	// Channel for incoming commands from the client
	incoming := make(chan commandRequest) // unbuffered: at most one large paste resident beyond the one being handled

	// Read loop: decode incoming commands and forward to incoming channel
	go func() {
		defer close(incoming)
		for {
			var cmd commandRequest
			if err := wsjson.Read(ctx, conn, &cmd); err != nil {
				return
			}
			select {
			case incoming <- cmd:
			case <-ctx.Done():
				return
			}
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return

		case ev := <-events:
			if err := wsjson.Write(ctx, conn, pushMessage{
				Type: ev.Type,
				Data: ev.Data,
			}); err != nil {
				return
			}

		case msg := <-framePushes:
			if err := wsjson.Write(ctx, conn, msg); err != nil {
				return
			}

		case cmd, ok := <-incoming:
			if !ok {
				return
			}
			var resp commandResponse
			switch cmd.Method {
			case "surface.paste_image":
				resp = handlePasteImage(cmd, be, images)
			case "surface.paste_file":
				resp = handlePasteFile(cmd, be, images)
			case "surface.frames.subscribe":
				resp = handleFramesSubscribe(cmd, be, connCtx, frameSubs, framePushes)
			case "surface.frames.unsubscribe":
				resp = handleFramesUnsubscribe(cmd, frameSubs, framePushes)
			case "surface.fit":
				resp = handleFit(cmd, be, connCtx, fits, framePushes)
			case "surface.fit.release":
				resp = handleFitRelease(cmd, fits)
			default:
				resp = dispatch(cmd, be)
			}
			// The subscribe response above must reach the wire before the
			// first frame push: dispatching a subscribe starts the forwarder
			// goroutine synchronously, but that goroutine can only ever land
			// its first send on framePushes, which this same writer goroutine
			// won't service until it loops back around to `select` — i.e.
			// after the Write below returns.
			if err := wsjson.Write(ctx, conn, resp); err != nil {
				return
			}
		}
	}
}

// dispatch runs one command against the backend and shapes the response.
func dispatch(cmd commandRequest, be backend.Backend) commandResponse {
	result, err := be.Handle(cmd.Method, cmd.Params)
	if err != nil {
		return errorFor(cmd.ID, err)
	}
	return commandResponse{ID: cmd.ID, OK: true, Result: result}
}

// errorFor shapes a backend error, keeping its code when it has one.
func errorFor(id string, err error) commandResponse {
	var berr *backend.Error
	if errors.As(err, &berr) {
		return errorResponse(id, berr.Code, berr.Message)
	}
	return errorResponse(id, "proxy_error", err.Error())
}

func errorResponse(id, code, message string) commandResponse {
	return commandResponse{ID: id, OK: false, Error: &rpcError{Code: code, Message: message}}
}

// handlePasteImage materializes an image the phone pasted and types its path
// into the surface, which is how a terminal agent receives a picture at all —
// see package imagepaste. It is bridge-local and backend-agnostic: the path
// goes in through the backend's own surface.send_text, so it works the same
// for cmux, herdr, and a composite of both (whose namespaced ids the backend
// resolves).
//
// The optional `text` is a message the user typed to go WITH the image; it is
// appended after the path so the whole thing is ONE surface.send_text — one
// message to the agent, not a path and a caption as two separate submissions.
// `submit` appends the Enter that sends it.
func handlePasteImage(cmd commandRequest, be backend.Backend, images *imagepaste.Store) commandResponse {
	surfaceID, _ := cmd.Params["surface_id"].(string)
	if surfaceID == "" {
		return errorResponse(cmd.ID, "invalid_params", "surface_id is required")
	}
	encoded, _ := cmd.Params["image_base64"].(string)
	if encoded == "" {
		return errorResponse(cmd.ID, "invalid_params", "image_base64 is required")
	}
	// Bound the DECODED size before allocating it: base64 is 4/3 of the payload,
	// so checking the encoded length first keeps an oversized paste from being
	// decoded at all.
	if len(encoded) > imagepaste.MaxImageBytes/3*4+4 {
		return errorResponse(cmd.ID, "image_too_large",
			fmt.Sprintf("image exceeds the %d byte limit", imagepaste.MaxImageBytes))
	}
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return errorResponse(cmd.ID, "invalid_params", "image_base64 is not valid base64")
	}

	saved, err := images.Save(data)
	if err != nil {
		return errorResponse(cmd.ID, "paste_image_error", err.Error())
	}
	return typeSavedPath(cmd, be, images, surfaceID, saved)
}

// handlePasteFile materializes an arbitrary file the phone attached (a PDF, a
// log, an archive) and types its path into the surface, the same way as an
// image — the file is stored as-is and Claude Code reads it from the path.
// Backend-agnostic: the path goes in through the backend's surface.send_text.
func handlePasteFile(cmd commandRequest, be backend.Backend, images *imagepaste.Store) commandResponse {
	surfaceID, _ := cmd.Params["surface_id"].(string)
	if surfaceID == "" {
		return errorResponse(cmd.ID, "invalid_params", "surface_id is required")
	}
	encoded, _ := cmd.Params["data_base64"].(string)
	if encoded == "" {
		return errorResponse(cmd.ID, "invalid_params", "data_base64 is required")
	}
	if len(encoded) > imagepaste.MaxFileBytes/3*4+4 {
		return errorResponse(cmd.ID, "file_too_large",
			fmt.Sprintf("file exceeds the %d byte limit", imagepaste.MaxFileBytes))
	}
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return errorResponse(cmd.ID, "invalid_params", "data_base64 is not valid base64")
	}
	filename, _ := cmd.Params["filename"].(string)
	saved, err := images.SaveFile(data, filename)
	if err != nil {
		return errorResponse(cmd.ID, "paste_file_error", err.Error())
	}
	return typeSavedPath(cmd, be, images, surfaceID, saved)
}

// typeSavedPath composes one message from a materialized file: the shell-quoted
// path (which already ends in a space), then the optional `text` caption, then
// Enter when `submit` is set — sent as a SINGLE surface.send_text so the path
// and its caption reach the agent as one prompt, not two.
func typeSavedPath(cmd commandRequest, be backend.Backend, images *imagepaste.Store, surfaceID string, saved imagepaste.Result) commandResponse {
	text := saved.Text
	if caption, _ := cmd.Params["text"].(string); caption != "" {
		text += caption
	}
	if submit, _ := cmd.Params["submit"].(bool); submit {
		text += "\n"
	}
	params := map[string]any{"surface_id": surfaceID, "text": text}
	if wsID, ok := cmd.Params["workspace_id"]; ok {
		params["workspace_id"] = wsID
	}
	if _, err := be.Handle("surface.send_text", params); err != nil {
		// The file never made it into the surface; don't leave it on disk for
		// the full retention window.
		images.Remove(saved.Path)
		return errorFor(cmd.ID, err)
	}
	result, _ := json.Marshal(map[string]any{
		"surface_id": surfaceID,
		"path":       saved.Path,
		"bytes":      saved.Bytes,
		"format":     saved.Format,
	})
	return commandResponse{ID: cmd.ID, OK: true, Result: json.RawMessage(result)}
}

// handleFramesSubscribe starts (or restarts) a live terminal-frame stream for
// a surface. The backend must implement backend.FrameSource — cmux doesn't,
// so this answers `unsupported` for it, standalone or namespaced behind
// multi. Resubscribing an already-subscribed surface silently tears down the
// old stream and starts a fresh one (a new full frame): no
// `surface.frames.ended` is pushed for it, since the caller asked for exactly
// this and isn't losing anything it didn't intend to replace.
func handleFramesSubscribe(cmd commandRequest, be backend.Backend, connCtx context.Context, subs map[string]context.CancelFunc, framePushes chan<- pushMessage) commandResponse {
	surfaceID, _ := cmd.Params["surface_id"].(string)
	if surfaceID == "" {
		return errorResponse(cmd.ID, "invalid_params", "surface_id is required")
	}
	src, ok := be.(backend.FrameSource)
	if !ok {
		return errorResponse(cmd.ID, "unsupported", "this backend has no frame stream")
	}

	if cancel, exists := subs[surfaceID]; exists {
		cancel()
		delete(subs, surfaceID)
	}

	subCtx, cancel := context.WithCancel(connCtx)
	events, info, err := src.Frames(subCtx, surfaceID, intParam(cmd.Params, "cols"), intParam(cmd.Params, "rows"))
	if err != nil {
		cancel()
		return errorFor(cmd.ID, err)
	}
	subs[surfaceID] = cancel
	go forwardFrames(subCtx, surfaceID, events, framePushes)

	result, _ := json.Marshal(map[string]any{
		"surface_id": surfaceID,
		"width":      info.Width,
		"height":     info.Height,
	})
	return commandResponse{ID: cmd.ID, OK: true, Result: json.RawMessage(result)}
}

// handleFramesUnsubscribe tears down a surface's frame stream, if one is
// running, and tells the phone why it ended. Unsubscribing a surface with no
// active stream is not an error — it's the steady state after a stream ends
// on its own and the phone hasn't resubscribed yet.
func handleFramesUnsubscribe(cmd commandRequest, subs map[string]context.CancelFunc, framePushes chan<- pushMessage) commandResponse {
	surfaceID, _ := cmd.Params["surface_id"].(string)
	if surfaceID == "" {
		return errorResponse(cmd.ID, "invalid_params", "surface_id is required")
	}
	if cancel, ok := subs[surfaceID]; ok {
		cancel()
		delete(subs, surfaceID)
		select {
		case framePushes <- pushMessage{Type: "surface.frames.ended", Data: map[string]any{
			"surface_id": surfaceID,
			"reason":     "unsubscribed",
		}}:
		default:
			// framePushes is already full of live frame traffic for other
			// surfaces; dropping this notice is fine, the phone already knows
			// it unsubscribed.
		}
	}
	result, _ := json.Marshal(map[string]any{"ok": true})
	return commandResponse{ID: cmd.ID, OK: true, Result: json.RawMessage(result)}
}

// forwardFrames relays one subscription's FrameEvents onto framePushes as
// wire pushes until the stream ends or ctx is cancelled (unsubscribe,
// resubscribe, or connection close).
func forwardFrames(ctx context.Context, surfaceID string, events <-chan backend.FrameEvent, framePushes chan<- pushMessage) {
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-events:
			if !ok {
				return
			}
			var msg pushMessage
			if ev.Frame != nil {
				f := ev.Frame
				msg = pushMessage{Type: "surface.frame", Data: map[string]any{
					"surface_id": surfaceID,
					"seq":        f.Seq,
					"full":       f.Full,
					"width":      f.Width,
					"height":     f.Height,
					"encoding":   f.Encoding,
					"bytes":      f.Bytes,
				}}
			} else {
				msg = pushMessage{Type: "surface.frames.ended", Data: map[string]any{
					"surface_id": surfaceID,
					"reason":     ev.Ended,
				}}
			}
			select {
			case framePushes <- msg:
			case <-ctx.Done():
				return
			}
			if ev.Frame == nil {
				return // the backend already ended the stream
			}
		}
	}
}

// handleFit starts (or resizes) a "fit to phone" PTY resize for a surface.
// The backend must implement backend.Fitter — cmux doesn't, so this answers
// `unsupported` for it, standalone or namespaced behind multi. A second
// surface.fit for a surface this connection already has a fit for writes a
// live resize into the existing controller rather than starting another one
// (a connection may hold at most one fit per surface).
func handleFit(cmd commandRequest, be backend.Backend, connCtx context.Context, fits map[string]backend.FitHandle, framePushes chan<- pushMessage) commandResponse {
	surfaceID, _ := cmd.Params["surface_id"].(string)
	if surfaceID == "" {
		return errorResponse(cmd.ID, "invalid_params", "surface_id is required")
	}
	cols := intParam(cmd.Params, "cols")
	rows := intParam(cmd.Params, "rows")
	if cols <= 0 || rows <= 0 {
		return errorResponse(cmd.ID, "invalid_params", "cols and rows must be positive")
	}
	if cols > maxFitDimension || rows > maxFitDimension {
		return errorResponse(cmd.ID, "invalid_params", fmt.Sprintf("cols and rows must be at most %d", maxFitDimension))
	}

	if h, exists := fits[surfaceID]; exists {
		if err := h.Resize(cols, rows); err != nil {
			return errorResponse(cmd.ID, "fit_error", err.Error())
		}
		return fitResult(cmd.ID, surfaceID, cols, rows)
	}

	fitter, ok := be.(backend.Fitter)
	if !ok {
		return errorResponse(cmd.ID, "unsupported", "this backend has no fit-to-phone support")
	}
	h, err := fitter.Fit(connCtx, surfaceID, cols, rows)
	if err != nil {
		return errorFor(cmd.ID, err)
	}
	fits[surfaceID] = h
	go forwardFitEnded(connCtx, surfaceID, h, framePushes)

	return fitResult(cmd.ID, surfaceID, cols, rows)
}

func fitResult(id, surfaceID string, cols, rows int) commandResponse {
	result, _ := json.Marshal(map[string]any{"surface_id": surfaceID, "cols": cols, "rows": rows})
	return commandResponse{ID: id, OK: true, Result: json.RawMessage(result)}
}

// handleFitRelease ends a surface's fit, if one is running, restoring the
// runtime's own layout size. Releasing a surface with no active fit is not an
// error — it's the steady state after a fit ends on its own and the phone
// hasn't re-fit yet.
func handleFitRelease(cmd commandRequest, fits map[string]backend.FitHandle) commandResponse {
	surfaceID, _ := cmd.Params["surface_id"].(string)
	if surfaceID == "" {
		return errorResponse(cmd.ID, "invalid_params", "surface_id is required")
	}
	if h, ok := fits[surfaceID]; ok {
		h.Release()
		delete(fits, surfaceID)
	}
	result, _ := json.Marshal(map[string]any{"ok": true})
	return commandResponse{ID: cmd.ID, OK: true, Result: json.RawMessage(result)}
}

// forwardFitEnded relays one fit's Done signal as a surface.fit.ended push.
// An empty Reason means Done closed only because ctx was cancelled — our own
// release (explicit, or the connection closing) — for which no push is sent;
// see backend.FitHandle.Done's doc.
func forwardFitEnded(ctx context.Context, surfaceID string, h backend.FitHandle, framePushes chan<- pushMessage) {
	select {
	case <-h.Done():
	case <-ctx.Done():
		return
	}
	reason := h.Reason()
	if reason == "" {
		return
	}
	msg := pushMessage{Type: "surface.fit.ended", Data: map[string]any{
		"surface_id": surfaceID,
		"reason":     reason,
	}}
	select {
	case framePushes <- msg:
	case <-ctx.Done():
	}
}

// intParam reads an optional integer command param sent over JSON (so it
// decodes as float64), defaulting to 0.
func intParam(params map[string]any, key string) int {
	if v, ok := params[key].(float64); ok {
		return int(v)
	}
	return 0
}
