// Package herdr is the herdr backend (herdr.dev): a Rust runtime that owns
// terminals for coding agents and exposes them over a local socket API. Its
// model — workspace → tab → pane, one terminal per pane, per-pane agent
// status — is translated onto the phone's cmux-shaped vocabulary here.
//
// Connection model: every command is one short-lived socket connection (see
// client.go). One long-lived connection runs `events.subscribe`; it is the
// source of connection state, of the pane cache used to answer reads without
// an extra round-trip, and of notifications, which herdr does not keep as a
// list — they are synthesised from agent-status transitions to blocked/done.
package herdr

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/itsmaleen/cmux-companion/bridge/internal/backend"
	"github.com/itsmaleen/cmux-companion/bridge/internal/transcripts"
)

// Config selects the herdr socket.
type Config struct {
	SocketPath    string
	BridgeVersion string
}

// Backend implements backend.Backend over herdr's socket API.
type Backend struct {
	cfg        Config
	client     *client
	hub        *backend.Hub
	dispatcher *transcripts.Dispatcher
	connected  atomic.Bool

	mu         sync.Mutex
	panes      map[string]paneInfo // by pane_id, from the snapshot + pane events
	lastStatus map[string]string   // pane_id → agent_status at last observation
	notifs     []notification
	notifSeq   uint64
	// statusSubs holds the per-pane agent-status subscriptions (cancel funcs).
	statusSubs map[string]context.CancelFunc
	runCtx     context.Context
	// dial opens the lifecycle subscription; dialStatus opens one pane's
	// agent-status subscription. Tests swap them for canned streams.
	dial       func(ctx context.Context) (subscription, error)
	dialStatus func(ctx context.Context, paneID string) (subscription, error)
}

// New builds a herdr backend. It does not connect; Run does.
func New(cfg Config) *Backend {
	if cfg.SocketPath == "" {
		cfg.SocketPath = DefaultSocketPath()
	}
	b := &Backend{
		cfg:        cfg,
		client:     &client{socketPath: cfg.SocketPath, timeout: 10 * time.Second},
		hub:        backend.NewHub(),
		dispatcher: transcripts.New(),
		panes:      map[string]paneInfo{},
		lastStatus: map[string]string{},
		statusSubs: map[string]context.CancelFunc{},
		runCtx:     context.Background(),
	}
	b.dial = b.dialSubscription
	b.dialStatus = b.dialStatusSubscription
	return b
}

func (b *Backend) Info() backend.Info {
	return backend.Info{
		Kind: "herdr",
		Capabilities: backend.Capabilities{
			Browser:       false,
			AgentStatus:   true,
			Notifications: "push",
		},
	}
}

func (b *Backend) Hub() *backend.Hub { return b.hub }

func (b *Backend) Connected() bool { return b.connected.Load() }

// Ping round-trips herdr's `ping`.
func (b *Backend) Ping() error {
	var pong struct {
		Version  string `json:"version"`
		Protocol int    `json:"protocol"`
	}
	return b.client.callInto("ping", nil, &pong)
}

// Version returns herdr's reported version and protocol number.
func (b *Backend) Version() (version string, protocol int, err error) {
	var pong struct {
		Version  string `json:"version"`
		Protocol int    `json:"protocol"`
	}
	if err := b.client.callInto("ping", nil, &pong); err != nil {
		return "", 0, err
	}
	return pong.Version, pong.Protocol, nil
}

// Run holds the event subscription open, reconnecting with backoff, and keeps
// the pane cache, connection state, and notification list current from it.
func (b *Backend) Run(ctx context.Context) {
	b.mu.Lock()
	b.runCtx = ctx
	b.mu.Unlock()
	defer b.stopAllStatusSubs()
	backoff := time.Second
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		sub, err := b.dial(ctx)
		if err != nil {
			log.Printf("herdr: connect error: %v (retry in %s)", err, backoff)
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

		// Bootstrap after the subscription is acknowledged so no transition
		// lands between the snapshot and the first event.
		if err := b.loadSnapshot(); err != nil {
			log.Printf("herdr: snapshot error: %v", err)
			sub.close()
			b.setConnected(false, "socket_unavailable")
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			continue
		}
		log.Printf("herdr: connected")
		backoff = time.Second
		b.setConnected(true, "")

		b.consume(ctx, sub)
		sub.close()
		b.stopAllStatusSubs()
		b.setConnected(false, "socket_unavailable")
	}
}

