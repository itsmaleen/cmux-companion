import Foundation
import Combine
import UserNotifications
import UIKit

@MainActor
final class AppState: ObservableObject {
    @Published var connectionStatus: ConnectionStatus = .disconnected
    @Published var notifications: [BridgeNotification] = []
    @Published var workspaces: [Workspace] = []
    @Published var currentWorkspaceID: String?
    // `focusedSurfaceID` (what the layout shows) is derived from these plus
    // localFocusedSurfaceID, so the live frame stream follows every change to
    // them — a workspace switch, for instance, only learns its focused
    // surface once pane.list answers, well after focusSurface ran.
    @Published var surfaces: [Surface] = [] {
        didSet { reconcileFrameSubscription() }
    }
    @Published var panes: [Pane] = [] {
        didSet { reconcileFrameSubscription() }
    }
    @Published var isPairingPresented = false
    // Paired bridges (Macs) and which one is currently active. Persisted in the
    // Keychain via BridgeStore; a single phone can hold several and switch.
    @Published var bridges: [SavedBridge] = []
    @Published var selectedBridgeID: UUID?
    // Tracks the last surface explicitly focused by the user; used when surface.list
    // doesn't return is_focused and pane.list is unavailable.
    @Published private(set) var localFocusedSurfaceID: String? {
        didSet { reconcileFrameSubscription() }
    }
    @Published var surfaceContent: [String: String] = [:]
    // Surfaces whose screen moved on their most recent read_text poll. TUIs
    // repaint continuously while an agent/command runs and go static when
    // idle, so "the last poll saw a change" works as a liveness signal with
    // no protocol support. Drives the working indicator on pane cards.
    @Published var workingSurfaces: Set<String> = []
    // The read depth last used per surface; a depth flip (focus change swaps
    // 50-line previews for deep reads) changes content without meaning the
    // surface is active, so those polls don't update workingSurfaces.
    private var lastReadDepth: [String: Int] = [:]
    // One pending activity probe per surface — rapid key taps coalesce into
    // the newest instead of queueing a deep read per tap (this codebase has
    // been RPC-flooded before; see startContentPolling's debounce).
    private var pendingProbes: [String: DispatchWorkItem] = [:]
    // Per-surface read ordering. Two in-flight reads' detached trims can
    // finish out of order; a response older than the newest applied one is
    // dropped instead of rolling content back (and casting a bogus
    // workingSurfaces vote from the rollback).
    private var readSeq: [String: Int] = [:]
    private var appliedReadSeq: [String: Int] = [:]
    @Published var browserURLs: [String: String] = [:]
    // Which terminal runtime the connected bridge fronts ("cmux" or "herdr")
    // and what it can do. Set from the `connected` payload; a protocol-1 bridge
    // is cmux. Views hide affordances the runtime can't serve (browser
    // surfaces) and prefer its agent status over read-diffing when it has one.
    @Published var backendKind: String = "cmux"
    @Published var capabilities: BackendCapabilities = .cmux
    // Which runtime's workspaces to show when the bridge fronts more than one
    // (a composite bridge reports backend "cmux+herdr" and namespaces every id
    // by runtime). Purely a phone-side view filter: switching is instant and
    // the bridge keeps serving both. Persisted across launches.
    @Published var runtimeFilter: RuntimeFilter = RuntimeFilter.stored {
        didSet {
            guard runtimeFilter != oldValue else { return }
            UserDefaults.standard.set(runtimeFilter.rawValue, forKey: RuntimeFilter.storageKey)
            enforceRuntimeFilter()
        }
    }
    // Runtime-reported agent status per surface (idle/working/blocked/done/
    // unknown), from surface.list and surface.updated pushes. Only populated
    // when the backend has the agent_status capability; a surface present here
    // takes its working indicator from this instead of the read-diff vote.
    @Published var agentStatus: [String: String] = [:]
    // Which sidebar tab is showing. Owned here (not in MainTabView) so a tapped
    // notification can bring the relevant surface into view on the Layout tab.
    @Published var selectedTab: SidebarTab = .layout

    // MARK: - Live terminal frames
    //
    // Only the FOCUSED surface streams frames; the secondary strip keeps the
    // polled text. A feed is high-frequency (sub-second deltas), so its
    // payloads never go through @Published — only the existence of a
    // surface's ScreenModel does, which is all a card needs to decide whether to
    // show the live screen or fall back to polled text.

    /// One live screen per surface currently streaming (the focused surface,
    /// once its subscribe succeeds). Only the dictionary's KEYS are meaningful
    /// to SwiftUI; a card checks `screenModels[id] != nil` to switch modes and
    /// then observes the ScreenModel itself, which publishes a version bump
    /// per update — updates never go through this dictionary.
    @Published private(set) var screenModels: [String: ScreenModel] = [:]
    /// Surfaces whose backend answered `unsupported` to
    /// `surface.frames.subscribe` — not re-probed on every focus.
    @Published private(set) var framesUnsupported: Set<String> = []
    /// Surfaces whose real pane is currently resized to the phone's grid
    /// ("fit to phone"), with that grid. herdr only; released when focus
    /// moves away, on disconnect, or when the bridge reports the fit ended.
    @Published private(set) var fittedSurfaces: [String: FitGrid] = [:]
    /// The last size each surface's live card reported, so a fit can be
    /// computed (and re-computed after a rotation) without asking the view.
    private var liveCardSizes: [String: CGSize] = [:]
    /// Surfaces with a `surface.frames.subscribe` in flight or acknowledged.
    /// Distinct from `screenModels`'s keys only during the brief window between
    /// sending the subscribe and its response landing.
    private var framesSubscribed: Set<String> = []
    /// The surface frames are (or should be) streaming for — the target
    /// `updateFrameSubscription` reconciles `framesSubscribed`/`screenModels`
    /// against. Tracked separately from `focusedSurfaceID` (a computed
    /// property derived from panes/surfaces) so an in-flight subscribe can
    /// tell whether focus moved on while it was outstanding.
    private var frameFocusedSurfaceID: String?

    /// True for the in-memory session seeded by `-UITestFixture frames` (see
    /// `seedFixtureState`). Lets views skip work that only makes sense with a
    /// real bridge/device — e.g. requesting microphone permission, which
    /// would otherwise cover every screenshot with a system alert.
    private(set) var isFixtureMode = false

    // The live instance, so AppDelegate can route a tapped notification to it.
    // Weak so it doesn't keep a torn-down state alive.
    static private(set) weak var current: AppState?
    // A notification tapped before any AppState existed (cold launch straight
    // from a notification). Drained in init.
    private static var pendingLaunchTap: (surfaceID: String, workspaceID: String?)?
    // A surface to focus once the bridge finishes connecting, for a tap that
    // arrived while the session was still coming up. Applied in clientDidConnect.
    private var pendingNavigationTarget: (surfaceID: String, workspaceID: String?)?

    private var lastFocusedSurface: [String: String] = [:]
    // Claude conversation transcripts, kept separate from surfaceContent (which
    // mirrors the live terminal). Keyed by surface ID.
    @Published var claudeTranscript: [String: String] = [:]
    // What a claude surface's card renders: its transcript with the live
    // terminal viewport appended. Composed here, once per change, rather than in
    // the view — a SwiftUI body runs far more often than the text changes, and
    // this string is tens of kilobytes.
    @Published var claudeCardText: [String: String] = [:]
    // The session id the bridge resolved each surface's transcript to, keyed by
    // surface ID. Surfaced in the history viewer for diagnosing wrong-session reports.
    @Published var claudeTranscriptSession: [String: String] = [:]
    // Fingerprint of the transcript rendering we already hold, echoed back to
    // the bridge so an unchanged transcript costs a tiny response instead of
    // re-sending the whole conversation on every poll.
    private var claudeTranscriptFingerprint: [String: String] = [:]
    // Every in-flight transcript request, so polls don't stack up. Deliberately
    // NOT the published loading set: flipping that twice per poll would fire
    // objectWillChange — and re-render the layout — every few seconds.
    private var claudeTranscriptInFlight: Set<String> = []
    // Per-surface request generation. The timeout safety net below fires 15s
    // after ITS OWN request, by which time a newer request may hold the
    // in-flight mark — clearing it then would let polls stack up and race. Each
    // request stamps a generation and only clears state it still owns. Same
    // guard applies to the response: two responses' detached trims can finish
    // out of order, and applying the older one last leaves stale text pinned
    // under a current fingerprint, which `unchanged` then keeps forever.
    private var claudeTranscriptSeq: [String: Int] = [:]
    private var claudeTranscriptAppliedSeq: [String: Int] = [:]
    // In-flight requests the user is waiting on (history viewer open, explicit
    // refresh), which are the only ones that show a spinner.
    @Published var claudeTranscriptLoading: Set<String> = []
    // Surfaces bound to a session whose transcript file is gone. Distinguishes
    // "this conversation's file no longer exists" from "nothing said yet" —
    // both arrive as empty text.
    @Published var claudeTranscriptMissing: Set<String> = []
    // Non-nil while the full-screen conversation-history viewer is presented.
    @Published var presentedHistory: HistoryTarget?
    // Short-lived confirmation or failure text for an image paste, shown over the
    // focused card. Images are the one action whose result isn't obvious from the
    // terminal alone — a path scrolls by, an error is silent.
    @Published var imagePasteStatus: String?
    private var imagePasteStatusClear: DispatchWorkItem?
    // An image attached to the compose bar, waiting for the user to add a
    // message and send. Held here (not in the input bar) so the "Paste Image"
    // quick action and the bar's attach button feed the same pending slot, and
    // so sending composes the image and the typed text into one message.
    @Published var pendingAttachment: ImagePaste.Attachment?
    // Flipped by the "Add File" quick action to ask the layout to open the
    // Photo / File / Paste menu (the quick-action builder has no view state).
    @Published var addFileRequested = false
    // An attach is encoding right now. A send issued during this window must
    // wait for the image rather than going out as text alone (which is how the
    // caption and image ended up as two messages).
    private var attachInFlight = false
    // Carries the workspace the send was issued from, so a deferred send doesn't
    // pair its original surface with whatever workspace is current when the
    // attachment finally lands (which, on a composite bridge, can name a
    // different runtime and be rejected).
    private var deferredComposedSend: (text: String, withEnter: Bool, surfaceID: String, workspaceID: String?)?
    // Monotonic token for the latest attach request. A detached encode that
    // finishes after a newer selection — or after switching bridges — carries a
    // stale generation and is dropped, so it can't overwrite the current
    // attachment or resurrect one past a session reset.
    private var attachGeneration = 0
    private let bridgeStore = BridgeStore()
    private var client: BridgeClient?
    private var discovery: BridgeDiscovery?

