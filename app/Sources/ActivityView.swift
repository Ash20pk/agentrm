import Sparkle
import SwiftUI

/// The Activity window: the rack unit behind the menu. Now (the waterfall and everything running),
/// the decisions to review, what was stopped, setup and limits.
struct ActivityView: View {
    enum Tab: String, CaseIterable {
        case now = "Now", collected = "Collected", week = "Week", decisions = "Decisions", stopped = "Stopped", vault = "Vault", setup = "Setup", limits = "Limits"
    }

    let updater: SPUUpdater
    @EnvironmentObject var store: Store
    @State var tab: Tab = .now

    var body: some View {
        VStack(spacing: 0) {
            HStack(spacing: 18) {
                Text("AGENTRM").font(Face.mono(13, .bold)).tracking(2)
                HStack(spacing: 4) {
                    ForEach(Tab.allCases, id: \.self) { t in
                        Button { tab = t } label: {
                            Text(t.rawValue.uppercased()).font(Face.mono(11, .semibold)).tracking(1)
                                .foregroundStyle(tab == t ? Ink.bg : Ink.label)
                                .padding(.horizontal, 10).padding(.vertical, 6)
                                .background(tab == t ? Ink.text : .clear, in: RoundedRectangle(cornerRadius: 5))
                                .contentShape(Rectangle())
                        }
                        .buttonStyle(.plain)
                        .accessibilityAddTraits(tab == t ? .isSelected : [])
                    }
                }
                Spacer()
                if let s = store.status {
                    if s.config.dry_run {
                        let days = store.readiness.days
                        LampLabel(lamp: Lamp.waiting.color, text: days < 7 ? "Dry run, day \(days + 1) of 7" : "Dry run, \(days) days")
                    } else {
                        LampLabel(lamp: Lamp.inUse.color, text: "Auto-stop on")
                    }
                }
            }
            .padding(.horizontal, 22).padding(.vertical, 12)
            Rectangle().fill(Ink.hair).frame(height: 1)
            Group {
                switch tab {
                case .now: NowTab()
                case .collected: CollectedTab()
                case .week: WeekTab()
                case .decisions: DecisionsTab()
                case .stopped: StoppedTab()
                case .vault: VaultTab()
                case .setup: SetupTab(updater: updater)
                case .limits: LimitsTab()
                }
            }
            .frame(maxWidth: .infinity, maxHeight: .infinity, alignment: .top)
        }
        .frame(minWidth: 960, minHeight: 660)
        .background(Ink.bg)
        .foregroundStyle(Ink.text)
        .font(Face.body(14))
        .environment(\.colorScheme, .dark)
        .task {
            await store.refreshSetup()
            await store.refreshReview()
        }
    }
}

// MARK: - Now

struct NowTab: View {
    @EnvironmentObject var store: Store
    @State private var keeping: Resource?

    var body: some View {
        ScrollView {
            VStack(alignment: .leading, spacing: 14) {
                if let s = store.status, let r = store.reading {
                    HStack(alignment: .top, spacing: 14) {
                        waterfall
                        LazyVGrid(columns: [GridItem(.flexible()), GridItem(.flexible())], spacing: 10) {
                            Readout(label: "Grant", value: Fmt.gb(max(0, s.headroom_mb)), unit: "GB",
                                    color: s.headroom_mb < 512 ? Lamp.nobody.color : Lamp.inUse.color)
                            Readout(label: "Nobody", value: Fmt.gb(r.nobodyMB), unit: "GB",
                                    color: r.nobodyMB > 0 ? Lamp.nobody.color : Ink.text)
                            Readout(label: "Swap", value: "\(Int(s.swap_rate_mb_min ?? 0))", unit: "MB/m",
                                    color: (s.swap_rate_mb_min ?? 0) > s.swapLimit ? Lamp.nobody.color : Ink.text)
                            Readout(label: "Freed", value: Fmt.gb(freedThisWeek(s)), unit: "GB")
                        }
                        .frame(width: 280)
                    }
                    VStack(spacing: 0) {
                        if r.items.isEmpty {
                            Text("Nothing heavy is running.").font(Face.body(13)).foregroundStyle(Ink.dim)
                                .frame(maxWidth: .infinity).padding(20)
                        }
                        ForEach(Array(r.items.enumerated()), id: \.element.id) { i, item in
                            ItemRow(item: item, dryRun: s.config.dry_run, first: i == 0, onKeep: { keeping = $0 })
                        }
                    }
                    .panel()
                } else {
                    Text("The background daemon is not running. Start it from Setup.")
                        .font(Face.body(14)).foregroundStyle(Ink.dim).padding(20)
                }
            }
            .padding(22)
        }
        .sheet(item: $keeping) { r in KeepSheet(resource: r) { keeping = nil }.environmentObject(store) }
    }

