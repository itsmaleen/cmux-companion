import SwiftUI

struct PaneCardView: View {
    let title: String
    let isFocused: Bool
    let hasNotification: Bool
    let isTranscribing: Bool
    let transcript: String
    var terminalText: String = ""
    var contentScale: CGFloat = 1.0
    /// The focused surface's live terminal frame stream, when the bridge
    /// supports it and a subscribe has succeeded. Non-nil switches the card
    /// from the polled-text `TerminalTextView` to `LivePaneView`, a real ANSI
    /// emulator fed frames directly (bypassing SwiftUI's render cycle — see
    /// `AppState.FrameFeed`). nil (no support, subscribe still pending, or a
    /// non-focused card) renders exactly as before.
    var frameFeed: FrameFeed?
    /// Reports the live card's content size (the area a fitted pane should
    /// fill), on first layout and whenever it changes.
    var onLiveSizeChanged: ((CGSize) -> Void)? = nil
    /// What a live card shows ABOVE the emulator: the conversation for an
    /// agent surface, the polled scrollback for a plain shell.
    var liveHistoryText: String = ""
    /// True when `liveHistoryText` is a polled screen read (scrollback plus
    /// the visible screen), so the rows the emulator already shows must be
    /// trimmed off its end; false for a conversation transcript.
    var liveHistoryIsPolledScreen: Bool = false
    var isBrowser: Bool = false
    var browserURL: String = ""
    /// When true, scrolling to the top of a focused card opens the conversation
    /// full-screen (claude agent surfaces, whose card already shows it inline).
    var canOpenHistory: Bool = false
    var onOpenHistory: (() -> Void)?
    /// The surface's screen moved on its latest poll — its agent/command is
    /// actively producing output. Shows the pulsing dots in the title bar.
    var isWorking: Bool = false
    /// Reports selection begin/end in the terminal text so the parent can
    /// hold back gestures that would fight a selection drag.
    var onSelectionActiveChanged: ((Bool) -> Void)? = nil
    /// Find-in-surface: every match is highlighted, the current one is scrolled
    /// to, and the count is reported back for the "3/17" readout.
    var searchQuery: String = ""
    var currentMatchIndex: Int = 0
    var onSearchMatchCount: ((Int) -> Void)? = nil
    /// Bumped by the parent to jump this card to the newest output.
    var scrollToBottomRequest: Int = 0
    /// Asks the parent to bump `scrollToBottomRequest`.
    var onJumpToBottom: (() -> Void)? = nil
    @State private var notificationPulse = false
    @State private var terminalAtTop = false
    @State private var terminalAtBottom = true