    init() {
        // Screenshot fixture mode: `-UITestFixture frames` seeds a fully
        // in-memory session (no bridge, no keychain/pairing store touched) so
        // ios/scripts/screenshots.sh can capture the live-frame card without a
        // Mac. Argument-gated only — there's no way to reach this from normal
        // app use.
        if Self.launchArgument("-UITestFixture") == "frames" {
            seedFixtureState(focusedSurfaceID: Self.launchArgument("-UITestFocus"))
            Self.current = self
            return
        }
        let loaded = bridgeStore.loadAll()
        bridges = loaded.bridges
        selectedBridgeID = loaded.selectedID ?? loaded.bridges.first?.id
        startDiscovery()
        connectSelected()
        requestNotificationPermission()
        Self.current = self
        if let tap = Self.pendingLaunchTap {
            Self.pendingLaunchTap = nil
            navigateToSurface(surfaceID: tap.surfaceID, workspaceID: tap.workspaceID)
        }
    }

    /// Reads a `-flag value` pair from the process's launch arguments, the
    /// way `xcrun simctl launch <bundle> -UITestFixture frames` passes them.
    /// Checked directly against `ProcessInfo.arguments` rather than through
    /// `UserDefaults`'s argument-domain registration so fixture mode can't be
    /// tripped by a stale default surviving between launches.
    private static func launchArgument(_ flag: String) -> String? {
        let args = ProcessInfo.processInfo.arguments
        guard let index = args.firstIndex(of: flag), index + 1 < args.count else { return nil }
        return args[index + 1]
    }

    /// Seeds a self-contained session for screenshot fixture mode: one shell
    /// surface and one claude surface, a fake conversation, and their live
    /// frame feeds loaded from the bundled `Fixtures/frames-*.ndjson` files.
    /// No bridge is created and the keychain-backed BridgeStore is never
    /// touched.
    private func seedFixtureState(focusedSurfaceID: String?) {
        isFixtureMode = true
        let shellID = "herdr:w2:p7"
        let claudeID = "herdr:w1:p1"
        let workspaceID = "fixture"

        // ContentView shows PairingView whenever `bridges` is empty,
        // regardless of `connectionStatus` — this in-memory-only bridge (never
        // passed to `bridgeStore.persist`, so the keychain is never touched)
        // is just enough to satisfy that check and reach MainTabView.
        let fixtureBridge = SavedBridge(
            name: "fixture",
            credentials: PairingCredentials(host: "fixture", port: 0, token: "fixture")
        )
        bridges = [fixtureBridge]
        selectedBridgeID = fixtureBridge.id

        connectionStatus = .connected
        backendKind = "herdr"
        capabilities = BackendCapabilities(browser: false, agentStatus: true, notifications: "polled")
        let workspaceDict: [String: Any] = ["id": workspaceID, "title": "fixture"]
        workspaces = [Workspace(workspaceDict)].compactMap { $0 }
        currentWorkspaceID = workspaceID
        let shellDict: [String: Any] = [
            "id": shellID, "type": "terminal", "title": "~/interview-prep",
            "workspace_id": workspaceID, "is_focused": false
        ]
        let claudeDict: [String: Any] = [
            "id": claudeID, "type": "terminal", "title": "Herdr companion app integration",
            "workspace_id": workspaceID, "is_focused": false,
            "resume_binding": ["kind": "claude"] as [String: Any]
        ]
        surfaces = [Surface(shellDict), Surface(claudeDict)].compactMap { $0 }

        claudeTranscript[claudeID] = Self.fixtureClaudeTranscript
        recomposeClaudeCard(claudeID)
        // The shell surface has no transcript fallback, so its SECONDARY
        // (non-focused) tile — which shows polled text, not the live frame
        // feed — would otherwise sit on the perpetual "Loading…" state.
        surfaceContent[shellID] = "$ bun test\n11 pass, 0 fail"

        let focusID = focusedSurfaceID ?? claudeID
        localFocusedSurfaceID = focusID
        frameFocusedSurfaceID = focusID

        loadFixtureScreen(surfaceID: shellID, resourceName: "screen-shell")
        loadFixtureScreen(surfaceID: claudeID, resourceName: "screen-claude")
    }

    /// A short fake conversation for the fixture claude surface's card.
    private static let fixtureClaudeTranscript = """
    ▌ You
    Can you wire the herdr companion app integration into the iOS layout?

    ▌ Claude
    Looked at WorkspaceLayoutView and PaneCardView — the focused-card path is
    the one that needs the new live view; the strip can keep polling text.

    ▌ You
    Sounds right. Keep the fallback for surfaces without frame support.

    ▌ Claude
    Doing that now — an `unsupported` subscribe error just drops the surface
    back to the plain text card, no visible error state.

    ▌ You
    Good. Ping me once the screenshots look right.

    ▌ Claude
    Working on it — building the fixture pane now.
    """

    /// Loads one bundled `Fixtures/<resourceName>.json` file — a recorded
    /// `surface.screen` full update — into a fresh ScreenModel for
    /// `surfaceID`. A missing/unparsable resource just leaves that surface
    /// without a model — its card falls back to plain text, same as a real
    /// `unsupported` surface would.
    private func loadFixtureScreen(surfaceID: String, resourceName: String) {
        guard let url = Bundle.main.url(forResource: resourceName, withExtension: "json", subdirectory: "Fixtures")
            ?? Bundle.main.url(forResource: resourceName, withExtension: "json"),
              let data = try? Data(contentsOf: url),
              let update = try? JSONDecoder().decode(ScreenUpdate.self, from: data) else {
            return
        }
        let model = ScreenModel()
        model.apply(update)
        screenModels[surfaceID] = model
    }

    // MARK: - Notification navigation

    /// Routes a tapped notification to the live AppState, or stashes it until one
    /// comes up (cold launch straight from a notification).
    static func handleNotificationTap(surfaceID: String, workspaceID: String?) {
        if let current {
            current.navigateToSurface(surfaceID: surfaceID, workspaceID: workspaceID)
        } else {
            pendingLaunchTap = (surfaceID, workspaceID)
        }
    }

    /// Brings the surface referenced by a tapped notification into view: shows the
    /// Layout tab and, once connected, selects its workspace and focuses it.
    func navigateToSurface(surfaceID: String, workspaceID: String?) {
        selectedTab = .layout
        guard connectionStatus.isConnected else {
            // Cold launch: apply once clientDidConnect fires.
            pendingNavigationTarget = (surfaceID, workspaceID)
            return
        }
        applyNavigation(surfaceID: surfaceID, workspaceID: workspaceID)
    }

    private func applyNavigation(surfaceID: String, workspaceID: String?) {
        // An alert from the runtime the filter hides: show that runtime rather
        // than selecting a workspace the strip can't display.
        if let runtime = runtime(ofID: workspaceID ?? surfaceID),
           let target = RuntimeFilter(rawValue: runtime),
           runtimeFilter != .all, runtimeFilter != target {
            runtimeFilter = target
        }
        // Focus only after the workspace switch lands: selectWorkspace's
        // completion restores that workspace's last-remembered focus, which
        // would overwrite an eagerly applied one — and focusSurface records
        // lastFocusedSurface under currentWorkspaceID, which is stale until
        // the completion updates it.
        if let wsID = workspaceID, wsID != currentWorkspaceID {
            selectWorkspace(wsID) { [weak self] in
                self?.finishNavigation(surfaceID: surfaceID)
            }
        } else {
            finishNavigation(surfaceID: surfaceID)
        }
    }

    private func finishNavigation(surfaceID: String) {
        focusSurface(surfaceID)
        // That surface is now on screen, so its pending notifications are stale.
        if notifications.contains(where: { $0.surfaceID == surfaceID }) {
            notifications.removeAll { $0.surfaceID == surfaceID }
        }
    }

    // MARK: - Runtimes

    /// The runtimes behind the connected bridge, e.g. ["cmux", "herdr"].
    var availableRuntimes: [String] {
        backendKind.split(separator: "+").map(String.init).filter { !$0.isEmpty }
    }

    /// Whether the bridge fronts more than one runtime, so the filter applies.
    var isComposite: Bool { availableRuntimes.count > 1 }

    /// The workspaces the current runtime filter lets through.
    var visibleWorkspaces: [Workspace] {
        guard isComposite, let runtime = runtimeFilter.runtime else { return workspaces }
        return workspaces.filter { $0.backend == runtime }
    }

    /// A workspace's label, tagged with its runtime when both are on screen.
    func displayTitle(for ws: Workspace) -> String {
        guard isComposite, runtimeFilter == .all, let backend = ws.backend else { return ws.title }
        return "\(ws.title) · \(backend)"
    }

    /// The runtime an id (workspace or surface) belongs to, from its namespace.
    func runtime(ofID id: String) -> String? {
        guard isComposite, let colon = id.firstIndex(of: ":") else { return nil }
        let prefix = String(id[..<colon])
        return availableRuntimes.contains(prefix) ? prefix : nil
    }

    /// Keeps the current workspace inside the filter: when the filter hides it,
    /// the first visible workspace takes over.
    private func enforceRuntimeFilter() {
        guard isComposite, connectionStatus.isConnected else { return }
        let visible = visibleWorkspaces
        if let current = currentWorkspaceID, visible.contains(where: { $0.id == current }) { return }
        if let first = visible.first {
            selectWorkspace(first.id)
        }
    }

    /// The bridge whose session is (or should be) active.
    var selectedBridge: SavedBridge? {
        if let id = selectedBridgeID, let b = bridges.first(where: { $0.id == id }) { return b }
        return bridges.first
    }

    private func requestNotificationPermission() {
        UNUserNotificationCenter.current().requestAuthorization(options: [.alert, .sound, .badge]) { granted, _ in
            print("[Notification] push permission: \(granted)")
        }
    }

