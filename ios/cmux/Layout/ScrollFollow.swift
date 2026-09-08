import CoreGraphics

/// The "keep following new output" decision for a scrolling terminal view.
///
/// Split out of `TerminalTextView` and free of UIKit so it can be exercised by
/// `ios/scripts/test-scroll-follow.swift`. The rules are small but they were
/// wrong in a way no build catches: a programmatic scroll that landed short of
/// the bottom used to turn following OFF permanently, and the card then sat
/// mid-output while new lines arrived below it.
enum ScrollFollow {
    /// How close to the bottom still counts as "at the bottom". A few points of
    /// slack, because a fractional content height (line fragments rarely land on
    /// whole points) otherwise reads as scrolled-up forever.
    static let bottomThreshold: CGFloat = 24

    /// Whether the viewport is at (or within `threshold` of) the end of the content.
    static func isAtBottom(
        offsetY: CGFloat,
        contentHeight: CGFloat,
        viewportHeight: CGFloat,
        threshold: CGFloat = bottomThreshold
    ) -> Bool {
        offsetY >= contentHeight - viewportHeight - threshold
    }

    /// The offset that puts the end of the content at the bottom of the viewport.
    /// Never negative: content shorter than the viewport has nothing to scroll.
    static func bottomOffset(
        contentHeight: CGFloat,
        viewportHeight: CGFloat,
        bottomInset: CGFloat
    ) -> CGFloat {
        max(0, contentHeight - viewportHeight + bottomInset)
    }

    /// Whether following should stay on after a scroll event.
    ///
    /// The `isProgrammatic` gate is the fix: UIScrollView reports self-inflicted
    /// offsets through the same delegate callback as finger ones, so without it
    /// a single short landing — `scrollToBottom` computing its target from a
    /// contentSize that was stale because the card was mid-animation — reads as
    /// "the user scrolled up" and following never comes back.
    static func follow(current: Bool, atBottom: Bool, isProgrammatic: Bool) -> Bool {
        isProgrammatic ? current : atBottom
    }

    /// How the viewport should be re-anchored across a text replacement.
    ///
    /// `TerminalTextView.Coordinator.apply` swaps the whole `attributedText`
    /// on every poll — there is no incremental diff — so whatever the raw
    /// `contentOffset` was pointing at before the swap may now be different
    /// content, or past the end of a shorter document. `.keepOffset` and
    /// `.keepDistanceFromBottom` exist so a reader mid-scrollback doesn't get
    /// yanked around by output they aren't looking at.
    enum Anchor: Equatable {
        /// Follow the newest output. Used while `autoScroll` is on, or when
        /// there is no prior text to anchor to.
        case followBottom
        /// Keep the same raw offset (clamped to the new content). Correct for
        /// an append — new lines added below the viewport don't move
        /// anything above it — and for a no-op update.
        case keepOffset
        /// Shift the offset by exactly how much content was added above.
        /// Correct for a prepend (history loaded above the viewport).
        case shiftByAddedHeight
        /// Keep the same distance from the bottom of the content. Correct
        /// for a replace — a full-screen TUI repainting its viewport, or the
        /// card composition changing (transcript+live vs. live-only) — where
        /// neither an exact offset nor an added-height delta means anything,
        /// but "how far up the reader had scrolled" still roughly does.
        case keepDistanceFromBottom
    }

    /// Decides the anchor for a text replacement. Pure and cheap: `oldText`
    /// and `newText` can be thousands of lines, but `hasPrefix`/`hasSuffix`
    /// on Swift `String` are each a single linear scan, not quadratic.
    static func anchor(autoScroll: Bool, oldText: String, newText: String) -> Anchor {
        if autoScroll { return .followBottom }
        if oldText.isEmpty { return .followBottom }
        if oldText == newText { return .keepOffset }
        // Append: everything the reader was looking at is still there,
        // unchanged, above whatever landed after it.
        if newText.hasPrefix(oldText) { return .keepOffset }
        // Prepend: everything the reader was looking at is still there,
        // unchanged, below whatever landed before it.
        if newText.hasSuffix(oldText) { return .shiftByAddedHeight }
        return .keepDistanceFromBottom
    }
}