    var body: some View {
        ZStack {
            // Glass background
            RoundedRectangle(cornerRadius: 12)
                .fill(.ultraThinMaterial.opacity(isFocused ? 0.4 : 0.2))
                .environment(\.colorScheme, .dark)

            // Notification pulse ring
            if hasNotification {
                RoundedRectangle(cornerRadius: 12)
                    .stroke(Color.orange, lineWidth: 2.5)
                    .opacity(notificationPulse ? 0.4 : 1.0)
                    .animation(
                        .easeInOut(duration: 1.2).repeatForever(autoreverses: true),
                        value: notificationPulse
                    )
                    .onAppear { notificationPulse = true }
                    .onDisappear { notificationPulse = false }
            }

            // Focus glow border
            RoundedRectangle(cornerRadius: 12)
                .stroke(borderColor, lineWidth: isFocused ? 1.5 : 0.5)

            // Content
            VStack(spacing: 0) {
                // Title bar
                titleBar

                // Body area
                ZStack(alignment: .topLeading) {
                    if isBrowser {
                        VStack(spacing: 8) {
                            Image(systemName: "globe")
                                .font(.system(size: isFocused ? 28 : 16))
                                .foregroundStyle(.white.opacity(0.2))
                            if !browserURL.isEmpty {
                                Text(browserURL
                                    .replacingOccurrences(of: "https://", with: "")
                                    .replacingOccurrences(of: "http://", with: ""))
                                    .font(.system(size: isFocused ? 10 : 7, design: .monospaced))
                                    .foregroundStyle(.white.opacity(0.4))
                                    .lineLimit(2)
                                    .multilineTextAlignment(.center)
                            }
                            if !title.isEmpty {
                                Text(title)
                                    .font(.system(size: isFocused ? 9 : 7, weight: .medium))
                                    .foregroundStyle(.white.opacity(0.3))
                                    .lineLimit(1)
                            }
                        }
                        .frame(maxWidth: .infinity, maxHeight: .infinity)
                    } else if let frameFeed {
                        liveContentView(frameFeed)
                    } else if !terminalText.isEmpty {
                        terminalContentView
                    } else {
                        // Loading state for terminals before content arrives
                        VStack(spacing: 10) {
                            ProgressView()
                                .scaleEffect(0.7)
                                .tint(.white.opacity(0.3))
                            Text("Loading…")
                                .font(.system(size: 9, weight: .medium, design: .monospaced))
                                .foregroundStyle(.white.opacity(0.2))
                        }
                        .frame(maxWidth: .infinity, maxHeight: .infinity)
                    }

                    // Live speech transcript OVERLAYS the content while recording.
                    // It must not be a sibling branch of `terminalContentView`:
                    // that view is a UITextView (UIViewRepresentable), and
                    // swapping it out mid-recording leaves its UIKit layer on
                    // screen so the transcript never appears. Overlaying keeps the
                    // terminal mounted and paints the transcript on top.
                    if isTranscribing && !transcript.isEmpty {
                        Text(transcript)
                            .font(.system(size: 11, weight: .regular, design: .monospaced))
                            .foregroundColor(.green.opacity(0.9))
                            .lineLimit(4)
                            .frame(maxWidth: .infinity, alignment: .leading)
                            .padding(.horizontal, 10)
                            .padding(.vertical, 8)
                            .background(.ultraThinMaterial.opacity(0.9))
                            .environment(\.colorScheme, .dark)
                    }
                }
                .frame(maxWidth: .infinity, maxHeight: .infinity)
                .clipped()
            }
        }
        .shadow(color: isFocused ? .white.opacity(0.08) : .clear, radius: 12)
        .overlay(
            RoundedRectangle(cornerRadius: 12)
                .fill(isTranscribing ? Color.red.opacity(0.03) : .clear)
        )
    }

    // MARK: - Live terminal content

    /// The live card: one vertical scroll container holding the surface's
    /// history (conversation or polled scrollback) above the emulator, anchored
    /// to the bottom so the live screen is what you see until you drag up.
    private func liveContentView(_ feed: FrameFeed) -> some View {
        LiveCardContent(
            feed: feed,
            historyText: liveHistoryText,
            historyIsPolledScreen: liveHistoryIsPolledScreen,
            fontSize: (isFocused ? 9 : 7) * contentScale,
            scrollToBottomRequest: scrollToBottomRequest,
            onSizeChanged: onLiveSizeChanged
        )
        .overlay(alignment: .topTrailing) {
            if isFocused && canOpenHistory {
                historyButton
                    .padding(.trailing, 10)
                    .padding(.top, 8)
            }
        }
        .overlay(alignment: .bottomTrailing) {
            if isFocused {
                jumpToBottomButton
                    .padding(.trailing, 10)
                    .padding(.bottom, 10)
            }
        }
    }

    private var historyButton: some View {
        Button {
            onOpenHistory?()
        } label: {
            Image(systemName: "text.bubble")
                .font(.system(size: 12, weight: .semibold))
                .foregroundStyle(.white.opacity(0.75))
                .frame(width: 28, height: 28)
                .background(
                    Circle()
                        .fill(.ultraThinMaterial)
                        .environment(\.colorScheme, .dark)
                )
                .overlay(Circle().stroke(Color.white.opacity(0.15), lineWidth: 0.5))
        }
        .accessibilityLabel("View conversation history")
    }

    // MARK: - Terminal content

