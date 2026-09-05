// Package multi composes several backends behind one phone-facing surface, so
// a Mac running both cmux and herdr shows both sets of workspaces under one
// bridge. Every id the phone sees is namespaced with the member's kind
// (`cmux:<id>`, `herdr:w1:p1`); commands are routed by that prefix and results
// and push events are rewritten back into the namespace.
package multi

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/itsmaleen/cmux-companion/bridge/internal/backend"
)

// Member is one runtime under the composite.
type Member struct {
	Kind    string
	Backend backend.Backend
}

// Backend implements backend.Backend over several members.
type Backend struct {
	members []Member
	hub     *backend.Hub

	mu sync.Mutex
	// current is the member that answers commands carrying no id (the phone's
	// "current workspace" lives in one runtime at a time). It follows
	// workspace.select and starts as the first connected member.
	current string
	// transMu serializes member connection transitions so the aggregate
	// up/down decision and its broadcast happen in one critical section.
	transMu sync.Mutex
}

// New builds a composite of the given members, in priority order.
func New(members ...Member) *Backend {
	return &Backend{members: members, hub: backend.NewHub()}
}

// Kinds lists the member kinds in order.
func (b *Backend) Kinds() []string {
	out := make([]string, 0, len(b.members))
	for _, m := range b.members {
		out = append(out, m.Kind)
	}
	return out
}

func (b *Backend) Info() backend.Info {
	var caps backend.Capabilities
	notif := map[string]bool{}
	for _, m := range b.members {
		c := m.Backend.Info().Capabilities
		caps.Browser = caps.Browser || c.Browser
		caps.AgentStatus = caps.AgentStatus || c.AgentStatus
		caps.Frames = caps.Frames || c.Frames
		notif[c.Notifications] = true
	}
	switch {
	case len(notif) == 1:
		for k := range notif {
			caps.Notifications = k
		}
	default:
		caps.Notifications = "mixed"
	}
	return backend.Info{Kind: strings.Join(b.Kinds(), "+"), Capabilities: caps}
}

func (b *Backend) Hub() *backend.Hub { return b.hub }

func (b *Backend) Connected() bool {
	for _, m := range b.members {
		if m.Backend.Connected() {
			return true
		}
	}
	return false
}

// Ping succeeds when at least one member answers, reporting every failure
// otherwise.
func (b *Backend) Ping() error {
	var errs []string
	for _, m := range b.members {
		if err := m.Backend.Ping(); err != nil {
			errs = append(errs, m.Kind+": "+err.Error())
		} else {
			return nil
		}
	}
	return fmt.Errorf("%s", strings.Join(errs, "; "))
}

// Run runs every member and forwards their events into the composite hub,
// namespaced. backend.connected is forwarded whenever a member comes up (the
// phone refreshes its lists); backend.disconnected only once no member is
// left, since the phone treats it as "everything went away".
func (b *Backend) Run(ctx context.Context) {
	var wg sync.WaitGroup
	for _, m := range b.members {
		wg.Add(1)
		go func(m Member) {
			defer wg.Done()
			m.Backend.Run(ctx)
		}(m)
		wg.Add(1)
		go func(m Member) {
			defer wg.Done()
			b.forward(ctx, m)
		}(m)
	}
	wg.Wait()
}

func (b *Backend) forward(ctx context.Context, m Member) {
	events := m.Backend.Hub().Subscribe()
	defer m.Backend.Hub().Unsubscribe(events)
	for {
		select {
		case <-ctx.Done():
			return
		case ev := <-events:
			b.forwardEvent(m, ev)
		}
	}
}

func (b *Backend) forwardEvent(m Member, ev backend.Event) {
	switch ev.Type {
	case "backend.connected":
		b.transMu.Lock()
		defer b.transMu.Unlock()
		b.mu.Lock()
		if b.current == "" {
			b.current = m.Kind
		}
		b.mu.Unlock()
		b.hub.Broadcast(ev)
	case "backend.disconnected":
		b.transMu.Lock()
		defer b.transMu.Unlock()
		if !b.Connected() {
			b.hub.Broadcast(ev)
			return
		}
		// One runtime dropped while another is up: not a bridge-wide outage,
		// but the phone may be sitting in the dead runtime's workspace. Tell it
		// which member changed so it refreshes its lists (the dead member's
		// workspaces disappear from workspace.list) and re-selects.
		b.hub.Broadcast(backend.Event{Type: "backend.changed", Data: map[string]any{
			"backend":   m.Kind,
			"connected": false,
			"remaining": b.connectedKinds(),
		}})
	default:
		data, err := toMap(ev.Data)
		if err != nil {
			b.hub.Broadcast(ev)
			return
		}
		prefixItem(m.Kind, data)
		b.hub.Broadcast(backend.Event{Type: ev.Type, Data: data})
	}
}

