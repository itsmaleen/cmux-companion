package claude

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/itsmaleen/cmux-companion/bridge/internal/transcriptrender"
)

// sessionIDPattern matches a Claude session UUID. checkpoint_id comes from
// cmux's surface metadata and is interpolated into a filesystem glob, so it is
// validated to this shape first — otherwise `../` or glob metacharacters
// (`*`, `?`, `[`) could escape ~/.claude/projects or match unintended files.
var sessionIDPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// maxTranscriptFileBytes bounds how much of a transcript file is read into
// memory. Claude session files grow forward, so for a larger file only the
// tail (most recent conversation) is read.
const maxTranscriptFileBytes = 32 * 1024 * 1024

// cacheEntry holds an already-rendered transcript with its file identity.
type cacheEntry struct {
	modTime time.Time
	size    int64
	text    string
}

// maxCachedRenderings bounds the rendered-transcript cache. Entries are keyed
// by path AND message bound, both client-influenced, and each holds up to
// maxRenderBytes — so an unbounded map lets a client pin hundreds of megabytes
// by walking max_messages, and ordinary session churn retains renderings of
// transcripts that were deleted long ago. A handful of entries is all the cache
// was ever for: the same surface polling the same transcript.
const maxCachedRenderings = 8

// renderCache is a small insertion-ordered cache with a hard entry cap.
type renderCache struct {
	mu      sync.Mutex
	entries map[string]*cacheEntry
	order   []string // oldest first
}

func (c *renderCache) get(key string) (*cacheEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[key]
	return entry, ok
}

func (c *renderCache) put(key string, entry *cacheEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = make(map[string]*cacheEntry, maxCachedRenderings)
	}
	if _, exists := c.entries[key]; !exists {
		c.order = append(c.order, key)
		for len(c.order) > maxCachedRenderings {
			delete(c.entries, c.order[0])
			c.order = c.order[1:]
		}
	}
	c.entries[key] = entry
}

// Resolver locates and renders Claude session transcripts.
type Resolver struct {
	home  string
	hooks *hookStore
	cache renderCache
}

// NewResolver creates a Resolver that resolves $HOME via os.UserHomeDir.
func NewResolver() *Resolver {
	home, _ := os.UserHomeDir()
	return newResolver(home)
}

func newResolver(home string) *Resolver {
	return &Resolver{home: home, hooks: newHookStore(home)}
}

// Request describes one transcript read.
type Request struct {
	// SurfaceID is the cmux surface asking. It is the key into cmux's hook
	// session store, which is the reliable surface → transcript binding.
	SurfaceID string
	// ResumeBinding is the surface's cmux resume_binding, which says whether
	// this is a Claude surface at all and names its session.
	ResumeBinding map[string]any
	// MaxMessages bounds how many trailing messages are rendered.
	MaxMessages int
	// KnownFingerprint is the Fingerprint of the text the caller already holds.
	// When it still matches, the file is neither read nor rendered and the
	// result comes back Unchanged with no Text — which is what makes polling
	// this RPC on every refresh cycle cheap.
	KnownFingerprint string
}

// Result is the outcome of resolving and rendering a surface's transcript.
type Result struct {
	// Supported reports whether the binding describes a Claude surface at all.
	Supported bool
	// SessionID is the session the transcript was read from, or — when the
	// bound session's file is gone — the session the surface points at.
	SessionID string
	// Text is the rendered transcript, empty when no file was found or when
	// Unchanged is set.
	Text string
	// SessionMissing reports that the surface names a specific session whose
	// transcript file is not on disk. Distinguishes "this conversation's file
	// is gone" from "this surface hasn't said anything yet", which otherwise
	// look identical (both empty Text) to the client.
	SessionMissing bool
	// Fingerprint identifies this exact rendering (file identity + message
	// bound). Hand it back as Request.KnownFingerprint to skip re-sending
	// unchanged text.
	Fingerprint string
	// Unchanged reports that the transcript still matches KnownFingerprint, so
	// Text was deliberately not produced.
	Unchanged bool
	// Source names the strategy that resolved the file, for diagnosing a
	// surface that shows the wrong conversation. See resolveTranscriptPath.
	Source string
}