    private func scheduleLocalNotification(_ n: BridgeNotification) {
        let center = UNUserNotificationCenter.current()
        let content = UNMutableNotificationContent()
        content.title = n.title
        if let subtitle = n.subtitle { content.subtitle = subtitle }
        if let body = n.body { content.body = body }
        content.sound = .default
        // Carry the surface/workspace so tapping the notification can bring that
        // surface into view (see AppDelegate.userNotificationCenter(_:didReceive:)).
        var info: [String: String] = [:]
        if let sid = n.surfaceID { info["surface_id"] = sid }
        if let wid = n.workspaceID { info["workspace_id"] = wid }
        content.userInfo = info

        let request = UNNotificationRequest(
            identifier: n.id,
            content: content,
            trigger: nil // deliver immediately
        )
        center.add(request)
    }

    // MARK: - Pairing

    // A parsed pairing request awaiting the user's confirmation because it would
    // add a new, not-yet-trusted bridge and switch input to it (a hostile
    // QR/deep-link could otherwise silently redirect all keystrokes).
    @Published var pendingPairing: PairingCredentials?

    /// Handles a pairing URL. `trusted` is true when the user initiated the
    /// pairing inside the app (scanning a QR from the pairing sheet, or manual
    /// entry) — those are explicit actions and are committed directly. It is
    /// false for an external `cmux-bridge://` deep link opened by another app,
    /// where a new bridge must be confirmed before it can take over input.
    func handlePairingURL(_ url: URL, trusted: Bool = false) {
        guard url.scheme == "cmux-bridge",
              url.host == "pair",
              let components = URLComponents(url: url, resolvingAgainstBaseURL: false),
              let host = components.queryItems?.first(where: { $0.name == "host" })?.value,
              let portStr = components.queryItems?.first(where: { $0.name == "port" })?.value,
              let port = Int(portStr),
              let token = components.queryItems?.first(where: { $0.name == "token" })?.value
        else { return }

        let tailscaleHost = components.queryItems?.first(where: { $0.name == "tailscale_host" })?.value
        let backend = components.queryItems?.first(where: { $0.name == "backend" })?.value
        let credentials = PairingCredentials(host: host, port: port, token: token, tailscaleHost: tailscaleHost, backend: backend)

        // A bridge we already trust (same token) is just refreshing its network
        // coordinates — update it in place and switch to it, no confirmation.
        if bridges.contains(where: { $0.credentials.token == token }) {
            commitPairing(credentials)
            return
        }
        // Trust-on-first-use, or an explicit in-app pairing action: commit now.
        if bridges.isEmpty || trusted {
            commitPairing(credentials)
            return
        }
        // A new, unknown bridge arriving via an external deep link while others
        // exist: confirm before switching input to it.
        pendingPairing = credentials
    }

    /// Adds or updates a bridge from confirmed credentials, makes it active, and
    /// connects. A bridge with a matching token is updated in place (keeping its
    /// name); otherwise a new one is appended.
    func commitPairing(_ credentials: PairingCredentials) {
        let bridge: SavedBridge
        if let idx = bridges.firstIndex(where: { $0.credentials.token == credentials.token }) {
            bridges[idx].credentials = credentials
            bridge = bridges[idx]
        } else {
            bridge = SavedBridge(name: SavedBridge.defaultName(for: credentials), credentials: credentials)
            bridges.append(bridge)
        }
        selectedBridgeID = bridge.id
        persistBridges()
        resetSessionState()
        connect(to: bridge.credentials)
        isPairingPresented = false
        pendingPairing = nil
    }

    func confirmPendingPairing() {
        guard let pending = pendingPairing else { return }
        commitPairing(pending)
    }

    func cancelPendingPairing() {
        pendingPairing = nil
    }

    // MARK: - Bridge management

    /// Switches the active session to another paired bridge.
    func selectBridge(_ id: UUID) {
        guard id != selectedBridgeID, let bridge = bridges.first(where: { $0.id == id }) else { return }
        selectedBridgeID = id
        persistBridges()
        resetSessionState()
        connect(to: bridge.credentials)
    }

    /// Renames a paired bridge; an empty name falls back to the derived default.
    func renameBridge(_ id: UUID, to name: String) {
        guard let idx = bridges.firstIndex(where: { $0.id == id }) else { return }
        let trimmed = name.trimmingCharacters(in: .whitespacesAndNewlines)
        bridges[idx].name = trimmed.isEmpty ? SavedBridge.defaultName(for: bridges[idx].credentials) : trimmed
        persistBridges()
    }

    /// Removes a paired bridge. If it was the active one, switches to another
    /// (or goes disconnected when none remain).
    func removeBridge(_ id: UUID) {
        let wasSelected = (id == selectedBridgeID)
        bridges.removeAll { $0.id == id }
        if wasSelected {
            client?.disconnect()
            client = nil
            selectedBridgeID = bridges.first?.id
            resetSessionState()
            if let next = selectedBridge {
                connect(to: next.credentials)
            } else {
                connectionStatus = .disconnected
            }
        }
        persistBridges()
    }

    /// Removes every paired bridge and returns to the pairing screen.
    func unpairAll() {
        client?.disconnect()
        client = nil
        bridges = []
        selectedBridgeID = nil
        persistBridges()
        resetSessionState()
        connectionStatus = .disconnected
    }

    private func persistBridges() {
        bridgeStore.persist(bridges: bridges, selectedID: selectedBridgeID)
    }

    /// Clears all per-connection UI state so switching bridges doesn't briefly
    /// show the previous Mac's workspaces/surfaces/notifications.
    private func resetSessionState() {
        notifications = []
        workspaces = []
        currentWorkspaceID = nil
        surfaces = []
        panes = []
        localFocusedSurfaceID = nil
        surfaceContent = [:]
        workingSurfaces = []
        agentStatus = [:]
        lastReadDepth = [:]
        readSeq = [:]
        appliedReadSeq = [:]
        for probe in pendingProbes.values { probe.cancel() }
        pendingProbes = [:]
        browserURLs = [:]
        claudeTranscript = [:]
        claudeCardText = [:]
        claudeTranscriptSession = [:]
        claudeTranscriptFingerprint = [:]
        claudeTranscriptInFlight = []
        claudeTranscriptSeq = [:]
        claudeTranscriptAppliedSeq = [:]
        claudeTranscriptLoading = []
        claudeTranscriptMissing = []
        presentedHistory = nil
        imagePasteStatusClear?.cancel()
        imagePasteStatus = nil
        pendingAttachment = nil
        addFileRequested = false
        attachInFlight = false
        // Invalidate any in-flight encode so a task started for the previous
        // bridge can't land an attachment (or a send) in the new session.
        attachGeneration += 1
        deferredComposedSend = nil
        lastFocusedSurface = [:]
        screenModels = [:]
        framesSubscribed = []
        framesUnsupported = []
        fittedSurfaces = [:]
        frameFocusedSurfaceID = nil
    }

    // MARK: - Connection

    func connectSelected() {
        guard let bridge = selectedBridge else { return }
        connect(to: bridge.credentials)
    }

    func connect(to credentials: PairingCredentials) {
        client?.disconnect()
        connectionStatus = .connecting
        let newClient = BridgeClient(credentials: credentials)
        newClient.delegate = self
        client = newClient
        newClient.connect()
    }

    // MARK: - Discovery

    private func startDiscovery() {
        let disc = BridgeDiscovery()
        disc.delegate = self
        self.discovery = disc
        disc.start()
    }

    // MARK: - Commands

    func selectWorkspace(_ id: String, then completion: (() -> Void)? = nil) {
        send(method: "workspace.select", params: ["workspace_id": id]) { [weak self] _ in
            self?.currentWorkspaceID = id
            self?.localFocusedSurfaceID = self?.lastFocusedSurface[id]
            self?.refreshSurfaces()
            self?.refreshPanes()
            completion?()
        }
    }

    func focusSurface(_ id: String) {
        localFocusedSurfaceID = id
        if let wsID = currentWorkspaceID {
            lastFocusedSurface[wsID] = id
        }
        // Immediately deep-read the newly focused surface so its content is
        // there right away instead of waiting for the next poll.
        let surface = surfaces.first(where: { $0.id == id })
        if surface?.isBrowser != true {
            readSurfaceText(id, lines: Self.focusedHistoryLines)
        }
        // Same for a claude/opencode surface's conversation, which IS its card
        // content.
        if surface?.hasTranscript == true {
            loadClaudeTranscript(id)
        }
        updateFrameSubscription(focusedSurfaceID: id)
        send(method: "surface.focus", params: ["surface_id": id]) { [weak self] _ in
            self?.refreshSurfaces()
            self?.refreshPanes()
        }
    }

    func focusPane(_ id: String) {
        send(method: "pane.focus", params: ["pane_id": id]) { [weak self] _ in
            self?.refreshPanes()
        }
    }

    func togglePaneZoom(_ surfaceID: String) {
        sendKey("cmd+shift+enter", to: surfaceID)
    }

    func cycleSurface() {
        let list = surfaces
        guard !list.isEmpty else { return }
        let currentIndex = list.firstIndex(where: { $0.id == focusedSurfaceID }) ?? -1
        let nextIndex = (currentIndex + 1) % list.count
        focusSurface(list[nextIndex].id)
    }

    func cycleSurfaceBackward() {
        let list = surfaces
        guard !list.isEmpty else { return }
        // Match cycleSurface's "nothing focused" fallback (-1) so forward and
        // backward agree on where cycling starts from an unfocused state.
        let currentIndex = list.firstIndex(where: { $0.id == focusedSurfaceID }) ?? -1
        let prevIndex = (currentIndex - 1 + list.count) % list.count
        focusSurface(list[prevIndex].id)
    }

    func cycleWorkspace() {
        let list = visibleWorkspaces
        guard !list.isEmpty else { return }
        let currentIndex = list.firstIndex(where: { $0.id == currentWorkspaceID }) ?? -1
        let nextIndex = (currentIndex + 1) % list.count
        selectWorkspace(list[nextIndex].id)
    }

    func cyclePane() {
        guard !panes.isEmpty else { return }
        let focusedIndex = panes.firstIndex(where: { $0.isFocused }) ?? -1
        let nextIndex = (focusedIndex + 1) % panes.count
        focusPane(panes[nextIndex].id)
    }

    func cycleTabInFocusedPane() {
        guard let focused = panes.first(where: { $0.isFocused }),
              focused.surfaceIDs.count > 1 else { return }
        let currentIndex = focused.surfaceIDs.firstIndex(of: focused.focusedSurfaceID ?? "") ?? -1
        let nextIndex = (currentIndex + 1) % focused.surfaceIDs.count
        focusSurface(focused.surfaceIDs[nextIndex])
    }

