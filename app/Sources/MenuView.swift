import Sparkle
import SwiftUI

struct MenuView: View {
    let updater: SPUUpdater
    @EnvironmentObject var store: Store
    @Environment(\.openSettings) private var openSettings
    @State private var claiming: Resource?
    @State private var claimPurpose = ""
    @State private var gate = false

    var body: some View {
        VStack(alignment: .leading, spacing: 12) {
            header
            if !store.pendingApprovals.isEmpty { approvals }
            if let s = store.status {
                memory(s)
                ForEach(store.failingSources, id: \.0) { src, why in
                    Label("\(src) unavailable: \(why). Its resources are kept as last seen.", systemImage: "exclamationmark.circle")
                        .font(.caption).foregroundStyle(.orange).lineLimit(2)
                }
                Divider()
                leases
                Divider()
                running(s)
                if let stopped = s.stopped, !stopped.isEmpty {
                    Divider()
                    recentlyStopped(stopped)
                }
            } else {
                daemonDown
            }
            if let err = store.lastActionError {
                Text(err).font(.caption).foregroundStyle(.red).fixedSize(horizontal: false, vertical: true)
            }
            Divider()
            footer
        }
        .padding(14)
        .frame(width: 500)
        .sheet(item: $claiming) { r in claimSheet(r) }
        .sheet(isPresented: $gate) { AutoStopGate(isPresented: $gate).environmentObject(store) }
    }

    private var header: some View {
        HStack {
            Text("AgentRM").font(.headline)
            if store.status?.config.dry_run == true {
                Text("DRY RUN").font(.caption2.bold()).padding(.horizontal, 6).padding(.vertical, 2)
                    .background(.yellow.opacity(0.3), in: Capsule())
                    .help("Unclaimed things are reported but never paused or stopped.")
            }
            Spacer()
            if let s = store.status {
                Text("swept \(s.last_sweep, style: .relative) ago").font(.caption).foregroundStyle(.secondary)
            }
        }
    }

    private var approvals: some View {
        VStack(alignment: .leading, spacing: 6) {
            Label("Waiting for your approval", systemImage: "hand.raised.fill").font(.subheadline.bold()).foregroundStyle(.orange)
            ForEach(store.pendingApprovals) { a in
                HStack(alignment: .top) {
                    VStack(alignment: .leading, spacing: 1) {
                        Text(a.what).font(.callout)
                        Text("asked from \(a.from ?? "a terminal") · expires \(a.expires, style: .relative)")
                            .font(.caption2).foregroundStyle(.secondary)
                        Text("If you did not ask for this, an agent may have. Deny it.").font(.caption2).foregroundStyle(.secondary)
                    }
                    Spacer()
                    Button("Deny") { store.answer(a.id, approve: false) }
                    Button("Approve") { store.answer(a.id, approve: true) }.keyboardShortcut(.defaultAction)
                }
            }
        }
        .padding(8)
        .background(.orange.opacity(0.08), in: RoundedRectangle(cornerRadius: 8))
    }

    private func memory(_ s: Status) -> some View {
        let used = max(0, s.mem.total_mb - s.mem.available_mb)
        let pressure = [1: "normal", 2: "warning", 4: "critical"][s.mem.pressure_level ?? 1] ?? "?"
        return VStack(alignment: .leading, spacing: 6) {
            gauge("Memory", value: used, total: s.mem.total_mb,
                  detail: "\(s.mem.available_mb.gb) available · \(s.mem.compressed_mb.gb) compressed · pressure \(pressure)",
                  warn: store.underPressure)
            gauge("Swap", value: s.mem.swap_used_mb, total: max(s.mem.swap_total_mb, 1),
                  detail: "\(s.mem.swap_used_mb.gb) of \(s.mem.swap_total_mb.gb) · swapping out \(Int(s.swap_rate_mb_min ?? 0)) MB/min",
                  warn: (s.swap_rate_mb_min ?? 0) > 300)
            HStack {
                Text("Can grant now").font(.caption.bold())
                Text(max(0, s.headroom_mb).gb).font(.caption.bold())
                    .foregroundStyle(s.headroom_mb < 512 ? .red : .primary)
                Spacer()
                Text("\(s.pending_mb.gb) promised · \(String(format: "%.1f", s.config.headroom_gb)) GB kept free")
                    .font(.caption).foregroundStyle(.secondary)
            }
            .accessibilityElement(children: .combine)
            if let disk = s.mem.disk_free_gb, let load = s.mem.load1, let cores = s.mem.cores {
                Text("Disk \(Int(disk)) GB free · load \(String(format: "%.1f", load)) on \(cores) cores")
                    .font(.caption).foregroundStyle(disk < 10 ? .red : .secondary)
            }
            ForEach(Array((s.waiters ?? []).enumerated()), id: \.offset) { i, w in
                Text("Waiting \(i + 1): \(w.session.prefix(12)) wants \(w.need_mb.gb) for \(w.kind)\((w.priority ?? 0) > 0 ? " · urgent" : "")")
                    .font(.caption).foregroundStyle(.orange)
            }
        }
    }