// Render returns the rendered transcript for a surface.
func (r *Resolver) Render(req Request) (Result, error) {
	if req.ResumeBinding == nil {
		return Result{}, nil
	}
	kind, _ := req.ResumeBinding["kind"].(string)
	if kind != "claude" {
		return Result{}, nil
	}

	res := Result{Supported: true}

	transcriptPath, boundSession, source, err := r.resolveTranscriptPath(req)
	res.Source = source
	if err != nil {
		res.SessionID = boundSession
		return res, err
	}
	if transcriptPath == "" {
		// Claude surface, but nothing to read: either the surface has no
		// session yet, or it names one whose file has since disappeared.
		res.SessionID = boundSession
		res.SessionMissing = boundSession != ""
		return res, nil
	}

	res.SessionID = strings.TrimSuffix(filepath.Base(transcriptPath), ".jsonl")

	info, err := os.Stat(transcriptPath)
	if err != nil {
		if os.IsNotExist(err) {
			// Deleted between resolution and read (session cleanup racing the
			// request) — same outcome as never resolving it: report the
			// session missing, not an opaque transcript_error.
			res.SessionMissing = boundSession != ""
			return res, nil
		}
		return res, err
	}

	res.Fingerprint = fingerprint(transcriptPath, info, req.MaxMessages)
	if req.KnownFingerprint != "" && req.KnownFingerprint == res.Fingerprint {
		res.Unchanged = true
		return res, nil
	}

	// Check cache. The rendering depends on the message bound as well as the
	// file, so that is part of the key — two clients asking for different
	// depths must not be served each other's text.
	cacheKey := fmt.Sprintf("%s\x00%d", transcriptPath, req.MaxMessages)
	if entry, ok := r.cache.get(cacheKey); ok {
		if entry.modTime.Equal(info.ModTime()) && entry.size == info.Size() {
			res.Text = entry.text
			return res, nil
		}
	}

	data, err := readTail(transcriptPath, info.Size(), maxTranscriptFileBytes)
	if err != nil {
		if os.IsNotExist(err) {
			res.SessionMissing = boundSession != ""
			return res, nil
		}
		return res, err
	}

	rendered := renderJSONL(data, req.MaxMessages)
	r.cache.put(cacheKey, &cacheEntry{
		modTime: info.ModTime(),
		size:    info.Size(),
		text:    rendered,
	})
	res.Text = rendered
	return res, nil
}

