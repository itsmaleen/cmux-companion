# Proposal for cmux: styled screen reads and a frame subscription on the control socket

Drafted 2026-09-05 against cmux v0.64.22 (file references are to that tag).
Companion to `plans/responsive-sessions.md` Phase D. This is the text to
turn into an upstream issue/PR; nothing has been posted yet.

## Why

Third-party companions (merry, and anything else that fronts the local
socket) can only read a surface as plain text today:

- `surface.read_text {surface_id, lines, scrollback}` is implemented in
  `Sources/TerminalController.swift` (dispatch at `:1399`, body
  `v2SurfaceReadText` around `:2404`) on top of `ghostty_surface_read_text`
  (`:5279`), which has no styled variant. A `format` param is silently
  ignored.
- The socket has no server-initiated push for terminal output. `events.stream`
  (`Sources/CmuxEventStream.swift`, `CmuxSocketEventMapper.swift`) is the
  domain event bus. `cmux pipe-pane` is one-shot (`CLI/cmux.swift` ~24120):
  it calls `surface.read_text` once and pipes the result.
- There is no resize, cursor or scroll-position access.

cmux already has everything needed, but only for its first-party mobile app:

- `Sources/Mobile/MobileTerminalByteTee.swift` taps raw PTY bytes through
  `ghostty_surface_set_pty_tee_cb` (a cmux libghostty fork addition) and
  republishes them as `terminal.bytes`.
- `Sources/Mobile/MobileTerminalRenderObserver.swift` and
  `MobileHostConnectionEventQueue.swift` implement a `terminal.render_grid`
  topic: a full frame, then deltas, with "poison until full resync" when a
  delta is dropped, gated by subscriber counts.
- These are served over `Sources/Mobile/MobileHostService.swift`
  (iroh transport, Stack-auth pairing), not the Unix control socket.

Open upstream threads asking for the socket version:

- Issue #3003 "cmux attach": `surface.attach` / `surface.write` /
  `surface.resize` / `surface.detach`.
- Issue #11303: per-connection terminal viewport override for phone clients,
  citing `mobile.terminal.viewport` and `browser.viewport.set` as precedent.
- PR #11514 "secure web bridge for live Mac sessions" (`cmux serve-web`,
  Ghostty VT replay for browsers).

## Proposed additive API

### 1. `surface.read_text` gains `format`

```
surface.read_text {surface_id, lines?, scrollback?, format?: "text" | "ansi"}
→ {text, base64, format}
```

`format:"ansi"` returns the same rows with SGR sequences so a client can show
colour. Implementation: a second capture path in `v2SurfaceReadText`. Either
link the styled read that `Packages/iOS/CmuxMobileTerminal/.../GhosttySurfaceView.swift:2871`
stubs out (`ghostty_surface_read_text_html` "not available in this build") and
translate to SGR, or walk the Ghostty screen cells the way the mobile render
observer does. Precedent for styled capture already in tree:
`RemoteTmuxControlConnection+Commands.swift:206` uses `tmux capture-pane -e`.

### 2. `surface.frames.subscribe` on the control socket

```
surface.frames.subscribe   {surface_id, cols?, rows?}  → {surface_id, width, height}
surface.frames.unsubscribe {surface_id}                → {ok: true}

push  {"type":"surface.frame","data":{"surface_id","seq","full","width","height","encoding":"ansi","bytes":"<base64>"}}
push  {"type":"surface.frames.ended","data":{"surface_id","reason":"closed"|"error"|"backpressure"|"unsubscribed"}}
```

Semantics mirror the private `terminal.render_grid` queue: the first frame
after subscribe is a full repaint; deltas follow; if the connection falls
behind, the server ends the stream with `backpressure` and the client
resubscribes for a fresh full frame (no delta is ever dropped silently).
`cols`/`rows` request a per-subscriber grid (issue #11303); omitted means the
surface's native size. This is the same contract merry already consumes from
herdr (`herdr terminal session observe`), so one client codepath serves both
runtimes.

Hook points: register the methods in the `switch request.method` in
`Sources/TerminalController.swift:1277` and the per-domain coordinators under
`Packages/macOS/CmuxControlSocket/Sources/CmuxControlSocket/Coordinator/`;
classify them in `ControlCommandExecutionPolicy.swift`; reuse
`MobileTerminalByteTee` for the byte source and
`MobileHostConnectionEventQueue`'s full/delta discipline; gate on the
socket's existing password/automation auth.

### 3. Later: `surface.resize` and `surface.attach`

Issue #3003's `attach`/`write`/`resize`/`detach` is the natural superset. The
subscription above is the read half; `surface.send_text`/`send_key` already
cover the write half for companions that do not need a raw PTY.

## What merry does meanwhile

- Bridge and phone treat frames as a per-surface capability: subscribe, and
  on `unsupported` fall back to polled `surface.read_text` in the same card.
- If PR #11514's web bridge lands first, the bridge can consume its VT replay
  stream instead and translate to `surface.frame`.

## Contribution notes

`CONTRIBUTING.md` covers the macOS/Xcode/Zig setup; the PR template asks for
a demo video for behaviour changes. The socket API docs at cmux.com/docs/api
do not yet list `surface.read_text` or `events.stream`, so a docs addition
should accompany the change.