    func sendText(_ text: String, to surfaceID: String) {
        send(method: "surface.send_text", params: ["surface_id": surfaceID, "text": text])
        scheduleActivityProbe(for: surfaceID)
    }

    func sendKey(_ key: String, to surfaceID: String) {
        send(method: "surface.send_key", params: ["surface_id": surfaceID, "key": key])
        scheduleActivityProbe(for: surfaceID)
    }

    /// Sending input is the moment the user most wants to see whether work
    /// started, and a background surface's next poll can be ~15s out — probe
    /// it sooner so the working indicator reacts promptly.
    private func scheduleActivityProbe(for surfaceID: String) {
        pendingProbes[surfaceID]?.cancel()
        let probe = DispatchWorkItem { [weak self] in
            guard let self else { return }
            self.pendingProbes[surfaceID] = nil
            // Probe at the depth this surface was last read at, so the
            // response diffs against comparable content and always gets to
            // vote — the focused-vs-background depth can flip between
            // scheduling and firing.
            let depth = self.lastReadDepth[surfaceID]
                ?? (surfaceID == self.focusedSurfaceID ? Self.focusedHistoryLines : 50)
            self.readSurfaceText(surfaceID, lines: depth)
        }
        pendingProbes[surfaceID] = probe
        DispatchQueue.main.asyncAfter(deadline: .now() + 1.5, execute: probe)
    }

    /// Flip a surface's working flag, mutating the published set only on real
    /// transitions so idle polls don't fire objectWillChange.
    private func setWorking(_ working: Bool, for surfaceID: String) {
        if working != workingSurfaces.contains(surfaceID) {
            if working {
                workingSurfaces.insert(surfaceID)
            } else {
                workingSurfaces.remove(surfaceID)
            }
        }
    }

    /// Reads a surface's text. cmux's `surface.read_text` returns the last
    /// `lines` rows *including terminal scrollback* — so a large `lines` value
    /// yields the scrollable history directly, with no separate load step. The
    /// focused surface asks for a deep window (history); background surfaces a
    /// shallow one (cheap live preview). Note: full-screen TUIs like claude-code
    /// repaint a fixed viewport and keep little/no terminal scrollback, so for
    /// those this returns roughly the visible screen — that's a cmux limitation,
    /// not a bug here. See [[project_cmux_no_scrollback]].
    func readSurfaceText(_ surfaceID: String, lines: Int = 50) {
        let seq = (readSeq[surfaceID] ?? 0) + 1
        readSeq[surfaceID] = seq
        var params: [String: Any] = ["surface_id": surfaceID, "lines": lines]
        if let wsID = currentWorkspaceID {
            params["workspace_id"] = wsID
        }
        send(method: "surface.read_text", params: params) { [weak self] result in
            guard self != nil, let text = result["text"] as? String else { return }
            // Trim/cap off the main actor: a deep focused read is thousands of
            // lines, and doing that inline stalls whatever animation (keyboard,
            // card resize) happens to be in flight when the poll lands.
            Task.detached(priority: .userInitiated) { [weak self] in
                let content = Self.capHistoryLines(Self.trimTerminalText(text))
                await MainActor.run {
                    guard let self else { return }
                    // Never apply (or let vote) a response that lost the race
                    // to a newer one — it would roll live content back.
                    guard seq > (self.appliedReadSeq[surfaceID] ?? 0) else { return }
                    self.appliedReadSeq[surfaceID] = seq
                    let previous = self.surfaceContent[surfaceID]
                    // A depth-consistent poll that sees movement means the
                    // surface is working; an identical read means idle. A poll
                    // at a new depth changed the text without implying
                    // activity, so it doesn't vote.
                    // A backend that reports agent status (herdr) is
                    // authoritative for surfaces it has classified; only
                    // unclassified ones fall back to the diff heuristic.
                    if self.agentStatus[surfaceID] == nil,
                       self.lastReadDepth[surfaceID] == lines || previous == nil {
                        self.setWorking(previous != nil && previous != content, for: surfaceID)
                    }
                    self.lastReadDepth[surfaceID] = lines
                    // Diff before assigning: a no-op write to a @Published
                    // dictionary still fires objectWillChange every poll.
                    guard previous != content else { return }
                    self.surfaceContent[surfaceID] = content
                    // A claude card shows this viewport below its conversation.
                    self.recomposeClaudeCard(surfaceID)
                }
            }
        }
    }

    /// How many rows to request for the focused surface — deep enough to scroll
    /// back through, bounded so a huge scrollback can't produce a payload that
    /// stalls the socket or an attributed string UITextView can't lay out.
    static let focusedHistoryLines = 1500

    // MARK: - Live terminal frames

    /// Reconciles which surface should be streaming frames with `newFocus`.
    /// Called on every focus change (explicit `focusSurface`, and the
    /// auto-select paths in `refreshSurfaces`/`refreshSurfacesAndFocusNew`).
    /// A no-op when focus didn't actually move, so re-running the auto-select
    /// branches on an unrelated refresh doesn't tear down and re-subscribe a
    /// feed that's already correct.
    /// Points the frame stream at whatever surface the layout currently
    /// shows. Cheap when nothing changed; called from the property observers
    /// on surfaces/panes/localFocusedSurfaceID so no code path that moves
    /// focus can leave the stream on the previous surface.
    private func reconcileFrameSubscription() {
        updateFrameSubscription(focusedSurfaceID: focusedSurfaceID)
    }

    private func updateFrameSubscription(focusedSurfaceID newFocus: String?) {
        guard frameFocusedSurfaceID != newFocus else { return }
        if let previous = frameFocusedSurfaceID {
            // A fit changes the pane on the Mac; it is only ever held for the
            // surface on screen.
            releaseFit(previous)
            unsubscribeFrames(previous)
        }
        frameFocusedSurfaceID = newFocus
        guard let surfaceID = newFocus else { return }
        subscribeFrames(for: surfaceID)
    }

    /// Sends `surface.frames.subscribe` for `surfaceID` and, on success,
    /// creates its ScreenModel. A backend that answers `unsupported` is
    /// remembered in `framesUnsupported` so later focuses don't re-probe it;
    /// any other failure (not_found, frames_error, a dropped connection) just
    /// leaves the surface without a feed — its card falls back to text.
    private func subscribeFrames(for surfaceID: String) {
        guard !framesUnsupported.contains(surfaceID) else { return }
        guard !framesSubscribed.contains(surfaceID) else { return }
        guard connectionStatus.isConnected else { return }
        framesSubscribed.insert(surfaceID)
        var params: [String: Any] = ["surface_id": surfaceID]
        if let grid = fittedSurfaces[surfaceID] {
            // Observe at the fitted grid, not the layout's: herdr's observer
            // crops/pads to whatever size it is asked for.
            params["cols"] = grid.columns
            params["rows"] = grid.rows
        }
        sendRaw(method: "surface.screen.subscribe", params: params) { [weak self] response in
            guard let self else { return }
            // Focus moved on while this was in flight — undo it rather than
            // stream frames for a surface that's no longer on screen.
            guard self.frameFocusedSurfaceID == surfaceID else {
                self.framesSubscribed.remove(surfaceID)
                if response.ok {
                    self.sendRaw(method: "surface.screen.unsubscribe", params: ["surface_id": surfaceID]) { _ in }
                }
                return
            }
            guard response.ok else {
                self.framesSubscribed.remove(surfaceID)
                if response.error?.code == "unsupported" {
                    self.framesUnsupported.insert(surfaceID)
                }
                // A transient failure (not_found, a dropped request) on a
                // restart would otherwise leave a stale ScreenModel frozen on
                // its last frame. Drop it so the card falls back to polled text
                // and the next focus or poll can retry.
                self.screenModels.removeValue(forKey: surfaceID)
                return
            }
            if self.screenModels[surfaceID] == nil {
                self.screenModels[surfaceID] = self.makeScreenModel(for: surfaceID)
            }
        }
    }

    /// A feed whose resync request restarts the surface's stream.
    private func makeScreenModel(for surfaceID: String) -> ScreenModel {
        let model = ScreenModel()
        model.onResyncNeeded = { [weak self] in self?.resyncFrames(surfaceID) }
        return model
    }

    /// Restarts a live stream so the bridge sends a fresh full frame — the
    /// phone's view can no longer reconstruct the screen from what it kept
    /// (see ScreenModel.onResyncNeeded). A resubscribe of an already-subscribed
    /// surface restarts the stream on the bridge; the feed itself is kept so
    /// the card doesn't flicker back to text.
    private func resyncFrames(_ surfaceID: String) {
        guard surfaceID == frameFocusedSurfaceID, framesSubscribed.contains(surfaceID) else { return }
        framesSubscribed.remove(surfaceID)
        subscribeFrames(for: surfaceID)
    }

    // MARK: Fit to phone

    /// The point size a fitted pane is sized for: the same size the polled
    /// text card uses, readable without pinching.
    private static let fitFontSize: CGFloat = 9

    /// Records the live card's size for `surfaceID`; while the surface is
    /// fitted, a size change (rotation, keyboard) re-fits it.
    func reportLiveCardSize(_ surfaceID: String, _ size: CGSize) {
        guard size.width > 0, size.height > 0 else { return }
        guard liveCardSizes[surfaceID] != size else { return }
        liveCardSizes[surfaceID] = size
        if fittedSurfaces[surfaceID] != nil {
            let grid = fitGrid(for: size)
            if fittedSurfaces[surfaceID] != grid {
                sendFit(surfaceID, grid)
            }
        }
    }

    /// "Fit to phone": resize the surface's real pane on the Mac to the grid
    /// that fills its card here, so a full-screen TUI lays itself out for the
    /// phone (opencode drops its sidebar, Claude Code wraps to the width).
    /// Toggles: a fitted surface is released back to its layout's size.
    func toggleFitToPhone(_ surfaceID: String) {
        if fittedSurfaces[surfaceID] != nil {
            releaseFit(surfaceID)
            return
        }
        guard let size = liveCardSizes[surfaceID] else { return }
        sendFit(surfaceID, fitGrid(for: size))
    }

    private func fitGrid(for size: CGSize) -> FitGrid {
        let cell = LiveCellMetrics.cell(fontSize: Self.fitFontSize)
        let grid = FrameFit.gridToFit(size: size, cellWidth: cell.width, cellHeight: cell.height)
        return FitGrid(columns: grid.columns, rows: grid.rows)
    }

