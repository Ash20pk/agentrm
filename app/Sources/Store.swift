import AppKit
import Foundation
import ServiceManagement
import UserNotifications
import os

let log = Logger(subsystem: "dev.agentrm", category: "app")

@MainActor
final class Store: ObservableObject {
    @Published var status: Status?
    @Published var setup: SetupCheck?
    @Published var review: ReviewResp?
    @Published var daemonError: String?
    @Published var lastActionError: String?
    @Published var registrationError: String?
    @Published var loginItemStatus: SMAppService.Status = .notRegistered
    @Published var hookPreview = ""
    @Published var receipt: Receipt?
    /// The meter once a minute for the last hour, oldest first: the Activity window's waterfall.
    /// Kept in memory only, so it starts empty when the app starts.
    @Published var waterfall: [[Segment]] = []
    private var waterfallMinute = -1

    private var timer: Timer?
    /// A preview store shows fixed data and never calls the daemon.
    private var frozen = false
    private var lastEventSeq = -1
    let service = SMAppService.agent(plistName: "dev.agentrm.daemon.plist")
    let notifications = NotificationHandler()

    var onboarded: Bool {
        get { UserDefaults.standard.bool(forKey: "onboarded") }
        set { UserDefaults.standard.set(newValue, forKey: "onboarded"); objectWillChange.send() }
    }

    /// When dry run started, for the auto-stop gate.
    var dryRunSince: Date {
        if let d = UserDefaults.standard.object(forKey: "dryRunSince") as? Date { return d }
        let now = Date()
        UserDefaults.standard.set(now, forKey: "dryRunSince")
        return now
    }

    init() {
        loginItemStatus = service.status
        notifications.store = self
        notifications.install()
        if service.status != .enabled && service.status != .requiresApproval {
            registerDaemon()
        }
        timer = Timer.scheduledTimer(withTimeInterval: 4, repeats: true) { [weak self] _ in
            Task { @MainActor in
                // A slow `agentrm status` (a busy daemon) must not pile up behind itself.
                guard let self, self.refreshesInFlight == 0 else { return }
                await self.refresh()
            }
        }
        timer?.tolerance = 1
        Task {
            await refresh()
            await restartDaemonIfStale()
            await refreshSetup()
            await refreshReview()
            await repairIfMoved()
        }
    }

    #if DEBUG
    /// For snapshots: fixed data, no daemon registration, no timers, no notifications.
    init(preview status: Status?, setup: SetupCheck?, review: ReviewResp?) {
        frozen = true
        self.status = status
        self.setup = setup
        self.review = review
        loginItemStatus = status == nil ? .notRegistered : .enabled
    }

    func setPreviewWaterfall(_ w: [[Segment]]) { waterfall = w }
    #endif

    var leases: [Lease] { (status?.leases ?? [:]).values.sorted { $0.created < $1.created } }
    var resources: [Resource] { (status?.resources ?? [:]).values.sorted { $0.mem_mb > $1.mem_mb } }
    var orphans: [Resource] { resources.filter { $0.lease_id == nil && $0.pinned != true && $0.orphan_since != nil } }
    var pendingApprovals: [Approval] {
        (status?.approvals ?? [:]).values.filter { $0.state == "pending" && $0.expires > Date() }.sorted { $0.created < $1.created }
    }
    var failingSources: [(String, String)] {
        (status?.sources ?? [:]).filter { !$0.value.isEmpty }.sorted { $0.key < $1.key }.map { ($0.key, $0.value) }
    }

    var underPressure: Bool {
        guard let s = status else { return false }
        return (s.mem.pressure_level ?? 1) >= 4 || (s.swap_rate_mb_min ?? 0) > 300
    }

    func refresh() async {
        if frozen { return }
        refreshesStarted += 1
        let seq = refreshesStarted
        refreshesInFlight += 1
        defer { refreshesInFlight -= 1 }
        let result: Result<Status, Error>
        do {
            let data = try await CLI.run(["status", "--json"])
            result = .success(try CLI.decoder.decode(Status.self, from: data))
        } catch {
            result = .failure(error)
        }
        // A refresh that started earlier but finished later must not overwrite a newer answer.
        guard seq > refreshApplied else { return }
        refreshApplied = seq
        switch result {
        case .success(let s):
            status = s
            daemonError = nil
            notifications.post(newEvents(s.events ?? []))
            record()
        case .failure(let error):
            status = nil
            daemonError = error.localizedDescription
        }
        loginItemStatus = service.status
    }

    private var refreshesInFlight = 0
    private var refreshesStarted = 0
    private var refreshApplied = 0
    private var restartedStaleDaemon = false

    /// The version of the helper bundled with this app, which is what the daemon should run.
    var bundledVersion: String {
        Bundle.main.object(forInfoDictionaryKey: "CFBundleShortVersionString") as? String ?? ""
    }