    private func gauge(_ title: String, value: Double, total: Double, detail: String, warn: Bool) -> some View {
        VStack(alignment: .leading, spacing: 2) {
            HStack {
                Text(title).font(.caption.bold())
                Spacer()
                Text(detail).font(.caption).foregroundStyle(.secondary)
            }
            ProgressView(value: min(value, total), total: total)
                .tint(warn ? .red : (value / total > 0.8 ? .orange : .accentColor))
                .accessibilityLabel(title)
                .accessibilityValue(detail)
        }
    }

    private var leases: some View {
        VStack(alignment: .leading, spacing: 6) {
            Text("Leases").font(.subheadline.bold())
            if store.leases.isEmpty {
                Text("Nobody holds anything.").font(.caption).foregroundStyle(.secondary)
            }
            ForEach(store.leases) { l in
                HStack(alignment: .top) {
                    VStack(alignment: .leading, spacing: 1) {
                        HStack(spacing: 4) {
                            Image(systemName: l.verified == false ? "questionmark.circle" : "checkmark.seal")
                                .foregroundStyle(l.verified == false ? .orange : .green)
                                .accessibilityLabel(l.verified == false ? "Unverified" : "Verified")
                                .help(l.evidence ?? "")
                            Text("\(l.kind) · \(String(format: "%.1f GB", l.mem_gb))").font(.callout)
                        }
                        Text(l.purpose).font(.caption).foregroundStyle(.secondary).lineLimit(2)
                        Text("\(l.agent) · \(l.session.prefix(12)) · \(l.evidence ?? "")")
                            .font(.caption2).foregroundStyle(.tertiary).lineLimit(1)
                    }
                    Spacer()
                    Button("Release") { store.release(l) }.controlSize(.small)
                }
            }
        }
    }

    private func running(_ s: Status) -> some View {
        VStack(alignment: .leading, spacing: 6) {
            Text("Running").font(.subheadline.bold())
            if store.resources.isEmpty {
                Text("Nothing heavy is running.").font(.caption).foregroundStyle(.secondary)
            }
            ForEach(store.resources) { r in
                HStack(alignment: .top) {
                    VStack(alignment: .leading, spacing: 1) {
                        Text(r.name).font(.callout).lineLimit(1)
                        Text("\(r.kind) · \(r.mem_mb.gb) · \(r.detail)").font(.caption).foregroundStyle(.secondary).lineLimit(1)
                        owner(r, s).font(.caption2)
                    }
                    Spacer()
                    Menu {
                        if r.paused_at != nil { Button("Resume") { store.resume(r.id) } }
                        if r.lease_id == nil { Button("Claim…") { claimPurpose = ""; claiming = r } }
                        Button(r.pinned == true ? "Unpin" : "Pin (never pause or stop)") { store.pin(r) }
                        Divider()
                        Button("Stop now", role: .destructive) { store.stop(r.id) }
                    } label: { Image(systemName: "ellipsis.circle") }
                        .menuStyle(.borderlessButton).fixedSize()
                        .accessibilityLabel("Actions for \(r.name)")
                }
            }
        }
    }

    @ViewBuilder
    private func owner(_ r: Resource, _ s: Status) -> some View {
        if let paused = r.paused_at {
            let stopAt = paused.addingTimeInterval(Double(s.config.pause_minutes ?? 30) * 60)
            Text("paused, nothing lost: stops \(stopAt, style: .relative) unless claimed").foregroundStyle(.purple)
        } else if r.pinned == true {
            Text("pinned").foregroundStyle(.blue)
        } else if let lid = r.lease_id, let l = s.leases?[lid] {
            Text("held by \(l.agent) · \(l.purpose)").foregroundStyle(.green).lineLimit(1)
        } else if let since = r.orphan_since {
            let at = since.addingTimeInterval(Double(s.config.grace_minutes) * 60)
            if s.config.dry_run {
                Text("unclaimed (dry run: would pause at \(at, style: .time))").foregroundStyle(.orange)
            } else {
                Text("unclaimed: pauses \(at, style: .relative)").foregroundStyle(.red)
            }
        } else {
            Text("unclaimed").foregroundStyle(.orange)
        }
    }