    private var terminalContentView: some View {
        // UITextView-backed so output is selectable (copy) and URLs are tappable.
        // A claude card's text is its conversation with the live screen below it;
        // pulling past the top opens the same conversation full-screen, which is
        // an easier read than this card's 9pt type.
        TerminalTextView(
            text: terminalText,
            fontSize: (isFocused ? 9 : 7) * contentScale,
            textOpacity: isFocused ? 0.85 : 0.5,
            onScrolledToTop: (isFocused && canOpenHistory) ? { onOpenHistory?() } : nil,
            onTopStateChanged: (isFocused && canOpenHistory) ? { terminalAtTop = $0 } : nil,
            onSelectionActiveChanged: onSelectionActiveChanged,
            searchQuery: searchQuery,
            currentMatchIndex: currentMatchIndex,
            onSearchMatchCount: onSearchMatchCount,
            scrollToBottomRequest: scrollToBottomRequest,
            onAtBottomChanged: { terminalAtBottom = $0 }
        )
        .overlay(alignment: .top) {
            // Only surface the affordance once the user is at the top; a further
            // pull-up past the top opens the full-screen reader.
            if isFocused && canOpenHistory && terminalAtTop {
                historyHint
                    .padding(.top, 4)
                    .transition(.opacity.combined(with: .move(edge: .top)))
            }
        }
        .overlay(alignment: .bottomTrailing) {
            // Offered only when it would do something: the card is scrolled up
            // and no search is holding the position on a match.
            if isFocused && !terminalAtBottom && searchQuery.isEmpty {
                jumpToBottomButton
                    .padding(.trailing, 10)
                    .padding(.bottom, 10)
                    .transition(.opacity.combined(with: .scale(scale: 0.85)))
            }
        }
        .animation(.easeInOut(duration: 0.18), value: terminalAtTop)
        .animation(.easeInOut(duration: 0.18), value: terminalAtBottom)
    }

    private var jumpToBottomButton: some View {
        Button {
            onJumpToBottom?()
        } label: {
            Image(systemName: "chevron.down")
                .font(.system(size: 13, weight: .semibold))
                .foregroundStyle(.white.opacity(0.75))
                .frame(width: 32, height: 32)
                .background(
                    Circle()
                        .fill(.ultraThinMaterial)
                        .environment(\.colorScheme, .dark)
                )
                .overlay(Circle().stroke(Color.white.opacity(0.15), lineWidth: 0.5))
                // The visual stays a small 32pt puck (it shouldn't cover more
                // of the card than it needs to), but the tappable area is
                // widened to Apple's 44x44 minimum so it's reliably hittable
                // on a phone where this sits at a card's corner.
                .frame(width: 44, height: 44)
                .contentShape(Rectangle())
        }
        .accessibilityLabel("Jump to newest output")
    }

    private var historyHint: some View {
        HStack(spacing: 5) {
            Image(systemName: "arrow.up")
                .font(.system(size: 8, weight: .semibold))
            Text("pull up to expand")
                .font(.system(size: 8, weight: .medium, design: .monospaced))
        }
        .foregroundStyle(.white.opacity(0.5))
        .padding(.horizontal, 10)
        .padding(.vertical, 4)
        .background(Capsule().fill(.ultraThinMaterial).environment(\.colorScheme, .dark))
        .allowsHitTesting(false)
    }

    // MARK: - Title bar

    private var titleBar: some View {
        HStack(spacing: 6) {
            // Surface type icon
            Image(systemName: isBrowser ? "globe" : "terminal")
                .font(.system(size: 9, weight: .medium))
                .foregroundStyle(isFocused ? .white.opacity(0.5) : .white.opacity(0.2))

            // Title
            Text(title.isEmpty ? "untitled" : title)
                .font(.system(size: 10, weight: .medium, design: .monospaced))
                .foregroundStyle(isFocused ? .white.opacity(0.7) : .white.opacity(0.3))
                .lineLimit(1)
                .truncationMode(.middle)

            Spacer()

            // Working indicator — the screen is moving, so a message/command
            // visibly triggered work.
            if isWorking {
                WorkingDots()
            }

            // Recording indicator
            if isTranscribing {
                HStack(spacing: 4) {
                    Circle()
                        .fill(Color.red)
                        .frame(width: 6, height: 6)
                        .shadow(color: .red.opacity(0.6), radius: 3)
                    Text("REC")
                        .font(.system(size: 8, weight: .bold, design: .monospaced))
                        .foregroundStyle(.red.opacity(0.8))
                }
            }
        }
        .padding(.horizontal, 10)
        .padding(.vertical, 6)
        .background(
            Rectangle()
                .fill(isFocused ? Color.white.opacity(0.06) : Color.white.opacity(0.02))
        )
        // Top corners only
        .clipShape(
            UnevenRoundedRectangle(
                topLeadingRadius: 12,
                bottomLeadingRadius: 0,
                bottomTrailingRadius: 0,
                topTrailingRadius: 12
            )
        )
    }

