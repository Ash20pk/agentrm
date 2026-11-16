import SwiftUI

struct MenuView: View {
    @EnvironmentObject var store: Store
    @Environment(\.openSettings) private var openSettingsAction
    @State private var claiming: Resource?
    @State private var claimPurpose = ""

    var body: some View {
        VStack(alignment: .leading, spacing: 12) {
            header
            if let s = store.status {
                memory(s)
                Divider()
                leases
                Divider()
                running(s)
                if let events = s.events, !events.isEmpty {
                    Divider()
                    recent(events)
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
        .frame(width: 480)
        .sheet(item: $claiming) { r in claimSheet(r) }
    }

    private var header: some View {
        HStack {
            Text("AgentRM").font(.headline)
            if store.status?.config.dry_run == true {
                Text("DRY RUN").font(.caption2.bold()).padding(.horizontal, 6).padding(.vertical, 2)
                    .background(.yellow.opacity(0.3), in: Capsule())
                    .help("Orphans are reported but never stopped. Turn on auto-stop below.")
            }
            Spacer()
            if let s = store.status {
                Text("swept \(s.last_sweep, style: .relative) ago").font(.caption).foregroundStyle(.secondary)
            }
        }
    }

    private func memory(_ s: Status) -> some View {
        let used = max(0, s.mem.total_mb - s.mem.available_mb)
        return VStack(alignment: .leading, spacing: 6) {
            gauge("Memory", value: used, total: s.mem.total_mb,
                  detail: "\(s.mem.available_mb.gb) available · \(s.mem.compressed_mb.gb) compressed · \(s.mem.free_percent)% free",
                  warn: store.underPressure)
            gauge("Swap", value: s.mem.swap_used_mb, total: max(s.mem.swap_total_mb, 1),
                  detail: "\(s.mem.swap_used_mb.gb) of \(s.mem.swap_total_mb.gb)", warn: false)
            if let disk = s.mem.disk_free_gb, let load = s.mem.load1, let cores = s.mem.cores {
                HStack {
                    Text("Disk \(Int(disk)) GB free").foregroundStyle(disk < 10 ? .red : .secondary)
                    Text("·").foregroundStyle(.secondary)
                    Text(String(format: "Load %.1f on %d cores", load, cores))
                        .foregroundStyle(load > Double(cores) * 1.5 ? .red : .secondary)
                }.font(.caption)
            }
            gauge("Committed", value: s.committed_mb, total: s.budget_mb,
                  detail: "\(s.committed_mb.gb) of \(s.budget_mb.gb) budget (\(Int(s.config.reserve_gb)) GB kept for macOS)",
                  warn: s.committed_mb > s.budget_mb)
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
        }
    }

    private var leases: some View {
        VStack(alignment: .leading, spacing: 6) {
            Text("Leases").font(.subheadline.bold())
            if store.leases.isEmpty {
                Text("No agent holds anything.").font(.caption).foregroundStyle(.secondary)
            }
            ForEach(store.leases) { l in
                HStack(alignment: .top) {
                    VStack(alignment: .leading, spacing: 1) {
                        HStack(spacing: 4) {
                            Image(systemName: l.verified == false ? "questionmark.circle" : "checkmark.seal")
                                .foregroundStyle(l.verified == false ? .orange : .green)
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
                        if r.lease_id == nil { Button("Claim…") { claimPurpose = ""; claiming = r } }
                        Button(r.pinned == true ? "Unpin" : "Pin (never auto-stop)") { store.pin(r) }
                        Divider()
                        Button("Stop now", role: .destructive) { store.stop(r) }
                    } label: { Image(systemName: "ellipsis.circle") }
                        .menuStyle(.borderlessButton).fixedSize()
                }
            }
        }
    }

    @ViewBuilder
    private func owner(_ r: Resource, _ s: Status) -> some View {
        if r.pinned == true {
            Text("pinned").foregroundStyle(.blue)
        } else if let lid = r.lease_id, let l = s.leases?[lid] {
            Text("held by \(l.agent) · \(l.purpose)").foregroundStyle(.green).lineLimit(1)
        } else if let since = r.orphan_since {
            let stopAt = since.addingTimeInterval(Double(s.config.grace_minutes) * 60)
            if s.config.dry_run {
                Text("unclaimed (dry run: would stop at \(stopAt, style: .time))").foregroundStyle(.orange)
            } else {
                Text("unclaimed: stops \(stopAt, style: .relative)").foregroundStyle(.red)
            }
        } else {
            Text("unclaimed").foregroundStyle(.orange)
        }
    }

    private func recent(_ events: [AgentEvent]) -> some View {
        VStack(alignment: .leading, spacing: 3) {
            Text("Recent").font(.subheadline.bold())
            ForEach(events.suffix(4).reversed()) { e in
                Text("\(e.time, style: .time)  \(e.text)").font(.caption2).foregroundStyle(.secondary).lineLimit(1)
            }
        }
    }

    private var daemonDown: some View {
        VStack(alignment: .leading, spacing: 8) {
            Label("The agentrm daemon is not running.", systemImage: "exclamationmark.triangle")
            if let e = store.registrationError ?? store.daemonError { Text(e).font(.caption).foregroundStyle(.secondary).lineLimit(3) }
            if store.loginItemStatus == .requiresApproval {
                Text("macOS needs your approval: System Settings → General → Login Items → allow AgentRM.")
                    .font(.caption)
                Button("Open Login Items") { SMAppServiceOpen.loginItems() }
            } else {
                Button("Start daemon") { store.registerDaemon() }
            }
        }
    }

    private var footer: some View {
        HStack {
            Toggle("Auto-stop orphans", isOn: Binding(
                get: { store.status?.config.dry_run == false },
                set: { store.setAutoStop($0) }))
            .toggleStyle(.switch).controlSize(.small).disabled(store.status == nil)
            Spacer()
            Button("Settings…") {
                NSApp.activate(ignoringOtherApps: true)
                openSettingsAction()
            }
            Button("Quit") { NSApp.terminate(nil) }
        }
    }

    private func claimSheet(_ r: Resource) -> some View {
        VStack(alignment: .leading, spacing: 10) {
            Text("Claim \(r.name)").font(.headline)
            Text("Say why you need it. The claim lasts 4 hours; it's then stopped if unclaimed again.")
                .font(.caption).foregroundStyle(.secondary)
            TextField("Purpose", text: $claimPurpose)
            HStack {
                Spacer()
                Button("Cancel") { claiming = nil }
                Button("Claim") {
                    store.claim(r, purpose: claimPurpose, hours: 4)
                    claiming = nil
                }.keyboardShortcut(.defaultAction).disabled(claimPurpose.trimmingCharacters(in: .whitespaces).isEmpty)
            }
        }
        .padding(16).frame(width: 360)
    }
}

enum SMAppServiceOpen {
    static func loginItems() {
        if let url = URL(string: "x-apple.systempreferences:com.apple.LoginItems-Settings.extension") {
            NSWorkspace.shared.open(url)
        }
    }
}