// resolveTranscriptPath returns the transcript .jsonl file for a surface, or ""
// if none is found. boundSession is the well-formed session id the surface is
// bound to, whether or not its file exists; source names the strategy that won.
//
// Every strategy binds to the surface's SPECIFIC session. Many surfaces share
// one cwd (cmux worktrees), so "newest file in the project dir" would collapse
// them all onto the same transcript, and is only ever a last resort for a
// surface that names no session at all.
//
//  1. hook_store_session — the session the surface's checkpoint_id names,
//     looked up in cmux's hook store, which recorded the path Claude Code
//     itself reported. Best available: the path is recorded, never derived, so
//     it holds for a relocated CLAUDE_CONFIG_DIR or a nested workflow session
//     that no glob under ~/.claude/projects finds.
//  2. projects_glob — derive <checkpoint_id>.jsonl under ~/.claude/projects.
//     Covers a Claude session cmux's hooks never recorded.
//  3. hook_store_surface — the session the store says this surface is running
//     right now, when its checkpoint resolved to nothing.
//  4. hook_store_surface_recent — the newest session recorded for this surface,
//     for a surface whose resume_binding names none at all.
//  5. cwd_latest — nothing names a session anywhere; newest transcript for the cwd.
//
// The surface-keyed strategies rank BELOW the checkpoint ones deliberately. A
// surface's store record outlives the session that wrote it (SessionEnd clears
// the active pointer, the record stays), so a surface that has since started a
// new session would otherwise be served the old conversation indefinitely.
func (r *Resolver) resolveTranscriptPath(req Request) (path string, boundSession string, source string, err error) {
	projectsDir := filepath.Join(r.home, ".claude", "projects")

	if checkpointID, _ := req.ResumeBinding["checkpoint_id"].(string); checkpointID != "" {
		// Only accept a well-formed session UUID — it's interpolated into a
		// glob, so `../` or `*?[` in a malformed value must never reach the
		// filesystem.
		if sessionIDPattern.MatchString(checkpointID) {
			boundSession = checkpointID

			// Strategy 1: that session's recorded transcript path.
			if rec, ok := r.hooks.bySession(checkpointID); ok {
				if resolved, ok := storedTranscript(rec.TranscriptPath, rec.SessionID); ok {
					return resolved, boundSession, "hook_store_session", nil
				}
			}

			// Strategy 2: derive the path under ~/.claude/projects.
			match, err := findTranscriptByID(projectsDir, checkpointID)
			if err != nil {
				return "", boundSession, "", err
			}
			if match != "" {
				return match, boundSession, "projects_glob", nil
			}

			// Strategy 3: the session this surface is running right now. The
			// checkpoint named a session with nothing on disk, so prefer the
			// live conversation over reporting nothing — but only from the
			// store's authoritative pointer, never the historical scan.
			if rec, ok := r.hooks.activeBySurface(req.SurfaceID); ok {
				if resolved, ok := storedTranscript(rec.TranscriptPath, rec.SessionID); ok {
					return resolved, rec.SessionID, "hook_store_surface", nil
				}
			}
		}
		// The surface names a session and that file is not on disk. Stop here
		// rather than falling through to the cwd heuristic below: sessions can
		// outlive their files (cmux keeps a checkpoint_id pointed at a session
		// that was never written, or was since removed), and with many surfaces
		// sharing one cwd the fallback confidently returns a DIFFERENT
		// surface's conversation. An empty history beats someone else's.
		return "", boundSession, "", nil
	}

	// Strategy 4: no checkpoint at all — the newest session recorded for this
	// surface. Historical, but still bound to THIS surface.
	if rec, ok := r.hooks.recentBySurface(req.SurfaceID); ok {
		if sessionIDPattern.MatchString(rec.SessionID) {
			boundSession = rec.SessionID
		}
		if resolved, ok := storedTranscript(rec.TranscriptPath, rec.SessionID); ok {
			return resolved, boundSession, "hook_store_surface_recent", nil
		}
	}

	// A surface the hook store knows but whose recorded file is gone: report it
	// missing rather than guessing from the cwd, for the same reason.
	if boundSession != "" {
		return "", boundSession, "", nil
	}

	// Strategy 5 (last resort): no session named anywhere — encode the cwd and
	// take the most-recently-modified session in that project dir.
	if cwd, _ := req.ResumeBinding["cwd"].(string); cwd != "" {
		dir := filepath.Join(projectsDir, encodeCWD(cwd))
		if _, err := os.Stat(dir); err == nil {
			path, err := latestJSONL(dir)
			if path == "" || err != nil {
				return "", "", "", err
			}
			return path, "", "cwd_latest", nil
		}
	}

	return "", "", "", nil
}

// maxProjectsGlobDepth is how many directory levels below ~/.claude/projects a
// transcript is looked for. Sessions normally sit one level down (the encoded
// cwd), but Claude nests some of them (a workflow session lands in a directory
// named for its parent session), so a single level misses those entirely.
const maxProjectsGlobDepth = 5

// findTranscriptByID looks for <sessionID>.jsonl anywhere under projectsDir, up
// to maxProjectsGlobDepth levels deep. sessionID must already be validated as a
// UUID — it is interpolated into the glob pattern.
func findTranscriptByID(projectsDir, sessionID string) (string, error) {
	wildcards := ""
	for depth := 1; depth <= maxProjectsGlobDepth; depth++ {
		wildcards = filepath.Join(wildcards, "*")
		matches, err := filepath.Glob(filepath.Join(projectsDir, wildcards, sessionID+".jsonl"))
		if err != nil {
			return "", err
		}
		for _, match := range matches {
			// Defense in depth: the match must resolve inside projectsDir.
			if isWithin(projectsDir, match) {
				return match, nil
			}
		}
	}
	return "", nil
}

