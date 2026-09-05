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
        feed.sink = context.coordinator
        return view
    }

    func updateUIView(_ uiView: DisplayOnlyTerminalView, context: Context) {
        context.coordinator.view = uiView
        // The card can be handed a different surface's feed (focus changed
        // and SwiftUI reused this UIViewRepresentable instance rather than
        // recreating it) — re-point the sink so frames land on this view.
        if feed.sink !== context.coordinator {
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

    /// Applies frames to the SwiftTerm view. A separate object (rather than
    /// LivePaneView itself) because FrameSink must be a class — FrameFeed
    /// holds it `weak`.
    @MainActor
    final class Coordinator: FrameSink {
        weak var view: DisplayOnlyTerminalView?
        var lastScrollToBottomRequest = 0

        func receive(_ frame: TerminalFrame) {
            guard let view else { return }
            if frame.full {
                view.getTerminal().resetToInitialState()
                view.applyFrameGeometry(columns: frame.width, rows: frame.height)
            }
            view.feed(byteArray: Array(frame.bytes)[...])
        }
    }
}

/// A SwiftTerm `TerminalView` that never accepts input focus and keeps its
/// column count pinned to the last live frame's declared width — SwiftTerm's
/// own `layoutSubviews` otherwise silently re-fits the terminal's column
/// count to whatever the view's current bounds allow, which would rewrap a
/// frame's exact layout every time the card's size changes.
final class DisplayOnlyTerminalView: TerminalView {
    /// The most recent full frame's declared size, in terminal cells. Font
    /// size is refit to this on every layout pass so a card resize (rotation,
    /// split-view) doesn't leave the column count out of sync with what the
    /// bridge is actually sending.
    private var lastColumns = 0
    private var lastRows = 0

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

    /// Sets the terminal to `columns` × `rows` and fits the font so `columns`
    /// spans the view's current width (or clamps to the minimum, letting the
    /// view scroll horizontally instead of shrinking further).
    func applyFrameGeometry(columns: Int, rows: Int) {
        lastColumns = columns
        lastRows = rows
        refit()
    }

    override func layoutSubviews() {
        // Runs SwiftTerm's own auto-fit-to-bounds resize first; `refit()`
        // below then re-asserts the last frame's real dimensions, so that
        // auto-fit is never actually visible on screen.
        super.layoutSubviews()
        refit()
    }

    private func refit() {
        guard lastColumns > 0, lastRows > 0, bounds.width > 0 else { return }
        let probeFont = UIFont.monospacedSystemFont(ofSize: 1, weight: .regular)
        let advance = ("M" as NSString).size(withAttributes: [.font: probeFont]).width
        let size = FrameFit.fontSize(forColumns: lastColumns, viewWidth: bounds.width, advanceOfMAt1pt: advance)
        if abs(size - font.pointSize) > 0.05 {
            font = UIFont.monospacedSystemFont(ofSize: size, weight: .regular)
        }
        resize(cols: lastColumns, rows: lastRows)
    }
}
