import ServiceManagement
import Sparkle
import SwiftUI

/// The menu-bar window: a faceplate with the LED meter, three gauges and a lamp per thing running.
/// Colour is state: green in use, amber waiting, red nobody, ice blue paused, grey you and macOS.
struct MenuView: View {
    let updater: SPUUpdater
    @EnvironmentObject var store: Store
    @Environment(\.openWindow) private var openWindow
    @State private var keeping: Resource?
    @State private var gate = false
    @State private var listHeight: CGFloat = 100

    private let maxListHeight: CGFloat = 300

    var body: some View {
        VStack(alignment: .leading, spacing: 12) {
            header
            if let err = store.lastActionError { errorStrip(err) }
            ForEach(store.pendingApprovals) { approval($0) }
            if let s = store.status, let r = store.reading {
                ForEach(store.failingSources, id: \.0) { src, why in
                    strip(color: Lamp.waiting.color, text: "Can't read \(src): \(why). Showing what it last reported.")
                }
                ForEach(s.leaks ?? []) { leak in
                    strip(color: Lamp.nobody.color, text: leakText(leak))
                }
                meter(s, r)
                gauges(s, r)
                if !r.items.isEmpty { items(s, r) }
                if r.items.allSatisfy({ $0.lamp == .inUse || $0.lamp == .system }), let stopped = s.stopped, !stopped.isEmpty {
                    stoppedPanel(stopped)
                }
                if s.config.dry_run && store.readiness.ready { readyPanel }
            } else {
                offline
            }
            footer
        }
        .padding(14)
        .frame(width: 380)
        .background(Ink.bg)
        .foregroundStyle(Ink.text)
        .font(Face.body(14))
        .environment(\.colorScheme, .dark)
        .sheet(item: $keeping) { r in KeepSheet(resource: r) { keeping = nil }.environmentObject(store) }
        .sheet(isPresented: $gate) { AutoStopGate(isPresented: $gate).environmentObject(store) }
    }

    // MARK: header

    private var header: some View {
        HStack(spacing: 10) {
            Text("AGENTRM").font(Face.mono(13, .bold)).tracking(2)
            LampLabel(lamp: headerLamp.color, text: headerLamp.text)
            Spacer()
            if let s = store.status {
                if s.config.dry_run {
                    LampLabel(lamp: Lamp.waiting.color, text: "Dry run")
                        .help("AgentRM asks and reports, but never pauses or stops anything.")
                } else {
                    LampLabel(lamp: Lamp.inUse.color, text: "Auto-stop")
                }
            }
        }
    }

    private var headerLamp: (color: Color, text: String) {
        guard let s = store.status else { return (Ink.scale, "Offline") }
        if store.underPressure { return (Lamp.nobody.color, "Overload") }
        if !store.pendingApprovals.isEmpty || (store.reading?.nobodyMB ?? 0) > 0 { return (Lamp.waiting.color, "Needs you") }
        let age = -s.last_sweep.timeIntervalSinceNow
        return age < 60 ? (Lamp.inUse.color, "All clear") : (Ink.scale, "Last read \(Fmt.since(s.last_sweep)) ago")
    }

    // MARK: approval

    @ViewBuilder
    private func approval(_ a: Approval) -> some View {
        if let ask = a.vault { VaultApprovalCard(approval: a, ask: ask) } else { plainApproval(a) }
    }

    private func plainApproval(_ a: Approval) -> some View {
        VStack(alignment: .leading, spacing: 8) {
            LampLabel(lamp: Lamp.waiting.color, text: "Approval, expires \(Fmt.rel(a.expires))")
            Text(a.what).font(Face.body(17, .semibold)).fixedSize(horizontal: false, vertical: true)
            Text(a.path == "/leak"
                 ? "AgentRM is asking: its memory keeps climbing and nothing explains it. Nothing happens unless you approve."
                 : "Asked from \(a.from ?? "a terminal"). If you didn't type it, an agent did.")
                .font(Face.body(12)).foregroundStyle(Color(hex: 0xC9C0A8)).fixedSize(horizontal: false, vertical: true)
            HStack(spacing: 8) {
                Spacer()
                Button("Deny") { store.answer(a.id, approve: false) }.buttonStyle(.key)
                Button("Approve") { store.answer(a.id, approve: true) }.buttonStyle(.key(.amber)).keyboardShortcut(.defaultAction)
            }
        }
        .padding(12)
        .panel(Ink.amberPanel, stroke: Color(hex: 0x8A6A12))
        .shadow(color: Lamp.waiting.color.opacity(0.12), radius: 12)
    }

