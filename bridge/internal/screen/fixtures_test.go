package screen

import (
	"encoding/json"
	"os"
	"testing"
)

// TestWriteScreenFixtures regenerates the phone's screenshot-mode fixtures
// (ios/cmux/Fixtures/screen-shell.json, screen-claude.json) from the raw
// frame fixtures already checked in there. It only runs when
// WRITE_SCREEN_FIXTURES=1 is set — a normal `go test` run never touches these
// files — so this is invoked once by hand and the resulting JSON committed.
func TestWriteScreenFixtures(t *testing.T) {
	if os.Getenv("WRITE_SCREEN_FIXTURES") != "1" {
		t.Skip("set WRITE_SCREEN_FIXTURES=1 to (re)generate ios/cmux/Fixtures/screen-*.json")
	}

	writeScreenFixture(t, "frames-shell.ndjson", "screen-shell.json", "herdr:w2:p7")
	writeScreenFixture(t, "frames-claude.ndjson", "screen-claude.json", "herdr:w1:p1")
}

// writeScreenFixture feeds inputName's single full frame through a Renderer
// at the frame's own width/height and writes the resulting `surface.screen`
// data object — stamped with surfaceID, the way screen.FromFrames stamps a
// live stream's updates — to ios/cmux/Fixtures/outputName.
func writeScreenFixture(t *testing.T, inputName, outputName, surfaceID string) {
	t.Helper()
	f := loadFixture(t, inputName)
	r := New(f.Width, f.Height)
	if err := r.Feed(f); err != nil {
		t.Fatalf("%s: Feed: %v", inputName, err)
	}
	update := r.Update(true)
	update.SurfaceID = surfaceID

	out, err := json.MarshalIndent(update, "", "  ")
	if err != nil {
		t.Fatalf("%s: marshal: %v", inputName, err)
	}
	out = append(out, '\n')

	path := "../../../ios/cmux/Fixtures/" + outputName
	if err := os.WriteFile(path, out, 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	t.Logf("wrote %s (%d bytes, %d lines)", path, len(out), len(update.Lines))
}
