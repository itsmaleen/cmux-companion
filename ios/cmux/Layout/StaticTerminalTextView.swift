import SwiftUI
import UIKit

/// A non-scrolling, self-sizing rendering of terminal or transcript text, for
/// stacking inside another scroll container — the live card shows a surface's
/// history in one of these above the emulator, and the container scrolls both
/// as a single document. Same renderer as `TerminalTextView` (links, opacity,
/// wrapping), rendered synchronously so the height SwiftUI measures is always
/// the height of the text on screen.
struct StaticTerminalTextView: UIViewRepresentable {
    let text: String
    let fontSize: CGFloat
    var textOpacity: Double = 0.85
    var trimMarkdownLinks: Bool = false

    func makeCoordinator() -> Coordinator { Coordinator() }

    func makeUIView(context: Context) -> UITextView {
        let tv = UITextView()
        tv.isEditable = false
        tv.isSelectable = true
        tv.isScrollEnabled = false
        tv.linkTextAttributes = [
            .foregroundColor: UIColor(red: 0.45, green: 0.72, blue: 1.0, alpha: 1.0),
            .underlineStyle: NSUnderlineStyle.single.rawValue,
        ]
        tv.backgroundColor = .clear
        tv.textContainerInset = UIEdgeInsets(top: 6, left: 8, bottom: 6, right: 8)
        tv.textContainer.lineFragmentPadding = 0
        tv.autocorrectionType = .no
        tv.autocapitalizationType = .none
        context.coordinator.render(snapshot, into: tv)
        return tv
    }

    func updateUIView(_ tv: UITextView, context: Context) {
        context.coordinator.render(snapshot, into: tv)
    }

    func sizeThatFits(_ proposal: ProposedViewSize, uiView: UITextView, context: Context) -> CGSize? {
        guard let width = proposal.width, width > 0 else { return nil }
        let size = uiView.sizeThatFits(CGSize(width: width, height: .greatestFiniteMagnitude))
        return CGSize(width: width, height: ceil(size.height))
    }

    private var snapshot: TerminalTextSnapshot {
        TerminalTextSnapshot(text: text, fontSize: fontSize, textOpacity: textOpacity, trimMarkdownLinks: trimMarkdownLinks)
    }

    final class Coordinator {
        private var applied: TerminalTextSnapshot?

        func render(_ snapshot: TerminalTextSnapshot, into tv: UITextView) {
            guard snapshot != applied else { return }
            applied = snapshot
            tv.attributedText = TerminalTextRenderer.build(snapshot).attributed
        }
    }
}
