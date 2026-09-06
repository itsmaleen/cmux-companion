import SwiftUI
import SwiftTerm
import UIKit

/// Renders a surface's live terminal screen with a real ANSI emulator
/// (SwiftTerm), fed by frames the bridge streams over `surface.frames.*` (see
/// `AppState.FrameFeed`). Display-only — the app's own input bar handles
/// typing — so the emulator never becomes first responder and shows no
/// caret/keyboard UI of its own. It does not scroll on its own either: it
/// sizes itself to its grid and the card's scroll container (history above,
/// live screen below) owns all scrolling.
struct LivePaneView: UIViewRepresentable {
    let feed: FrameFeed

    func makeUIView(context: Context) -> DisplayOnlyTerminalView {
        let view = DisplayOnlyTerminalView(
            frame: .zero,
            font: UIFont.monospacedSystemFont(ofSize: FrameFit.maxFontSize, weight: .regular)
        )
        context.coordinator.view = view
        attach(view)
        feed.sink = context.coordinator
        return view
    }

    func updateUIView(_ uiView: DisplayOnlyTerminalView, context: Context) {
        context.coordinator.view = uiView
        // The card can be handed a different surface's feed (focus changed
        // and SwiftUI reused this UIViewRepresentable instance rather than
        // recreating it) — re-point the sink so frames land on this view.
        if feed.sink !== context.coordinator {
            attach(uiView)
            feed.sink = context.coordinator
        }
    }

    private func attach(_ view: DisplayOnlyTerminalView) {
        view.onResyncNeeded = { [weak feed] in feed?.requestResync() }
        view.onUsedRowsChanged = { [weak feed] rows in feed?.geometry.updateUsedRows(rows) }
    }

    /// The emulator is exactly as tall as its grid at the font that fits the
    /// proposed width, so the card's scroll container can stack history
    /// above it and scroll both as one document.
    func sizeThatFits(_ proposal: ProposedViewSize, uiView: DisplayOnlyTerminalView, context: Context) -> CGSize? {
        guard let width = proposal.width, width > 0,
              let height = uiView.fittedHeight(forWidth: width) else { return nil }
        return CGSize(width: width, height: height)
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
    /// Reports how many rows from the top currently hold content, whenever
    /// that changes — the card trims the polled history above the emulator
    /// by exactly that many lines.
    var onUsedRowsChanged: ((Int) -> Void)?
    private var lastUsedRows = -1

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
        // The card's ScrollView scrolls; a scrollable emulator inside it
        // would swallow every vertical drag.
        isScrollEnabled = false
        showsVerticalScrollIndicator = false
        showsHorizontalScrollIndicator = false
    }

    /// The height this view needs to show its grid at the font that fits
    /// `width`, computed the way SwiftTerm sizes a cell. nil before the
    /// first full frame.
    func fittedHeight(forWidth width: CGFloat) -> CGFloat? {
        guard lastColumns > 0, lastRows > 0 else { return nil }
        let size = FrameFit.fontSize(forColumns: lastColumns, viewWidth: width, advanceOfMAt1pt: Self.advanceOfMAt1pt)
        let font = UIFont.monospacedSystemFont(ofSize: size, weight: .regular)
        let cellHeight = ceil(CTFontGetAscent(font) + CTFontGetDescent(font) + CTFontGetLeading(font))
        return cellHeight * CGFloat(lastRows)
    }

    private static let advanceOfMAt1pt: CGFloat = {
        let probeFont = UIFont.monospacedSystemFont(ofSize: 1, weight: .regular)
        return ("M" as NSString).size(withAttributes: [.font: probeFont]).width
    }()

    /// Counts rows top-down to the last one with content and reports a change.
    private func reportUsedRows() {
        guard applied else { return }
        let terminal = getTerminal()
        var used = 0
        for row in 0..<terminal.rows {
            if let line = terminal.getLine(row: row), !line.translateToString(trimRight: true).isEmpty {
                used = row + 1
            }
        }
        if used != lastUsedRows {
            lastUsedRows = used
            onUsedRowsChanged?(used)
        }
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
            reportUsedRows()
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
        invalidateIntrinsicContentSize()
        reportUsedRows()
        if replayIncomplete {
            onResyncNeeded?()
        }
    }

    /// Fits the font so `lastColumns` spans the view's width, clamped to
    /// FrameFit's range (at the minimum the view scrolls horizontally instead
    /// of shrinking further).
    private func refitFont() {
        let size = FrameFit.fontSize(forColumns: lastColumns, viewWidth: bounds.width, advanceOfMAt1pt: Self.advanceOfMAt1pt)
        if abs(size - font.pointSize) > 0.05 {
            font = UIFont.monospacedSystemFont(ofSize: size, weight: .regular)
        }
    }
}

/// The cell size SwiftTerm will use for a monospaced font of `fontSize`
/// points — the same arithmetic `DisplayOnlyTerminalView` sizes itself with.
/// Used to turn a card's size into a "fit to phone" grid.
enum LiveCellMetrics {
    static func cell(fontSize: CGFloat) -> (width: CGFloat, height: CGFloat) {
        let font = UIFont.monospacedSystemFont(ofSize: fontSize, weight: .regular)
        let width = ("M" as NSString).size(withAttributes: [.font: font]).width
        let height = ceil(CTFontGetAscent(font) + CTFontGetDescent(font) + CTFontGetLeading(font))
        return (width, height)
    }
}
