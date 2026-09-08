package transcripts

import (
	"os/exec"
	"testing"
)

// TestBoundOpencodeSessionMissing checks that a resume_binding naming an
// opencode session that is not in the database reports session_missing rather
// than rendering an empty existing session.
func TestBoundOpencodeSessionMissing(t *testing.T) {
	if _, err := exec.LookPath("sqlite3"); err != nil {
		t.Skip("sqlite3 not available")
	}
	d := New()
	if !d.opencode.Available() {
		t.Skip("no opencode database on this machine")
	}
	res, err := d.Render(Request{
		SurfaceID:     "s1",
		ResumeBinding: map[string]any{"kind": "opencode", "checkpoint_id": "ses_definitelyNotARealSession999"},
		MaxMessages:   50,
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !res.Supported || res.AgentKind != "opencode" {
		t.Fatalf("want supported opencode, got supported=%v kind=%q", res.Supported, res.AgentKind)
	}
	if !res.SessionMissing {
		t.Fatalf("a bound-but-absent session should report SessionMissing; got missing=%v text=%q", res.SessionMissing, res.Text)
	}
}