// Handle routes one command. Commands naming an id go to that id's member;
// list/ping/clear commands without one fan out or go to the current member.
func (b *Backend) Handle(method string, params map[string]any) (json.RawMessage, error) {
	if params == nil {
		params = map[string]any{}
	}
	kind, stripped, err := b.route(params)
	if err != nil {
		return nil, err
	}

	if kind == "" {
		switch method {
		case "system.ping":
			return b.fanOut(method, stripped, func(results map[string]json.RawMessage) any {
				return map[string]any{"pong": true, "backends": keys(results)}
			})
		case "workspace.list":
			return b.fanOut(method, stripped, mergeWorkspaces)
		case "notification.list":
			return b.fanOut(method, stripped, mergeNotifications)
		case "notification.clear":
			// Strict: a clear that only reached one runtime must not report
			// success, or the phone drops alerts the other runtime still holds.
			return b.fanOutStrict(method, stripped, func(map[string]json.RawMessage) any { return map[string]any{"ok": true} })
		}
		kind = b.currentKind()
		if kind == "" {
			return nil, backend.Errorf("backend_unavailable", "no runtime is connected")
		}
	}

	m := b.member(kind)
	if m == nil {
		return nil, backend.Errorf("unknown_backend", "no backend "+kind)
	}
	raw, err := m.Backend.Handle(method, stripped)
	if err != nil {
		return nil, err
	}
	if method == "workspace.select" {
		// Only a select that succeeded moves the current runtime; a failed one
		// must leave id-less commands pointed where they were.
		b.mu.Lock()
		b.current = kind
		b.mu.Unlock()
	}
	return prefixResult(kind, raw)
}

