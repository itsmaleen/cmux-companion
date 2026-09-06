package herdr

import (
	"context"
	"testing"
	"time"

	"github.com/itsmaleen/cmux-companion/bridge/internal/backend"
)

// Screens is a thin wrapper over Frames (via screen.FromFrames); this
// exercises that the wiring actually produces rendered updates end to end,
// leaving the render/coalesce logic itself to package screen's own tests.
func TestScreensRendersFedFrames(t *testing.T) {
	b, _ := backendWithPane(t, "w1:p1", 3, 10)
	b.observeCmd = fakeObserveCmd(t, `
echo '{"type":"terminal.frame","seq":1,"width":10,"height":3,"encoding":"ansi","full":true,"bytes":"G1syShtbMTsxSGhp"}'
echo '{"type":"terminal.closed"}'
`)
	// Bytes above decode to "\x1b[2J\x1b[1;1Hhi" (a plain "hi" repaint).

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, info, err := b.Screens(ctx, "w1:p1", 0, 0)
	if err != nil {
		t.Fatalf("Screens: %v", err)
	}
	if info.Width != 10 || info.Height != 3 {
		t.Fatalf("info = %+v, want native 10x3", info)
	}

	var updates []*backend.ScreenUpdate
	deadline := time.After(2 * time.Second)
loop:
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				break loop
			}
			if ev.Update != nil {
				updates = append(updates, ev.Update)
			} else if ev.Ended != "" {
				if ev.Ended != "closed" {
					t.Fatalf("ended reason = %q, want closed", ev.Ended)
				}
			}
		case <-deadline:
			t.Fatal("timed out waiting for the stream to end")
		}
	}
	if len(updates) == 0 {
		t.Fatal("no screen updates rendered")
	}
	first := updates[0]
	if !first.Full || first.SurfaceID != "w1:p1" {
		t.Fatalf("first update = %+v, want a full update stamped with the surface id", first)
	}
	var sawHi bool
	for _, l := range first.Lines {
		var text string
		for _, r := range l.Runs {
			text += r.T
		}
		if text == "hi" {
			sawHi = true
		}
	}
	if !sawHi {
		t.Fatalf("no rendered line reads \"hi\": %+v", first.Lines)
	}
}

func TestScreensNotFoundWhenPaneMissing(t *testing.T) {
	f := newFakeHerdr(t)
	b := connectedBackend(t, f)

	_, _, err := b.Screens(context.Background(), "w1:missing", 0, 0)
	var berr *backend.Error
	if err == nil || !asBackendError(err, &berr) || berr.Code != "not_found" {
		t.Fatalf("err = %v, want not_found", err)
	}
}
