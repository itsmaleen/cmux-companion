// Package transcripts answers "what conversation is this surface showing"
// across every agent the bridge understands, independent of which backend
// (cmux, herdr) is asking.
//
// Agents differ in both halves of that question. Claude Code is usually bound
// by the runtime itself (a resume_binding naming the session) and stores its
// transcript as JSONL; opencode is sometimes bound the same way (herdr reports
// it directly) and sometimes not bound at all (a plain cmux surface, where the
// opencode integration is opt-in and usually absent), and it stores its
// conversation in SQLite. Keeping the dispatch here means each backend asks one
// question and stays ignorant of how a specific agent is identified or read.
package transcripts

import (
	"encoding/json"

	"github.com/itsmaleen/cmux-companion/bridge/internal/claude"
	"github.com/itsmaleen/cmux-companion/bridge/internal/opencode"
)

// Request describes one transcript resolution, gathered by the calling
// backend from whatever metadata its runtime exposes for the surface.
type Request struct {
	// SurfaceID identifies the surface to the calling backend (a cmux hook
	// store key, a herdr pane id).
	SurfaceID string
	// ResumeBinding is the runtime's own binding for the surface, when it has
	// one: {"kind": "claude"|"opencode", "cwd": "...", "checkpoint_id": "..."}.
	// A nil map means the runtime names no agent for this surface at all.
	ResumeBinding map[string]any
	// SurfaceTitle is the surface's terminal title, used only to identify
	// WHICH opencode session a surface with no resume_binding is running:
	// opencode sets it to "OC | <session title>" (truncated).
	SurfaceTitle string
	// Directory is the surface's working directory, used the same way as
	// SurfaceTitle when ResumeBinding carries no cwd of its own.
	Directory string
	// TTY is the surface's controlling terminal device, used to confirm an
	// opencode process is actually running there before trusting its title.
	// Backends that already know the agent from ResumeBinding (herdr) can
	// leave this empty.
	TTY string
	// MaxMessages bounds how many trailing messages are rendered.
	MaxMessages int
	// KnownFingerprint is the Fingerprint of the text the caller already
	// holds; an unchanged transcript answers Unchanged with no Text.
	KnownFingerprint string
}

// Result is the outcome shared by every backend's transcript command.
type Result struct {
	// Supported reports whether this surface names (or was shown to run) an
	// agent this package can read.
	Supported bool
	// AgentKind is "claude" or "opencode", empty when Supported is false.
	AgentKind string
	// Text is the rendered transcript, empty when nothing was found or when
	// Unchanged is set.
	Text string
	// SessionID is the session the transcript was read from, or the session
	// the surface points at when that session's data is missing.
	SessionID string
	// SessionTitle is the agent's own title for the session, when it has one
	// distinct from SessionID (opencode; Claude has none).
	SessionTitle string
	// SessionMissing reports that the surface names a specific session whose
	// data is not on disk/in the database.
	SessionMissing bool
	// Fingerprint identifies this exact rendering. Hand it back as
	// Request.KnownFingerprint to skip re-sending unchanged text.
	Fingerprint string
	// Unchanged reports that the transcript still matches KnownFingerprint, so
	// Text was deliberately not produced.
	Unchanged bool
	// Source names the strategy that resolved the conversation, for
	// diagnosing a surface that shows the wrong (or no) conversation.
	Source string
}

// Dispatcher routes a Request to the agent-specific resolver that can answer
// it.
type Dispatcher struct {
	claude   *claude.Resolver
	opencode *opencode.Store
}

// New builds a Dispatcher backed by fresh Claude and opencode resolvers.
func New() *Dispatcher {
	return &Dispatcher{
		claude:   claude.NewResolver(),
		opencode: opencode.NewStore(),
	}
}

// Render resolves and renders the conversation behind req.
func (d *Dispatcher) Render(req Request) (Result, error) {
	kind, _ := req.ResumeBinding["kind"].(string)

	switch kind {
	case "claude":
		res, err := d.claude.Render(claude.Request{
			SurfaceID:        req.SurfaceID,
			ResumeBinding:    req.ResumeBinding,
			MaxMessages:      req.MaxMessages,
			KnownFingerprint: req.KnownFingerprint,
		})
		if err != nil {
			return Result{}, err
		}
		return fromClaude(res), nil

	case "opencode":
		if checkpointID, _ := req.ResumeBinding["checkpoint_id"].(string); checkpointID != "" {
			// The runtime bound this surface to a specific session. If that
			// session is not in the database it was deleted — report it
			// missing rather than rendering it as an empty existing session.
			if exists, err := d.opencode.SessionExists(checkpointID); err == nil && !exists {
				return Result{Supported: true, AgentKind: "opencode", SessionID: checkpointID, SessionMissing: true, Source: "resume_binding"}, nil
			}
			return d.renderOpencodeSession(checkpointID, req.MaxMessages, req.KnownFingerprint, "resume_binding")
		}
		// The runtime knows this is opencode but not which session — fall
		// through to title/cwd matching below, using whatever cwd the
		// binding carries.
		if cwd, _ := req.ResumeBinding["cwd"].(string); cwd != "" && req.Directory == "" {
			req.Directory = cwd
		}
		return d.renderOpencodeByTitle(req)
	}

	// No resume_binding at all: the only agent this package can still find is
	// opencode, and only when the caller can prove a real opencode process is
	// on the surface's terminal — never inferred from the title alone, which
	// a shell can be made to say anything with.
	if !d.RunsOpencode(req.TTY) {
		return Result{}, nil
	}
	return d.renderOpencodeByTitle(req)
}