    /// After a Sparkle update the daemon launchd started from the old app keeps running old code;
    /// if the API version changed, every hook fails open and nothing is enforced. Restart it once
    /// per launch when it reports another version, or refuses to talk to this one.
    func restartDaemonIfStale() async {
        if frozen || restartedStaleDaemon || service.status != .enabled { return }
        let running = status?.daemon_version
        let versionMismatch = running != nil && running != "dev" && running != bundledVersion
        let apiMismatch = status == nil && (daemonError ?? "").contains("API version")
        guard versionMismatch || apiMismatch else { return }
        restartedStaleDaemon = true
        let was = running ?? "an older API", now = bundledVersion
        log.notice("daemon runs \(was, privacy: .public), app bundles \(now, privacy: .public): restarting it")
        let label = "gui/\(getuid())/dev.agentrm.daemon"
        if (try? await Self.launchctl(["kickstart", "-k", label])) == nil {
            // Not loaded under that label (or launchctl refused): register it afresh.
            try? await service.unregister()
            registerDaemon()
        }
        try? await Task.sleep(nanoseconds: 2_000_000_000)
        await refresh()
    }

    private static func launchctl(_ args: [String]) async throws {
        try await withCheckedThrowingContinuation { (cont: CheckedContinuation<Void, Error>) in
            DispatchQueue.global(qos: .userInitiated).async {
                let p = Process()
                p.executableURL = URL(fileURLWithPath: "/bin/launchctl")
                p.arguments = args
                p.standardOutput = FileHandle.nullDevice
                p.standardError = FileHandle.nullDevice
                do {
                    try p.run()
                    p.waitUntilExit()
                } catch {
                    cont.resume(throwing: error)
                    return
                }
                if p.terminationStatus == 0 {
                    cont.resume()
                } else {
                    cont.resume(throwing: CLI.Failure(message: "launchctl \(args.joined(separator: " ")) exited \(p.terminationStatus)"))
                }
            }
        }
    }

    private func record() {
        guard let r = reading else { return }
        let minute = Int(Date().timeIntervalSince1970 / 60)
        if minute == waterfallMinute, !waterfall.isEmpty {
            waterfall[waterfall.count - 1] = r.segments
        } else {
            waterfall.append(r.segments)
            waterfallMinute = minute
            if waterfall.count > 60 { waterfall.removeFirst(waterfall.count - 60) }
        }
    }

    func refreshSetup() async {
        if frozen { return }
        if let data = try? await CLI.run(["setup", "check", "--json"]) {
            setup = try? CLI.decoder.decode(SetupCheck.self, from: data)
        }
    }

    func refreshReview() async {
        if frozen { return }
        if let data = try? await CLI.run(["review", "--all", "--json"]) {
            review = try? CLI.decoder.decode(ReviewResp.self, from: data)
        }
    }

    func loadHookPreview() async {
        if frozen { hookPreview = "{ \"hooks\": { \"PreToolUse\": [ … agentrm hook pre --proto 2 … ] } }"; return }
        hookPreview = await CLI.text(["setup", "preview", "claude"])
    }

    /// If AgentRM was moved or updated, the hooks and the CLI link still point at the old copy.
    /// Re-point what is already installed (nothing new is installed). Only for an installed app.
    func repairIfMoved() async {
        guard let s = setup, !(s.problems ?? []).isEmpty,
              Bundle.main.bundlePath.hasPrefix("/Applications/") else { return }
        do {
            _ = try await CLI.run(["setup", "repair"])
            log.notice("repaired hooks and links after a move or update")
            await refreshSetup()
        } catch {
            lastActionError = "Could not repair the hooks: \(error.localizedDescription)"
        }
    }

    func act(_ args: [String]) {
        Task {
            do {
                _ = try await CLI.run(args)
                lastActionError = nil
            } catch {
                lastActionError = error.localizedDescription
            }
            await refresh()
            await refreshSetup()
        }
    }