    // MARK: leaks

    private func leakText(_ l: Leak) -> String {
        var text = "Leaking: \(l.name ?? "a process")"
        if let pid = l.pid { text += " (pid \(pid))" }
        if let from = l.start_mb, let now = l.now_mb { text += ", \(Fmt.gb(from)) to \(Fmt.gb(now)) GB" }
        if let since = l.since { text += " in \(Fmt.since(since))" }
        if let rate = l.rate_mb_per_hour { text += String(format: ", +%.0f MB/h", rate) }
        if l.dismissed == true { text += ". Left alone." }
        return text
    }

    // MARK: meter

    private func meter(_ s: Status, _ r: Reading) -> some View {
        VStack(alignment: .leading, spacing: 8) {
            HStack {
                Silk("Memory")
                Spacer()
                Text("\(Fmt.gb(r.totalMB - s.mem.available_mb)) / \(Fmt.gb(r.totalMB)) GB\(store.underPressure ? ", swapping" : "")")
                    .font(Face.mono(12)).foregroundStyle(store.underPressure ? Lamp.nobody.textColor : Ink.dim)
            }
            LEDMeter(segments: r.segments)
            LazyVGrid(columns: Array(repeating: GridItem(.flexible(), alignment: .leading), count: 3), alignment: .leading, spacing: 4) {
                ForEach(Lamp.allCases.sorted { $0.legendOrder < $1.legendOrder }, id: \.self) { lamp in
                    if let mb = r.mbByLamp[lamp], mb >= 50 { legend(LampDot(lamp), lamp.name, mb) }
                }
                legend(LampDot(color: Ink.off, glow: false), "Free", r.freeMB + r.keptMB)
            }
        }
        .padding(.horizontal, 12).padding(.top, 14).padding(.bottom, 10)
        .panel(stroke: store.underPressure ? Color(hex: 0x5A2A2D) : Ink.line)
    }

    private func legend(_ dot: LampDot, _ name: String, _ mb: Double) -> some View {
        HStack(spacing: 6) {
            dot
            Text("\(name) \(Fmt.gb(mb))").font(Face.body(12)).foregroundStyle(Ink.dim).lineLimit(1)
        }
    }

    // MARK: gauges

    private func gauges(_ s: Status, _ r: Reading) -> some View {
        let grant = max(0, s.headroom_mb)
        let swap = Int(s.swap_rate_mb_min ?? 0)
        return HStack(spacing: 8) {
            Readout(label: "Grant", value: Fmt.gb(grant), unit: "GB",
                    color: grant < 512 ? Lamp.nobody.color : Lamp.inUse.color)
                .help("Memory a new lease can get now: available, minus promises, minus \(String(format: "%.1f", s.config.headroom_gb)) GB kept free.")
            VStack(alignment: .leading, spacing: 6) {
                Silk("Pressure")
                PressureLamps(level: s.mem.pressure_level ?? 1).padding(.top, 4)
                Text(["", "Normal", "Warning", "", "Critical"][min(s.mem.pressure_level ?? 1, 4)])
                    .font(Face.body(12)).foregroundStyle((s.mem.pressure_level ?? 1) >= 4 ? Lamp.nobody.textColor : Ink.dim)
            }
            .frame(maxWidth: .infinity, alignment: .leading).padding(10).panel()
            Readout(label: "Swap", value: "\(swap)", unit: "MB/m", color: swap > 300 ? Lamp.nobody.color : Ink.text)
        }
        .fixedSize(horizontal: false, vertical: true)
    }

    // MARK: rows

    private func items(_ s: Status, _ r: Reading) -> some View {
        ScrollView {
            VStack(spacing: 0) {
                ForEach(Array(r.items.enumerated()), id: \.element.id) { i, item in
                    ItemRow(item: item, dryRun: s.config.dry_run, first: i == 0, onKeep: { keeping = $0 })
                }
            }
            .frame(width: 352)
            .background(GeometryReader { g in Color.clear.preference(key: ListHeight.self, value: g.size.height) })
        }
        .scrollIndicators(.never) // a legacy scroller would take 17 pt and clip the keys
        .scrollBounceBehavior(.basedOnSize)
        .frame(height: min(listHeight, maxListHeight))
        .onPreferenceChange(ListHeight.self) { listHeight = $0 }
        .clipShape(RoundedRectangle(cornerRadius: 10, style: .continuous))
        .panel()
    }