    private func sendFit(_ surfaceID: String, _ grid: FitGrid) {
        guard connectionStatus.isConnected else { return }
        sendRaw(method: "surface.fit", params: ["surface_id": surfaceID, "cols": grid.columns, "rows": grid.rows]) { [weak self] response in
            guard let self else { return }
            guard response.ok else {
                if let code = response.error?.code, code != "unsupported" {
                    print("surface.fit failed: \(code) \(response.error?.message ?? "")")
                }
                return
            }
            // Focus may have moved while the fit was in flight. A fit resizes
            // the real pane on the Mac, so it is only ever held for the surface
            // on screen — record it and restart the stream only if this is
            // still the focused surface; otherwise release it right back, or
            // the Mac pane stays resized with nothing tracking it (releaseFit
            // during the focus switch found nothing to release yet).
            guard surfaceID == self.frameFocusedSurfaceID else {
                self.sendRaw(method: "surface.fit.release", params: ["surface_id": surfaceID]) { _ in }
                return
            }
            self.fittedSurfaces[surfaceID] = grid
            self.restartFrames(surfaceID)
        }
    }

    private func releaseFit(_ surfaceID: String) {
        guard fittedSurfaces.removeValue(forKey: surfaceID) != nil else { return }
        sendRaw(method: "surface.fit.release", params: ["surface_id": surfaceID]) { _ in }
        restartFrames(surfaceID)
    }

    /// The bridge ended a fit on its own (herdr closed the controller); the
    /// pane is back at its layout size, so stream it at that size again.
    private func handleFitEnded(_ push: SurfaceFramesEndedPush) {
        guard fittedSurfaces.removeValue(forKey: push.surfaceID) != nil else { return }
        restartFrames(push.surfaceID)
    }

    /// Restarts the focused surface's frame stream so it picks up the pane's
    /// new grid with a fresh full frame.
    private func restartFrames(_ surfaceID: String) {
        guard surfaceID == frameFocusedSurfaceID, framesSubscribed.contains(surfaceID) else { return }
        framesSubscribed.remove(surfaceID)
        subscribeFrames(for: surfaceID)
    }

    /// Stops streaming frames for `surfaceID` and drops its feed immediately
    /// (rather than waiting for the bridge's ack) so the card falls back to
    /// text the instant focus moves away.
    private func unsubscribeFrames(_ surfaceID: String) {
        let wasActive = framesSubscribed.remove(surfaceID) != nil || screenModels[surfaceID] != nil
        screenModels.removeValue(forKey: surfaceID)
        guard wasActive else { return }
        sendRaw(method: "surface.screen.unsubscribe", params: ["surface_id": surfaceID]) { _ in }
    }

    /// A `surface.screen` push landed: apply it to the surface's model (the
    /// card observing the model repaints). Stray updates for a surface we've
    /// since moved focus away from are dropped.
    private func handleScreen(_ update: ScreenUpdate) {
        guard update.surfaceID == frameFocusedSurfaceID else { return }
        let model = screenModels[update.surfaceID] ?? {
            let model = makeScreenModel(for: update.surfaceID)
            screenModels[update.surfaceID] = model
            return model
        }()
        model.apply(update)
    }

    /// A `surface.frames.ended` push landed: the bridge stopped streaming.
    /// Drops the feed so the card falls back to text right away. Unless the
    /// reason was our own unsubscribe, and the surface is still the focused
    /// one, resubscribes after a short delay — `backpressure` in particular
    /// means the bridge dropped us for being slow, and a fresh subscribe
    /// yields a new full frame to recover from.
    private func handleFramesEnded(_ push: SurfaceFramesEndedPush) {
        // An `unsubscribed` end is the echo of our own unsubscribe, whose
        // state was already cleared when it was sent. A surface switch that
        // lands back on the same surface (focus flaps between the locally
        // tapped surface and the pane-derived one until pane.list catches up)
        // sends unsubscribe + subscribe back to back, so this echo routinely
        // arrives AFTER the new subscription — acting on it tore down the
        // live feed the bridge was in fact streaming, and the card fell back
        // to text.
        guard push.reason != "unsubscribed" else { return }
        framesSubscribed.remove(push.surfaceID)
        screenModels.removeValue(forKey: push.surfaceID)
        guard push.surfaceID == frameFocusedSurfaceID else { return }
        DispatchQueue.main.asyncAfter(deadline: .now() + 1) { [weak self] in
            guard let self, self.frameFocusedSurfaceID == push.surfaceID else { return }
            self.subscribeFrames(for: push.surfaceID)
        }
    }

    /// Loads a claude surface's conversation from its session transcript (via
    /// the bridge's `claude.transcript`). Claude runs as a full-screen TUI whose
    /// terminal keeps no scrollback, so this — not the terminal mirror — is the
    /// conversation, and it is what the surface's card renders.
    ///
    /// Safe to call on every poll: the bridge is handed the fingerprint of the
    /// text we already hold and answers `unchanged` without re-sending it.
    ///
    /// - Parameter showsSpinner: whether the user is waiting on this request
    ///   (history viewer, explicit refresh). Background polls pass false so they
    ///   don't animate a spinner over content that is already on screen.
    func loadClaudeTranscript(_ surfaceID: String, showsSpinner: Bool = false) {
        guard !claudeTranscriptInFlight.contains(surfaceID) else { return }
        claudeTranscriptInFlight.insert(surfaceID)
        let seq = (claudeTranscriptSeq[surfaceID] ?? 0) + 1
        claudeTranscriptSeq[surfaceID] = seq
        if showsSpinner {
            claudeTranscriptLoading.insert(surfaceID)
        }
        var params: [String: Any] = ["surface_id": surfaceID, "max_messages": 300]
        if let wsID = currentWorkspaceID {
            params["workspace_id"] = wsID
        }
        if let known = claudeTranscriptFingerprint[surfaceID] {
            params["known_fingerprint"] = known
        }
        // Safety net: BridgeClient drops pending completions on disconnect
        // without calling them, so clear the in-flight marks on a timeout too —
        // otherwise the viewer's spinner would hang forever and polling would
        // stop for this surface. Both paths do an idempotent remove, so the
        // race is harmless.
        DispatchQueue.main.asyncAfter(deadline: .now() + 15) { [weak self] in
            guard let self, self.claudeTranscriptSeq[surfaceID] == seq else { return }
            self.claudeTranscriptInFlight.remove(surfaceID)
            self.claudeTranscriptLoading.remove(surfaceID)
        }
        send(method: "claude.transcript", params: params) { [weak self] result in
            guard let self else { return }
            if self.claudeTranscriptSeq[surfaceID] == seq {
                self.claudeTranscriptInFlight.remove(surfaceID)
                self.claudeTranscriptLoading.remove(surfaceID)
            }
            // BridgeClient fires this completion for ANY reply carrying our id,
            // including `ok:false`, where `result` is empty. Every field would
            // then read as "no session, no text" and blank a conversation that
            // is on screen and still valid. A real answer always carries
            // `supported`, so its absence means the request failed — keep what
            // we have and let the next poll retry.
            guard result["supported"] != nil else { return }
            let sessionID = result["session_id"] as? String ?? ""
            let missing = result["session_missing"] as? Bool ?? false
            self.claudeTranscriptSession[surfaceID] = sessionID
            if missing {
                self.claudeTranscriptMissing.insert(surfaceID)
            } else {
                self.claudeTranscriptMissing.remove(surfaceID)
            }
            // Nothing resolved (no session yet, file gone) leaves this nil, which
            // drops any stale fingerprint so the next poll asks for the full text.
            let raw = result["fingerprint"] as? String
            let fingerprint = (raw?.isEmpty == false) ? raw : nil
            // The transcript we hold is still current — the bridge deliberately
            // sent no text. Keep what's on screen.
            if result["unchanged"] as? Bool == true {
                // Same ordering rule as the apply path below: an older
                // response must not put its fingerprint on newer text.
                if let fingerprint, seq >= (self.claudeTranscriptAppliedSeq[surfaceID] ?? 0) {
                    self.claudeTranscriptFingerprint[surfaceID] = fingerprint
                }
                return
            }

            let text = result["text"] as? String ?? ""
            // Diagnostic: the surface we asked for vs the session the bridge
            // resolved it to, and how. If these ever look mismatched, this line
            // names the exact ids to chase. Logged only when the transcript
            // actually changed, so a 3s poll doesn't flood the console.
            let source = result["source"] as? String ?? ""
            print("[Transcript] surface=\(surfaceID) -> session=\(sessionID) via \(source) (\(text.count) chars)\(missing ? " MISSING FILE" : "")")
            // Trim off the main actor — transcripts run to tens of thousands of
            // characters, and this lands on a poll that may overlap an animation.
            Task.detached(priority: .userInitiated) { [weak self] in
                let trimmed = Self.trimTerminalText(text)
                await MainActor.run {
                    guard let self else { return }
                    // Detached trims are not ordered against each other, so a
                    // response older than the newest applied one is dropped
                    // rather than rolling the conversation back.
                    guard seq > (self.claudeTranscriptAppliedSeq[surfaceID] ?? 0) else { return }
                    self.claudeTranscriptAppliedSeq[surfaceID] = seq
                    // The fingerprint is stamped with the text it describes. Set
                    // earlier, a dropped-out-of-order response would leave the
                    // newest fingerprint attached to older text, and every later
                    // poll would answer `unchanged` and keep it there.
                    if let fingerprint { self.claudeTranscriptFingerprint[surfaceID] = fingerprint }
                    else { self.claudeTranscriptFingerprint.removeValue(forKey: surfaceID) }
                    guard self.claudeTranscript[surfaceID] != trimmed else { return }
                    self.claudeTranscript[surfaceID] = trimmed
                    self.recomposeClaudeCard(surfaceID)
                }
            }
        }
    }

    /// Divider between the conversation and the live terminal viewport on a
    /// claude card. The viewport repeats claude's last screen, which is what
    /// makes permission prompts and the input box visible from the phone.
    private static let liveScreenDivider = "──────────  live screen  ──────────"

