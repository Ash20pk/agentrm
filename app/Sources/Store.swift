import Foundation
import ServiceManagement
import UserNotifications
import os

let log = Logger(subsystem: "dev.agentrm", category: "app")

@MainActor
final class Store: ObservableObject {
    @Published var status: Status?
    @Published var setup: SetupCheck?
    @Published var daemonError: String?
    @Published var lastActionError: String?
    @Published var registrationError: String?
    @Published var loginItemStatus: SMAppService.Status = .notRegistered

    private var timer: Timer?
    private var lastEventSeq = -1
    let service = SMAppService.agent(plistName: "dev.agentrm.daemon.plist")

    init() {
        loginItemStatus = service.status
        log.notice("daemon service status at launch: \(String(describing: self.service.status.rawValue), privacy: .public)")
        UNUserNotificationCenter.current().requestAuthorization(options: [.alert, .sound]) { _, _ in }
        if service.status != .enabled && service.status != .requiresApproval {
            registerDaemon()
        }
        timer = Timer.scheduledTimer(withTimeInterval: 4, repeats: true) { [weak self] _ in
            Task { @MainActor in await self?.refresh() }
        }
        Task { await refresh(); await refreshSetup() }
    }

    var leases: [Lease] {
        (status?.leases ?? [:]).values.sorted { $0.created < $1.created }
    }

    var resources: [Resource] {
        (status?.resources ?? [:]).values.sorted { $0.mem_mb > $1.mem_mb }
    }

    var orphans: [Resource] { resources.filter { $0.lease_id == nil && $0.pinned != true && $0.orphan_since != nil } }

    var underPressure: Bool {
        guard let s = status else { return false }
        return s.mem.free_percent >= 0 && s.mem.free_percent < s.config.min_free_percent
    }

    func refresh() async {
        do {
            let data = try await CLI.run(["status", "--json"])
            let s = try CLI.decoder.decode(Status.self, from: data)
            status = s
            daemonError = nil
            notifyNewEvents(s.events ?? [])
        } catch {
            status = nil
            daemonError = error.localizedDescription
        }
        loginItemStatus = service.status
    }

    func refreshSetup() async {
        if let data = try? await CLI.run(["setup", "check"]) {
            setup = try? CLI.decoder.decode(SetupCheck.self, from: data)
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
    func stop(_ r: Resource) { act(["stop", r.id]) }
    func pin(_ r: Resource) { act([r.pinned == true ? "unpin" : "pin", r.id]) }
    func claim(_ r: Resource, purpose: String, hours: Int) {
        act(["claim", r.id, "--purpose", purpose, "--ttl", String(hours * 60), "--agent", "you", "--session", "you"])
    }
    func setAutoStop(_ on: Bool) { act(["config", "--dry-run=\(on ? "false" : "true")"]) }
    func setLimits(reserve: Double, grace: Int, minFree: Int) {
        act(["config", "--reserve", String(reserve), "--grace", String(grace), "--min-free", String(minFree)])
    }
    func setup(_ what: String, install: Bool) { act(install ? ["setup", what] : ["setup", what, "--uninstall"]) }

    func registerDaemon() {
        do {
            try service.register()
            log.notice("registered daemon, status now \(String(describing: self.service.status.rawValue), privacy: .public)")
        } catch {
            log.error("register failed: \(error.localizedDescription, privacy: .public) \(String(describing: error), privacy: .public)")
            registrationError = "Could not register the background daemon: \(error.localizedDescription)"
        }
        loginItemStatus = service.status
    }

    func unregisterDaemon() {
        try? service.unregister()
        loginItemStatus = service.status
    }

    private func notifyNewEvents(_ events: [AgentEvent]) {
        let maxSeq = events.map(\.seq).max() ?? 0
        defer { lastEventSeq = max(lastEventSeq, maxSeq) }
        // First load, or the daemon restarted: don't replay history.
        if lastEventSeq < 0 || maxSeq < lastEventSeq { return }
        for e in events where e.seq > lastEventSeq && ["ORPHAN", "STOPPED", "STOP"].contains(e.kind) {
            let content = UNMutableNotificationContent()
            content.title = e.kind == "ORPHAN" ? "Unclaimed resource" : "Stopped"
            content.body = e.text
            UNUserNotificationCenter.current().add(
                UNNotificationRequest(identifier: "agentrm-\(e.seq)", content: content, trigger: nil))
        }
    }
}
