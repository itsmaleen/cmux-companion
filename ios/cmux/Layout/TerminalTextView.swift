import SwiftUI
import UIKit

/// A UITextView-backed terminal renderer that supports text selection (copy)
/// and tappable links — neither of which SwiftUI's `Text` can do. It scrolls
/// itself and keeps pinned to the bottom for live output, but yields to the
/// user the moment they scroll up or start a selection.
///
/// Attributed-string construction and link detection run off the main thread
/// (see `TerminalTextRenderer`): a deep scrollback read is thousands of lines,
/// and building that inline in `updateUIView` — plus UIKit's synchronous
/// whole-document rescan that `dataDetectorTypes` performs on every
/// `attributedText` assignment — caused visible hitches whenever a poll landed
/// during an animation (keyboard, card resize).
struct TerminalTextView: UIViewRepresentable {
    let text: String
    let fontSize: CGFloat
    let textOpacity: Double
    /// When true, trailing markdown/punctuation is trimmed from detected links
    /// (e.g. `**url**` → `url`). Used by the transcript viewer, whose text is
    /// raw markdown. Live terminal cards keep links exactly as detected.
    var trimMarkdownLinks: Bool = false
    /// Fired (throttled) when the user deliberately over-scrolls past the top
    /// (a rubber-band pull) — the hook for opening the conversation-history viewer.
    var onScrolledToTop: (() -> Void)? = nil
    /// Reports whether the content is scrolled to its top, so callers can show a
    /// "pull for history" affordance only when it's actually reachable.
    var onTopStateChanged: ((Bool) -> Void)? = nil
    /// Reports when a text selection starts or clears, so parents can veto
    /// gestures (surface-cycle swipes) that would fight a selection drag.
    var onSelectionActiveChanged: ((Bool) -> Void)? = nil
    /// Active search query. Every match is highlighted and the one at
    /// `currentMatchIndex` is scrolled into view.
    var searchQuery: String = ""
    /// Which match is the current one, as an index into the matches found in
    /// this text. Clamped internally, so a stale index is never fatal.
    var currentMatchIndex: Int = 0
    /// Reports how many matches the current query has, so the parent can render
    /// "3/17" and enable stepping. Fires only when the count changes.
    var onSearchMatchCount: ((Int) -> Void)? = nil
    /// Bump to jump to the newest output and resume following it. Deliberately
    /// NOT part of the rendered snapshot: a scroll request must not rebuild the
    /// attributed string.
    var scrollToBottomRequest: Int = 0
    /// Reports whether the view is pinned to the bottom, so the parent can offer
    /// a jump-to-bottom affordance only when it would do something.
    var onAtBottomChanged: ((Bool) -> Void)? = nil

    func makeCoordinator() -> Coordinator { Coordinator() }

    func makeUIView(context: Context) -> UITextView {
        let tv = UITextView()
        tv.isEditable = false
        tv.isSelectable = true
        tv.isScrollEnabled = true
        // Links come from our own detector pass in TerminalTextRenderer, not
        // dataDetectorTypes; tapping a .link attribute behaves the same either
        // way, but our pass runs off-main.
        tv.linkTextAttributes = [
            .foregroundColor: UIColor(red: 0.45, green: 0.72, blue: 1.0, alpha: 1.0),
            .underlineStyle: NSUnderlineStyle.single.rawValue,
        ]
        tv.backgroundColor = .clear
        tv.textContainerInset = UIEdgeInsets(top: 6, left: 8, bottom: 6, right: 8)
        tv.textContainer.lineFragmentPadding = 0
        tv.showsVerticalScrollIndicator = false
        tv.alwaysBounceVertical = true
        tv.delegate = context.coordinator
        // Terminal output is not natural language — don't fight the user with it.
        tv.autocorrectionType = .no
        tv.autocapitalizationType = .none
        context.coordinator.render(TerminalTextSnapshot(self), into: tv)
        return tv
    }

    func updateUIView(_ tv: UITextView, context: Context) {
        let coord = context.coordinator
        coord.onScrolledToTop = onScrolledToTop
        coord.onTopStateChanged = onTopStateChanged
        coord.onSelectionActiveChanged = onSelectionActiveChanged
        coord.onSearchMatchCount = onSearchMatchCount
        coord.onAtBottomChanged = onAtBottomChanged
        coord.render(TerminalTextSnapshot(self), into: tv)
        coord.handleScrollRequest(scrollToBottomRequest, in: tv)
    }