    private func stoppedPanel(_ stopped: [StoppedItem]) -> some View {
        let today = stopped.filter { Calendar.current.isDateInToday($0.stopped_at) }
        let shown = (today.isEmpty ? stopped : today).suffix(3).reversed()
        let freed = today.reduce(0) { $0 + $1.resource.mem_mb }
        return VStack(spacing: 0) {
            HStack {
                Silk(today.isEmpty ? "Stopped earlier" : "Stopped today")
                Spacer()
                if freed > 0 { Silk("Freed \(Fmt.gb(freed)) GB") }
            }
            .padding(.horizontal, 12).padding(.top, 9).padding(.bottom, 2)
            ForEach(Array(shown)) { item in
                HStack(spacing: 10) {
                    LampDot(color: Color(hex: 0x3A3F46), glow: false)
                    VStack(alignment: .leading, spacing: 1) {
                        Text(item.resource.name).font(Face.body(14, .semibold)).lineLimit(1)
                        Text("\(Fmt.rel(item.stopped_at)), \(item.why)").font(Face.body(12)).foregroundStyle(Ink.dim).lineLimit(1)
                    }
                    Spacer()
                    Button("Restore") { store.restore(item.resource.id) }.buttonStyle(.key)
                }
                .padding(.horizontal, 12).padding(.vertical, 9)
            }
        }
        .panel()
    }

    private var readyPanel: some View {
        let r = store.readiness
        return HStack(spacing: 10) {
            VStack(alignment: .leading, spacing: 1) {
                Text("Ready for auto-stop").font(Face.body(14, .semibold))
                Text("\(r.days) days of dry run, \(r.reviewed - r.wrong) of \(r.reviewed) reviewed decisions right")
                    .font(Face.body(12)).foregroundStyle(Color(hex: 0xA7C9B2))
            }
            Spacer()
            Button("Turn on") { gate = true }.buttonStyle(.key(.go))
        }
        .padding(.horizontal, 12).padding(.vertical, 10)
        .panel(Ink.greenPanel, stroke: Color(hex: 0x2E5A3D))
    }

    // MARK: strips, offline, footer

    private func strip(color: Color, text: String) -> some View {
        HStack(spacing: 8) {
            LampDot(color: color)
            Text(text).font(Face.body(12)).foregroundStyle(Ink.dim).fixedSize(horizontal: false, vertical: true)
            Spacer(minLength: 0)
        }
        .padding(9).panel()
    }

    private func errorStrip(_ err: String) -> some View {
        HStack(alignment: .top, spacing: 8) {
            LampDot(Lamp.nobody).padding(.top, 4)
            Text(err).font(Face.body(12)).foregroundStyle(Lamp.nobody.textColor).fixedSize(horizontal: false, vertical: true)
            Spacer(minLength: 0)
            Button { store.lastActionError = nil } label: { Image(systemName: "xmark").font(.system(size: 10, weight: .bold)) }
                .buttonStyle(.plain).foregroundStyle(Ink.label).accessibilityLabel("Dismiss")
        }
        .padding(9).panel(stroke: Color(hex: 0x5A2A2D))
    }

    private var offline: some View {
        VStack(spacing: 12) {
            LEDMeter(segments: Array(repeating: .off, count: 16), height: 40, showScale: false)
            Text("The background daemon is not running.").font(Face.body(15, .semibold))
            if let e = store.registrationError ?? store.daemonError {
                Text(e).font(Face.mono(11)).foregroundStyle(Ink.label).multilineTextAlignment(.center).lineLimit(4)
            }
            if store.loginItemStatus == .requiresApproval {
                Text("macOS needs your approval in System Settings, General, Login Items.")
                    .font(Face.body(12)).foregroundStyle(Ink.dim).multilineTextAlignment(.center)
                Button("Open Login Items") { SMAppServiceOpen.loginItems() }.buttonStyle(.key(.go, large: true))
            } else {
                Button("Start daemon") { store.registerDaemon() }.buttonStyle(.key(.go, large: true))
            }
        }
        .frame(maxWidth: .infinity)
        .padding(16).panel()
    }