    func release(_ l: Lease) { act(["release", l.id]) }
    func stop(_ id: String) { act(["stop", id]) }
    func resume(_ id: String) { act(["resume", id]) }
    func restore(_ id: String) { act(["restore", id]) }
    func pin(_ r: Resource) { act([r.pinned == true ? "unpin" : "pin", r.id]) }
    func claim(_ id: String, purpose: String, hours: Int) {
        act(["claim", id, "--purpose", purpose, "--ttl", String(hours * 60)])
    }
    func answer(_ approvalID: String, approve: Bool) { act([approve ? "approve" : "deny", approvalID]) }
    func mark(_ d: Decision, right: Bool) {
        Task {
            _ = try? await CLI.run(["review", "mark", d.id, right ? "right" : "wrong"])
            await refreshReview()
        }
    }
    func setAutoStop(_ on: Bool) {
        if !on { UserDefaults.standard.set(Date(), forKey: "dryRunSince") }
        act(["config", "--dry-run=\(on ? "false" : "true")"])
    }
    func setLimits(headroom: Double, grace: Int, idle: Int, pause: Int) {
        act(["config", "--headroom", String(headroom), "--grace", String(grace), "--idle", String(idle), "--pause", String(pause)])
    }
    /// Stops now what ended sessions left: your one click, so it is yours to make.
    func collect() { act(["collect"]) }
    func setLevel(_ category: String, on: Bool) { act(["config", "--auto-stop", "\(category)=\(on ? "on" : "off")"]) }
    func refreshReceipt(days: Int = 7) async {
        if frozen { return }
        if let data = try? await CLI.run(["receipt", "--days", String(days), "--json"]) {
            receipt = try? CLI.decoder.decode(Receipt.self, from: data)
        }
    }
    /// What ended sessions left running, collectable now: the first-minute view.
    var leftBehind: [Resource] {
        resources.filter { $0.owner_gone_at != nil && $0.lease_id == nil && $0.ask_only != true && $0.pinned != true }
    }
    var askOnlyLeftBehind: [Resource] {
        resources.filter { $0.owner_gone_at != nil && $0.lease_id == nil && $0.ask_only == true }
    }
    func setup(_ what: String, install: Bool) { act(install ? ["setup", what] : ["setup", what, "--uninstall"]) }

    /// Removes everything AgentRM installed into other tools, stops the daemon, and quits. If
    /// any step fails, it says which and changes nothing else: the daemon stays on, so paused
    /// resources can still be resumed, and Uninstall can be tried again.
    func uninstall() {
        Task {
            let done: String
            do {
                let out = try await CLI.run(["setup", "uninstall"], timeout: 60)
                done = String(data: out, encoding: .utf8) ?? ""
            } catch {
                let alert = NSAlert()
                alert.alertStyle = .warning
                alert.messageText = "AgentRM could not uninstall everything"
                alert.informativeText = "\(error.localizedDescription)\n\nThe background daemon is still on and the app stays installed. Fix the problem above, then choose Uninstall again."
                alert.runModal()
                await refreshSetup()
                return
            }
            unregisterDaemon()
            UserDefaults.standard.removeObject(forKey: "onboarded")
            let alert = NSAlert()
            alert.messageText = "AgentRM is uninstalled"
            let steps = done.split(separator: "\n").filter { !$0.hasPrefix("To remove the app") }.joined(separator: "\n")
            alert.informativeText = "\(steps)\n\nThe background daemon is off. Move AgentRM to the Bin to finish. Its data is in ~/.agentrm."
            alert.runModal()
            NSWorkspace.shared.activateFileViewerSelecting([Bundle.main.bundleURL])
            NSApp.terminate(nil)
        }
    }

    func registerDaemon() {
        do {
            try service.register()
        } catch {
            log.error("register failed: \(error.localizedDescription, privacy: .public)")
            registrationError = "Could not register the background daemon: \(error.localizedDescription)"
        }
        loginItemStatus = service.status
    }

    func unregisterDaemon() {
        try? service.unregister()
        loginItemStatus = service.status
    }

    private func newEvents(_ events: [AgentEvent]) -> [AgentEvent] {
        let maxSeq = events.map(\.seq).max() ?? 0
        defer { lastEventSeq = max(lastEventSeq, maxSeq) }
        if lastEventSeq < 0 || maxSeq < lastEventSeq { return [] } // first load, or a daemon restart
        return events.filter { $0.seq > lastEventSeq }
    }

    // ---- the auto-stop gate ----

    struct Readiness {
        var days: Int
        var wouldAct: Int
        var reviewed: Int
        var wrong: Int
        var ready: Bool
        var summary: String
    }

    /// Whether dry run has shown enough to recommend auto-stop: a week of it, at least five
    /// reviewed decisions, and under 2% of them wrong.
    var readiness: Readiness {
        let days = Calendar.current.dateComponents([.day], from: dryRunSince, to: Date()).day ?? 0
        let ds = (review?.decisions ?? []).filter { $0.dry_run && $0.action != "reclaim" }
        let reviewed = review?.reviewed ?? 0
        let wrong = review?.wrong ?? 0
        let rate = reviewed > 0 ? Double(wrong) / Double(reviewed) : 1
        let ready = days >= 7 && reviewed >= 5 && rate < 0.02
        var s = "In \(days) day\(days == 1 ? "" : "s") of dry run, AgentRM would have paused or stopped \(ds.count) thing\(ds.count == 1 ? "" : "s"). "
        if reviewed == 0 {
            s += "You have not reviewed any of those decisions yet."
        } else {
            s += "You reviewed \(reviewed); \(wrong) \(wrong == 1 ? "was" : "were") wrong."
        }
        return Readiness(days: days, wouldAct: ds.count, reviewed: reviewed, wrong: wrong, ready: ready, summary: s)
    }
}