    final class Coordinator: NSObject, UITextViewDelegate {
        /// Whether new output should keep the view pinned to the bottom.
        /// True while the user is at (or near) the bottom; false once they
        /// scroll up to read scrollback.
        var autoScroll = true
        var onScrolledToTop: (() -> Void)?
        var onTopStateChanged: ((Bool) -> Void)?
        var onSelectionActiveChanged: ((Bool) -> Void)?
        var onSearchMatchCount: ((Int) -> Void)?
        var onAtBottomChanged: ((Bool) -> Void)?
        /// Last count handed to onSearchMatchCount, so an unchanged count
        /// doesn't publish (and re-render the layout) on every poll.
        private var reportedMatchCount: Int?
        /// True while this class is driving the scroll position.
        ///
        /// UIScrollView delivers scrollViewDidScroll for programmatic offsets as
        /// well as finger ones, and `autoScroll` must only ever answer "does the
        /// USER want to follow new output". Without this flag one short landing
        /// — `scrollToBottom` computing its target from a contentSize that was
        /// stale because the card was mid-animation — flips autoScroll off, and
        /// the card then never follows its output again for the rest of the
        /// session. That is the stuck-mid-scroll symptom this flag fixes.
        private var isProgrammaticScroll = false
        private var lastHandledScrollRequest = 0
        private var reportedAtBottom: Bool?

        /// Jumps to the newest output and resumes following it.
        fileprivate func handleScrollRequest(_ request: Int, in tv: UITextView) {
            guard request != lastHandledScrollRequest else { return }
            lastHandledScrollRequest = request
            autoScroll = true
            scrollToBottom(tv)
        }

        /// Runs `body` without letting its scrolling be mistaken for the user's.
        private func programmatically(_ body: () -> Void) {
            isProgrammaticScroll = true
            body()
            isProgrammaticScroll = false
        }

        private func reportAtBottom(_ atBottom: Bool) {
            guard reportedAtBottom != atBottom else { return }
            reportedAtBottom = atBottom
            onAtBottomChanged?(atBottom)
        }
        private var lastTopFire: TimeInterval = 0
        private var atTop = false
        private var selectionActive = false

        /// What the text view currently shows. Compared against incoming
        /// snapshots, NOT tv.attributedText: the injected link attributes mean
        /// a freshly built string never equals what's on screen, and SwiftUI
        /// calls updateUIView far more often than the text changes.
        private var applied: TerminalTextSnapshot?
        /// The snapshot most recently handed to the background builder, so a
        /// re-render of the parent view doesn't queue a duplicate build.
        private var pending: TerminalTextSnapshot?
        private var generation = 0

        fileprivate func render(_ snapshot: TerminalTextSnapshot, into tv: UITextView) {
            if snapshot == pending || snapshot == applied { return }
            pending = snapshot
            generation += 1
            let gen = generation
            Task.detached(priority: .userInitiated) { [weak self, weak tv] in
                let built = TerminalTextRenderer.build(snapshot)
                await MainActor.run {
                    // Only the newest build may land — an older one finishing
                    // late would put stale text (and a stale scroll) on screen.
                    guard let self, let tv, gen == self.generation else { return }
                    self.apply(built, for: snapshot, to: tv)
                }
            }
        }

