import Foundation

// MARK: - Message types

enum BridgeMessage {
    case connected(ConnectedPayload)
    case notificationCreated(BridgeNotification)
    case notificationCleared
    /// The bridge (re)connected to its terminal runtime (cmux or herdr).
    case backendConnected
    /// The bridge lost its terminal runtime.
    case backendDisconnected
    /// A surface's runtime-detected agent status or title changed (protocol 2,
    /// backends with the `agent_status` capability).
    case surfaceUpdated(SurfaceUpdate)
        /// A rendered-screen update for a surface this connection subscribed to
    /// via `surface.screen.subscribe`: only the lines that changed.
    case surfaceScreen(ScreenUpdate)
    /// The bridge stopped streaming screen updates for a surface (it was
    /// unsubscribed, the surface closed, or the bridge dropped us).
    case surfaceScreenEnded(SurfaceFramesEndedPush)
    /// A "fit to phone" resize of a surface ended on the bridge's side (the
    /// controller herdr gave us was closed, or errored). Same shape as
    /// `surface.frames.ended`.
    case surfaceFitEnded(SurfaceFramesEndedPush)
    case commandResponse(CommandResponse)
    /// A push type this client doesn't know. Ignored rather than mis-parsed, so
    /// a newer bridge can add events without an older phone reacting to them.
    case ignored
}

/// What the runtime behind the bridge can do, from the `connected` payload.
/// Absent on protocol-1 bridges, which are always cmux.
struct BackendCapabilities: Decodable, Equatable {
    let browser: Bool
    let agentStatus: Bool
    let notifications: String

    enum CodingKeys: String, CodingKey {
        case browser
        case agentStatus = "agent_status"
        case notifications
    }

    static let cmux = BackendCapabilities(browser: true, agentStatus: false, notifications: "polled")
}

struct ConnectedPayload: Decodable {
    let bridgeVersion: String
    let protocolVersion: Int
    /// "cmux" or "herdr"; nil from a protocol-1 bridge (cmux).
    let backend: String?
    let backendConnected: Bool?
    /// Protocol-1 name for backendConnected; still sent by protocol-2 bridges.
    let cmuxConnected: Bool?
    let capabilities: BackendCapabilities?

    enum CodingKeys: String, CodingKey {
        case bridgeVersion = "bridge_version"
        case protocolVersion = "protocol_version"
        case backend
        case backendConnected = "backend_connected"
        case cmuxConnected = "cmux_connected"
        case capabilities
    }

    var isBackendConnected: Bool { backendConnected ?? cmuxConnected ?? false }
    var backendKind: String { backend ?? "cmux" }
    var effectiveCapabilities: BackendCapabilities { capabilities ?? .cmux }
}

struct SurfaceUpdate: Decodable {
    let surfaceID: String
    let workspaceID: String?
    let agentStatus: String?
    let agent: String?
    let title: String?

    enum CodingKeys: String, CodingKey {
        case surfaceID = "surface_id"
        case workspaceID = "workspace_id"
        case agentStatus = "agent_status"
        case agent, title
    }
}

struct CommandResponse {
    let id: String
    let ok: Bool
    let result: [String: Any]
    let error: CommandError?
}

struct CommandError {
    let code: String
    let message: String
}

/// A `surface.frame` push: a full repaint or a delta of raw PTY bytes for a
/// surface's live terminal screen. `bytes` is base64 and decoded by the
/// caller off the main actor — see AppState.handleFrame.
/// One styled run of a rendered screen line. `a` is a bit set: 1 bold,
/// 2 italic, 4 underline, 8 dim, 16 inverse, 32 strikethrough.
struct ScreenRun: Decodable, Equatable {
    let t: String
    let fg: String?
    let bg: String?
    let a: Int?
}

struct ScreenLine: Decodable {
    let i: Int
    let runs: [ScreenRun]
}

struct ScreenCursor: Decodable, Equatable {
    let x: Int
    let y: Int
    let visible: Bool
}