    private var borderColor: Color {
        if hasNotification { return Color.orange.opacity(0.6) }
        if isFocused { return Color.white.opacity(0.5) }
        return Color.white.opacity(0.08)
    }
}

/// Three dots pulsing in sequence — shown in a card's title bar while its
/// agent/command is actively producing output.
private struct WorkingDots: View {
    @State private var animating = false

    var body: some View {
        HStack(spacing: 2.5) {
            ForEach(0..<3, id: \.self) { i in
                Circle()
                    .fill(Color.green)
                    .frame(width: 3.5, height: 3.5)
                    .opacity(animating ? 0.15 : 0.9)
                    .animation(
                        .easeInOut(duration: 0.45)
                            .repeatForever(autoreverses: true)
                            .delay(Double(i) * 0.15),
                        value: animating
                    )
            }
        }
        .onAppear { animating = true }
        .onDisappear { animating = false }
    }
}

/// Identifies which surface's conversation history to present full-screen.
struct HistoryTarget: Identifiable, Equatable {
    let id: String
    let title: String
}

/// Full-screen reader for a claude surface's conversation. The card already
/// renders the same transcript inline; this is the roomier, larger-type view of
/// it. Opens pinned to the latest exchange, scroll up for older history.
struct TranscriptSheetView: View {
    @EnvironmentObject var appState: AppState
    let target: HistoryTarget

    var body: some View {
        ZStack {
            Color.black.ignoresSafeArea()
            VStack(spacing: 0) {
                header
                Divider().overlay(Color.white.opacity(0.1))
                body(for: appState.claudeTranscript[target.id] ?? "")
            }
        }
        .onAppear {
            // Always refetch on open so we show the latest session data, not a
            // stale cache from a previous viewing. Any cached text stays visible
            // (pinned to the bottom) until the fresh transcript replaces it.
            appState.loadClaudeTranscript(target.id, showsSpinner: true)
        }
    }

    private var header: some View {
        HStack(spacing: 12) {
            VStack(alignment: .leading, spacing: 2) {
                Text("Conversation history")
                    .font(.system(size: 13, weight: .semibold))
                    .foregroundStyle(.white.opacity(0.9))
                Text(target.title)
                    .font(.system(size: 10, design: .monospaced))
                    .foregroundStyle(.white.opacity(0.4))
                    .lineLimit(1)
                // Diagnostic: the session the bridge resolved this surface to.
                // Lets a "wrong session" report name the exact session at a glance.
                if let session = appState.claudeTranscriptSession[target.id], !session.isEmpty {
                    Text("session \(session.prefix(8))")
                        .font(.system(size: 9, design: .monospaced))
                        .foregroundStyle(.white.opacity(0.25))
                        .lineLimit(1)
                }
            }
            Spacer()
            Button { appState.loadClaudeTranscript(target.id, showsSpinner: true) } label: {
                Image(systemName: "arrow.clockwise").font(.system(size: 15))
            }
            .tint(.white.opacity(0.6))
            .disabled(appState.claudeTranscriptLoading.contains(target.id))
            Button { appState.presentedHistory = nil } label: {
                Image(systemName: "xmark.circle.fill").font(.system(size: 22))
            }
            .tint(.white.opacity(0.5))
        }
        .padding(.horizontal, 16)
        .padding(.vertical, 12)
    }