    /// The daemon's total (every stop of the week, at what each gave back); the list of stops
    /// it sends is only the last 50.
    private func freedThisWeek(_ s: Status) -> Double {
        if let w = s.freed_week_mb { return w }
        let weekAgo = Date().addingTimeInterval(-7 * 86400)
        return (s.stopped ?? []).filter { $0.stopped_at > weekAgo }.reduce(0) { $0 + $1.freed }
    }

    private var waterfall: some View {
        let cols = store.waterfall
        let n = cols.last?.count ?? 16
        let padded: [[Segment]] = Array(repeating: Array(repeating: Segment.off, count: n), count: max(0, 60 - cols.count)) + cols
        return VStack(alignment: .leading, spacing: 10) {
            HStack(spacing: 12) {
                Silk("Memory, last 60 min, one column a minute")
                Spacer()
                ForEach(Lamp.allCases.sorted { $0.legendOrder < $1.legendOrder }, id: \.self) { l in
                    HStack(spacing: 5) { LampDot(l, size: 7); Text(l.name).font(Face.body(12)).foregroundStyle(Ink.dim) }
                }
            }
            HStack(alignment: .bottom, spacing: 2) {
                ForEach(Array(padded.enumerated()), id: \.offset) { i, col in
                    VStack(spacing: 2) {
                        ForEach(Array(col.reversed().enumerated()), id: \.offset) { _, seg in
                            RoundedRectangle(cornerRadius: 1).fill(color(seg)).opacity(i < 60 - cols.count ? 0.5 : 1)
                        }
                    }
                }
            }
            .frame(height: 160)
            .accessibilityElement()
            .accessibilityLabel("Memory over the last \(cols.count) minutes")
            HStack {
                // Columns are the minutes the app has recorded, not wall time (none while asleep).
                Text(cols.count < 60 ? "\(cols.count) MIN RECORDED" : "-60 MIN").font(Face.mono(9)).foregroundStyle(Ink.scale)
                Spacer()
                Text("NOW").font(Face.mono(9)).foregroundStyle(Ink.scale)
            }
        }
        .padding(14).panel()
    }

    private func color(_ s: Segment) -> Color {
        switch s {
        case .lit(let l): return l == .system ? Color(hex: 0x5B616B) : l.color
        case .kept: return Color(hex: 0x30343A)
        case .off: return Ink.off
        }
    }
}

// MARK: - Collected

/// What exited agents left behind, session by session, and what the collector did with it.
struct CollectedTab: View {
    @EnvironmentObject var store: Store
    @State private var keeping: Resource?

