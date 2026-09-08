package herdr

import (
	"context"
	"encoding/base64"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/itsmaleen/cmux-companion/bridge/internal/backend"
)

// fakeObserveCmd substitutes the real `herdr terminal session observe`
// subprocess with a tiny shell script printing canned NDJSON, so Frames can
// be exercised without a real herdr daemon.
func fakeObserveCmd(t *testing.T, script string) func(ctx context.Context, paneID string, cols, rows int) *exec.Cmd {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "observe.sh")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatal(err)
	}
	return func(ctx context.Context, paneID string, cols, rows int) *exec.Cmd {
		return exec.CommandContext(ctx, "/bin/sh", path)
	}
}

// backendWithPane builds a connected herdr backend whose fake socket answers
// pane.get and session.snapshot for paneID, so Frames' native-size lookup
// (and its existence check) succeeds.
func backendWithPane(t *testing.T, paneID string, viewportRows, rectWidth int) (*Backend, *fakeHerdr) {
	t.Helper()
	f := newFakeHerdr(t)
	pane := map[string]any{
		"pane_id": paneID, "workspace_id": "w1",
		"scroll": map[string]any{"viewport_rows": float64(viewportRows)},
	}
	f.set("pane.get", map[string]any{"pane": pane})
	f.set("session.snapshot", map[string]any{"snapshot": map[string]any{
		"layouts": []any{
			map[string]any{
				"workspace_id": "w1", "tab_id": "w1:t1",
				"panes": []any{
					map[string]any{"pane_id": paneID, "rect": map[string]any{"width": float64(rectWidth), "height": float64(viewportRows)}},
				},
			},
		},
	}})
	b := connectedBackend(t, f)
	return b, f
}

func drainFrameEvents(t *testing.T, ch <-chan backend.FrameEvent, n int, timeout time.Duration) []backend.FrameEvent {
	t.Helper()
	var out []backend.FrameEvent
	for i := 0; i < n; i++ {
		select {
		case ev, ok := <-ch:
			if !ok {
				t.Fatalf("channel closed early after %d events, want %d", len(out), n)
			}
			out = append(out, ev)
		case <-time.After(timeout):
			t.Fatalf("timed out waiting for event %d/%d", i+1, n)
		}
	}
	return out
}

func TestFramesFullDeltaClosed(t *testing.T) {
	b, _ := backendWithPane(t, "w1:p1", 24, 100)
	b.observeCmd = fakeObserveCmd(t, `
echo '{"type":"terminal.frame","seq":1,"width":100,"height":24,"encoding":"ansi","full":true,"bytes":"AAA="}'
echo '{"type":"terminal.frame","seq":2,"width":100,"height":24,"encoding":"ansi","full":false,"bytes":"BBB="}'
echo '{"type":"terminal.closed"}'
`)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, info, err := b.Frames(ctx, "w1:p1", 0, 0)
	if err != nil {
		t.Fatalf("Frames: %v", err)
	}
	if info.Width != 100 || info.Height != 24 {
		t.Fatalf("info = %+v, want native 100x24 from layout rect + viewport", info)
	}

	evs := drainFrameEvents(t, ch, 3, 2*time.Second)

	if evs[0].Frame == nil || !evs[0].Frame.Full || evs[0].Frame.Seq != 1 || evs[0].Frame.Bytes != "AAA=" {
		t.Fatalf("frame 1 = %+v", evs[0])
	}
	if evs[1].Frame == nil || evs[1].Frame.Full || evs[1].Frame.Seq != 2 || evs[1].Frame.Bytes != "BBB=" {
		t.Fatalf("frame 2 = %+v", evs[1])
	}
	if evs[2].Frame != nil || evs[2].Ended != "closed" {
		t.Fatalf("terminal.closed should end the stream with reason closed, got %+v", evs[2])
	}
	if _, ok := <-ch; ok {
		t.Fatal("channel should be closed after the ended event")
	}
}