// ensureStatusSub starts the agent-status subscription for a pane if none is
// running. Must not be called with b.mu held.
func (b *Backend) ensureStatusSub(paneID string) {
	b.mu.Lock()
	if _, running := b.statusSubs[paneID]; running {
		b.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(b.runCtx)
	b.statusSubs[paneID] = cancel
	b.mu.Unlock()
	go b.runStatusSub(ctx, paneID)
}

func (b *Backend) stopStatusSub(paneID string) {
	b.mu.Lock()
	cancel, ok := b.statusSubs[paneID]
	delete(b.statusSubs, paneID)
	b.mu.Unlock()
	if ok {
		cancel()
	}
}

func (b *Backend) stopAllStatusSubs() {
	b.mu.Lock()
	subs := b.statusSubs
	b.statusSubs = map[string]context.CancelFunc{}
	b.mu.Unlock()
	for _, cancel := range subs {
		cancel()
	}
}

// runStatusSub holds one pane's agent-status stream open until its context is
// cancelled (pane closed, backend disconnected) or herdr says the pane is gone.
func (b *Backend) runStatusSub(ctx context.Context, paneID string) {
	backoff := time.Second
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		sub, err := b.dialStatus(ctx, paneID)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			var rpc *rpcError
			if errors.As(err, &rpc) && strings.Contains(rpc.Code, "not_found") {
				// The pane is gone; pane.closed will (or did) drop it from the cache.
				b.stopStatusSub(paneID)
				return
			}
			log.Printf("herdr: status subscription %s: %v (retry in %s)", paneID, err, backoff)
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
		backoff = time.Second
		b.consume(ctx, sub)
		sub.close()
	}
}

func (b *Backend) setConnected(up bool, reason string) {
	if b.connected.Swap(up) == up {
		return
	}
	if up {
		b.hub.Broadcast(backend.Event{
			Type: "backend.connected",
			Data: map[string]any{"backend": "herdr", "bridge_version": b.cfg.BridgeVersion},
		})
	} else {
		b.hub.Broadcast(backend.Event{
			Type: "backend.disconnected",
			Data: map[string]any{"backend": "herdr", "reason": reason},
		})
	}
}

// loadSnapshot seeds the pane cache from session.snapshot. It records every
// pane's current status as "seen" without notifying: a bridge restart must not
// re-announce agents that were already blocked.
func (b *Backend) loadSnapshot() error {
	var snap sessionSnapshot
	if err := b.client.callInto("session.snapshot", nil, &snap); err != nil {
		return err
	}
	b.mu.Lock()
	b.panes = make(map[string]paneInfo, len(snap.Snapshot.Panes))
	b.lastStatus = make(map[string]string, len(snap.Snapshot.Panes))
	for _, p := range snap.Snapshot.Panes {
		b.panes[p.PaneID] = p
		b.lastStatus[p.PaneID] = p.AgentStatus
	}
	b.mu.Unlock()
	for _, p := range snap.Snapshot.Panes {
		b.ensureStatusSub(p.PaneID)
	}
	return nil
}

// consume applies events until the stream ends or ctx is done.
func (b *Backend) consume(ctx context.Context, sub subscription) {
	lines := make(chan []byte, 64)
	go func() {
		defer close(lines)
		for {
			line, err := sub.readLine()
			if err != nil {
				return
			}
			select {
			case lines <- line:
			case <-ctx.Done():
				return
			}
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return
		case line, ok := <-lines:
			if !ok {
				return
			}
			// A line that raced the cancellation belongs to a subscription
			// that is already dead (pane closed, backend reconnecting); the
			// select may still pick it over ctx.Done, so re-check here.
			if ctx.Err() != nil {
				return
			}
			b.applyEvent(line)
		}
	}
}