// fingerprint identifies one rendering of a transcript: the file's identity
// plus the message bound that shaped the text. The path is hashed rather than
// echoed so the client never receives local filesystem layout it has no use for.
func fingerprint(path string, info os.FileInfo, maxMessages int) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%d\x00%d\x00%d",
		path, info.ModTime().UnixNano(), info.Size(), maxMessages)))
	return hex.EncodeToString(sum[:8])
}

// readTail reads at most maxBytes from the end of the file at path. Claude
// session files grow forward, so the tail is the most recent conversation and
// this bounds memory for very large sessions. A partial first line (from
// cutting mid-line) is harmless — renderJSONL skips unparseable lines.
func readTail(path string, size, maxBytes int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if size > maxBytes {
		if _, err := f.Seek(size-maxBytes, io.SeekStart); err != nil {
			return nil, err
		}
	}
	// LimitReader, not ReadFile: the file is being appended to live, so the
	// size this decision was made on is already stale and an unbounded read
	// would follow the writer past maxBytes.
	return io.ReadAll(io.LimitReader(f, maxBytes))
}

// isWithin reports whether path is inside dir (or equal to it).
func isWithin(dir, path string) bool {
	rel, err := filepath.Rel(dir, filepath.Clean(path))
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// encodeCWD converts an absolute cwd path to the Claude project directory name.
// e.g. "/Users/marlin/wrinkles" → "-Users-marlin-wrinkles"
func encodeCWD(cwd string) string {
	stripped := strings.TrimPrefix(cwd, "/")
	replacer := strings.NewReplacer("/", "-", ".", "-", "_", "-")
	return "-" + replacer.Replace(stripped)
}

// latestJSONL returns the most-recently-modified *.jsonl file in dir, or "".
func latestJSONL(dir string) (string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}

	var bestPath string
	var bestMod time.Time

	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		full := filepath.Join(dir, e.Name())
		info, err := e.Info()
		if err != nil {
			continue
		}
		if bestPath == "" || info.ModTime().After(bestMod) {
			bestPath = full
			bestMod = info.ModTime()
		}
	}
	return bestPath, nil
}

// ---- Pure rendering helpers (exported for tests) ----------------------------

// jsonlLine is the top-level structure of each JSONL event line.
type jsonlLine struct {
	Type    string `json:"type"`
	Message struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

// renderJSONL parses raw JSONL data and returns the rendered transcript.
// Only the last maxMessages user/assistant messages are included, and the
// result is byte-capped by the shared renderer.
func renderJSONL(data []byte, maxMessages int) string {
	var messages []transcriptrender.Message
	for _, rawLine := range strings.Split(string(data), "\n") {
		rawLine = strings.TrimSpace(rawLine)
		if rawLine == "" {
			continue
		}
		var line jsonlLine
		if err := json.Unmarshal([]byte(rawLine), &line); err != nil {
			continue
		}
		if line.Type != "user" && line.Type != "assistant" {
			continue
		}
		text := extractContent(line.Message.Content)
		if strings.TrimSpace(text) == "" {
			continue
		}
		// Role comes from the validated line.Type, not message.role, which can
		// be absent or unexpected on some events.
		messages = append(messages, transcriptrender.Message{
			IsUser: line.Type == "user",
			Text:   text,
		})
	}
	return transcriptrender.Render(messages, "Claude", maxMessages)
}

// extractContent converts a raw JSON content field (string or block array)
// into plain text.
func extractContent(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}

	// Try string first.
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}

	// Try array of blocks.
	var blocks []map[string]any
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return ""
	}

	var parts []string
	for _, block := range blocks {
		blockType, _ := block["type"].(string)
		switch blockType {
		case "text":
			if t, _ := block["text"].(string); t != "" {
				parts = append(parts, t)
			}
		case "tool_use":
			if name, _ := block["name"].(string); name != "" {
				input, _ := block["input"].(map[string]any)
				parts = append(parts, transcriptrender.ToolLine(name, input))
			}
			// thinking, tool_result, and anything else: skip
		}
	}
	return strings.Join(parts, "\n")
}