// RunsOpencode reports whether a real opencode process has tty as its
// controlling terminal. This is the fact a backend needs before it may label a
// surface its runtime does not bind as opencode; the title alone never is.
func (d *Dispatcher) RunsOpencode(tty string) bool {
	if tty == "" || !d.opencode.Available() {
		return false
	}
	_, running := d.opencode.TTYs()[opencode.NormalizeTTY(tty)]
	return running
}

// renderOpencodeByTitle resolves WHICH opencode session a surface with no
// session id is running, from its title and working directory. Several
// opencode surfaces routinely share one directory, so the title is required,
// not merely preferred: a surface whose session has no title yet reports
// opencode_unidentified and renders empty rather than guessing the newest
// session in the directory.
func (d *Dispatcher) renderOpencodeByTitle(req Request) (Result, error) {
	session, ok := d.opencode.ResolveSession(req.SurfaceTitle, req.Directory)
	if !ok {
		return Result{Supported: true, AgentKind: "opencode", Source: "opencode_unidentified"}, nil
	}
	res, err := d.renderOpencodeSession(session.ID, req.MaxMessages, req.KnownFingerprint, "opencode_title_cwd")
	if err != nil {
		return Result{}, err
	}
	res.SessionTitle = session.Title
	return res, nil
}

// renderOpencodeSession renders one known opencode session, short-circuiting
// on a fingerprint match the same way Claude's resolver does.
func (d *Dispatcher) renderOpencodeSession(sessionID string, maxMessages int, knownFingerprint, source string) (Result, error) {
	fingerprint, err := d.opencode.Fingerprint(sessionID, maxMessages)
	if err != nil {
		return Result{}, err
	}
	res := Result{
		Supported:   true,
		AgentKind:   "opencode",
		SessionID:   sessionID,
		Fingerprint: fingerprint,
		Source:      source,
	}
	if knownFingerprint != "" && knownFingerprint == fingerprint {
		res.Unchanged = true
		return res, nil
	}
	text, err := d.opencode.Render(sessionID, maxMessages)
	if err != nil {
		return Result{}, err
	}
	res.Text = text
	return res, nil
}

func fromClaude(res claude.Result) Result {
	if !res.Supported {
		return Result{}
	}
	return Result{
		Supported:      res.Supported,
		AgentKind:      "claude",
		Text:           res.Text,
		SessionID:      res.SessionID,
		SessionMissing: res.SessionMissing,
		Fingerprint:    res.Fingerprint,
		Unchanged:      res.Unchanged,
		Source:         res.Source,
	}
}

// MaxMessages reads and bounds the client-supplied message limit.
func MaxMessages(params map[string]any) int {
	maxMessages := 200
	if v, ok := params["max_messages"].(float64); ok && v > 0 {
		maxMessages = int(v)
		if maxMessages > 2000 {
			maxMessages = 2000 // bound client-supplied work
		}
	}
	return maxMessages
}

// KnownFingerprint reads the fingerprint the client already holds.
func KnownFingerprint(params map[string]any) string {
	s, _ := params["known_fingerprint"].(string)
	return s
}

// Encode renders res in the wire shape shared by every backend's transcript
// command. Every field name from the pre-opencode shape is kept so an older
// phone build's parser keeps working; agent_kind and session_title are new.
func Encode(res Result) json.RawMessage {
	result, _ := json.Marshal(map[string]any{
		"supported":       res.Supported,
		"agent_kind":      res.AgentKind,
		"text":            res.Text,
		"session_id":      res.SessionID,
		"session_title":   res.SessionTitle,
		"session_missing": res.SessionMissing,
		// Hand back on the next poll as known_fingerprint: an unchanged
		// transcript then answers without re-reading or re-sending it.
		"fingerprint": res.Fingerprint,
		"unchanged":   res.Unchanged,
		"source":      res.Source,
	})
	return result
}