    @ViewBuilder
    private func body(for text: String) -> some View {
        if text.isEmpty {
            VStack(spacing: 10) {
                if appState.claudeTranscriptLoading.contains(target.id) {
                    ProgressView().tint(.white.opacity(0.4))
                    Text("Loading history…")
                        .font(.system(size: 11, design: .monospaced))
                        .foregroundStyle(.white.opacity(0.3))
                } else if appState.claudeTranscriptMissing.contains(target.id) {
                    // Say so plainly rather than showing the newest session in
                    // the same folder, which is a different conversation.
                    Text("This surface's session file is missing")
                        .font(.system(size: 12, design: .monospaced))
                        .foregroundStyle(.white.opacity(0.35))
                    Text("This surface is bound to session \(appState.claudeTranscriptSession[target.id]?.prefix(8) ?? ""), whose transcript is no longer on disk.")
                        .font(.system(size: 10, design: .monospaced))
                        .foregroundStyle(.white.opacity(0.25))
                        .multilineTextAlignment(.center)
                        .padding(.horizontal, 40)
                } else {
                    Text("No conversation history")
                        .font(.system(size: 12, design: .monospaced))
                        .foregroundStyle(.white.opacity(0.3))
                }
            }
            .frame(maxWidth: .infinity, maxHeight: .infinity)
        } else {
            TerminalTextView(text: text, fontSize: 12.5, textOpacity: 0.9, trimMarkdownLinks: true)
                .padding(.horizontal, 4)
        }
    }
}

/// History above the live emulator, scrolled as one document. Observes the
/// feed's geometry (not its frames) so the emulator's height and the history
/// trim follow the pane's grid without re-rendering on every frame.
private struct LiveCardContent: View {
    let feed: FrameFeed
    @ObservedObject private var geometry: FrameGeometry
    let historyText: String
    let historyIsPolledScreen: Bool
    let fontSize: CGFloat
    let scrollToBottomRequest: Int
    let onSizeChanged: ((CGSize) -> Void)?

    private static let liveID = "live-screen"

    init(feed: FrameFeed, historyText: String, historyIsPolledScreen: Bool, fontSize: CGFloat, scrollToBottomRequest: Int, onSizeChanged: ((CGSize) -> Void)?) {
        self.feed = feed
        _geometry = ObservedObject(wrappedValue: feed.geometry)
        self.historyText = historyText
        self.historyIsPolledScreen = historyIsPolledScreen
        self.fontSize = fontSize
        self.scrollToBottomRequest = scrollToBottomRequest
        self.onSizeChanged = onSizeChanged
    }

    private func liveHeight(width: CGFloat, fallback: CGFloat) -> CGFloat {
        LiveCellMetrics.fittedHeight(columns: geometry.columns, rows: geometry.rows, width: width) ?? fallback
    }

    private var history: String {
        historyIsPolledScreen
            ? FrameFit.historyAboveLiveScreen(polledText: historyText, usedRows: geometry.usedRows)
            : historyText
    }

    var body: some View {
        GeometryReader { geo in
        ScrollViewReader { proxy in
            ScrollView(.vertical) {
                VStack(alignment: .leading, spacing: 0) {
                    if !history.isEmpty {
                        StaticTerminalTextView(text: history, fontSize: fontSize, trimMarkdownLinks: !historyIsPolledScreen)
                        Rectangle()
                            .fill(Color.white.opacity(0.12))
                            .frame(height: 1)
                            .padding(.horizontal, 8)
                            .padding(.bottom, 4)
                    }
                    // The height comes from the published grid, not from the
                    // view's own measurement: a first full frame that lands
                    // after the card was laid out would otherwise leave the
                    // emulator at the zero height it was measured with until
                    // something else (the keyboard, a rotation) re-laid out
                    // the card. Until the grid is known the emulator fills the
                    // card, so the card never looks empty.
                    LivePaneView(feed: feed)
                        .frame(height: liveHeight(width: geo.size.width, fallback: geo.size.height))
                        .id(Self.liveID)
                }
                // Content shorter than the card sits at the top like a
                // terminal would, instead of being pushed to the bottom by
                // the bottom anchor below.
                .frame(maxWidth: .infinity, minHeight: geo.size.height, alignment: .topLeading)
            }
            .defaultScrollAnchor(.bottom)
            .scrollIndicators(.hidden)
            .onChange(of: geo.size, initial: true) { _, size in
                onSizeChanged?(size)
            }
            .onChange(of: scrollToBottomRequest) { _, _ in
                withAnimation(.easeOut(duration: 0.2)) {
                    proxy.scrollTo(Self.liveID, anchor: .bottom)
                }
            }
        }
        }
    }
}