// applyEvent updates the cache from one pushed event line and publishes
// whatever the phone should hear about.
func (b *Backend) applyEvent(line []byte) {
	var env struct {
		Event string          `json:"event"`
		Data  json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(line, &env); err != nil || env.Event == "" {
		return
	}
	switch env.Event {
	case "pane_updated", "pane_created":
		var data struct {
			Pane paneInfo `json:"pane"`
		}
		if err := json.Unmarshal(env.Data, &data); err != nil || data.Pane.PaneID == "" {
			return
		}
		b.observePane(data.Pane, "")
		b.ensureStatusSub(data.Pane.PaneID)
	case "pane_moved":
		var data struct {
			PreviousPaneID string   `json:"previous_pane_id"`
			Pane           paneInfo `json:"pane"`
		}
		if err := json.Unmarshal(env.Data, &data); err != nil || data.Pane.PaneID == "" {
			return
		}
		if data.PreviousPaneID != "" && data.PreviousPaneID != data.Pane.PaneID {
			b.stopStatusSub(data.PreviousPaneID)
		}
		b.observePane(data.Pane, data.PreviousPaneID)
		b.ensureStatusSub(data.Pane.PaneID)
	case "pane_closed":
		var data struct {
			PaneID string `json:"pane_id"`
		}
		if err := json.Unmarshal(env.Data, &data); err != nil || data.PaneID == "" {
			return
		}
		b.stopStatusSub(data.PaneID)
		b.mu.Lock()
		delete(b.panes, data.PaneID)
		delete(b.lastStatus, data.PaneID)
		b.mu.Unlock()
	case "pane.agent_status_changed":
		// From a per-pane status subscription: carries status and presentation
		// but not the full pane record, so merge into the cached one.
		var data struct {
			PaneID       string `json:"pane_id"`
			WorkspaceID  string `json:"workspace_id"`
			AgentStatus  string `json:"agent_status"`
			Agent        string `json:"agent"`
			Title        string `json:"title"`
			DisplayAgent string `json:"display_agent"`
		}
		if err := json.Unmarshal(env.Data, &data); err != nil || data.PaneID == "" {
			return
		}
		p, err := b.pane(data.PaneID)
		if err != nil {
			// herdr no longer knows this pane: the event is from a subscription
			// that outlived its pane. Synthesising a record here would put a
			// closed pane back in the cache and push a ghost surface.updated.
			return
		}
		p.AgentStatus = data.AgentStatus
		p.Agent = data.Agent
		p.Title = data.Title
		p.DisplayAgent = data.DisplayAgent
		if data.WorkspaceID != "" {
			p.WorkspaceID = data.WorkspaceID
		}
		b.observePane(p, "")
	}
}

// observePane stores a pane and, on an agent-status transition into blocked
// or done, records and pushes a notification. Every status or title change is
// also pushed as surface.updated so the phone can reflect it without polling.
func (b *Backend) observePane(p paneInfo, previousID string) {
	b.mu.Lock()
	if previousID != "" && previousID != p.PaneID {
		// Cross-workspace move: same terminal, new public id. Carry the seen
		// status across so the move itself can't look like a transition.
		if s, ok := b.lastStatus[previousID]; ok {
			b.lastStatus[p.PaneID] = s
		}
		delete(b.panes, previousID)
		delete(b.lastStatus, previousID)
	}
	prev, known := b.lastStatus[p.PaneID]
	prevPane, hadPane := b.panes[p.PaneID]
	titleChanged := !hadPane || surfaceTitle(prevPane) != surfaceTitle(p)
	b.panes[p.PaneID] = p
	b.lastStatus[p.PaneID] = p.AgentStatus

	var notif *notification
	if known && prev != p.AgentStatus && p.Agent != "" {
		switch p.AgentStatus {
		case "blocked":
			notif = b.newNotificationLocked(p, "Needs your input")
		case "done":
			notif = b.newNotificationLocked(p, "Finished")
		}
	}
	changed := !known || prev != p.AgentStatus || titleChanged
	b.mu.Unlock()

	if changed {
		b.hub.Broadcast(backend.Event{Type: "surface.updated", Data: map[string]any{
			"surface_id":   p.PaneID,
			"workspace_id": p.WorkspaceID,
			"agent_status": p.AgentStatus,
			"agent":        p.Agent,
			"title":        surfaceTitle(p),
		}})
	}
	if notif != nil {
		b.hub.Broadcast(backend.Event{Type: "notification.created", Data: *notif})
	}
}

func (b *Backend) newNotificationLocked(p paneInfo, body string) *notification {
	b.notifSeq++
	n := notification{
		ID:          "herdr:" + p.PaneID + ":" + itoa(b.notifSeq),
		Title:       surfaceTitle(p),
		Subtitle:    agentLabel(p),
		Body:        body,
		WorkspaceID: p.WorkspaceID,
		SurfaceID:   p.PaneID,
	}
	b.notifs = append(b.notifs, n)
	if len(b.notifs) > maxNotifications {
		b.notifs = b.notifs[len(b.notifs)-maxNotifications:]
	}
	return &n
}

const maxNotifications = 100

// pane returns the cached pane, refreshing from herdr when it is unknown
// (a pane created before the subscription, or a stale cache after reconnect).
func (b *Backend) pane(paneID string) (paneInfo, error) {
	b.mu.Lock()
	p, ok := b.panes[paneID]
	b.mu.Unlock()
	if ok {
		return p, nil
	}
	var res struct {
		Pane paneInfo `json:"pane"`
	}
	if err := b.client.callInto("pane.get", map[string]any{"pane_id": paneID}, &res); err != nil {
		return paneInfo{}, err
	}
	b.mu.Lock()
	b.panes[res.Pane.PaneID] = res.Pane
	if _, seen := b.lastStatus[res.Pane.PaneID]; !seen {
		b.lastStatus[res.Pane.PaneID] = res.Pane.AgentStatus
	}
	b.mu.Unlock()
	return res.Pane, nil
}

func itoa(v uint64) string {
	if v == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	return string(buf[i:])
}

// --- herdr wire types (the subset the bridge reads) ---

type paneInfo struct {
	PaneID                string            `json:"pane_id"`
	TerminalID            string            `json:"terminal_id"`
	WorkspaceID           string            `json:"workspace_id"`
	TabID                 string            `json:"tab_id"`
	Focused               bool              `json:"focused"`
	Cwd                   string            `json:"cwd"`
	ForegroundCwd         string            `json:"foreground_cwd"`
	Label                 string            `json:"label"`
	Agent                 string            `json:"agent"`
	Title                 string            `json:"title"`
	TerminalTitle         string            `json:"terminal_title"`
	TerminalTitleStripped string            `json:"terminal_title_stripped"`
	DisplayAgent          string            `json:"display_agent"`
	AgentStatus           string            `json:"agent_status"`
	AgentSession          *agentSessionInfo `json:"agent_session"`
	Scroll                *paneScrollInfo   `json:"scroll"`
	Revision              uint64            `json:"revision"`
}

type agentSessionInfo struct {
	Source string `json:"source"`
	Agent  string `json:"agent"`
	Kind   string `json:"kind"` // "id" | "path"
	Value  string `json:"value"`
}

type paneScrollInfo struct {
	OffsetFromBottom    uint64 `json:"offset_from_bottom"`
	MaxOffsetFromBottom uint64 `json:"max_offset_from_bottom"`
	ViewportRows        uint64 `json:"viewport_rows"`
}

type workspaceInfo struct {
	WorkspaceID string `json:"workspace_id"`
	Number      int    `json:"number"`
	Label       string `json:"label"`
	Focused     bool   `json:"focused"`
	PaneCount   int    `json:"pane_count"`
	TabCount    int    `json:"tab_count"`
	ActiveTabID string `json:"active_tab_id"`
	AgentStatus string `json:"agent_status"`
}

type layoutRect struct {
	X      int `json:"x"`
	Y      int `json:"y"`
	Width  int `json:"width"`
	Height int `json:"height"`
}

type layoutSnapshot struct {
	WorkspaceID   string     `json:"workspace_id"`
	TabID         string     `json:"tab_id"`
	Zoomed        bool       `json:"zoomed"`
	Area          layoutRect `json:"area"`
	FocusedPaneID string     `json:"focused_pane_id"`
	Panes         []struct {
		PaneID  string     `json:"pane_id"`
		Focused bool       `json:"focused"`
		Rect    layoutRect `json:"rect"`
	} `json:"panes"`
}

type sessionSnapshot struct {
	Snapshot struct {
		Version            string           `json:"version"`
		Protocol           int              `json:"protocol"`
		FocusedWorkspaceID string           `json:"focused_workspace_id"`
		FocusedTabID       string           `json:"focused_tab_id"`
		FocusedPaneID      string           `json:"focused_pane_id"`
		Workspaces         []workspaceInfo  `json:"workspaces"`
		Panes              []paneInfo       `json:"panes"`
		Layouts            []layoutSnapshot `json:"layouts"`
	} `json:"snapshot"`
}

// notification is the wire shape the phone already decodes for cmux.
type notification struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	Subtitle    string `json:"subtitle"`
	Body        string `json:"body"`
	WorkspaceID string `json:"workspace_id"`
	SurfaceID   string `json:"surface_id"`
	IsRead      bool   `json:"is_read"`
}