        fileprivate func apply(_ built: TerminalTextRenderer.Built, for snapshot: TerminalTextSnapshot, to tv: UITextView) {
            let attr = built.attributed
            // Don't clobber an in-progress selection (a live poll would
            // otherwise yank the text out from under the user mid-copy).
            // Resetting `pending` lets the next update retry once it clears.
            if tv.selectedRange.length > 0 {
                pending = applied
                return
            }

            // Decide how to re-anchor the viewport BEFORE the swap — it needs
            // the old text, old content size, and old offset, all of which
            // are about to be overwritten. See ScrollFollow.anchor.
            let oldText = applied?.text ?? ""
            let viewportHeight = tv.bounds.height
            let oldHeight = tv.contentSize.height
            let oldOffset = tv.contentOffset
            let distanceFromBottom = max(0, oldHeight - (oldOffset.y + viewportHeight))
            let anchor = ScrollFollow.anchor(autoScroll: autoScroll, oldText: oldText, newText: snapshot.text)

            tv.attributedText = attr
            applied = snapshot

            if reportedMatchCount != built.matches.count {
                reportedMatchCount = built.matches.count
                onSearchMatchCount?(built.matches.count)
            }

            // A search owns the scroll position while it is active: jumping to
            // the bottom on the next poll would throw the reader off the match
            // they are reading, which is the whole point of searching.
            if !snapshot.searchQuery.isEmpty, !built.matches.isEmpty {
                let index = TextSearch.clamp(snapshot.currentMatchIndex, to: built.matches.count)
                programmatically {
                    tv.layoutIfNeeded()
                    tv.scrollRangeToVisible(built.matches[index])
                }
                // Leave autoScroll as the user left it: closing the search then
                // returns the card to following live output if it was.
                return
            }

            switch anchor {
            case .followBottom:
                scrollToBottom(tv)

            case .keepOffset:
                // Layout synchronously, then re-set the same raw offset,
                // clamped to the (possibly shorter) new content — an append
                // below the viewport, or an unchanged text, moves nothing
                // above it.
                programmatically {
                    tv.layoutIfNeeded()
                    let maxOffsetY = max(0, tv.contentSize.height - viewportHeight)
                    let clampedY = min(oldOffset.y, maxOffsetY)
                    if abs(tv.contentOffset.y - clampedY) > 0.5 {
                        tv.setContentOffset(CGPoint(x: oldOffset.x, y: clampedY), animated: false)
                    }
                }

            case .shiftByAddedHeight:
                // Layout synchronously so contentSize reflects the prepended
                // text, then shift the offset by exactly the added height — the
                // viewport stays anchored on the text the user was reading.
                programmatically {
                    tv.layoutIfNeeded()
                    let delta = tv.contentSize.height - oldHeight
                    if delta > 0 {
                        tv.setContentOffset(CGPoint(x: oldOffset.x, y: oldOffset.y + delta), animated: false)
                    }
                }

            case .keepDistanceFromBottom:
                // A replace (full-screen repaint, or the card composition
                // itself changing) — neither the old offset nor an
                // added-height delta means anything against the new content,
                // but preserving how far the reader had scrolled up from the
                // end does.
                programmatically {
                    tv.layoutIfNeeded()
                    let newHeight = tv.contentSize.height
                    let target = max(0, newHeight - viewportHeight - distanceFromBottom)
                    tv.setContentOffset(CGPoint(x: oldOffset.x, y: target), animated: false)
                }
            }
        }

        private func scrollToBottom(_ tv: UITextView) {
            // Defer a runloop so layout reflects the new content size first.
            DispatchQueue.main.async { [weak self] in
                guard let self else { return }
                self.pinToBottom(tv)
                // A card whose frame is still settling (keyboard, focus change,
                // rotation) reports a contentSize that is short, so the first
                // pass can land above the real bottom. One more pass on the next
                // runloop, once layout has caught up, finishes the job — with
                // autoScroll untouched throughout, a short landing is now
                // recoverable instead of permanent.
                DispatchQueue.main.async { self.pinToBottom(tv) }
            }
        }

        private func pinToBottom(_ tv: UITextView) {
            programmatically {
                tv.layoutIfNeeded()
                let bottom = ScrollFollow.bottomOffset(
                    contentHeight: tv.contentSize.height,
                    viewportHeight: tv.bounds.height,
                    bottomInset: tv.adjustedContentInset.bottom
                )
                if bottom > 0, abs(tv.contentOffset.y - bottom) > 0.5 {
                    tv.setContentOffset(CGPoint(x: 0, y: bottom), animated: false)
                }
            }
            reportAtBottom(true)
        }

        func scrollViewDidScroll(_ scrollView: UIScrollView) {
            let atBottom = ScrollFollow.isAtBottom(
                offsetY: scrollView.contentOffset.y,
                contentHeight: scrollView.contentSize.height,
                viewportHeight: scrollView.bounds.height
            )
            // Only a finger may decide to stop following live output. See
            // isProgrammaticScroll.
            autoScroll = ScrollFollow.follow(
                current: autoScroll, atBottom: atBottom, isProgrammatic: isProgrammaticScroll
            )
            if !isProgrammaticScroll {
                reportAtBottom(atBottom)
            }

            // Report reaching the top so the history hint appears only then —
            // but only for USER-driven scrolls. A programmatic setContentOffset
            // (e.g. the prepend anchor) also lands here synchronously; mutating
            // SwiftUI @State from a view-update pass is a violation.
            if scrollView.isTracking || scrollView.isDragging || scrollView.isDecelerating {
                let nowAtTop = scrollView.contentOffset.y <= 4
                if nowAtTop != atTop {
                    atTop = nowAtTop
                    onTopStateChanged?(nowAtTop)
                }
            }

            // Trigger history on a deliberate over-scroll PAST the top — a
            // rubber-band pull (offset well negative), not just reaching the top.
            guard let onScrolledToTop,
                  scrollView.isTracking || scrollView.isDragging || scrollView.isDecelerating,
                  scrollView.contentOffset.y < -70
            else { return }
            let now = Date().timeIntervalSinceReferenceDate
            if now - lastTopFire > 1.5 {
                lastTopFire = now
                onScrolledToTop()
            }
        }

