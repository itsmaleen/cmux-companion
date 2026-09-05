import SwiftUI
import SwiftTerm
import UIKit

/// Renders a surface's live terminal screen with a real ANSI emulator
/// (SwiftTerm), fed by frames the bridge streams over `surface.frames.*` (see
/// `AppState.FrameFeed`). Display-only — the app's own input bar handles
/// typing — so the emulator never becomes first responder and shows no
/// caret/keyboard UI of its own; SwiftTerm's normal touch scrolling (its
/// scrollback, and horizontal scroll when a frame's columns don't fit at the
/// minimum font size) still works.
struct LivePaneView: UIViewRepresentable {
    let feed: FrameFeed
    /// Bumped by the parent to scroll this view's own scrollback to the live
    /// tail — the live-mode equivalent of PaneCardView's jump-to-bottom
    /// button, which otherwise targets TerminalTextView's scroll position.
    var scrollToBottomRequest: Int = 0

    func makeUIView(context: Context) -> DisplayOnlyTerminalView {
        let view = DisplayOnlyTerminalView(
            frame: .zero,
            font: UIFont.monospacedSystemFont(ofSize: FrameFit.maxFontSize, weight: .regular)
        )
        context.coordinator.view = view
        context.coordinator.lastScrollToBottomRequest = scrollToBottomRequest
        view.onResyncNeeded = { [weak feed] in feed?.requestResync() }
        feed.sink = context.coordinator
        return view
    }

    func updateUIView(_ uiView: DisplayOnlyTerminalView, context: Context) {
        context.coordinator.view = uiView
        // The card can be handed a different surface's feed (focus changed
        // and SwiftUI reused this UIViewRepresentable instance rather than
        // recreating it) — re-point the sink so frames land on this view.
        if feed.sink !== context.coordinator {
            uiView.onResyncNeeded = { [weak feed] in feed?.requestResync() }
            feed.sink = context.coordinator
        }
        if scrollToBottomRequest != context.coordinator.lastScrollToBottomRequest {
            context.coordinator.lastScrollToBottomRequest = scrollToBottomRequest
            uiView.scroll(toPosition: 1.0)
        }
    }

    static func dismantleUIView(_ uiView: DisplayOnlyTerminalView, coordinator: Coordinator) {
        coordinator.view = nil
    }

    func makeCoordinator() -> Coordinator {
        Coordinator()
    }

    /// Hands frames to the SwiftTerm view. A separate object (rather than
    /// LivePaneView itself) because FrameSink must be a class — FrameFeed
    /// holds it `weak`.
    @MainActor
    final class Coordinator: FrameSink {
        weak var view: DisplayOnlyTerminalView?
        var lastScrollToBottomRequest = 0

        func receive(_ frame: TerminalFrame) {
            view?.apply(frame)
        }
    }
}

/// A SwiftTerm `TerminalView` that never accepts input focus and always shows
/// the last full frame (plus the deltas since it) at that frame's declared
/// columns × rows.
///
/// SwiftTerm's own `layoutSubviews` re-fits the terminal's column count to
/// whatever the current bounds allow — and a resize of a terminal that
/// already holds content crops that content, permanently. That happens on
/// the first layout after the view is created, on rotation, and whenever the
/// card's size changes, so it is not enough to pin the size once: after
/// every such pass the view resets the emulator, re-asserts the frame's real
/// grid, and replays the frame bytes it kept. Frames are cursor-addressed
/// repaints, so the replay is exact.
final class DisplayOnlyTerminalView: TerminalView {
    /// Bounds how many delta bytes are kept for replay before the view gives
    /// up and asks for a fresh full frame instead.
    private static let maxDeltaBytes = 4 << 20

    private var lastColumns = 0
    private var lastRows = 0
    private var fullFrame: [UInt8]?
    private var deltas: [[UInt8]] = []
    private var deltaBytes = 0
    /// Whether the emulator currently reflects `fullFrame` + `deltas` at
    /// `lastColumns` × `lastRows`. False until the first replay succeeds
    /// (the view needs non-zero bounds to fit a font first).
    private var applied = false
    /// Set when deltas had to be dropped: the live screen is still right (each
    /// delta was fed as it arrived) but a replay would not be, so the next one
    /// asks for a resync.
    private var replayIncomplete = false
    /// Asks the feed for a fresh full frame (a resubscribe on the bridge).
    var onResyncNeeded: (() -> Void)?

    override var canBecomeFirstResponder: Bool { false }
    override var canBecomeFocused: Bool { false }

    override init(frame: CGRect, font: UIFont?) {
        super.init(frame: frame, font: font)
        commonInit()
    }

    required init?(coder: NSCoder) {
        super.init(coder: coder)
        commonInit()
    }

    private func commonInit() {
        nativeBackgroundColor = .black
        isOpaque = true
    }

    /// Applies one frame: a full frame replaces everything kept and is
    /// replayed from scratch; a delta is fed live and kept for later replays.
    func apply(_ frame: TerminalFrame) {
        let bytes = [UInt8](frame.bytes)
        if frame.full {
            fullFrame = bytes
            deltas = []
            deltaBytes = 0
            replayIncomplete = false
            lastColumns = frame.width
            lastRows = frame.height
            replay()
            return
        }
        // A delta before any full frame has nothing to apply to; the bridge
        // always starts a stream with a full frame, so just wait for it.
        guard fullFrame != nil else { return }
        if applied {
            feed(byteArray: bytes[...])
        }
        deltas.append(bytes)
        deltaBytes += bytes.count
        if deltaBytes > Self.maxDeltaBytes {
            deltas = []
            deltaBytes = 0
            replayIncomplete = true
            onResyncNeeded?()
        }
    }

    override func layoutSubviews() {
        // Runs SwiftTerm's own fit-to-bounds resize first (which may crop the
        // emulator's content); the replay below then restores the frame's
        // real grid and content.
        super.layoutSubviews()
        guard lastColumns > 0, lastRows > 0 else { return }
        let terminal = getTerminal()
        if !applied || terminal.cols != lastColumns || terminal.rows != lastRows {
            replay()
        }
    }

    /// Resets the emulator to the frame's grid and replays the kept bytes.
    private func replay() {
        guard lastColumns > 0, lastRows > 0, bounds.width > 0, bounds.height > 0, let full = fullFrame else {
            applied = false
            return
        }
        refitFont()
        getTerminal().resetToInitialState()
        resize(cols: lastColumns, rows: lastRows)
        feed(byteArray: full[...])
        for delta in deltas {
            feed(byteArray: delta[...])
        }
        applied = true
        if replayIncomplete {
            onResyncNeeded?()
        }
    }

    /// Fits the font so `lastColumns` spans the view's width, clamped to
    /// FrameFit's range (at the minimum the view scrolls horizontally instead
    /// of shrinking further).
    private func refitFont() {
        let probeFont = UIFont.monospacedSystemFont(ofSize: 1, weight: .regular)
        let advance = ("M" as NSString).size(withAttributes: [.font: probeFont]).width
        let size = FrameFit.fontSize(forColumns: lastColumns, viewWidth: bounds.width, advanceOfMAt1pt: advance)
        if abs(size - font.pointSize) > 0.05 {
            font = UIFont.monospacedSystemFont(ofSize: size, weight: .regular)
        }
    }
}
