# Responsive sessions on the phone (and why not mosh)

Investigation date: 2026-09-05. herdr 0.8.2 (protocol 20) and cmux 0.64.22
probed live on this Mac; mosh, Moshi and library facts from public sources.

## TL;DR

- **Do not implement mosh.** It is a transport (UDP + SSH bootstrap, GPLv3
  C++ client, no Swift/Go implementation) that solves roaming and lossy
  links. It does not solve rendering, and its architecture is the reason it
  has *no scrollback at all* (mosh issue #2, open since 2011). The Moshi app
  gets its scrollback from herdr/tmux, not from mosh.
- **Steal mosh's one good idea**: the server owns the terminal state and the
  client receives rendered screen diffs at a controlled frame rate. herdr
  already produces exactly that stream (`herdr terminal session observe`),
  verified live below. The bridge should relay it over the WebSocket we
  already have, and the phone should render it with a real terminal
  emulator (SwiftTerm, MIT).
- **History for alt-screen agents comes from the agent, not the terminal.**
  Claude Code and opencode repaint a fixed viewport; there is nothing to
  scroll back into. Claude history already comes from the JSONL transcript;
  opencode history comes from its SQLite DB (branch
  `worktree-opencode-sessions`, being ported onto the Backend interface).
- **Scrolling must never jump.** The card replaces its whole text every poll
  and only anchors one case (history prepended). Fix on the phone: a pure
  anchoring rule (append → keep offset, prepend → shift, replace → keep
  distance from bottom, following → bottom).

## mosh: what it is and why it is the wrong tool here

| Question | Answer | Source |
|---|---|---|
| Transport | SSH to bootstrap and authenticate, then `mosh-server` picks a UDP port in 60000–61000; all traffic is AES-OCB UDP under the State Synchronization Protocol (SSP). | mosh.org, USENIX ATC'12 paper |
| Benefits | Roaming across networks/sleep (server re-targets to the last authenticated source), tolerates loss, predictive local echo, server-side terminal emulator so the wire carries screen-state diffs at a controlled frame rate. | paper |
| Scrollback | **None, by design.** The client only ever holds the current screen-state object. Issue #2 has been open since Oct 2011; 1.4 (2022) added 24-bit colour and OSC 52, not scrollback. | github.com/mobile-shell/mosh/issues/2, mosh-1.4.0 release notes |
| Clients | Blink Shell embeds a C++ `libmoshios` (GPLv3). No pure-Swift or pure-Go client exists; `thyth/go-mosh` is a SWIG wrapper over the GPL C++ code. | blinksh/blink, thyth/go-mosh |
| Fit with merry | Would replace the whole pairing/bridge/Tailscale model with SSH keys + open UDP ports, drag GPLv3 into the app, and still leave the phone with a plain-text-only picture of a pane and no history. | — |

What we keep from the idea: **server-authoritative screen state + diff
stream + a real emulator on the client**. Our reliable WebSocket removes the
need for SSP; reconnect is handled by re-attaching, which yields a fresh
`full:true` frame.

## What the Moshi app actually does

Moshi (Moshi Tech Ltd, independent of herdr) is an SSH/mosh terminal client
with a session switcher for tmux windows / herdr tabs, on-device Whisper, and
a small in-app scrollback buffer. Its own docs say mosh transmits only the
visible screen; host history comes from tmux copy-mode or herdr's scrollback,
with touch-scroll forwarded as mouse-wheel events. "Responsive" there means
the whole herdr/tmux TUI is drawn by the phone's emulator at the phone's
size. We can match the experience without mosh: herdr's observer renders a
pane into any grid we ask for.

## Verified facts (live, 2026-09-05)

### herdr 0.8.2

- `herdr terminal session observe <pane_id> [--cols N --rows N]` streams
  NDJSON `{"type":"terminal.frame","seq":1,"width":W,"height":H,
  "encoding":"ansi","full":true,"bytes":"<base64 PTY bytes>"}` — a full
  repaint first, then `full:false` deltas (~100 bytes each while an agent
  animates its spinner, a few KB on a real repaint). Frames are bracketed
  with synchronized-output (`?2026h/l`) so an emulator can apply them
  atomically. Observed on an idle shell (one frame) and on a working Claude
  pane (10 frames in 4 s).
- **Observers do not disturb the Mac.** Observing `w2:p7` at 40×12 and at
  200×50 left its layout rect at 85×21 throughout, and its PTY was not
  resized. The observer *crops/pads* the screen model to the requested grid
  — it does not reflow. With no size given the frame is 120×40 regardless of
  the pane's size. ⇒ Observe at the pane's **native** size (cols from the
  layout rect width, rows from `scroll.viewport_rows`) and let the phone
  scale the font / pan; requesting a phone-sized grid would truncate lines.
- `terminal session control <pane> --cols --rows` is the write side
  (`terminal.input`, `terminal.resize`, `terminal.scroll`, `terminal.release`;
  single controller unless `--takeover`). It resizes the real PTY. Not
  exercised — that is the tmux "smallest client wins" tradeoff and would
  visibly change the Mac. Keep it behind an explicit "take over size" toggle
  if ever offered.
- `pane.read {source, lines, format:"ansi"|"text"}` returns real SGR colour
  when asked; today the bridge hardcodes `strip_ansi:true`. Plain-shell
  scrollback via `source:"recent"` is safe; deep reads on *idle agent* panes
  scroll the user's live pane (already clamped in `translate.go`).
- `pane.scroll_changed` is subscribable per pane and pushes
  `{offset_from_bottom, max_offset_from_bottom, viewport_rows}`.
- No output-changed event is subscribable (`pane_output_changed` closes the
  connection). No cursor field anywhere in the schema.
- herdr already reports `agent: "opencode"` with
  `agent_session.value: "ses_…"` for opencode panes (and Claude session ids
  for Claude panes) — session binding for both agents is free on herdr.

### cmux 0.64.22

- Socket API for content is only `surface.read_text {surface_id, lines,
  scrollback}` (CLI aliases `read-screen`, `capture-pane`). `format:"ansi"`
  is silently ignored; the text is plain. Real scrollback comes back for
  plain shells; alt-screen TUIs return roughly the visible screen.
- No `surface.resize`, no cursor, no screen/frame stream. `cmux events` is
  the app's event bus (notifications, feed, hooks), not terminal output.
- `cmux hooks opencode install` exists; it may bind opencode sessions into
  `resume_binding` the way Claude's hooks do (unverified — check
  `surface.list` after installing).
