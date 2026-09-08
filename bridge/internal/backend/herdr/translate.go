package herdr

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/itsmaleen/cmux-companion/bridge/internal/backend"
	"github.com/itsmaleen/cmux-companion/bridge/internal/transcripts"
)

// Handle translates one phone command onto herdr's API. Method names and
// result shapes follow shared/protocol.md; a herdr pane is a phone surface.
func (b *Backend) Handle(method string, params map[string]any) (json.RawMessage, error) {
	if params == nil {
		params = map[string]any{}
	}
	if !b.Connected() {
		return nil, backend.Errorf("backend_unavailable", "herdr is not connected")
	}
	var (
		out any
		err error
	)
	switch method {
	case "system.ping":
		err = b.Ping()
		out = map[string]any{"pong": true}

	case "workspace.list":
		out, err = b.workspaceList()
	case "workspace.current":
		out, err = b.workspaceCurrent()
	case "workspace.select":
		err = b.simple("workspace.focus", map[string]any{"workspace_id": params["workspace_id"]})
		out = okResult
	case "workspace.create":
		out, err = b.workspaceCreate(params)
	case "workspace.close":
		err = b.simple("workspace.close", map[string]any{"workspace_id": params["workspace_id"]})
		out = okResult

	case "surface.list":
		out, err = b.surfaceList(params)
	case "surface.focus":
		err = b.simple("pane.focus", map[string]any{"pane_id": params["surface_id"]})
		out = okResult
	case "surface.close":
		err = b.simple("pane.close", map[string]any{"pane_id": params["surface_id"]})
		out = okResult
	case "surface.split":
		out, err = b.surfaceSplit(params)
	case "surface.create":
		out, err = b.surfaceCreate(params)
	case "surface.read_text":
		out, err = b.readText(params)
	case "surface.send_text":
		err = b.sendText(params)
		out = okResult
	case "surface.send_key":
		err = b.sendKey(params)
		out = okResult

	case "pane.list":
		out, err = b.paneList(params)
	case "pane.focus":
		err = b.simple("pane.focus", map[string]any{"pane_id": params["pane_id"]})
		out = okResult

	case "notification.list":
		b.mu.Lock()
		list := append([]notification(nil), b.notifs...)
		b.mu.Unlock()
		if list == nil {
			list = []notification{}
		}
		out = map[string]any{"notifications": list}
	case "notification.clear":
		b.mu.Lock()
		b.notifs = nil
		b.mu.Unlock()
		out = okResult

	case "claude.transcript", "agent.transcript":
		return b.transcript(params)

	case "browser.url.get":
		return nil, backend.Errorf("unsupported", "herdr has no browser surfaces")

	default:
		return nil, backend.Errorf("unsupported_method", "herdr backend does not support "+method)
	}

	if err != nil {
		return nil, wrapErr(err)
	}
	raw, merr := json.Marshal(out)
	if merr != nil {
		return nil, backend.Errorf("internal", merr.Error())
	}
	return raw, nil
}

var okResult = map[string]any{"ok": true}

// wrapErr maps a herdr error object onto the phone's error code space.
func wrapErr(err error) error {
	var already *backend.Error
	if errors.As(err, &already) {
		return already
	}
	var rpc *rpcError
	if errors.As(err, &rpc) {
		return backend.Errorf(rpc.Code, rpc.Message)
	}
	return backend.Errorf("proxy_error", err.Error())
}

func (b *Backend) simple(method string, params map[string]any) error {
	_, err := b.client.call(method, params)
	return err
}

// --- workspaces ---

func (b *Backend) workspaceList() (any, error) {
	var res struct {
		Workspaces []workspaceInfo `json:"workspaces"`
	}
	if err := b.client.callInto("workspace.list", nil, &res); err != nil {
		return nil, err
	}
	list := make([]map[string]any, 0, len(res.Workspaces))
	for _, w := range res.Workspaces {
		list = append(list, map[string]any{
			"id":           w.WorkspaceID,
			"title":        w.Label,
			"number":       w.Number,
			"is_focused":   w.Focused,
			"agent_status": w.AgentStatus,
		})
	}
	return map[string]any{"workspaces": list}, nil
}