    private var footer: some View {
        HStack(spacing: 10) {
            Toggle(isOn: Binding(
                get: { store.status?.config.dry_run == false },
                set: { on in if on { gate = true } else { store.setAutoStop(false) } })) {
                Silk("Auto-stop", color: Ink.dim)
            }
            .toggleStyle(.switch).controlSize(.mini).tint(Lamp.inUse.color)
            .disabled(store.status == nil)
            Spacer()
            if let open = store.status?.fail_open {
                Silk("Open: \(open)", color: Lamp.waiting.color).lineLimit(1).help(open)
            } else if let w = store.status?.waiters, !w.isEmpty {
                Silk("\(w.count) queued, \(Fmt.gb(w.reduce(0) { $0 + $1.need_mb })) GB")
            }
            Button("Open") { NSApp.activate(ignoringOtherApps: true); openWindow(id: "activity") }
                .buttonStyle(.key).keyboardShortcut(",")
            Menu {
                if store.status?.fail_open != nil {
                    Button("Manage memory again") { store.bypass(minutes: 0) }
                } else {
                    Button("Let everything through for 1 hour") { store.bypass(minutes: 60) }
                }
                Divider()
                Button("Check for Updates…") { updater.checkForUpdates() }
                Divider()
                Button("Quit AgentRM") { NSApp.terminate(nil) }.keyboardShortcut("q")
            } label: {
                Image(systemName: "power").font(.system(size: 12, weight: .semibold)).foregroundStyle(Ink.dim)
            }
            .menuStyle(.borderlessButton).menuIndicator(.hidden).fixedSize()
            .accessibilityLabel("Quit or check for updates")
        }
    }
}

private struct ListHeight: PreferenceKey {
    static var defaultValue: CGFloat = 0
    static func reduce(value: inout CGFloat, nextValue: () -> CGFloat) { value = max(value, nextValue()) }
}

extension Lamp {
    /// Legend order on the meter: the order the segments are lit in.
    var legendOrder: Int { [.inUse: 0, .waiting: 1, .nobody: 2, .paused: 3, .system: 4][self] ?? 9 }
}

/// One channel on the panel: lamp, name, what it is doing, its size and its keys.
struct ItemRow: View {
    let item: Item
    let dryRun: Bool
    let first: Bool
    let onKeep: (Resource) -> Void
    @EnvironmentObject var store: Store

    var body: some View {
        HStack(spacing: 10) {
            LampDot(item.lamp)
            VStack(alignment: .leading, spacing: 3) {
                Text(item.name).font(Face.body(14, .semibold)).lineLimit(1)
                if let bar = countdown { bar }
                Text(item.detail).font(Face.body(12)).foregroundStyle(item.lamp.textColor).lineLimit(2)
                    .fixedSize(horizontal: false, vertical: true)
            }
            Spacer(minLength: 4)
            Text(Fmt.gb(item.mb)).font(Face.mono(13)).foregroundStyle(Ink.text)
            ForEach(Array(item.actions.enumerated()), id: \.offset) { _, a in key(a) }
        }
        .padding(.horizontal, 12).padding(.vertical, 9)
        .overlay(alignment: .top) { if !first { Rectangle().fill(Ink.hair).frame(height: 1) } }
        .contentShape(Rectangle())
        .contextMenu { if let r = item.resource { more(r) } }
        .help(item.resource.map { "\(Kind.name($0.kind)): \($0.detail)" } ?? "")
        .accessibilityElement(children: .contain)
        .accessibilityLabel("\(item.name), \(item.lamp.name), \(Fmt.gb(item.mb)) GB")
    }

    /// Ten ice-blue cells filling up towards the pause or the stop, when one is really coming.
    private var countdown: AnyView? {
        guard let d = item.deadline, item.window > 0, item.lamp == .paused || !dryRun else { return nil }
        let left = max(0, d.timeIntervalSinceNow)
        let filled = Int(((1 - left / item.window) * 10).rounded()).clamped(0, 10)
        return AnyView(HStack(spacing: 2) {
            ForEach(0..<10) { i in
                Rectangle().fill(i < filled ? Lamp.paused.color : Ink.off).frame(height: 4)
            }
        }
        .accessibilityLabel(item.lamp == .paused ? "Stops in \(Fmt.clock(d))" : "Pauses in \(Fmt.clock(d))"))
    }

    @ViewBuilder
    private func key(_ a: Item.Action) -> some View {
        switch a {
        case .keep: if let r = item.resource { Button("Keep") { onKeep(r) }.buttonStyle(.key) }
        case .stop: Button("Stop") { store.stop(item.id) }.buttonStyle(.key(.danger))
        case .release(let id): Button("Release") { store.act(["release", id]) }.buttonStyle(.key)
        case .resume: Button("Resume") { store.resume(item.id) }.buttonStyle(.key)
        case .unpin: if let r = item.resource { Button("Unpin") { store.pin(r) }.buttonStyle(.key) }
        }
    }