    private func recentlyStopped(_ stopped: [StoppedItem]) -> some View {
        DisclosureGroup("Stopped by AgentRM (\(stopped.count))") {
            ForEach(stopped.suffix(5).reversed()) { item in
                HStack {
                    VStack(alignment: .leading, spacing: 1) {
                        Text(item.resource.name).font(.caption)
                        Text("\(item.stopped_at, style: .relative) ago · \(item.why)").font(.caption2).foregroundStyle(.secondary).lineLimit(1)
                    }
                    Spacer()
                    Button("Restore") { store.restore(item.resource.id) }.controlSize(.small)
                }
            }
        }
        .font(.caption)
    }

    private var daemonDown: some View {
        VStack(alignment: .leading, spacing: 8) {
            Label("The AgentRM daemon is not running.", systemImage: "exclamationmark.triangle")
            if let e = store.registrationError ?? store.daemonError {
                Text(e).font(.caption).foregroundStyle(.secondary).lineLimit(4)
            }
            if store.loginItemStatus == .requiresApproval {
                Text("macOS needs your approval: System Settings → General → Login Items → allow AgentRM.").font(.caption)
                Button("Open Login Items") { SMAppServiceOpen.loginItems() }
            } else {
                Button("Start daemon") { store.registerDaemon() }
            }
        }
    }

    private var footer: some View {
        HStack {
            Toggle("Auto-stop", isOn: Binding(
                get: { store.status?.config.dry_run == false },
                set: { on in
                    if on { gate = true } else { store.setAutoStop(false) }
                }))
            .toggleStyle(.switch).controlSize(.small).disabled(store.status == nil)
            .help("Pause, then stop, what nobody claims. Off: dry run, which only reports.")
            Spacer()
            Button("Settings…") {
                NSApp.activate(ignoringOtherApps: true)
                openSettings()
            }
            Menu {
                Button("Check for Updates…") { updater.checkForUpdates() }
                Button("Quit AgentRM") { NSApp.terminate(nil) }
            } label: { Image(systemName: "power") }
                .menuStyle(.borderlessButton).fixedSize()
                .accessibilityLabel("Quit or check for updates")
        }
    }

    private func claimSheet(_ r: Resource) -> some View {
        VStack(alignment: .leading, spacing: 10) {
            Text("Claim \(r.name)").font(.headline)
            Text("Say why you need it. The claim lasts 4 hours while you use it; unused, you are asked again.")
                .font(.caption).foregroundStyle(.secondary)
            TextField("Purpose", text: $claimPurpose)
            HStack {
                Spacer()
                Button("Cancel") { claiming = nil }
                Button("Claim") {
                    store.claim(r.id, purpose: claimPurpose, hours: 4)
                    claiming = nil
                }.keyboardShortcut(.defaultAction).disabled(claimPurpose.trimmingCharacters(in: .whitespaces).isEmpty)
            }
        }
        .padding(16).frame(width: 360)
    }
}

/// Before auto-stop is turned on: what dry run showed, and whether that is enough.
struct AutoStopGate: View {
    @Binding var isPresented: Bool
    @EnvironmentObject var store: Store

    var body: some View {
        let r = store.readiness
        VStack(alignment: .leading, spacing: 12) {
            Label(r.ready ? "Dry run looks good" : "Not enough evidence yet", systemImage: r.ready ? "checkmark.shield" : "exclamationmark.shield")
                .font(.headline).foregroundStyle(r.ready ? .green : .orange)
            Text(r.summary)
            if !r.ready {
                Text("We recommend at least 7 days of dry run and 5 reviewed decisions with under 2% wrong before AgentRM pauses and stops things by itself. Review the decisions in Settings → History.")
                    .font(.callout).foregroundStyle(.secondary)
            }
            Text("With auto-stop on, unclaimed things are paused after \(store.status?.config.grace_minutes ?? 10) minutes (nothing is lost) and stopped \(store.status?.config.pause_minutes ?? 30) minutes later. Everything stopped can be restored.")
                .font(.callout)
            HStack {
                Spacer()
                Button("Keep dry run") { isPresented = false }.keyboardShortcut(.cancelAction)
                Button(r.ready ? "Turn on auto-stop" : "Turn it on anyway") {
                    store.setAutoStop(true)
                    isPresented = false
                }
                .keyboardShortcut(r.ready ? .defaultAction : .init("t"))
            }
        }
        .padding(18).frame(width: 440)
        .task { await store.refreshReview() }
    }
}

enum SMAppServiceOpen {
    static func loginItems() {
        if let url = URL(string: "x-apple.systempreferences:com.apple.LoginItems-Settings.extension") {
            NSWorkspace.shared.open(url)
        }
    }
}
