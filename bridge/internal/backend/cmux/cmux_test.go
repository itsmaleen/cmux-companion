package cmux

import (
	"encoding/json"
	"testing"
)

func TestAnnotateSurfacesLabelsUnboundOpencodeByTTY(t *testing.T) {
	raw := json.RawMessage(`{"surfaces":[
		{"id":"claude","title":"✳ Claude Code","resume_binding":{"kind":"claude","checkpoint_id":"abc"}},
		{"id":"oc","title":"OC | Some session","resume_binding":null},
		{"id":"shell","title":"~/repo"},
		{"id":"liar","title":"OC | not really"}
	]}`)
	terminals := map[string]map[string]any{
		"claude": {"tty": "/dev/ttys001"},
		"oc":     {"tty": "/dev/ttys002"},
		"shell":  {"tty": "/dev/ttys003"},
		"liar":   {"tty": "/dev/ttys004"},
	}
	asked := map[string]bool{}
	terminal := func(id string) map[string]any { asked[id] = true; return terminals[id] }
	runs := func(tty string) bool { return tty == "/dev/ttys002" }

	out := annotateSurfaces(raw, terminal, runs)

	var payload struct {
		Surfaces []map[string]any `json:"surfaces"`
	}
	if err := json.Unmarshal(out, &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	kinds := map[string]string{}
	for _, s := range payload.Surfaces {
		id, _ := s["id"].(string)
		binding, _ := s["resume_binding"].(map[string]any)
		kind, _ := binding["kind"].(string)
		kinds[id] = kind
	}
	if kinds["claude"] != "claude" {
		t.Errorf("claude binding changed: %q", kinds["claude"])
	}
	if kinds["oc"] != "opencode" {
		t.Errorf("opencode surface not labelled: %q", kinds["oc"])
	}
	if kinds["shell"] != "" || kinds["liar"] != "" {
		t.Errorf("surfaces without an opencode process were labelled: shell=%q liar=%q", kinds["shell"], kinds["liar"])
	}
	if asked["claude"] {
		t.Error("terminal table consulted for a surface cmux already bound")
	}
}

func TestAnnotateSurfacesLeavesReplyAloneWhenNothingToLabel(t *testing.T) {
	raw := json.RawMessage(`{"surfaces":[{"id":"shell","title":"~","pixel_frame":{"x":0,"y":0,"width":1512,"height":949}}]}`)
	out := annotateSurfaces(raw, func(string) map[string]any { return map[string]any{"tty": "/dev/ttys009"} }, func(string) bool { return false })
	if string(out) != string(raw) {
		t.Errorf("reply rewritten without any change:\n got %s\nwant %s", out, raw)
	}
	garbage := json.RawMessage(`not json`)
	if got := annotateSurfaces(garbage, nil, nil); string(got) != string(garbage) {
		t.Errorf("unparseable reply not passed through: %s", got)
	}
}