    /// Rebuilds a claude surface's card text from its transcript and the live
    /// terminal mirror. A no-op for surfaces with no transcript, whose cards
    /// render the mirror alone.
    private func recomposeClaudeCard(_ surfaceID: String) {
        guard let transcript = claudeTranscript[surfaceID], !transcript.isEmpty else {
            claudeCardText.removeValue(forKey: surfaceID)
            return
        }
        let live = surfaceContent[surfaceID] ?? ""
        let composed = live.isEmpty
            ? transcript
            : transcript + "\n\n" + Self.liveScreenDivider + "\n\n" + live
        // Diff before assigning: a no-op write to a @Published dictionary still
        // fires objectWillChange.
        guard claudeCardText[surfaceID] != composed else { return }
        claudeCardText[surfaceID] = composed
    }

    /// The text a surface's card renders. Claude surfaces show their
    /// conversation with the live screen below it; everything else shows the
    /// live terminal mirror.
    ///
    /// Gated on the surface's CURRENT kind, not on whether a transcript was
    /// ever loaded: when claude exits, the surface goes back to being a plain
    /// shell, and a cached conversation would otherwise stay pinned above its
    /// output for the rest of the session.
    func cardText(for surface: Surface) -> String {
        guard surface.hasTranscript, let text = claudeCardText[surface.id] else {
            return surfaceContent[surface.id] ?? ""
        }
        return text
    }

    /// Trim trailing whitespace per line and remove blank trailing lines.
    /// Plain character ops, no regex — this runs over thousands of lines per
    /// poll, and a per-line NSRegularExpression here dominated the poll cost.
    nonisolated private static func trimTerminalText(_ text: String) -> String {
        var lines = text.split(separator: "\n", omittingEmptySubsequences: false)
        for i in lines.indices {
            while let last = lines[i].last, last.isWhitespace {
                lines[i].removeLast()
            }
        }
        while lines.last?.isEmpty == true {
            lines.removeLast()
        }
        return lines.joined(separator: "\n")
    }

    /// Bound history so a very long session doesn't produce an attributed
    /// string UITextView can't lay out interactively.
    nonisolated private static let maxHistoryLines = 5000

    nonisolated private static func capHistoryLines(_ text: String) -> String {
        let lines = text.split(separator: "\n", omittingEmptySubsequences: false)
        guard lines.count > maxHistoryLines else { return text }
        return lines.suffix(maxHistoryLines).joined(separator: "\n")
    }

    /// Asks the layout to present the Add-File source menu.
    func requestAddFile() { addFileRequested = true }

    /// Attaches the image currently on the pasteboard to the compose bar.
    func attachClipboardImage() {
        guard let image = ImagePaste.pasteboardImage() else {
            showImagePasteStatus("No image on the clipboard")
            return
        }
        prepareAttachment("Preparing image…") {
            ImagePaste.attachment(from: image).map { .ready($0) } ?? .unreadable
        }
    }

    /// Attaches an image chosen from the photo library (raw file `data`).
    func attachPhoto(data: Data) {
        prepareAttachment("Preparing image…") {
            guard let image = UIImage(data: data), let att = ImagePaste.attachment(from: image) else {
                return .unreadable
            }
            return .ready(att)
        }
    }

    /// Attaches a file chosen from the Files app. Reads the bytes under the
    /// security scope the picker grants — bounded so a huge file can't exhaust
    /// memory — then encodes off the main actor.
    func attachFile(url: URL) {
        prepareAttachment("Preparing file…") {
            let scoped = url.startAccessingSecurityScopedResource()
            defer { if scoped { url.stopAccessingSecurityScopedResource() } }
            guard let handle = try? FileHandle(forReadingFrom: url) else { return .unreadable }
            defer { try? handle.close() }
            // Read one byte past the limit: if we get it, the file is too big.
            let data = (try? handle.read(upToCount: ImagePaste.maxFileBytes + 1)) ?? Data()
            if data.count > ImagePaste.maxFileBytes { return .tooLarge }
            guard let att = ImagePaste.attachment(fileData: data, filename: url.lastPathComponent) else {
                return .unreadable
            }
            return .ready(att)
        }
    }

    /// The result of preparing an attachment off the main actor.
    private enum AttachOutcome {
        case ready(ImagePaste.Attachment)
        case tooLarge
        case unreadable
    }

    /// Shared attach path: run a (possibly heavy) encode off the main actor,
    /// then — only if this is still the newest attach request — set the pending
    /// attachment and, when a send fired while it was encoding, run that send so
    /// the caption goes with the attachment. A stale result (a newer selection
    /// started, or the session was reset) is dropped.
    private func prepareAttachment(_ status: String, _ build: @escaping @Sendable () -> AttachOutcome) {
        showImagePasteStatus(status, autoClear: false)
        attachGeneration += 1
        let generation = attachGeneration
        attachInFlight = true
        Task.detached(priority: .userInitiated) { [weak self] in
            let outcome = build()
            await MainActor.run {
                guard let self, self.attachGeneration == generation else { return }
                self.attachInFlight = false
                switch outcome {
                case .ready(let attachment):
                    self.pendingAttachment = attachment
                    let noun = attachment.kind == .image ? "Image" : "File"
                    self.showImagePasteStatus("\(noun) attached — add a message, then send")
                    // Replay a send deferred while this was encoding — only on
                    // success, so a failed attach never sends the caption alone.
                    if let deferred = self.deferredComposedSend {
                        self.deferredComposedSend = nil
                        self.sendComposed(text: deferred.text, withEnter: deferred.withEnter,
                                          to: deferred.surfaceID, workspaceID: deferred.workspaceID)
                    }
                case .tooLarge:
                    self.deferredComposedSend = nil
                    self.showImagePasteStatus("That file is over \(Self.byteLabel(ImagePaste.maxFileBytes)) — too large to send")
                case .unreadable:
                    self.deferredComposedSend = nil
                    self.showImagePasteStatus("Couldn't read that file")
                }
            }
        }
    }

    /// Removes the pending attachment without sending it, and invalidates any
    /// in-flight encode so it can't resurrect one.
    func clearPendingImage() {
        pendingAttachment = nil
        deferredComposedSend = nil
        attachGeneration += 1
        attachInFlight = false
        imagePasteStatusClear?.cancel()
        imagePasteStatus = nil
    }

    /// Sends a composed message to a surface: the pending attachment's path
    /// first (if any), then the typed text, then Enter when `withEnter` — all in
    /// ONE bridge call so the file and its caption reach the agent as a single
    /// prompt, never two.
    func sendComposed(text: String, withEnter: Bool, to surfaceID: String) {
        sendComposed(text: text, withEnter: withEnter, to: surfaceID, workspaceID: currentWorkspaceID)
    }

    private func sendComposed(text: String, withEnter: Bool, to surfaceID: String, workspaceID: String?) {
        let trimmedIsEmpty = text.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty

        // An attach is still encoding: hold this send until it lands so the
        // caption and the attachment go out together, not as two messages. Keep
        // the FIRST deferred send; ignore piled-up ones rather than losing the
        // earlier caption to the later.
        if pendingAttachment == nil, attachInFlight {
            if deferredComposedSend == nil {
                deferredComposedSend = (text, withEnter, surfaceID, workspaceID)
            }
            return
        }

        let attachment = pendingAttachment

        // Text-only: unchanged behaviour.
        guard let attachment else {
            guard !trimmedIsEmpty else { return }
            sendText(withEnter ? text + "\n" : text, to: surfaceID)
            return
        }

        // Don't consume the attachment if we can't actually send: keep the chip
        // and tell the user, rather than clearing it into a dropped request.
        guard connectionStatus.isConnected else {
            showImagePasteStatus("Not connected — try again in a moment")
            return
        }

        // Clear the chip immediately; the send is in flight.
        pendingAttachment = nil
        let noun = attachment.kind == .image ? "image" : "file"
        showImagePasteStatus("Sending \(noun)…", autoClear: false)

        // One request carries the payload, the caption, and whether to submit,
        // so the bridge types the path + text as a SINGLE message. No
        // workspace_id: the namespaced surface_id already routes it, and pairing
        // it with a now-current workspace from a different runtime would be
        // rejected by a composite bridge.
        var params: [String: Any] = ["surface_id": surfaceID, "submit": withEnter]
        let method: String
        switch attachment.kind {
        case .image:
            method = "surface.paste_image"
            params["image_base64"] = attachment.base64
            params["image_format"] = attachment.format
        case .file:
            method = "surface.paste_file"
            params["data_base64"] = attachment.base64
            params["filename"] = attachment.label
        }
        if !trimmedIsEmpty {
            params["text"] = text
        }
        send(method: method, params: params) { [weak self] result in
            guard let self else { return }
            // An error reply reaches this completion with an empty result (see
            // BridgeClient), so a missing path means it failed. Restore the
            // attachment so the user can retry — but only if nothing newer has
            // taken the slot, so a late failure can't clobber a fresh selection.
            guard result["path"] is String else {
                if self.pendingAttachment == nil {
                    self.pendingAttachment = attachment
                }
                self.showImagePasteStatus("\(noun.capitalized) attach failed")
                return
            }
            let bytes = (result["bytes"] as? Int) ?? attachment.bytes
            self.showImagePasteStatus("\(noun.capitalized) sent (\(Self.byteLabel(bytes)))")
            self.scheduleActivityProbe(for: surfaceID)
        }
    }

    private func showImagePasteStatus(_ text: String, autoClear: Bool = true) {
        imagePasteStatusClear?.cancel()
        imagePasteStatus = text
        guard autoClear else { return }
        let clear = DispatchWorkItem { [weak self] in self?.imagePasteStatus = nil }
        imagePasteStatusClear = clear
        DispatchQueue.main.asyncAfter(deadline: .now() + 2.5, execute: clear)
    }

    nonisolated private static func byteLabel(_ bytes: Int) -> String {
        bytes >= 1024 * 1024
            ? String(format: "%.1f MB", Double(bytes) / (1024 * 1024))
            : "\(max(1, bytes / 1024)) KB"
    }

    func readBrowserURL(_ surfaceID: String) {
        send(method: "browser.url.get", params: ["surface_id": surfaceID]) { [weak self] result in
            if let url = result["url"] as? String {
                self?.browserURLs[surfaceID] = url
            }
        }
    }

    func createWorkspace(name: String? = nil) {
        var params: [String: Any] = [:]
        if let name { params["name"] = name }
        send(method: "workspace.create", params: params) { [weak self] _ in
            self?.refreshWorkspaces()
            self?.refreshSurfaces()
        }
    }