- ⇒ For cmux a real terminal view needs an upstream change (an ANSI
  `surface.read_screen` and/or an output-frame subscription on the socket,
  along the lines of the earlier `notification.subscribe` proposal in
  `plans/cmux-pr-subscribe.md`). Until then cmux panes stay on polled plain
  text, rendered through the same phone view.

### The phone today (`ios/cmux/Layout/TerminalTextView.swift`)

- A `UITextView`, white monospace text, no SGR parsing, fixed font sizes.
- Whole `attributedText` replaced on every 3 s poll; the only scroll anchor
  is the "history prepended" case. Switching a card between
  transcript+live and live-only (`AppState.cardText(for:)`) is a replace
  and jumps the reader.
- No column negotiation: the PTY wraps at the Mac's width, then
  `UITextView` soft-wraps again at card width.
- 1500-line focused reads, 5000-line cap, 8 MiB WebSocket receive limit.

## Plan

### Phase A — scroll never jumps (phone only) — in progress

`ScrollFollow.anchor(autoScroll:oldText:newText:)` → `.followBottom` /
`.keepOffset` (append) / `.shiftByAddedHeight` (prepend) /
`.keepDistanceFromBottom` (replace), applied in
`TerminalTextView.Coordinator.apply`; 44 pt jump-to-bottom target. Pure
rules tested in `ios/scripts/test-scroll-follow.swift`.

### Phase B — opencode history on both backends — in progress

Port `internal/opencode` + `internal/transcriptrender` from
`worktree-opencode-sessions` behind a backend-agnostic transcript dispatcher
(`agent_kind` ∈ claude/opencode on the wire). herdr: session id straight
from `agent_session`. cmux: `resume_binding` if the opencode hooks are
installed, else the branch's tty/process + `"OC | <title>"` match. Phone:
`Surface.hasTranscript` replaces `isClaudeAgent` at the gating sites.

### Phase C — live frames + a real emulator (herdr first)

Bridge:
- New commands `surface.frames.subscribe {surface_id, cols?, rows?}` /
  `surface.frames.unsubscribe`; new push `surface.frame {surface_id, seq,
  full, width, height, bytes}` (base64, same as herdr). One observer
  subprocess per subscribed pane (`herdr terminal session observe`), fanned
  out per WebSocket client, torn down when the last client unsubscribes or
  disconnects. Default grid = the pane's native size; refuse to resize the
  PTY.
- Frame-rate control like mosh: coalesce deltas into ≤ 20 fps, and on
  (re)subscribe always start with a `full:true` frame.
- `Capabilities.Frames = true` for herdr, `false` for cmux.
- Optional: ANSI `surface.read_text {format:"ansi"}` for herdr plain shells
  (history with colour), gated by capability.

Phone:
- Add SwiftTerm (SPM, MIT) via `ios/project.yml` + `xcodegen generate`.
- `LivePaneView`: a `TerminalView` fed `surface.frame` bytes; sized to the
  frame's cols×rows, font scaled to fit the card width (pinch to zoom,
  horizontal pan when zoomed); cursor and colours come for free.
- Card = one vertical scroll container: `[history] [live viewport]`.
  History is the transcript (agents) or ANSI/plain scrollback (shells);
  the live viewport is the emulator, pinned as the last element and updated
  in place so Phase A's anchoring still holds. Follow-bottom = viewport
  fully visible.
- Input keeps using `surface.send_text` / `send_key`; no `control` channel.
- Fallback: surfaces without frames (cmux, or herdr unavailable) render the
  polled text in the same container.

### Phase D — cmux parity (upstream)

Propose to cmux: `surface.read_screen {format:"ansi"}` and a socket
subscription that streams output frames (or lets the bridge attach to the
Ghostty surface's byte stream). Until it lands, cmux stays on the polled
path; when it lands the bridge exposes the same `surface.frame` push.

## Open questions

- herdr observe with a default (no size) grid is 120×40 — is that a herdr
  default or the pane's PTY size? Use explicit `--cols/--rows` from the
  layout rect and `viewport_rows` and verify against `pane.read visible`
  line widths during Phase C.
- Whether `cmux hooks opencode install` populates `resume_binding` (decides
  how much of the tty fallback cmux needs in practice).
- Frame volume on Tailscale for many subscribed panes: only the focused
  pane should stream; background cards keep polling text at the slow rate.