func TestFramesLargeLine(t *testing.T) {
	// A base64 payload north of the 64KiB scanner start-buffer, comfortably
	// under the 4MiB cap, exercising bufio.Scanner's growth path.
	big := base64.StdEncoding.EncodeToString(make([]byte, 100*1024))
	line := `{"type":"terminal.frame","seq":1,"width":80,"height":24,"encoding":"ansi","full":true,"bytes":"` + big + `"}`

	b, _ := backendWithPane(t, "w1:p1", 24, 80)
	b.observeCmd = fakeObserveCmd(t, "cat <<'EOF'\n"+line+"\nEOF\n")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, _, err := b.Frames(ctx, "w1:p1", 0, 0)
	if err != nil {
		t.Fatalf("Frames: %v", err)
	}
	evs := drainFrameEvents(t, ch, 1, 2*time.Second)
	if evs[0].Frame == nil || evs[0].Frame.Bytes != big {
		t.Fatalf("large frame bytes length = %d, want %d", len(evs[0].Frame.Bytes), len(big))
	}
}

func TestFramesCtxCancelKillsSubprocess(t *testing.T) {
	b, _ := backendWithPane(t, "w1:p1", 24, 80)
	b.observeCmd = fakeObserveCmd(t, `
echo '{"type":"terminal.frame","seq":1,"width":80,"height":24,"encoding":"ansi","full":true,"bytes":"AAA="}'
sleep 30
`)

	ctx, cancel := context.WithCancel(context.Background())
	ch, _, err := b.Frames(ctx, "w1:p1", 0, 0)
	if err != nil {
		t.Fatalf("Frames: %v", err)
	}
	drainFrameEvents(t, ch, 1, 2*time.Second)

	cancel()
	// If the subprocess weren't killed, its 30s sleep would keep the pipe
	// open and this channel would never close within the test timeout.
	select {
	case ev, ok := <-ch:
		if ok {
			t.Fatalf("expected the channel to close on cancel with no further event, got %+v", ev)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("channel did not close after ctx cancel; subprocess likely not killed")
	}
}

func TestFramesBackpressureClosesStream(t *testing.T) {
	// Emit well more than frameChannelCap frames rapidly so the channel fills
	// while the test deliberately doesn't read.
	var script strings.Builder
	for i := 0; i < frameChannelCap*3; i++ {
		script.WriteString(`echo '{"type":"terminal.frame","seq":1,"width":80,"height":24,"encoding":"ansi","full":false,"bytes":"AAA="}'` + "\n")
	}

	b, _ := backendWithPane(t, "w1:p1", 24, 80)
	b.observeCmd = fakeObserveCmd(t, script.String())
	// Per-Backend, not a package global: no risk of racing another test's
	// in-flight pump goroutine over a shared var.
	b.frameBackpressureTimeout = 30 * time.Millisecond
	b.finalFrameEventTimeout = 2 * time.Second

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, _, err := b.Frames(ctx, "w1:p1", 0, 0)
	if err != nil {
		t.Fatalf("Frames: %v", err)
	}

	// Let the channel fill and the backpressure timeout trip before reading
	// anything at all.
	time.Sleep(300 * time.Millisecond)

	var sawEnded bool
	var endedReason string
	deadline := time.After(5 * time.Second)
drain:
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				break drain
			}
			if ev.Frame == nil {
				sawEnded = true
				endedReason = ev.Ended
			}
		case <-deadline:
			t.Fatal("timed out draining after backpressure")
		}
	}
	if !sawEnded || endedReason != "backpressure" {
		t.Fatalf("sawEnded=%v reason=%q, want an Ended event with reason backpressure", sawEnded, endedReason)
	}
}

func TestFramesNotFoundWhenPaneMissing(t *testing.T) {
	f := newFakeHerdr(t) // no pane.get canned: fakeHerdr answers unknown_method
	b := connectedBackend(t, f)

	_, _, err := b.Frames(context.Background(), "w1:missing", 0, 0)
	var berr *backend.Error
	if err == nil || !asBackendError(err, &berr) || berr.Code != "not_found" {
		t.Fatalf("err = %v, want not_found", err)
	}
}