func (b *Backend) workspaceCurrent() (any, error) {
	var res struct {
		Workspaces []workspaceInfo `json:"workspaces"`
	}
	if err := b.client.callInto("workspace.list", nil, &res); err != nil {
		return nil, err
	}
	for _, w := range res.Workspaces {
		if w.Focused {
			return map[string]any{"id": w.WorkspaceID, "title": w.Label}, nil
		}
	}
	if len(res.Workspaces) > 0 {
		w := res.Workspaces[0]
		return map[string]any{"id": w.WorkspaceID, "title": w.Label}, nil
	}
	return nil, backend.Errorf("not_found", "herdr has no workspaces")
}

func (b *Backend) workspaceCreate(params map[string]any) (any, error) {
	req := map[string]any{"focus": true}
	if name, _ := params["name"].(string); name != "" {
		req["label"] = name
	}
	if cwd, _ := params["cwd"].(string); cwd != "" {
		req["cwd"] = cwd
	} else if home, err := os.UserHomeDir(); err == nil {
		req["cwd"] = home
	}
	var res struct {
		Workspace workspaceInfo `json:"workspace"`
		RootPane  paneInfo      `json:"root_pane"`
	}
	if err := b.client.callInto("workspace.create", req, &res); err != nil {
		return nil, err
	}
	return map[string]any{
		"id":         res.Workspace.WorkspaceID,
		"title":      res.Workspace.Label,
		"surface_id": res.RootPane.PaneID,
	}, nil
}

// --- surfaces (herdr panes) ---

func (b *Backend) surfaceList(params map[string]any) (any, error) {
	req := map[string]any{}
	if wsID, _ := params["workspace_id"].(string); wsID != "" {
		req["workspace_id"] = wsID
	}
	var res struct {
		Panes []paneInfo `json:"panes"`
	}
	if err := b.client.callInto("pane.list", req, &res); err != nil {
		return nil, err
	}
	// A list is as good as a snapshot for the cache.
	b.mu.Lock()
	for _, p := range res.Panes {
		b.panes[p.PaneID] = p
		if _, seen := b.lastStatus[p.PaneID]; !seen {
			b.lastStatus[p.PaneID] = p.AgentStatus
		}
	}
	b.mu.Unlock()

	list := make([]map[string]any, 0, len(res.Panes))
	for _, p := range res.Panes {
		list = append(list, surfaceRecord(p))
	}
	return map[string]any{"surfaces": list}, nil
}

// surfaceRecord is the phone's Surface shape for a herdr pane. resume_binding
// mirrors what cmux reports for an agent surface so the phone's existing
// transcript affordance keys off it unchanged.
func surfaceRecord(p paneInfo) map[string]any {
	rec := map[string]any{
		"id":           p.PaneID,
		"title":        surfaceTitle(p),
		"type":         "terminal",
		"workspace_id": p.WorkspaceID,
		"tab_id":       p.TabID,
		"is_focused":   p.Focused,
		"agent_status": p.AgentStatus,
		"cwd":          cwdOf(p),
	}
	if p.Agent != "" {
		rec["agent"] = p.Agent
		binding := map[string]any{"kind": p.Agent, "cwd": cwdOf(p)}
		if p.AgentSession != nil && p.AgentSession.Kind == "id" && p.AgentSession.Value != "" {
			binding["checkpoint_id"] = p.AgentSession.Value
		}
		rec["resume_binding"] = binding
	}
	return rec
}

// surfaceTitle picks the most specific human label herdr has for a pane: an
// explicit rename, then the agent's own reported title, then the terminal
// title (Claude Code sets its conversation topic there), then the agent, then
// the directory.
func surfaceTitle(p paneInfo) string {
	for _, s := range []string{p.Label, p.Title, p.TerminalTitleStripped, p.TerminalTitle} {
		if s = strings.TrimSpace(s); s != "" {
			return s
		}
	}
	if p.DisplayAgent != "" {
		return p.DisplayAgent
	}
	if p.Agent != "" {
		return p.Agent
	}
	if cwd := cwdOf(p); cwd != "" {
		return filepath.Base(cwd)
	}
	return p.PaneID
}

func agentLabel(p paneInfo) string {
	if p.DisplayAgent != "" {
		return p.DisplayAgent
	}
	return p.Agent
}

func cwdOf(p paneInfo) string {
	if p.ForegroundCwd != "" {
		return p.ForegroundCwd
	}
	return p.Cwd
}