        func textViewDidChangeSelection(_ textView: UITextView) {
            // UIKit fires this for caret-only/no-op changes too; only real
            // select/deselect transitions should reach the parent.
            let active = textView.selectedRange.length > 0
            guard active != selectionActive else { return }
            selectionActive = active
            onSelectionActiveChanged?(active)
        }
    }
}

/// The inputs that determine a rendered attributed string, captured as a value
/// so the background builder works from an immutable copy.
private struct TerminalTextSnapshot: Equatable {
    let text: String
    let fontSize: CGFloat
    let textOpacity: Double
    let trimMarkdownLinks: Bool
    let searchQuery: String
    let currentMatchIndex: Int

    init(_ view: TerminalTextView) {
        text = view.text
        fontSize = view.fontSize
        textOpacity = view.textOpacity
        trimMarkdownLinks = view.trimMarkdownLinks
        searchQuery = view.searchQuery
        currentMatchIndex = view.currentMatchIndex
    }
}

/// Builds TerminalTextView's attributed string. A standalone enum (not a member
/// of the View type) so its statics carry no MainActor isolation and can run in
/// a detached task.
private enum TerminalTextRenderer {
    /// A rendered string plus where the active search matched it, so the
    /// coordinator can scroll to the current match and report the count without
    /// searching the text a second time on the main actor.
    struct Built {
        let attributed: NSAttributedString
        let matches: [NSRange]
    }

    /// Every match gets this wash; the current one gets `currentMatchColor` so
    /// stepping is visible without hunting.
    private static let matchColor = UIColor.systemYellow.withAlphaComponent(0.28)
    private static let currentMatchColor = UIColor.systemYellow.withAlphaComponent(0.85)

    static func build(_ snapshot: TerminalTextSnapshot) -> Built {
        let style = NSMutableParagraphStyle()
        style.lineSpacing = 1
        let attr = NSMutableAttributedString(
            string: snapshot.text,
            attributes: [
                .font: UIFont.monospacedSystemFont(ofSize: snapshot.fontSize, weight: .regular),
                .foregroundColor: UIColor.white.withAlphaComponent(snapshot.textOpacity),
                .paragraphStyle: style,
            ]
        )
        addLinks(to: attr, trimTrailingJunk: snapshot.trimMarkdownLinks)
        let matches = TextSearch.matches(of: snapshot.searchQuery, in: snapshot.text)
        let plan = TextSearch.highlightPlan(matches: matches, current: snapshot.currentMatchIndex)
        for range in plan.others {
            attr.addAttribute(.backgroundColor, value: matchColor, range: range)
        }
        if let current = plan.current {
            attr.addAttribute(.backgroundColor, value: currentMatchColor, range: current)
            // The current match sits on a near-opaque fill, where white text is
            // unreadable.
            attr.addAttribute(.foregroundColor, value: UIColor.black, range: current)
        }
        return Built(attributed: attr, matches: matches)
    }

    private static let linkDetector: NSDataDetector? =
        try? NSDataDetector(types: NSTextCheckingResult.CheckingType.link.rawValue)

    /// Trailing characters that prose/markdown commonly places right after a URL
    /// but that aren't part of it. Data detectors grab them greedily — e.g.
    /// `**https://example.com/x**` yields `https://example.com/x**`, so the tap
    /// opens a broken URL. `)` is handled separately to preserve balanced parens.
    private static let trailingJunk = CharacterSet(charactersIn: "*_~`.,;:!?\"']}>")