// Frames implements backend.FrameSource, routing by the namespaced
// surfaceID's prefix to the member that owns it and re-namespacing the
// Frame.SurfaceID of everything that member emits. A member that doesn't
// itself implement FrameSource (cmux) answers `unsupported`, exactly as a
// standalone instance of it would.
func (b *Backend) Frames(ctx context.Context, surfaceID string, cols, rows int) (<-chan backend.FrameEvent, backend.FrameInfo, error) {
	kind, id := splitID(surfaceID)
	if kind == "" {
		return nil, backend.FrameInfo{}, backend.Errorf("invalid_params", "surface_id is not namespaced: "+surfaceID)
	}
	m := b.member(kind)
	if m == nil {
		return nil, backend.FrameInfo{}, backend.Errorf("unknown_backend", "no backend "+kind)
	}
	src, ok := m.Backend.(backend.FrameSource)
	if !ok {
		return nil, backend.FrameInfo{}, backend.Errorf("unsupported", kind+" has no frame stream")
	}
	events, info, err := src.Frames(ctx, id, cols, rows)
	if err != nil {
		return nil, backend.FrameInfo{}, err
	}

	out := make(chan backend.FrameEvent, 1)
	go func() {
		defer close(out)
		for {
			select {
			case <-ctx.Done():
				return
			case ev, ok := <-events:
				if !ok {
					return
				}
				if ev.Frame != nil {
					f := *ev.Frame
					f.SurfaceID = joinID(kind, f.SurfaceID)
					ev = backend.FrameEvent{Frame: &f}
				}
				select {
				case out <- ev:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return out, info, nil
}

// fanOut runs a command on every connected member and merges the results;
// one member failing is tolerated as long as another answered.
func (b *Backend) fanOut(method string, params map[string]any, merge func(map[string]json.RawMessage) any) (json.RawMessage, error) {
	return b.fanOutWith(method, params, merge, false)
}

// fanOutStrict is fanOut where any connected member failing fails the call.
func (b *Backend) fanOutStrict(method string, params map[string]any, merge func(map[string]json.RawMessage) any) (json.RawMessage, error) {
	return b.fanOutWith(method, params, merge, true)
}

func (b *Backend) fanOutWith(method string, params map[string]any, merge func(map[string]json.RawMessage) any, strict bool) (json.RawMessage, error) {
	results := map[string]json.RawMessage{}
	var lastErr error
	for _, m := range b.members {
		if !m.Backend.Connected() {
			continue
		}
		raw, err := m.Backend.Handle(method, params)
		if err != nil {
			lastErr = err
			continue
		}
		results[m.Kind] = raw
	}
	if lastErr != nil && (strict || len(results) == 0) {
		return nil, lastErr
	}
	if len(results) == 0 {
		return nil, backend.Errorf("backend_unavailable", "no runtime is connected")
	}
	out, err := json.Marshal(merge(results))
	if err != nil {
		return nil, backend.Errorf("internal", err.Error())
	}
	return out, nil
}

func (b *Backend) connectedKinds() []string {
	var out []string
	for _, m := range b.members {
		if m.Backend.Connected() {
			out = append(out, m.Kind)
		}
	}
	return out
}

func (b *Backend) currentKind() string {
	b.mu.Lock()
	cur := b.current
	b.mu.Unlock()
	if m := b.member(cur); m != nil && m.Backend.Connected() {
		return cur
	}
	for _, m := range b.members {
		if m.Backend.Connected() {
			return m.Kind
		}
	}
	return ""
}

func (b *Backend) member(kind string) *Member {
	for i := range b.members {
		if b.members[i].Kind == kind {
			return &b.members[i]
		}
	}
	return nil
}

// idParams are the request fields that carry namespaced ids.
var idParams = []string{"workspace_id", "surface_id", "pane_id"}

// route finds the member a request addresses from its namespaced ids and
// returns the params with the namespace stripped. Ids from two different
// members in one request are an error.
func (b *Backend) route(params map[string]any) (kind string, stripped map[string]any, err error) {
	stripped = make(map[string]any, len(params))
	for k, v := range params {
		stripped[k] = v
	}
	for _, key := range idParams {
		v, ok := params[key].(string)
		if !ok || v == "" {
			continue
		}
		k, id := splitID(v)
		if k == "" {
			return "", nil, backend.Errorf("invalid_params", key+" is not namespaced: "+v)
		}
		if kind != "" && k != kind {
			return "", nil, backend.Errorf("invalid_params", "ids from different runtimes in one request")
		}
		kind = k
		stripped[key] = id
	}
	return kind, stripped, nil
}

// splitID splits "cmux:abc" into ("cmux", "abc"). herdr ids contain colons
// themselves ("w1:p1"), so only the first segment is the namespace.
func splitID(v string) (kind, id string) {
	i := strings.IndexByte(v, ':')
	if i <= 0 {
		return "", v
	}
	return v[:i], v[i+1:]
}

func joinID(kind, id string) string { return kind + ":" + id }

// --- result rewriting ---

// listKeys are result arrays whose items carry ids.
var listKeys = []string{"workspaces", "surfaces", "panes", "notifications"}

// objectKeys are nested single records that carry ids (cmux's
// workspace.current answers with the workspace under `workspace`).
var objectKeys = []string{"workspace", "surface", "pane"}

// itemIDKeys are the id fields rewritten on a record.
var itemIDKeys = []string{"id", "workspace_id", "surface_id", "pane_id", "focused_surface_id"}

func prefixResult(kind string, raw json.RawMessage) (json.RawMessage, error) {
	var data map[string]any
	if err := json.Unmarshal(raw, &data); err != nil {
		// Not an object (shouldn't happen); pass through untouched.
		return raw, nil
	}
	prefixItem(kind, data)
	for _, key := range objectKeys {
		if rec, ok := data[key].(map[string]any); ok {
			prefixItem(kind, rec)
		}
	}
	for _, key := range listKeys {
		items, _ := data[key].([]any)
		for _, it := range items {
			if rec, ok := it.(map[string]any); ok {
				prefixItem(kind, rec)
			}
		}
	}
	out, err := json.Marshal(data)
	if err != nil {
		return nil, backend.Errorf("internal", err.Error())
	}
	return out, nil
}

// prefixItem namespaces the id fields of one record in place.
func prefixItem(kind string, rec map[string]any) {
	for _, key := range itemIDKeys {
		if v, ok := rec[key].(string); ok && v != "" {
			rec[key] = joinID(kind, v)
		}
	}
	if ids, ok := rec["surface_ids"].([]any); ok {
		for i, v := range ids {
			if s, ok := v.(string); ok {
				ids[i] = joinID(kind, s)
			}
		}
	}
}

func mergeWorkspaces(results map[string]json.RawMessage) any {
	var merged []any
	for _, kind := range sortedKeys(results) {
		var data struct {
			Workspaces []map[string]any `json:"workspaces"`
		}
		if err := json.Unmarshal(results[kind], &data); err != nil {
			continue
		}
		for _, w := range data.Workspaces {
			prefixItem(kind, w)
			// Both runtimes can hold a workspace with the same name (the same
			// repo opened in each); the phone labels them from this.
			w["backend"] = kind
			merged = append(merged, w)
		}
	}
	if merged == nil {
		merged = []any{}
	}
	return map[string]any{"workspaces": merged}
}

func mergeNotifications(results map[string]json.RawMessage) any {
	var merged []any
	for _, kind := range sortedKeys(results) {
		var data struct {
			Notifications []map[string]any `json:"notifications"`
		}
		if err := json.Unmarshal(results[kind], &data); err != nil {
			continue
		}
		for _, n := range data.Notifications {
			prefixItem(kind, n)
			merged = append(merged, n)
		}
	}
	if merged == nil {
		merged = []any{}
	}
	return map[string]any{"notifications": merged}
}

func toMap(v any) (map[string]any, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	return m, nil
}

func keys(m map[string]json.RawMessage) []string { return sortedKeys(m) }

func sortedKeys(m map[string]json.RawMessage) []string {
	out := make([]string, 0, len(m))
	// Keep member priority order (cmux before herdr) rather than map order.
	for _, k := range []string{"cmux", "herdr"} {
		if _, ok := m[k]; ok {
			out = append(out, k)
		}
	}
	for k := range m {
		if k != "cmux" && k != "herdr" {
			out = append(out, k)
		}
	}
	return out
}
