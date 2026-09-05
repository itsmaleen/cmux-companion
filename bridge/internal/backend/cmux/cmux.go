// Package cmux is the cmux backend: the phone vocabulary is cmux's own socket
// API, so commands are proxied verbatim, notifications come from polling
// cmux's notification.list, Claude transcripts are bound through cmux's hook
// session store, and opencode transcripts (which cmux does not bind at all
// unless its optional opencode hooks are installed) are identified by tty and
// title.
package cmux

import (
	"context"
	"encoding/json"
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/itsmaleen/cmux-companion/bridge/internal/backend"
	"github.com/itsmaleen/cmux-companion/bridge/internal/poller"
	"github.com/itsmaleen/cmux-companion/bridge/internal/socket"
	"github.com/itsmaleen/cmux-companion/bridge/internal/transcripts"
)

// Config selects the cmux socket.
type Config struct {
	SocketPath   string
	Password     string
	PollInterval time.Duration
	// BridgeVersion is reported in the backend.connected event.
	BridgeVersion string
}

// Backend implements backend.Backend over cmux's Unix socket.
type Backend struct {
	cfg        Config
	client     *socket.Client
	hub        *backend.Hub
	poll       *poller.Poller
	dispatcher *transcripts.Dispatcher
	connected  atomic.Bool

	termMu      sync.Mutex
	terminals   []map[string]any
	terminalsAt time.Time
}

// New builds a cmux backend. It does not connect; Run does.
func New(cfg Config) *Backend {
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = time.Second
	}
	hub := backend.NewHub()
	client := socket.NewClient(cfg.SocketPath, cfg.Password)
	return &Backend{
		cfg:        cfg,
		client:     client,
		hub:        hub,
		poll:       poller.New(client, cfg.PollInterval, hub),
		dispatcher: transcripts.New(),
	}
}

func (b *Backend) Info() backend.Info {
	return backend.Info{
		Kind: "cmux",
		Capabilities: backend.Capabilities{
			Browser:       true,
			AgentStatus:   false,
			Notifications: "polled",
		},
	}
}

func (b *Backend) Hub() *backend.Hub { return b.hub }

func (b *Backend) Connected() bool { return b.connected.Load() }

// Ping opens a one-off connection and round-trips system.ping, so a socket
// that accepts connections but rejects every RPC (wrong uid, wrong password)
// is caught here rather than left flapping in the daemon.
func (b *Backend) Ping() error {
	client := socket.NewClient(b.cfg.SocketPath, b.cfg.Password)
	if err := client.Connect(); err != nil {
		return err
	}
	defer client.Close()
	_, err := client.Send("system.ping", nil)
	return err
}

// IsAuthRequired reports whether err is cmux saying the socket needs a
// password.
func IsAuthRequired(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "auth_required") || strings.Contains(s, "Authentication required")
}

// Run keeps the socket connected with exponential backoff and a 5s ping,
// publishing backend.connected / backend.disconnected on transitions. It also
// drives the notification poller.
func (b *Backend) Run(ctx context.Context) {
	stopPoller := make(chan struct{})
	go func() {
		<-ctx.Done()
		close(stopPoller)
	}()
	go b.poll.Run(stopPoller)

	backoff := time.Second
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		if err := b.client.Connect(); err != nil {
			log.Printf("cmux: connect error: %v (retry in %s)", err, backoff)
			b.setConnected(false, "socket_unavailable")
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			if backoff < 30*time.Second {
				backoff *= 2
			}
			continue
		}

		log.Printf("cmux: connected")
		backoff = time.Second
		b.setConnected(true, "")

	pingLoop:
		for {
			select {
			case <-ctx.Done():
				return
			case <-time.After(5 * time.Second):
				if _, err := b.client.Send("system.ping", nil); err != nil {
					log.Printf("cmux: connection lost: %v", err)
					b.setConnected(false, "socket_unavailable")
					break pingLoop
				}
			}
		}
	}
}

func (b *Backend) setConnected(up bool, reason string) {
	if b.connected.Swap(up) == up {
		return
	}
	if up {
		b.hub.Broadcast(backend.Event{
			Type: "backend.connected",
			Data: map[string]any{"backend": "cmux", "bridge_version": b.cfg.BridgeVersion},
		})
	} else {
		b.hub.Broadcast(backend.Event{
			Type: "backend.disconnected",
			Data: map[string]any{"backend": "cmux", "reason": reason},
		})
	}
}

// Handle proxies every command to cmux except the bridge-local transcript
// read, and resets the poller's seen set after a successful notification.clear
// so re-appearing notifications get pushed again.
func (b *Backend) Handle(method string, params map[string]any) (json.RawMessage, error) {
	switch method {
	case "claude.transcript", "agent.transcript":
		return b.transcript(params)
	}

	result, err := b.client.Send(method, params)
	if err != nil {
		return nil, backend.Errorf("proxy_error", err.Error())
	}
	if method == "notification.clear" {
		b.poll.ResetSeenIDs()
	}
	return result, nil
}