func (b *Backend) surfaceSplit(params map[string]any) (any, error) {
	direction, _ := params["direction"].(string)
	switch direction {
	case "left", "right":
		direction = "right"
	case "up", "down":
		direction = "down"
	default:
		return nil, backend.Errorf("invalid_params", "direction must be left/right/up/down")
	}
	req := map[string]any{"direction": direction, "focus": true}
	if sid, _ := params["surface_id"].(string); sid != "" {
		req["target_pane_id"] = sid
	}
	var res struct {
		Pane paneInfo `json:"pane"`
	}
	if err := b.client.callInto("pane.split", req, &res); err != nil {
		return nil, err
	}
	return map[string]any{"surface_id": res.Pane.PaneID}, nil
}

func (b *Backend) surfaceCreate(params map[string]any) (any, error) {
	if t, _ := params["type"].(string); t != "" && t != "terminal" {
		return nil, backend.Errorf("unsupported", "herdr only has terminal surfaces")
	}
	req := map[string]any{"focus": true}
	if wsID, _ := params["workspace_id"].(string); wsID != "" {
		req["workspace_id"] = wsID
	}
	var res struct {
		RootPane paneInfo `json:"root_pane"`
	}
	if err := b.client.callInto("tab.create", req, &res); err != nil {
		return nil, err
	}
	return map[string]any{"surface_id": res.RootPane.PaneID}, nil
}

// readText serves surface.read_text from pane.read. For a pane running a
// recognized agent the row count is clamped to the viewport: herdr answers a
// deeper read on an *idle* alternate-screen agent by driving that agent's
// mouse-scroll to page through its history — visibly moving the user's live
// pane on every poll. History for those comes from the transcript instead.
func (b *Backend) readText(params map[string]any) (any, error) {
	paneID, _ := params["surface_id"].(string)
	if paneID == "" {
		return nil, backend.Errorf("invalid_params", "surface_id is required")
	}
	lines := 50
	if v, ok := params["lines"].(float64); ok && v > 0 {
		lines = int(v)
	}
	p, err := b.pane(paneID)
	if err != nil {
		return nil, err
	}
	lines = clampReadLines(p, lines)

	var res struct {
		Read struct {
			Text      string `json:"text"`
			Truncated bool   `json:"truncated"`
		} `json:"read"`
	}
	req := map[string]any{"pane_id": paneID, "source": "recent", "lines": lines, "strip_ansi": true}
	if err := b.client.callInto("pane.read", req, &res); err != nil {
		return nil, err
	}
	return map[string]any{
		"surface_id": paneID,
		"text":       res.Read.Text,
		"truncated":  res.Read.Truncated,
		"lines":      lines,
	}, nil
}

// clampReadLines bounds a read of an agent pane to its visible rows.
func clampReadLines(p paneInfo, lines int) int {
	if p.Agent == "" || p.Scroll == nil || p.Scroll.ViewportRows == 0 {
		return lines
	}
	if rows := int(p.Scroll.ViewportRows); lines > rows {
		return rows
	}
	return lines
}

// sendText types text into a pane. A trailing newline is the phone's "and
// press Enter"; it is delivered as the Enter key rather than a raw LF so a
// raw-mode TUI (every agent) sees a real key press.
func (b *Backend) sendText(params map[string]any) error {
	paneID, _ := params["surface_id"].(string)
	text, _ := params["text"].(string)
	req := map[string]any{"pane_id": paneID}
	if strings.HasSuffix(text, "\n") {
		req["text"] = strings.TrimSuffix(text, "\n")
		req["keys"] = []string{"enter"}
	} else {
		req["text"] = text
	}
	if req["text"] == "" && req["keys"] == nil {
		return nil
	}
	return b.simple("pane.send_input", req)
}

// sendKey maps the phone's key names onto herdr's. cmd+shift+enter is the cmux
// app's pane-zoom shortcut, not a terminal key; herdr has pane.zoom for that.
func (b *Backend) sendKey(params map[string]any) error {
	paneID, _ := params["surface_id"].(string)
	key, _ := params["key"].(string)
	if key == "cmd+shift+enter" {
		return b.simple("pane.zoom", map[string]any{"pane_id": paneID, "mode": "toggle"})
	}
	mapped, ok := mapKey(key)
	if !ok {
		return backend.Errorf("invalid_params", "unsupported key "+key)
	}
	return b.simple("pane.send_keys", map[string]any{"pane_id": paneID, "keys": []string{mapped}})
}