    var body: some View {
        let s = store.status
        let ended = (s?.sessions ?? [:]).values.filter { $0.ended_at != nil }.sorted { $0.ended_at! > $1.ended_at! }
        // Pinned things are yours to keep, not left running: the same rule as the welcome screen.
        let waiting = (s?.resources ?? [:]).values.filter { $0.owner_gone_at != nil && $0.lease_id == nil && $0.pinned != true }
        let collected = (s?.stopped ?? []).filter { $0.resource.owner != nil && $0.stopped_at > Date().addingTimeInterval(-86400) }
        ScrollView {
            VStack(alignment: .leading, spacing: 14) {
                HStack(spacing: 10) {
                    Readout(label: "Sessions ended", value: "\(ended.count)", unit: "24 h")
                    Readout(label: "Left running", value: Fmt.gb(waiting.reduce(0) { $0 + $1.mem_mb }), unit: "GB",
                            color: waiting.isEmpty ? Ink.text : Lamp.nobody.color)
                    Readout(label: "Collected", value: Fmt.gb(collected.reduce(0) { $0 + $1.freed }), unit: "GB 24 h",
                            color: Lamp.inUse.color)
                }
                Text("When an agent exits, or its process dies without saying so, its leases are released and everything it started or held is collected: asked about, paused within minutes (nothing lost), then stopped. Processes it left running are found even when AgentRM knows nothing else about them. Anything used after the agent left is treated as someone else's and waits the normal time.")
                    .font(Face.body(13)).foregroundStyle(Ink.dim).fixedSize(horizontal: false, vertical: true)
                if ended.isEmpty {
                    Text("No agent has ended in the last 24 hours.").font(Face.body(13)).foregroundStyle(Ink.dim)
                        .frame(maxWidth: .infinity).padding(20).panel()
                }
                ForEach(ended) { sess in session(sess, s) }
            }
            .padding(22)
        }
        .sheet(item: $keeping) { r in KeepSheet(resource: r) { keeping = nil }.environmentObject(store) }
    }

    private func session(_ sess: AgentSession, _ s: Status?) -> some View {
        let items = (store.reading?.items ?? []).filter { $0.resource?.owner == sess.id }
        let stopped = (s?.stopped ?? []).filter { $0.resource.owner == sess.id }.reversed()
        return VStack(spacing: 0) {
            HStack(spacing: 10) {
                LampDot(color: Ink.scale, glow: false)
                VStack(alignment: .leading, spacing: 2) {
                    Text("\(store.agentTitle(sess.agent)) in \((sess.cwd as NSString).lastPathComponent.isEmpty ? sess.id.prefix(8).description : (sess.cwd as NSString).lastPathComponent)")
                        .font(Face.body(15, .semibold))
                    Text("Ended \(Fmt.rel(sess.ended_at ?? Date())): \(sess.end_reason ?? "")")
                        .font(Face.body(12)).foregroundStyle(Ink.dim)
                }
                Spacer()
                Silk(items.isEmpty && stopped.isEmpty ? "Left nothing" : "\(items.count) running, \(stopped.count) stopped")
            }
            .padding(.horizontal, 12).padding(.vertical, 10)
            ForEach(items) { item in
                ItemRow(item: item, dryRun: s?.config.dry_run ?? true, first: false, onKeep: { keeping = $0 })
            }
            ForEach(Array(stopped)) { st in
                HStack(spacing: 10) {
                    LampDot(color: Color(hex: 0x3A3F46), glow: false)
                    VStack(alignment: .leading, spacing: 2) {
                        Text(st.resource.name).font(Face.body(14, .semibold))
                        Text("Stopped \(Fmt.rel(st.stopped_at)), \(st.why)").font(Face.body(12)).foregroundStyle(Ink.dim).lineLimit(2)
                    }
                    Spacer()
                    Text(Fmt.size(st.resource.mem_mb)).font(Face.mono(13))
                    Button("Restore") { store.restore(st.resource.id) }.buttonStyle(.key)
                }
                .padding(.horizontal, 12).padding(.vertical, 9)
                .overlay(alignment: .top) { Rectangle().fill(Ink.hair).frame(height: 1) }
            }
        }
        .panel()
    }
}

// MARK: - Decisions

struct DecisionsTab: View {
    @EnvironmentObject var store: Store