/// A `surface.screen` push: the bridge's rendering of a pane. `full` carries
/// every row; otherwise `lines` holds only the rows that changed since the
/// previous update on the stream.
struct ScreenUpdate: Decodable {
    let surfaceID: String
    let seq: Int
    let cols: Int
    let rows: Int
    let full: Bool
    let cursor: ScreenCursor?
    let lines: [ScreenLine]

    enum CodingKeys: String, CodingKey {
        case surfaceID = "surface_id"
        case seq, cols, rows, full, cursor, lines
    }
}

/// A `surface.screen.ended` (or `surface.fit.ended`) push: the bridge stopped a
/// per-surface stream. `reason` is one of "closed", "error", "backpressure", or
/// "unsubscribed".
struct SurfaceFramesEndedPush: Decodable {
    let surfaceID: String
    let reason: String

    enum CodingKeys: String, CodingKey {
        case surfaceID = "surface_id"
        case reason
    }
}

// MARK: - Delegate

protocol BridgeClientDelegate: AnyObject {
    @MainActor func clientDidConnect(_ client: BridgeClient)
    @MainActor func clientDidDisconnect(_ client: BridgeClient, error: Error?)
    @MainActor func clientDidReceiveMessage(_ client: BridgeClient, message: BridgeMessage)
}

// MARK: - Client

final class BridgeClient: NSObject {
    let credentials: PairingCredentials
    weak var delegate: BridgeClientDelegate?

    private var task: URLSessionWebSocketTask?
    private var session: URLSession?
    private var completions: [String: ([String: Any]) -> Void] = [:]
    // A second completion table for callers that need the full response —
    // `ok` and `error.code` as well as `result` — e.g. surface.frames.subscribe
    // distinguishing an `unsupported` backend from a transient failure. Kept
    // separate from `completions` rather than changing its signature: most
    // call sites only ever look at `result` and would have to unwrap a
    // CommandResponse for nothing.
    private var rawCompletions: [String: (CommandResponse) -> Void] = [:]
    // `completions`/`rawCompletions` are written from the caller (main actor)
    // and read from the URLSession receive queue, so all access must be
    // serialized.
    private let completionsLock = NSLock()
    private var reconnectTask: Task<Void, Never>?
    private var shouldReconnect = true
    private var backoff: TimeInterval = 1

    init(credentials: PairingCredentials) {
        self.credentials = credentials
    }

    // MARK: - Public API

    func connect() {
        shouldReconnect = true
        startConnection()
    }

    func disconnect() {
        shouldReconnect = false
        reconnectTask?.cancel()
        task?.cancel(with: .normalClosure, reason: nil)
        task = nil
        // URLSession strongly retains its delegate (self) until invalidated —
        // without this every connect leaks a BridgeClient.
        session?.invalidateAndCancel()
        session = nil
    }

    func send(
        method: String,
        params: [String: Any],
        completion: (([String: Any]) -> Void)? = nil
    ) {
        let id = UUID().uuidString
        let payload: [String: Any] = ["id": id, "method": method, "params": params]

        // Serialize the payload and confirm we have a live socket *before*
        // registering the completion, so a failed send can't leak a callback
        // that will never fire.
        guard let data = try? JSONSerialization.data(withJSONObject: payload),
              let text = String(data: data, encoding: .utf8),
              let task else { return }

        if let completion {
            completionsLock.lock()
            completions[id] = completion
            completionsLock.unlock()
        }

        task.send(.string(text)) { [weak self] error in
            guard error != nil, let self else { return }
            // The send failed — drop the pending completion so callers that
            // chain follow-up work (refreshSurfaces, etc.) aren't stranded.
            self.completionsLock.lock()
            self.completions.removeValue(forKey: id)
            self.completionsLock.unlock()
        }
    }

    /// Like `send`, but hands the caller the full response — `ok` and
    /// `error.code` as well as `result` — for calls whose caller must
    /// distinguish error codes (e.g. `unsupported` vs. a transient failure).
    func sendCommand(
        method: String,
        params: [String: Any],
        onResponse: @escaping (CommandResponse) -> Void
    ) {
        let id = UUID().uuidString
        let payload: [String: Any] = ["id": id, "method": method, "params": params]

        guard let data = try? JSONSerialization.data(withJSONObject: payload),
              let text = String(data: data, encoding: .utf8),
              let task else { return }

        completionsLock.lock()
        rawCompletions[id] = onResponse
        completionsLock.unlock()

        task.send(.string(text)) { [weak self] error in
            guard error != nil, let self else { return }
            self.completionsLock.lock()
            self.rawCompletions.removeValue(forKey: id)
            self.completionsLock.unlock()
        }
    }