// mapKey translates a phone key name to herdr's key-combo syntax.
func mapKey(key string) (string, bool) {
	key = strings.ToLower(strings.TrimSpace(key))
	if key == "" {
		return "", false
	}
	parts := strings.Split(key, "+")
	base := parts[len(parts)-1]
	mods := parts[:len(parts)-1]
	switch base {
	case "escape":
		base = "esc"
	case "return":
		base = "enter"
	case "del":
		base = "delete"
	case "pageup", "page_up":
		base = "pageup"
	case "pagedown", "page_down":
		base = "pagedown"
	}
	for i, m := range mods {
		switch m {
		case "control", "ctrl":
			mods[i] = "ctrl"
		case "option", "alt":
			mods[i] = "alt"
		case "shift":
			mods[i] = "shift"
		case "cmd", "command", "super", "meta":
			// No terminal-level equivalent; herdr rejects it and so do we.
			return "", false
		default:
			return "", false
		}
	}
	return strings.Join(append(mods, base), "+"), true
}

// --- panes (herdr tab layouts) ---

// paneList reports the layout of the workspace's active tab. A herdr pane is
// one terminal, so each layout rect is a phone pane holding exactly one
// surface. Rects are in cells relative to the tab area; the phone normalises.
func (b *Backend) paneList(params map[string]any) (any, error) {
	var snap sessionSnapshot
	if err := b.client.callInto("session.snapshot", nil, &snap); err != nil {
		return nil, err
	}
	wsID, _ := params["workspace_id"].(string)
	if wsID == "" {
		wsID = snap.Snapshot.FocusedWorkspaceID
	}
	var activeTab string
	for _, w := range snap.Snapshot.Workspaces {
		if w.WorkspaceID == wsID {
			activeTab = w.ActiveTabID
			break
		}
	}
	panes := []map[string]any{}
	for _, layout := range snap.Snapshot.Layouts {
		if layout.WorkspaceID != wsID || layout.TabID != activeTab {
			continue
		}
		for _, lp := range layout.Panes {
			panes = append(panes, map[string]any{
				"id": lp.PaneID,
				"pixel_frame": map[string]any{
					"x":      lp.Rect.X - layout.Area.X,
					"y":      lp.Rect.Y - layout.Area.Y,
					"width":  lp.Rect.Width,
					"height": lp.Rect.Height,
				},
				"container_frame": map[string]any{
					"width":  layout.Area.Width,
					"height": layout.Area.Height,
				},
				"surface_ids":        []string{lp.PaneID},
				"focused_surface_id": lp.PaneID,
				"is_focused":         lp.Focused,
			})
		}
		break
	}
	return map[string]any{"panes": panes}, nil
}

// --- transcripts ---

// transcript renders the agent conversation behind a pane. herdr's Claude and
// opencode integration hooks both report their session id as the pane's
// agent_session, which surfaceRecord turns into a resume_binding the same
// shape cmux uses: for Claude the resolver finds <id>.jsonl under
// ~/.claude/projects; for opencode the dispatcher reads the session directly
// from opencode's database, no title/tty matching needed since herdr already
// names the exact session. Without an integration installed there is no
// session id, and Claude's resolver falls back to the newest transcript for
// the pane's cwd (opencode has no such fallback here: herdr reports no tty for
// title matching to key off).
func (b *Backend) transcript(params map[string]any) (json.RawMessage, error) {
	paneID, _ := params["surface_id"].(string)
	p, err := b.pane(paneID)
	if err != nil {
		return nil, wrapErr(err)
	}
	rec := surfaceRecord(p)
	binding, _ := rec["resume_binding"].(map[string]any)
	res, err := b.dispatcher.Render(transcripts.Request{
		SurfaceID:        paneID,
		ResumeBinding:    binding,
		SurfaceTitle:     surfaceTitle(p),
		Directory:        cwdOf(p),
		MaxMessages:      transcripts.MaxMessages(params),
		KnownFingerprint: transcripts.KnownFingerprint(params),
	})
	if err != nil {
		return nil, backend.Errorf("transcript_error", err.Error())
	}
	return transcripts.Encode(res), nil
}