func (b *Backend) transcript(params map[string]any) (json.RawMessage, error) {
	surfaceID, _ := params["surface_id"].(string)

	// Build surface.list params, forwarding workspace_id if provided.
	listParams := map[string]any{}
	if wsID, ok := params["workspace_id"]; ok {
		listParams["workspace_id"] = wsID
	}

	resumeBinding, found, err := b.surfaceBinding(listParams, surfaceID)
	if err != nil {
		return nil, backend.Errorf("transcript_error", err.Error())
	}

	// surface.list answers for ONE workspace — the current one when the
	// caller named none. A surface in any other workspace simply isn't in the
	// reply, and reporting "no agent here" for it would be wrong rather than
	// empty. cmux's terminal table spans every workspace, so it can say which
	// one to ask about.
	var terminal map[string]any
	if !found {
		terminal = b.terminal(surfaceID)
		if terminal != nil {
			workspaceID := stringField(terminal, "workspace_id")
			if wsID, _ := listParams["workspace_id"].(string); workspaceID != "" && workspaceID != wsID {
				resumeBinding, _, err = b.surfaceBinding(map[string]any{"workspace_id": workspaceID}, surfaceID)
				if err != nil {
					return nil, backend.Errorf("transcript_error", err.Error())
				}
			}
		}
	}

	req := transcripts.Request{
		SurfaceID:        surfaceID,
		ResumeBinding:    resumeBinding,
		MaxMessages:      transcripts.MaxMessages(params),
		KnownFingerprint: transcripts.KnownFingerprint(params),
	}

	// No cmux-recognized binding (Claude, or an installed opencode hook): the
	// only agent left findable is opencode, identified by its tty and title
	// rather than anything cmux itself reports.
	if kind, _ := resumeBinding["kind"].(string); kind == "" {
		if terminal == nil {
			terminal = b.terminal(surfaceID)
		}
		if terminal != nil {
			req.TTY = stringField(terminal, "tty")
			req.SurfaceTitle = stringField(terminal, "surface_title")
			req.Directory = stringField(terminal, "current_directory")
			if req.Directory == "" {
				req.Directory = stringField(terminal, "requested_working_directory")
			}
		}
	}

	res, err := b.dispatcher.Render(req)
	if err != nil {
		return nil, backend.Errorf("transcript_error", err.Error())
	}
	return transcripts.Encode(res), nil
}

// surfaceBinding fetches one surface's resume_binding from a surface.list
// call, reporting whether the surface was in the reply at all — an absent
// surface and a surface with no binding are different answers.
func (b *Backend) surfaceBinding(listParams map[string]any, surfaceID string) (map[string]any, bool, error) {
	listResult, err := b.client.Send("surface.list", listParams)
	if err != nil {
		return nil, false, err
	}
	var listPayload struct {
		Surfaces []map[string]any `json:"surfaces"`
	}
	if err := json.Unmarshal(listResult, &listPayload); err != nil {
		return nil, false, err
	}
	for _, s := range listPayload.Surfaces {
		if id, _ := s["id"].(string); id == surfaceID {
			binding, _ := s["resume_binding"].(map[string]any)
			return binding, true, nil
		}
	}
	return nil, false, nil
}

// terminalsCacheTTL bounds how often cmux is asked for its terminal table. The
// focused surface polls every few seconds and the table is large (every field
// cmux knows about every surface); which agent runs where changes far more
// slowly than that.
const terminalsCacheTTL = 2 * time.Second

// terminal returns cmux's terminal-table entry for a surface, which is the
// only place a surface's tty and raw title are exposed. Never fatal: a cmux
// build without debug.terminals just means opencode surfaces with no
// resume_binding go unidentified, same as before this existed.
func (b *Backend) terminal(surfaceID string) map[string]any {
	for _, t := range b.terminalTable() {
		if id, _ := t["surface_id"].(string); id == surfaceID {
			return t
		}
	}
	return nil
}

func (b *Backend) terminalTable() []map[string]any {
	b.termMu.Lock()
	defer b.termMu.Unlock()
	if time.Since(b.terminalsAt) < terminalsCacheTTL {
		return b.terminals
	}
	b.terminalsAt = time.Now()
	result, err := b.client.Send("debug.terminals", map[string]any{})
	if err != nil {
		b.terminals = nil
		return nil
	}
	var payload struct {
		Terminals []map[string]any `json:"terminals"`
	}
	if err := json.Unmarshal(result, &payload); err != nil {
		b.terminals = nil
		return nil
	}
	b.terminals = payload.Terminals
	return b.terminals
}

func stringField(row map[string]any, key string) string {
	if v, ok := row[key].(string); ok {
		return v
	}
	return ""
}