    @ViewBuilder
    private func more(_ r: Resource) -> some View {
        if r.paused_at != nil { Button("Resume") { store.resume(r.id) } }
        if r.lease_id == nil { Button("Keep…") { onKeep(r) } }
        Button(r.pinned == true ? "Unpin" : "Pin: never pause or stop") { store.pin(r) }
        Divider()
        Button("Stop now", role: .destructive) { store.stop(r.id) }
    }
}

extension Comparable {
    func clamped(_ lo: Self, _ hi: Self) -> Self { min(max(self, lo), hi) }
}

/// KEEP: claim it for a while with a reason, or pin it for good.
struct KeepSheet: View {
    let resource: Resource
    let done: () -> Void
    @EnvironmentObject var store: Store
    @State private var purpose = ""
    @State private var hours = 4

    var body: some View {
        VStack(alignment: .leading, spacing: 12) {
            HStack(spacing: 10) {
                LampDot(Lamp.nobody)
                Text("Keep \(resource.name)").font(Face.body(18, .semibold))
                Spacer()
                Text("\(Fmt.gb(resource.mem_mb)) GB").font(Face.mono(13)).foregroundStyle(Ink.dim)
            }
            Picker("For", selection: $hours) {
                Text("1 h").tag(1); Text("4 h").tag(4); Text("8 h").tag(8); Text("Always").tag(0)
            }
            .pickerStyle(.segmented).labelsHidden()
            if hours > 0 {
                TextField("What do you need it for?", text: $purpose).textFieldStyle(.roundedBorder)
                Text("While it's used, the claim stays alive. Unused, you're asked again before anything happens.")
                    .font(Face.body(12)).foregroundStyle(Ink.dim).fixedSize(horizontal: false, vertical: true)
            } else {
                Text("Pinned: AgentRM never pauses or stops it. Unpin it from the menu.")
                    .font(Face.body(12)).foregroundStyle(Ink.dim)
            }
            HStack {
                Spacer()
                Button("Cancel", action: done).buttonStyle(.key).keyboardShortcut(.cancelAction)
                Button(hours == 0 ? "Pin" : "Keep") {
                    if hours == 0 { store.pin(resource) } else { store.claim(resource.id, purpose: purpose, hours: hours) }
                    done()
                }
                .buttonStyle(.key(.go))
                .keyboardShortcut(.defaultAction)
                .disabled(hours > 0 && purpose.trimmingCharacters(in: .whitespaces).isEmpty)
            }
        }
        .padding(18).frame(width: 360)
        .background(Ink.bg).foregroundStyle(Ink.text).environment(\.colorScheme, .dark)
    }
}

/// Before auto-stop is turned on: what dry run showed, and whether that is enough.
struct AutoStopGate: View {
    @Binding var isPresented: Bool
    @EnvironmentObject var store: Store

    var body: some View {
        let r = store.readiness
        VStack(alignment: .leading, spacing: 12) {
            LampLabel(lamp: r.ready ? Lamp.inUse.color : Lamp.waiting.color, text: r.ready ? "Dry run looks good" : "Not enough evidence yet")
            Text(r.summary).font(Face.body(15)).fixedSize(horizontal: false, vertical: true)
            if !r.ready {
                Text("We recommend 7 days of dry run and 5 reviewed decisions with under 2% wrong before AgentRM pauses and stops things itself. Review them under Decisions in the Activity window.")
                    .font(Face.body(13)).foregroundStyle(Ink.dim).fixedSize(horizontal: false, vertical: true)
            }
            TrustLevelsView()
            Text("Or all at once: with auto-stop on, unclaimed things are paused after \(store.status?.config.grace_minutes ?? 10) min, nothing lost, and stopped \(store.status?.config.pause_minutes ?? 30) min later. Everything stopped can be restored.")
                .font(Face.body(13)).foregroundStyle(Ink.dim).fixedSize(horizontal: false, vertical: true)
            HStack {
                Spacer()
                Button("Keep dry run") { isPresented = false }.buttonStyle(.key).keyboardShortcut(.cancelAction)
                Button(r.ready ? "Turn on all" : "Turn on all anyway") {
                    store.setAutoStop(true)
                    isPresented = false
                }
                .buttonStyle(.key(r.ready ? .go : .danger))
                .keyboardShortcut(r.ready ? .defaultAction : .init("t"))
            }
        }
        .padding(18).frame(width: 560)
        .background(Ink.bg).foregroundStyle(Ink.text).environment(\.colorScheme, .dark)
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