    var body: some View {
        ScrollView {
            VStack(alignment: .leading, spacing: 12) {
                if let r = store.review {
                    Text(r.reviewed == 0 ? "Mark each decision right or wrong. This is how you know when auto-stop is safe."
                                         : "\(r.wrong) of \(r.reviewed) reviewed decisions were wrong (\(String(format: "%.1f", 100 * r.false_rate))%).")
                        .font(Face.body(13)).foregroundStyle(Ink.dim)
                }
                let ds = store.review?.decisions ?? []
                VStack(spacing: 0) {
                    if ds.isEmpty {
                        Text("No decisions yet. When AgentRM would pause or stop something, it shows up here.")
                            .font(Face.body(13)).foregroundStyle(Ink.dim).frame(maxWidth: .infinity).padding(20)
                    }
                    ForEach(Array(ds.enumerated()), id: \.element.id) { i, d in
                        HStack(spacing: 10) {
                            LampDot(d.action == "stop" ? .nobody : .paused)
                            VStack(alignment: .leading, spacing: 2) {
                                Text("\(d.dry_run ? "Would \(d.action)" : d.action.capitalized) \(d.name.isEmpty ? d.resource : d.name)")
                                    .font(Face.body(14, .semibold))
                                Text("\(Fmt.when(d.time)), \(d.why)").font(Face.body(12)).foregroundStyle(Ink.dim).lineLimit(2)
                            }
                            Spacer()
                            Text(Fmt.size(d.mem_mb)).font(Face.mono(13))
                            if let v = d.verdict {
                                LampLabel(lamp: v == "right" ? Lamp.inUse.color : Lamp.nobody.color, text: v)
                            } else {
                                Button("Right") { store.mark(d, right: true) }.buttonStyle(.key)
                                Button("Wrong") { store.mark(d, right: false) }.buttonStyle(.key(.danger))
                            }
                        }
                        .padding(.horizontal, 12).padding(.vertical, 10)
                        .overlay(alignment: .top) { if i > 0 { Rectangle().fill(Ink.hair).frame(height: 1) } }
                    }
                }
                .panel()
            }
            .padding(22)
        }
    }
}

// MARK: - Stopped

struct StoppedTab: View {
    @EnvironmentObject var store: Store

    var body: some View {
        ScrollView {
            let items = Array((store.status?.stopped ?? []).reversed())
            VStack(spacing: 0) {
                if items.isEmpty {
                    Text("Nothing stopped. Anything AgentRM stops is listed here and can be restored.")
                        .font(Face.body(13)).foregroundStyle(Ink.dim).frame(maxWidth: .infinity).padding(20)
                }
                ForEach(Array(items.enumerated()), id: \.element.id) { i, item in
                    HStack(spacing: 10) {
                        LampDot(color: Color(hex: 0x3A3F46), glow: false)
                        VStack(alignment: .leading, spacing: 2) {
                            Text(item.resource.name).font(Face.body(14, .semibold))
                            Text("\(Kind.name(item.resource.kind)), \(Fmt.when(item.stopped_at)), \(item.why)")
                                .font(Face.body(12)).foregroundStyle(Ink.dim).lineLimit(1)
                        }
                        Spacer()
                        Text(Fmt.size(item.resource.mem_mb)).font(Face.mono(13))
                        Button("Restore") { store.restore(item.resource.id) }.buttonStyle(.key)
                    }
                    .padding(.horizontal, 12).padding(.vertical, 10)
                    .overlay(alignment: .top) { if i > 0 { Rectangle().fill(Ink.hair).frame(height: 1) } }
                }
            }
            .panel()
            .padding(22)
        }
    }
}

// MARK: - Setup

private struct SetupRow<Trailing: View>: View {
    let on: Bool
    let title: String
    let detail: String
    @ViewBuilder let trailing: Trailing

    var body: some View {
        HStack(alignment: .top, spacing: 12) {
            LampDot(color: on ? Lamp.inUse.color : Ink.off, glow: on).padding(.top, 5)
            VStack(alignment: .leading, spacing: 3) {
                Text(title).font(Face.body(15, .semibold))
                Text(detail).font(Face.body(12)).foregroundStyle(Ink.dim).fixedSize(horizontal: false, vertical: true)
            }
            Spacer(minLength: 12)
            trailing
        }
        .padding(.horizontal, 14).padding(.vertical, 12)
        .accessibilityElement(children: .contain)
    }
}

struct SetupTab: View {
    let updater: SPUUpdater
    @EnvironmentObject var store: Store
    @State private var confirmUninstall = false