    // MARK: - Private

    /// Whether to try the tailscale host on next connection attempt.
    private var useTailscale = false

    private func startConnection() {
        // Release the previous session's strong hold on us (its delegate)
        // before spinning up a new one; otherwise each reconnect leaks.
        session?.invalidateAndCancel()

        let host: String
        if useTailscale, let tsHost = credentials.tailscaleHost, !tsHost.isEmpty {
            host = tsHost
        } else {
            host = credentials.host
        }

        let urlString = "ws://\(host):\(credentials.port)/ws"
        guard let url = URL(string: urlString) else { return }

        var request = URLRequest(url: url)
        request.setValue("Bearer \(credentials.token)", forHTTPHeaderField: "Authorization")
        request.timeoutInterval = useTailscale ? 10 : 5

        let session = URLSession(configuration: .default, delegate: self, delegateQueue: nil)
        self.session = session
        let task = session.webSocketTask(with: request)
        // The default receive limit (1 MiB) can be exceeded by large payloads
        // like claude session transcripts; raise it so those frames aren't dropped.
        task.maximumMessageSize = 8 * 1024 * 1024
        completionsLock.lock()
        self.task = task
        completionsLock.unlock()
        task.resume()
        receiveNext()
    }

    private func receiveNext() {
        task?.receive { [weak self] result in
            guard let self else { return }
            switch result {
            case .success(let message):
                self.handleMessage(message)
                self.receiveNext()
            case .failure:
                self.handleDisconnect(error: nil)
            }
        }
    }

    private func handleMessage(_ message: URLSessionWebSocketTask.Message) {
        guard case .string(let text) = message,
              let data = text.data(using: .utf8),
              let json = try? JSONSerialization.jsonObject(with: data) as? [String: Any]
        else { return }

        let bridgeMessage = parse(json: json)
        Task { @MainActor in
            self.delegate?.clientDidReceiveMessage(self, message: bridgeMessage)
        }

        // Dispatch command responses to completions. Fire the completion for
        // ANY response carrying a matching id — even an `ok:true` reply with no
        // `result` payload — otherwise chained follow-up work never runs and
        // the UI silently goes stale.
        if let id = json["id"] as? String {
            completionsLock.lock()
            let completion = completions.removeValue(forKey: id)
            let rawCompletion = rawCompletions.removeValue(forKey: id)
            completionsLock.unlock()
            if let completion {
                let result = json["result"] as? [String: Any] ?? [:]
                Task { @MainActor in completion(result) }
            }
            if let rawCompletion {
                let ok = json["ok"] as? Bool ?? false
                let result = json["result"] as? [String: Any] ?? [:]
                var cmdError: CommandError?
                if let errDict = json["error"] as? [String: Any] {
                    cmdError = CommandError(
                        code: errDict["code"] as? String ?? "",
                        message: errDict["message"] as? String ?? ""
                    )
                }
                let response = CommandResponse(id: id, ok: ok, result: result, error: cmdError)
                Task { @MainActor in rawCompletion(response) }
            }
        }
    }