    func closeWorkspace(_ id: String) {
        send(method: "workspace.close", params: ["workspace_id": id]) { [weak self] _ in
            self?.lastFocusedSurface.removeValue(forKey: id)
            // Select the next workspace only after the list is refreshed —
            // otherwise `workspaces` still contains the just-closed one.
            self?.refreshWorkspaces {
                guard let self, self.currentWorkspaceID == id else { return }
                if let next = self.visibleWorkspaces.first(where: { $0.id != id }) {
                    self.selectWorkspace(next.id)
                }
            }
        }
    }

    func splitSurface(direction: String, surfaceID: String? = nil) {
        let previousIDs = Set(surfaces.map(\.id))
        var params: [String: Any] = ["direction": direction]
        if let surfaceID { params["surface_id"] = surfaceID }
        send(method: "surface.split", params: params) { [weak self] _ in
            self?.refreshSurfacesAndFocusNew(previousIDs: previousIDs)
        }
    }

    func createSurface(type: String = "terminal") {
        let previousIDs = Set(surfaces.map(\.id))
        send(method: "surface.create", params: ["type": type]) { [weak self] _ in
            self?.refreshSurfacesAndFocusNew(previousIDs: previousIDs)
        }
    }

    func closeSurface(_ surfaceID: String) {
        send(method: "surface.close", params: ["surface_id": surfaceID]) { [weak self] _ in
            self?.forgetClaudeTranscript(surfaceID)
            self?.refreshSurfaces()
        }
    }

    /// Releases everything cached for one surface's conversation. A transcript
    /// and its composed card text are tens of kilobytes each, and nothing else
    /// prunes them before the session resets.
    ///
    /// Driven by an explicit close rather than by a surface's absence from
    /// `surfaces`: that list holds only the CURRENT workspace, so absence means
    /// "not in view", not "gone".
    private func forgetClaudeTranscript(_ surfaceID: String) {
        claudeTranscript.removeValue(forKey: surfaceID)
        claudeCardText.removeValue(forKey: surfaceID)
        claudeTranscriptSession.removeValue(forKey: surfaceID)
        claudeTranscriptFingerprint.removeValue(forKey: surfaceID)
        claudeTranscriptSeq.removeValue(forKey: surfaceID)
        claudeTranscriptAppliedSeq.removeValue(forKey: surfaceID)
        claudeTranscriptMissing.remove(surfaceID)
    }

    func clearNotifications() {
        send(method: "notification.clear", params: [:]) { [weak self] _ in
            self?.notifications = []
        }
    }

    func refreshWorkspaces(then completion: (() -> Void)? = nil) {
        // Both list + current must land before firing `completion`, otherwise
        // follow-up work (refreshSurfaces) can run against an empty workspace
        // list or an unset current ID depending on which reply arrives first.
        // Both callbacks run on the main actor, so `pending` needs no locking.
        var pending = 2
        let step = { [weak self] in
            pending -= 1
            if pending == 0 {
                self?.enforceRuntimeFilter()
                completion?()
            }
        }
        send(method: "workspace.list", params: [:]) { [weak self] result in
            if let list = result["workspaces"] as? [[String: Any]] {
                self?.setIfChanged(\.workspaces, to: list.compactMap(Workspace.init), by: Workspace.sameContent)
            }
            step()
        }
        send(method: "workspace.current", params: [:]) { [weak self] result in
            // cmux answers with the id under `workspace_id` (and the record
            // under `workspace`); herdr and the composite also give `id`.
            if let id = (result["id"] as? String) ?? (result["workspace_id"] as? String),
               id != self?.currentWorkspaceID {
                self?.currentWorkspaceID = id
            }
            step()
        }
    }

    func refreshSurfaces(workspaceID: String? = nil) {
        var params: [String: Any] = [:]
        if let id = workspaceID ?? currentWorkspaceID {
            params["workspace_id"] = id
        }
        send(method: "surface.list", params: params) { [weak self] result in
            if let list = result["surfaces"] as? [[String: Any]] {
                self?.setIfChanged(\.surfaces, to: list.compactMap(Surface.init), by: Surface.sameContent)
                self?.applyAgentStatuses()
                // Validate remembered surface still exists
                if let localID = self?.localFocusedSurfaceID,
                   self?.surfaces.contains(where: { $0.id == localID }) == false {
                    self?.localFocusedSurfaceID = nil
                }
                // Auto-select first surface if nothing is focused
                if self?.focusedSurfaceID == nil, let first = self?.surfaces.first {
                    self?.localFocusedSurfaceID = first.id
                }
                // Picks up both the auto-select above and a reconnect, where
                // `frameFocusedSurfaceID` was cleared but the previously
                // focused surface is still the one on screen.
                self?.updateFrameSubscription(focusedSurfaceID: self?.focusedSurfaceID)
            }
        }
    }

    /// Mirrors runtime-reported agent statuses from the surface list into
    /// `agentStatus` and the working indicator. No-op for a backend without the
    /// capability, whose surfaces carry no status.
    private func applyAgentStatuses() {
        guard capabilities.agentStatus else { return }
        for surface in surfaces {
            guard let status = surface.agentStatus else { continue }
            applyAgentStatus(status, for: surface.id)
        }
    }

    private func applyAgentStatus(_ status: String, for surfaceID: String) {
        if agentStatus[surfaceID] != status {
            agentStatus[surfaceID] = status
        }
        setWorking(status == "working", for: surfaceID)
    }

    private func refreshSurfacesAndFocusNew(previousIDs: Set<String>) {
        var params: [String: Any] = [:]
        if let id = currentWorkspaceID {
            params["workspace_id"] = id
        }
        send(method: "surface.list", params: params) { [weak self] result in
            guard let self else { return }
            if let list = result["surfaces"] as? [[String: Any]] {
                self.setIfChanged(\.surfaces, to: list.compactMap(Surface.init), by: Surface.sameContent)
                self.applyAgentStatuses()
                if let newSurface = self.surfaces.first(where: { !previousIDs.contains($0.id) }) {
                    self.focusSurface(newSurface.id) // also updates the frame subscription
                } else if self.focusedSurfaceID == nil, let first = self.surfaces.first {
                    self.localFocusedSurfaceID = first.id
                    self.updateFrameSubscription(focusedSurfaceID: first.id)
                }
            }
        }
    }

    func refreshPanes() {
        var params: [String: Any] = [:]
        if let id = currentWorkspaceID {
            params["workspace_id"] = id
        }
        send(method: "pane.list", params: params) { [weak self] result in
            if let list = result["panes"] as? [[String: Any]] {
                self?.setIfChanged(\.panes, to: list.compactMap(Pane.init), by: Pane.sameContent)
            }
        }
    }

    // MARK: - Helpers

    var focusedSurfaceID: String? {
        // Prefer pane-derived focus, then surface.is_focused, then local tracking
        panes.first(where: { $0.isFocused })?.focusedSurfaceID
            ?? surfaces.first(where: { $0.isFocused })?.id
            ?? localFocusedSurfaceID
    }

    var currentWorkspaceSurfaces: [Surface] {
        guard let wsID = currentWorkspaceID else { return surfaces }
        return surfaces.filter { $0.workspaceID == wsID }
    }

    func surfaceTitle(for id: String) -> String {
        surfaces.first(where: { $0.id == id })?.title ?? ""
    }

    func hasNotification(for pane: Pane) -> Bool {
        notifications.contains { n in
            guard let sid = n.surfaceID else { return false }
            return pane.surfaceIDs.contains(sid)
        }
    }

    func hasNotification(for surface: Surface) -> Bool {
        notifications.contains { $0.surfaceID == surface.id }
    }

    func clearNotificationsForFocusedSurface() {
        // Guard before mutating: this runs on delayed timers after every focus
        // change, and removeAll on a @Published array fires objectWillChange
        // even when nothing matches.
        guard let focusedID = focusedSurfaceID,
              notifications.contains(where: { $0.surfaceID == focusedID }) else { return }
        notifications.removeAll { $0.surfaceID == focusedID }
    }

    // MARK: - Private helpers

    /// Assigns a @Published list only when its content actually changed.
    /// The refresh RPCs re-fetch on every connect/foreground/focus change and
    /// usually return identical lists; an unconditional assign would fire
    /// objectWillChange each time and re-render every view observing AppState.
    /// Comparison is via an explicit closure, NOT Equatable conformance — these
    /// structs are SwiftUI view parameters, and conforming them to Equatable
    /// has caused missed re-renders here before.
    private func setIfChanged<T>(
        _ keyPath: ReferenceWritableKeyPath<AppState, [T]>, to value: [T],
        by same: (T, T) -> Bool
    ) {
        let current = self[keyPath: keyPath]
        if current.count != value.count || !zip(current, value).allSatisfy(same) {
            self[keyPath: keyPath] = value
        }
    }

    private func send(
        method: String,
        params: [String: Any],
        completion: (([String: Any]) -> Void)? = nil
    ) {
        client?.send(method: method, params: params, completion: completion)
    }

    /// Like `send`, but for callers that need the full response (error code
    /// included) — currently just the frame-subscription calls.
    private func sendRaw(
        method: String,
        params: [String: Any],
        onResponse: @escaping (CommandResponse) -> Void
    ) {
        client?.sendCommand(method: method, params: params, onResponse: onResponse)
    }
}

// MARK: - Live terminal frames

/// One decoded `surface.frame` push: either a full repaint (reset + resize +
/// feed) or a delta (feed as-is) for a surface's live terminal screen.
/// A "fit to phone" grid: what the real pane was resized to.
struct FitGrid: Equatable {
    let columns: Int
    let rows: Int
}

// MARK: - BridgeClientDelegate

extension AppState: BridgeClientDelegate {
    func clientDidConnect(_ client: BridgeClient) {
        guard client === self.client else { return }
        connectionStatus = .connected
        refreshWorkspaces {
            self.refreshSurfaces()
            self.refreshPanes()
            // A notification tapped before the session was up (cold launch).
            if let tap = self.pendingNavigationTarget {
                self.pendingNavigationTarget = nil
                self.applyNavigation(surfaceID: tap.surfaceID, workspaceID: tap.workspaceID)
            }
        }
    }