    var body: some View {
        let s = store.setup
        ScrollView {
            VStack(alignment: .leading, spacing: 14) {
                if let problems = s?.problems, !problems.isEmpty {
                    VStack(alignment: .leading, spacing: 8) {
                        ForEach(problems, id: \.self) { p in
                            HStack(alignment: .top, spacing: 8) { LampDot(Lamp.waiting).padding(.top, 4); Text(p).font(Face.body(13)) }
                        }
                        HStack { Spacer(); Button("Repair") { store.act(["setup", "repair"]) }.buttonStyle(.key(.amber)) }
                    }
                    .padding(12).panel(Ink.amberPanel, stroke: Color(hex: 0x8A6A12))
                }
                VStack(spacing: 0) {
                    SetupRow(on: store.daemonRunning, title: "Background daemon", detail: daemonDetail) {
                        if store.daemonRunning {
                            Button("Restart") { store.restartDaemon() }.buttonStyle(.key)
                        } else {
                            Button("Start") { store.startDaemon() }.buttonStyle(.key(.go))
                        }
                    }
                    divider
                    SetupRow(on: s?.claude_hooks_current == true, title: "Claude Code hooks",
                             detail: "Heavy commands ask for memory first; sessions answer questions about what they leave running. Four hooks in ~/.claude/settings.json, with a backup.") {
                        toggleKey(installed: s?.claude_hooks == true, "claude")
                    }
                    divider
                    SetupRow(on: s?.shim == true, title: "Codex, Gemini CLI, Aider, OpenCode, Cursor",
                             detail: "A PATH shim in front of docker, cargo, kind and similar. You and Claude pass straight through. A marked block in ~/.zshenv.") {
                        toggleKey(installed: s?.shim == true, "shim")
                    }
                    divider
                    SetupRow(on: s?.codex_note == true, title: "Codex instructions",
                             detail: "A marked section in ~/.codex/AGENTS.md asking Codex to take leases and answer AgentRM's questions.") {
                        toggleKey(installed: s?.codex_note == true, "codex")
                    }
                    divider
                    SetupRow(on: !(s?.cli_link ?? "").isEmpty, title: "The agentrm command", detail: cliDetail(s)) {
                        toggleKey(installed: !(s?.cli_link ?? "").isEmpty, "path")
                    }
                }
                .panel()
                HStack(spacing: 10) {
                    Text("Version \(Bundle.main.object(forInfoDictionaryKey: "CFBundleShortVersionString") as? String ?? "")")
                        .font(Face.mono(11)).foregroundStyle(Ink.label)
                    if let st = store.status, let u = st.selfUsage {
                        Text("Daemon \(Int(u.rss_mb)) MB, \(String(format: "%.1f", u.cpu_percent))% CPU, sweep \(Int(u.sweep_ms)) ms")
                            .font(Face.mono(11)).foregroundStyle(Ink.label)
                    }
                    Spacer()
                    Button("Check for updates") { updater.checkForUpdates() }.buttonStyle(.key)
                    Link(destination: URL(string: "https://github.com/Ash20pk/agentrm")!) {
                        Text("SOURCE").font(Face.mono(11, .semibold)).tracking(0.5)
                    }
                    Button("Uninstall…") { confirmUninstall = true }.buttonStyle(.key(.danger))
                }
            }
            .padding(22)
        }
        .confirmationDialog("Uninstall AgentRM?", isPresented: $confirmUninstall) {
            Button("Uninstall", role: .destructive) { store.uninstall() }
        } message: {
            Text("This removes the hooks, the shim, the Codex note and the command-line link, resumes anything paused, and turns off the daemon. Your data in ~/.agentrm stays.")
        }
    }

    private var divider: some View { Rectangle().fill(Ink.hair).frame(height: 1) }

    private func toggleKey(installed: Bool, _ what: String) -> some View {
        Button(installed ? "Remove" : "Install") { store.setup(what, install: !installed) }
            .buttonStyle(KeyStyle(tone: installed ? .plain : .go))
    }

    private var daemonDetail: String {
        store.daemonRunning
            ? "Runs while AgentRM is open and stops when you quit it, thawing anything it paused."
            : "Not running. Leases, questions and auto-stop need it."
    }