    private func parse(json: [String: Any]) -> BridgeMessage {
        let type = json["type"] as? String ?? ""
        let data = json["data"] as? [String: Any] ?? [:]

        switch type {
        case "connected":
            if let payloadData = try? JSONSerialization.data(withJSONObject: data),
               let payload = try? JSONDecoder().decode(ConnectedPayload.self, from: payloadData) {
                return .connected(payload)
            }
        case "notification.created":
            if let notifData = try? JSONSerialization.data(withJSONObject: data),
               let notif = try? JSONDecoder().decode(BridgeNotification.self, from: notifData) {
                return .notificationCreated(notif)
            }
        case "notification.cleared":
            return .notificationCleared
        case "cmux.connected", "backend.connected", "backend.changed":
            // backend.changed: one runtime of a composite bridge came or went
            // while another stayed up — same response as a (re)connect: refetch
            // the lists, which no longer include the dead runtime's workspaces.
            return .backendConnected
        case "cmux.disconnected", "backend.disconnected":
            return .backendDisconnected
        case "surface.updated":
            if let updateData = try? JSONSerialization.data(withJSONObject: data),
               let update = try? JSONDecoder().decode(SurfaceUpdate.self, from: updateData) {
                return .surfaceUpdated(update)
            }
            return .ignored
        case "surface.screen":
            if let screenData = try? JSONSerialization.data(withJSONObject: data),
               let update = try? JSONDecoder().decode(ScreenUpdate.self, from: screenData) {
                return .surfaceScreen(update)
            }
            return .ignored
        case "surface.screen.ended":
            if let endedData = try? JSONSerialization.data(withJSONObject: data),
               let ended = try? JSONDecoder().decode(SurfaceFramesEndedPush.self, from: endedData) {
                return .surfaceScreenEnded(ended)
            }
            return .ignored
        case "surface.fit.ended":
            if let endedData = try? JSONSerialization.data(withJSONObject: data),
               let ended = try? JSONDecoder().decode(SurfaceFramesEndedPush.self, from: endedData) {
                return .surfaceFitEnded(ended)
            }
            return .ignored
        default:
            break
        }

        // Command response envelope
        if let id = json["id"] as? String {
            let ok = json["ok"] as? Bool ?? false
            let result = json["result"] as? [String: Any] ?? [:]
            var cmdError: CommandError?
            if let errDict = json["error"] as? [String: Any] {
                cmdError = CommandError(
                    code: errDict["code"] as? String ?? "",
                    message: errDict["message"] as? String ?? ""
                )
            }
            return .commandResponse(CommandResponse(id: id, ok: ok, result: result, error: cmdError))
        }

        // Not a command response and not a push we know: ignore it. (This used
        // to fall back to .notificationCleared, which wiped the list on any
        // unknown push.)
        return .ignored
    }

    private func handleDisconnect(error: Error?) {
        // A clean server close fires BOTH receiveNext's .failure and the
        // didCloseWith delegate callback. Dedupe under the lock so we don't
        // schedule two reconnects (racing, doubled connections). The lock also
        // guards `task`, which both callbacks race to clear.
        completionsLock.lock()
        guard task != nil else {
            completionsLock.unlock()
            return
        }
        task = nil
        // Drop any in-flight completions; their responses will never arrive.
        // A fresh connect re-issues refreshWorkspaces/Surfaces from scratch.
        completions.removeAll()
        rawCompletions.removeAll()
        completionsLock.unlock()
        Task { @MainActor in
            self.delegate?.clientDidDisconnect(self, error: error)
        }
        guard shouldReconnect else { return }
        scheduleReconnect()
    }

    private func scheduleReconnect() {
        let delay = backoff
        backoff = min(backoff * 2, 30)

        // Alternate between LAN and Tailscale on each retry
        if credentials.tailscaleHost != nil {
            useTailscale.toggle()
        }

        reconnectTask = Task {
            try? await Task.sleep(nanoseconds: UInt64(delay * 1_000_000_000))
            guard !Task.isCancelled, self.shouldReconnect else { return }
            self.startConnection()
        }
    }
}

// MARK: - URLSessionWebSocketDelegate

extension BridgeClient: URLSessionWebSocketDelegate {
    func urlSession(
        _ session: URLSession,
        webSocketTask: URLSessionWebSocketTask,
        didOpenWithProtocol protocol: String?
    ) {
        backoff = 1
        useTailscale = false // Reset — LAN worked, prefer it next time
        Task { @MainActor in
            self.delegate?.clientDidConnect(self)
        }
    }

    func urlSession(
        _ session: URLSession,
        webSocketTask: URLSessionWebSocketTask,
        didCloseWith closeCode: URLSessionWebSocketTask.CloseCode,
        reason: Data?
    ) {
        handleDisconnect(error: nil)
    }
}