    /// Detects URLs in the plain text and adds `.link` attributes. With
    /// `trimTrailingJunk` the trailing markdown/punctuation the detector
    /// over-captures is trimmed off first (transcript viewer); without it the
    /// detector's match is extended across terminal hard-wraps when a URL was
    /// broken over multiple rows (live terminal cards).
    private static func addLinks(to attr: NSMutableAttributedString, trimTrailingJunk trim: Bool) {
        guard let detector = linkDetector else { return }
        let ns = attr.string as NSString
        let matches = detector.matches(in: attr.string, range: NSRange(location: 0, length: ns.length))
        // Line table for re-joining hard-wrapped URLs — live terminal text
        // only; transcript text is logical lines, never width-wrapped.
        let lines = trim ? [] : lineRanges(of: ns)
        let maxLineLength = lines.reduce(0) { max($0, $1.length) }
        var linkedEnd = 0
        for match in matches {
            // A wrapped URL's tail can re-match as its own link; if a previous
            // match was extended over this text, skip the scrap.
            guard match.range.location >= linkedEnd else { continue }
            var range = match.range
            var url = match.url
            if trim {
                while range.length > 0 {
                    let unit = ns.character(at: range.location + range.length - 1)
                    guard let scalar = Unicode.Scalar(unit) else { break }
                    if scalar == ")" {
                        // Keep a ) that closes a ( inside the URL (e.g.
                        // wiki/Foo_(bar)) — but trim unbalanced extras, as in
                        // "(see wiki/Foo_(bar))" where the last ) is prose.
                        let candidate = ns.substring(with: range)
                        let opens = candidate.lazy.filter { $0 == "(" }.count
                        let closes = candidate.lazy.filter { $0 == ")" }.count
                        if closes <= opens { break }
                    } else if !trailingJunk.contains(scalar) {
                        break
                    }
                    range.length -= 1
                }
                guard range.length > 0 else { continue }
                url = URL(string: ns.substring(with: range))
            } else if let joined = joinedAcrossWraps(match, in: ns, lines: lines, maxLineLength: maxLineLength) {
                range = joined.0
                url = joined.1
            }
            guard let url else { continue }
            attr.addAttribute(.link, value: url, range: range)
            linkedEnd = range.location + range.length
        }
    }

    /// Content range of each line (newlines excluded), in order.
    private static func lineRanges(of ns: NSString) -> [NSRange] {
        var ranges: [NSRange] = []
        var start = 0
        while start <= ns.length {
            let rest = NSRange(location: start, length: ns.length - start)
            let newline = ns.range(of: "\n", options: [], range: rest)
            if newline.location == NSNotFound {
                ranges.append(rest)
                break
            }
            ranges.append(NSRange(location: start, length: newline.location - start))
            start = newline.location + newline.length
        }
        return ranges
    }

    /// Characters that can legitimately continue a URL on the next row.
    private static let urlScalars: CharacterSet = {
        var set = CharacterSet.alphanumerics
        set.insert(charactersIn: "-._~:/?#[]@!$&'()*+,;=%")
        return set
    }()

    /// Rows shorter than this never count as "full width", so small snapshots
    /// can't trigger bogus joins.
    private static let minWrapWidth = 40

    /// A URL the terminal hard-wrapped shows up as a match that runs to the
    /// edge of a full-width row, with the URL's remainder starting the next
    /// row — linkifying just the first row produces a truncated link that
    /// leads nowhere. Returns the range spanning the break(s) plus the URL
    /// with them removed, or nil when the match doesn't look wrapped. The
    /// PTY's width isn't in the protocol, so "full width" means the longest
    /// row in this snapshot.
    private static func joinedAcrossWraps(
        _ match: NSTextCheckingResult, in ns: NSString, lines: [NSRange], maxLineLength: Int
    ) -> (NSRange, URL)? {
        guard maxLineLength >= minWrapWidth else { return nil }
        let matchEnd = match.range.location + match.range.length
        guard var lineIndex = lines.firstIndex(where: {
            NSLocationInRange(match.range.location, $0) && matchEnd == $0.location + $0.length
        }), lines[lineIndex].length == maxLineLength else { return nil }

        var urlString = ns.substring(with: match.range)
        var end = matchEnd
        while lineIndex + 1 < lines.count {
            let next = lines[lineIndex + 1]
            let nextText = ns.substring(with: next)
            let continuation = String(nextText.unicodeScalars.prefix(while: urlScalars.contains))
            guard !continuation.isEmpty else { break }
            urlString += continuation
            end = next.location + (continuation as NSString).length
            // Only a row that is entirely URL characters at full width can
            // wrap onward; anything less means the URL ended on this row.
            guard end == next.location + next.length, next.length == maxLineLength else { break }
            lineIndex += 1
        }
        guard end > matchEnd, let url = URL(string: urlString) else { return nil }
        // A continuation may only ever extend the URL's path/query — never its
        // authority. Terminal output is untrusted, and a crafted next row like
        // "@evil.example" would otherwise turn the displayed host into userinfo
        // and send the tap to a different server than the visible first row.
        guard let joined = URLComponents(string: urlString),
              let original = match.url.flatMap({ URLComponents(string: $0.absoluteString) }),
              let scheme = joined.scheme, scheme.lowercased() == original.scheme?.lowercased(),
              joined.user == original.user,
              let host = joined.host, host.lowercased() == original.host?.lowercased(),
              joined.port == original.port
        else { return nil }
        return (NSRange(location: match.range.location, length: end - match.range.location), url)
    }
}