    func clientDidDisconnect(_ client: BridgeClient, error: Error?) {
        // A client replaced by connect(to:) can still emit its close callback;
        // it must not flip the status of the bridge that replaced it.
        guard client === self.client else { return }
        connectionStatus = .reconnecting
        // In-flight transcript RPCs will never complete now; clear their marks
        // so the history viewer doesn't hang on a spinner and polling resumes
        // for these surfaces once the bridge is back.
        claudeTranscriptInFlight.removeAll()
        claudeTranscriptLoading.removeAll()
        // The dead socket's frame subscriptions die with it — drop feeds now
        // so the focused card falls back to text immediately instead of
        // freezing on the last frame, and clear `frameFocusedSurfaceID` so
        // reconnecting resubscribes from scratch rather than treating the
        // focused surface as already subscribed.
        screenModels = [:]
        framesSubscribed = []
        fittedSurfaces = [:]
        frameFocusedSurfaceID = nil
    }

    func clientDidReceiveMessage(_ client: BridgeClient, message: BridgeMessage) {
        guard client === self.client else { return }
        switch message {
        case .connected(let payload):
            connectionStatus = .connected(cmuxConnected: payload.isBackendConnected)
            if backendKind != payload.backendKind { backendKind = payload.backendKind }
            if capabilities != payload.effectiveCapabilities { capabilities = payload.effectiveCapabilities }
        case .notificationCreated(let n):
            notifications.insert(n, at: 0)
            print("[Notification] id=\(n.id) surfaceID=\(n.surfaceID ?? "nil") workspaceID=\(n.workspaceID ?? "nil") title=\(n.title)")
            print("[Notification] surface IDs: \(surfaces.map(\.id))")
            scheduleLocalNotification(n)
        case .notificationCleared:
            notifications = []
        case .backendConnected:
            connectionStatus = .connected(cmuxConnected: true)
            refreshWorkspaces {
                self.refreshSurfaces()
                self.refreshPanes()
            }
        case .backendDisconnected:
            connectionStatus = .connected(cmuxConnected: false)
            panes = []
        case .surfaceUpdated(let update):
            if capabilities.agentStatus, let status = update.agentStatus {
                applyAgentStatus(status, for: update.surfaceID)
            }
            // The title (Claude's conversation topic, an agent starting or
            // exiting) is part of the surface record; refetch the list so the
            // card header follows it. Coalesced by setIfChanged when nothing
            // visible changed.
            if update.workspaceID == nil || update.workspaceID == currentWorkspaceID {
                refreshSurfaces()
            }
        case .surfaceScreen(let update):
            handleScreen(update)
        case .surfaceScreenEnded(let push):
            handleFramesEnded(push)
        case .surfaceFitEnded(let push):
            handleFitEnded(push)
        case .commandResponse, .ignored:
            break
        }
    }
}

// MARK: - BridgeDiscoveryDelegate

extension AppState: BridgeDiscoveryDelegate {
    func discovery(_ discovery: BridgeDiscovery, didFind candidates: [PairingCredentials]) {
        guard connectionStatus == .disconnected, let stored = selectedBridge else { return }
        let creds = stored.credentials
        if let match = candidates.first(where: { $0.host == creds.host && $0.port == creds.port }) {
            connect(to: PairingCredentials(host: match.host, port: match.port, token: creds.token, tailscaleHost: creds.tailscaleHost))
        }
    }
}

// MARK: - Models

struct BridgeNotification: Identifiable, Decodable {
    let id: String
    let title: String
    let subtitle: String?
    let body: String?
    let workspaceID: String?
    let surfaceID: String?
    let isRead: Bool?

    enum CodingKeys: String, CodingKey {
        case id, title, subtitle, body
        case workspaceID = "workspace_id"
        case surfaceID = "surface_id"
        case isRead = "is_read"
    }
}

struct Workspace: Identifiable {
    let id: String
    /// The display title cmux computes for the workspace (custom title, else the
    /// active conversation topic, else the working directory) — matches what the
    /// cmux UI shows. Falls back through older fields, then the id.
    let title: String
    /// The runtime this workspace lives in ("cmux" / "herdr"), set by a
    /// composite bridge. nil from a single-runtime bridge.
    let backend: String?

    init?(_ dict: [String: Any]) {
        guard let id = dict["id"] as? String else { return nil }
        self.id = id
        // First non-blank candidate wins — a present-but-empty field must not
        // shadow a usable later fallback.
        self.title = [dict["title"], dict["custom_title"], dict["name"]]
            .compactMap { ($0 as? String)?.trimmingCharacters(in: .whitespacesAndNewlines) }
            .first { !$0.isEmpty }
            ?? id
        self.backend = dict["backend"] as? String
    }

    // Deliberately NOT an Equatable conformance; see AppState.setIfChanged.
    static func sameContent(_ a: Workspace, _ b: Workspace) -> Bool {
        a.id == b.id && a.title == b.title && a.backend == b.backend
    }
}

/// Which runtime's workspaces the phone shows from a composite bridge.
enum RuntimeFilter: String, CaseIterable, Identifiable {
    case all
    case cmux
    case herdr

    static let storageKey = "runtimeFilter"

    static var stored: RuntimeFilter {
        RuntimeFilter(rawValue: UserDefaults.standard.string(forKey: storageKey) ?? "") ?? .all
    }

    var id: String { rawValue }

    /// The runtime name this filter admits; nil for all.
    var runtime: String? { self == .all ? nil : rawValue }

    var label: String {
        switch self {
        case .all: return "All"
        case .cmux: return "cmux"
        case .herdr: return "herdr"
        }
    }

    var icon: String {
        switch self {
        case .all: return "square.grid.2x2"
        case .cmux: return "macwindow"
        case .herdr: return "terminal"
        }
    }
}

struct Surface: Identifiable {
    let id: String
    let title: String
    let type: String
    let workspaceID: String?
    let isFocused: Bool
    /// Agent kind from the runtime's resume_binding (e.g. "claude"), when this
    /// surface is running an agent. Used to offer conversation-transcript history.
    let agentKind: String?
    /// Runtime-detected agent status (idle/working/blocked/done/unknown), from
    /// backends with the agent_status capability (herdr). nil from cmux.
    let agentStatus: String?

    var isBrowser: Bool { type == "browser" }
    var isClaudeAgent: Bool { agentKind == "claude" }
    /// Whether this surface's conversation can be read via `agent.transcript`.
    /// Claude Code and opencode are both full-screen TUIs that keep no
    /// terminal scrollback, so their card content and history reader come
    /// from this instead of the terminal mirror.
    var hasTranscript: Bool { agentKind == "claude" || agentKind == "opencode" }

    init?(_ dict: [String: Any]) {
        guard let id = dict["id"] as? String else { return nil }
        self.id = id
        self.type = (dict["type"] as? String) ?? "terminal"
        self.title = (dict["title"] as? String) ?? self.type
        self.workspaceID = dict["workspace_id"] as? String
        self.isFocused = dict["is_focused"] as? Bool ?? false
        self.agentKind = (dict["resume_binding"] as? [String: Any])?["kind"] as? String
        self.agentStatus = dict["agent_status"] as? String
    }

    // Deliberately NOT an Equatable conformance; see AppState.setIfChanged.
    static func sameContent(_ a: Surface, _ b: Surface) -> Bool {
        a.id == b.id && a.title == b.title && a.type == b.type
            && a.workspaceID == b.workspaceID && a.isFocused == b.isFocused
            && a.agentKind == b.agentKind && a.agentStatus == b.agentStatus
    }
}

struct Pane: Identifiable {
    let id: String
    let pixelFrame: CGRect
    let containerFrame: CGSize
    let surfaceIDs: [String]
    let focusedSurfaceID: String?
    let isFocused: Bool

    var normalizedFrame: CGRect {
        guard containerFrame.width > 0, containerFrame.height > 0 else {
            return CGRect(x: 0, y: 0, width: 1, height: 1)
        }
        return CGRect(
            x: pixelFrame.origin.x / containerFrame.width,
            y: pixelFrame.origin.y / containerFrame.height,
            width: pixelFrame.size.width / containerFrame.width,
            height: pixelFrame.size.height / containerFrame.height
        )
    }

    init?(_ dict: [String: Any]) {
        guard let id = dict["id"] as? String else { return nil }
        self.id = id

        if let pf = dict["pixel_frame"] as? [String: Any] {
            let x = (pf["x"] as? Double).map { CGFloat($0) } ?? 0
            let y = (pf["y"] as? Double).map { CGFloat($0) } ?? 0
            let w = (pf["width"] as? Double).map { CGFloat($0) } ?? 100
            let h = (pf["height"] as? Double).map { CGFloat($0) } ?? 100
            pixelFrame = CGRect(x: x, y: y, width: w, height: h)
        } else {
            pixelFrame = CGRect(x: 0, y: 0, width: 100, height: 100)
        }

        if let cf = dict["container_frame"] as? [String: Any] {
            let w = (cf["width"] as? Double).map { CGFloat($0) } ?? 100
            let h = (cf["height"] as? Double).map { CGFloat($0) } ?? 100
            containerFrame = CGSize(width: w, height: h)
        } else {
            containerFrame = CGSize(width: 100, height: 100)
        }

        surfaceIDs = dict["surface_ids"] as? [String] ?? []
        focusedSurfaceID = dict["focused_surface_id"] as? String
        isFocused = dict["is_focused"] as? Bool ?? false
    }

    // Deliberately NOT an Equatable conformance; see AppState.setIfChanged.
    static func sameContent(_ a: Pane, _ b: Pane) -> Bool {
        a.id == b.id && a.pixelFrame == b.pixelFrame
            && a.containerFrame == b.containerFrame && a.surfaceIDs == b.surfaceIDs
            && a.focusedSurfaceID == b.focusedSurfaceID && a.isFocused == b.isFocused
    }
}

enum ConnectionStatus: Equatable {
    case disconnected
    case connecting
    case reconnecting
    case connected(cmuxConnected: Bool = true)

    static var connected: ConnectionStatus { .connected(cmuxConnected: true) }

    var isConnected: Bool {
        if case .connected = self { return true }
        return false
    }

    var label: String { label(backend: "cmux") }

    /// The status text, naming the runtime the bridge fronts when it is the
    /// part that's down.
    func label(backend: String) -> String {
        switch self {
        case .disconnected: return "Disconnected"
        case .connecting: return "Connecting…"
        case .reconnecting: return "Reconnecting…"
        case .connected(let runtimeUp):
            return runtimeUp ? "Connected" : "Bridge connected (\(backend) offline)"
        }
    }

    var color: String {
        switch self {
        case .connected(true): return "green"
        case .connected(false): return "yellow"
        case .reconnecting: return "yellow"
        case .connecting: return "yellow"
        case .disconnected: return "red"
        }
    }
}