    private func cliDetail(_ s: SetupCheck?) -> String {
        var d = "Links ~/.local/bin/agentrm to the copy inside this app, for agents and your terminal."
        if let s, !s.cli_link.isEmpty, !s.cli_link_dir_on_path { d += " ~/.local/bin is not on your PATH yet; add it to your shell profile." }
        return d
    }
}

// MARK: - Limits

struct LimitsTab: View {
    @EnvironmentObject var store: Store
    @State private var headroom = 1.0
    @State private var grace = 10
    @State private var idle = 30
    @State private var pause = 30
    @State private var loaded = false

    var body: some View {
        ScrollView {
            VStack(alignment: .leading, spacing: 14) {
                VStack(spacing: 0) {
                    row("Keep free after any grant", "A lease is granted only if this much memory is still free afterwards.",
                        String(format: "%.1f GB", headroom)) { Stepper("", value: $headroom, in: 0.5...8, step: 0.5) }
                    divider
                    row("Pause unclaimed things after", "Agents are asked first. Pausing freezes the process; nothing is lost.",
                        "\(grace) min") { Stepper("", value: $grace, in: 1...240, step: 5) }
                    divider
                    row("Then stop them after", "Everything stopped can be restored.",
                        pause == 0 ? "at once" : "\(pause) min") { Stepper("", value: $pause, in: 0...480, step: 10) }
                    divider
                    row("Ask a holder after", "A lease unused this long gets a question to the agent holding it.",
                        "\(idle) min") { Stepper("", value: $idle, in: 5...240, step: 5) }
                }
                .panel()
                TrustLevelsView()
                HStack(spacing: 8) {
                    LampDot(Lamp.paused)
                    Text("Unclaimed, then asked, then paused after \(grace) min, then \(pause == 0 ? "stopped at once" : "stopped \(pause) min later"), then restorable.")
                        .font(Face.body(13)).foregroundStyle(Ink.dim)
                }
                HStack {
                    Text("Never-stop list, command patterns and dev-server names are in ~/.agentrm/config.json. Changes there are picked up within a minute.")
                        .font(Face.body(12)).foregroundStyle(Ink.label)
                    Spacer()
                    Button("Open file") {
                        NSWorkspace.shared.open(FileManager.default.homeDirectoryForCurrentUser.appendingPathComponent(".agentrm/config.json"))
                    }.buttonStyle(.key)
                    Button("Revert") { loaded = false; load() }.buttonStyle(.key).disabled(!changed)
                    Button("Apply") { store.setLimits(headroom: headroom, grace: grace, idle: idle, pause: pause) }
                        .buttonStyle(.key(.go)).disabled(!changed).keyboardShortcut(.defaultAction)
                }
            }
            .padding(22)
        }
        .onAppear { load() }
        .onChange(of: store.status?.config.headroom_gb) { _, _ in load() }
    }

    private var divider: some View { Rectangle().fill(Ink.hair).frame(height: 1) }

    private var changed: Bool {
        guard let c = store.status?.config else { return false }
        return (c.headroom_gb, c.grace_minutes, c.idle_minutes, c.pause_minutes ?? 30) != (headroom, grace, idle, pause)
    }

    private func row<C: View>(_ title: String, _ help: String, _ value: String, @ViewBuilder control: () -> C) -> some View {
        HStack(spacing: 12) {
            VStack(alignment: .leading, spacing: 3) {
                Text(title).font(Face.body(15, .semibold))
                Text(help).font(Face.body(12)).foregroundStyle(Ink.dim)
            }
            Spacer()
            Text(value).font(Face.mono(15, .semibold)).foregroundStyle(Lamp.inUse.color)
            control().labelsHidden()
        }
        .padding(.horizontal, 14).padding(.vertical, 12)
    }

    private func load() {
        guard !loaded, let c = store.status?.config else { return }
        (headroom, grace, idle, pause) = (c.headroom_gb, c.grace_minutes, c.idle_minutes, c.pause_minutes ?? 30)
        loaded = true
    }
}
