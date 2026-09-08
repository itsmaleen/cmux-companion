package herdr

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/itsmaleen/cmux-companion/bridge/internal/backend"
)

// fakeControlCmd substitutes the real `herdr terminal session control`
// subprocess with a tiny shell script printing canned NDJSON (and optionally
// capturing stdin), mirroring fakeObserveCmd.
func fakeControlCmd(t *testing.T, script string) func(ctx context.Context, paneID string, cols, rows int) *exec.Cmd {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "control.sh")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatal(err)
	}
	return func(ctx context.Context, paneID string, cols, rows int) *exec.Cmd {
		return exec.CommandContext(ctx, "/bin/sh", path)
	}
}

func TestFitEstablishedByFirstFrame(t *testing.T) {
	b, _ := backendWithPane(t, "w1:p1", 24, 80)
	b.controlCmd = fakeControlCmd(t, `
echo '{"type":"terminal.frame","seq":1,"width":50,"height":15,"encoding":"ansi","full":true,"bytes":"AAA="}'
sleep 30
`)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h, err := b.Fit(ctx, "w1:p1", 50, 15)
	if err != nil {
		t.Fatalf("Fit: %v", err)
	}
	defer h.Release()

	select {
	case <-h.Done():
		t.Fatal("Done closed immediately after establishment; fit should still be running")
	case <-time.After(200 * time.Millisecond):
	}
}

func TestFitStillRunningWithNoFrameCountsAsEstablished(t *testing.T) {
	// herdr may not repaint an idle pane at all: per the design notes, "still
	// running after the establish timeout with no closed record" must also
	// count as established.
	b, _ := backendWithPane(t, "w1:p1", 24, 80)
	b.fitEstablishTimeout = 50 * time.Millisecond
	b.controlCmd = fakeControlCmd(t, `sleep 30`)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h, err := b.Fit(ctx, "w1:p1", 50, 15)
	if err != nil {
		t.Fatalf("Fit: %v", err)
	}
	defer h.Release()

	select {
	case <-h.Done():
		t.Fatal("Done closed unexpectedly; the fit should still be running")
	case <-time.After(100 * time.Millisecond):
	}
}

func TestFitClosedBeforeEstablishmentIsFitError(t *testing.T) {
	b, _ := backendWithPane(t, "w1:p1", 24, 80)
	b.controlCmd = fakeControlCmd(t, `
echo '{"type":"terminal.closed","reason":"terminal session control failed: terminal target w2:p7 not found"}'
`)

	_, err := b.Fit(context.Background(), "w1:p1", 50, 15)
	var berr *backend.Error
	if err == nil || !asBackendError(err, &berr) || berr.Code != "fit_error" {
		t.Fatalf("err = %v, want fit_error", err)
	}
	if !strings.Contains(berr.Message, "terminal target w2:p7 not found") {
		t.Fatalf("message = %q, want herdr's own reason included", berr.Message)
	}
}

func TestFitEOFBeforeEstablishmentIsFitError(t *testing.T) {
	b, _ := backendWithPane(t, "w1:p1", 24, 80)
	b.controlCmd = fakeControlCmd(t, `true`) // exits immediately, no output at all

	_, err := b.Fit(context.Background(), "w1:p1", 50, 15)
	var berr *backend.Error
	if err == nil || !asBackendError(err, &berr) || berr.Code != "fit_error" {
		t.Fatalf("err = %v, want fit_error", err)
	}
}

func TestFitResizeWritesStdinLine(t *testing.T) {
	b, _ := backendWithPane(t, "w1:p1", 24, 80)
	stdinCapture := filepath.Join(t.TempDir(), "stdin.txt")
	b.controlCmd = fakeControlCmd(t, `
echo '{"type":"terminal.frame","seq":1,"width":80,"height":24,"encoding":"ansi","full":true,"bytes":"AAA="}'
cat > `+stdinCapture+`
`)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h, err := b.Fit(ctx, "w1:p1", 80, 24)
	if err != nil {
		t.Fatalf("Fit: %v", err)
	}
	defer h.Release()

	if err := h.Resize(50, 15); err != nil {
		t.Fatalf("Resize: %v", err)
	}

	var data []byte
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		data, _ = os.ReadFile(stdinCapture)
		if len(data) > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	got := string(data)
	if !strings.Contains(got, `"type":"terminal.resize"`) || !strings.Contains(got, `"cols":50`) || !strings.Contains(got, `"rows":15`) {
		t.Fatalf("stdin capture = %q, want a terminal.resize line with cols 50 rows 15", got)
	}
}

func TestFitReleaseKillsProcessWithoutEndingViaDone(t *testing.T) {
	b, _ := backendWithPane(t, "w1:p1", 24, 80)
	b.controlCmd = fakeControlCmd(t, `
echo '{"type":"terminal.frame","seq":1,"width":80,"height":24,"encoding":"ansi","full":true,"bytes":"AAA="}'
sleep 30
`)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h, err := b.Fit(ctx, "w1:p1", 80, 24)
	if err != nil {
		t.Fatalf("Fit: %v", err)
	}

	h.Release()

	// Done closes once the process is reaped, but with an empty Reason: a
	// caller must be able to tell "I released this" apart from "this ended on
	// its own" purely from Reason, since Done itself fires in both cases (see
	// backend.FitHandle.Done's doc).
	select {
	case <-h.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("Done did not close after Release; subprocess likely not killed")
	}
	if reason := h.Reason(); reason != "" {
		t.Fatalf("Reason() = %q after an explicit Release, want empty", reason)
	}
}

func TestFitEndsOnItsOwnReportsClosedReason(t *testing.T) {
	b, _ := backendWithPane(t, "w1:p1", 24, 80)
	b.controlCmd = fakeControlCmd(t, `
echo '{"type":"terminal.frame","seq":1,"width":80,"height":24,"encoding":"ansi","full":true,"bytes":"AAA="}'
sleep 0.1
echo '{"type":"terminal.closed","reason":"pane closed"}'
`)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h, err := b.Fit(ctx, "w1:p1", 80, 24)
	if err != nil {
		t.Fatalf("Fit: %v", err)
	}
	defer h.Release()

	select {
	case <-h.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("Done did not close after the controller reported terminal.closed")
	}
	if reason := h.Reason(); reason != "closed" {
		t.Fatalf("Reason() = %q, want closed", reason)
	}
}

func TestFitEndsOnItsOwnWhenProcessDiesUnexpectedly(t *testing.T) {
	b, _ := backendWithPane(t, "w1:p1", 24, 80)
	b.controlCmd = fakeControlCmd(t, `
echo '{"type":"terminal.frame","seq":1,"width":80,"height":24,"encoding":"ansi","full":true,"bytes":"AAA="}'
`) // exits right after the frame, with no terminal.closed record

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h, err := b.Fit(ctx, "w1:p1", 80, 24)
	if err != nil {
		t.Fatalf("Fit: %v", err)
	}
	defer h.Release()

	select {
	case <-h.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("Done did not close after the controller exited unexpectedly")
	}
	if reason := h.Reason(); reason != "error" {
		t.Fatalf("Reason() = %q, want error", reason)
	}
}

func TestFitNotFoundWhenPaneMissing(t *testing.T) {
	f := newFakeHerdr(t) // no pane.get canned: fakeHerdr answers unknown_method
	b := connectedBackend(t, f)

	_, err := b.Fit(context.Background(), "w1:missing", 80, 24)
	var berr *backend.Error
	if err == nil || !asBackendError(err, &berr) || berr.Code != "not_found" {
		t.Fatalf("err = %v, want not_found", err)
	}
}
