import ServiceManagement
import Sparkle
import SwiftUI

struct SettingsView: View {
    let updater: SPUUpdater
    @EnvironmentObject var store: Store

    var body: some View {
        TabView {
            SetupTab().tabItem { Label("Setup", systemImage: "checklist") }
            LimitsTab().tabItem { Label("Limits", systemImage: "gauge.with.dots.needle.33percent") }
            HistoryTab().tabItem { Label("History", systemImage: "clock.arrow.circlepath") }
            AboutTab(updater: updater).tabItem { Label("About", systemImage: "info.circle") }
        }
        .frame(width: 600, height: 480)
        .environmentObject(store)
        .task { await store.refreshSetup() }
    }
}

private struct Row<Trailing: View>: View {
    let ok: Bool
    let title: String
    let detail: String
    @ViewBuilder let trailing: Trailing

    var body: some View {
        HStack(alignment: .top, spacing: 10) {
            Image(systemName: ok ? "checkmark.circle.fill" : "circle")
                .foregroundStyle(ok ? .green : .secondary).font(.title3)
                .accessibilityLabel(ok ? "On" : "Off")
            VStack(alignment: .leading, spacing: 2) {
                Text(title).font(.body.bold())
                Text(detail).font(.caption).foregroundStyle(.secondary).fixedSize(horizontal: false, vertical: true)
            }
            Spacer()
            trailing
        }
        .accessibilityElement(children: .contain)
    }
}

struct SetupTab: View {
    @EnvironmentObject var store: Store
    @State private var confirmUninstall = false

    var body: some View {
        let s = store.setup
        Form {
            if let problems = s?.problems, !problems.isEmpty {
                Section {
                    ForEach(problems, id: \.self) { p in
                        Label(p, systemImage: "exclamationmark.triangle.fill").foregroundStyle(.orange).font(.callout)
                    }
                    Button("Repair") { store.act(["setup", "repair"]) }
                }
            }
            Row(ok: store.loginItemStatus == .enabled, title: "Background daemon", detail: daemonDetail) {
                if store.loginItemStatus == .enabled {
                    Button("Turn off") { store.unregisterDaemon() }
                } else if store.loginItemStatus == .requiresApproval {
                    Button("Open Login Items") { SMAppServiceOpen.loginItems() }
                } else {
                    Button("Turn on") { store.registerDaemon() }
                }
            }
            Row(ok: s?.claude_hooks_current == true, title: "Claude Code hooks",
                detail: "Heavy commands ask for a lease first; sessions get questions about what they leave running. Four hooks in ~/.claude/settings.json; a backup is kept and your other settings are untouched.") {
                Button(s?.claude_hooks == true ? "Remove" : "Install") { store.setup("claude", install: s?.claude_hooks != true) }
            }
            Row(ok: s?.shim == true, title: "Other agents (Codex, Gemini CLI, Aider, OpenCode, Cursor)",
                detail: "A PATH shim in front of docker, cargo, kind and similar: for agents without hooks it asks AgentRM before a heavy command; for you and Claude it passes straight through. Adds a marked block to ~/.zshenv (and ~/.bash_profile if you have one).") {
                Button(s?.shim == true ? "Remove" : "Install") { store.setup("shim", install: s?.shim != true) }
            }
            Row(ok: s?.codex_note == true, title: "Codex instructions",
                detail: "A marked section in ~/.codex/AGENTS.md asking Codex to take leases for work the shim cannot recognise, and to answer AgentRM's questions.") {
                Button(s?.codex_note == true ? "Remove" : "Install") { store.setup("codex", install: s?.codex_note != true) }
            }
            Row(ok: !(s?.cli_link ?? "").isEmpty, title: "agentrm command line", detail: cliDetail(s)) {
                Button((s?.cli_link ?? "").isEmpty ? "Install" : "Remove") { store.setup("path", install: (s?.cli_link ?? "").isEmpty) }
            }
            Section {
                Button("Uninstall AgentRM…", role: .destructive) { confirmUninstall = true }
                    .help("Removes everything above, resumes anything paused, turns off the daemon and quits.")
            }
        }
        .formStyle(.grouped)
        .confirmationDialog("Uninstall AgentRM?", isPresented: $confirmUninstall) {
            Button("Uninstall", role: .destructive) { store.uninstall() }
        } message: {
            Text("This removes the hooks, the shim, the Codex note and the command-line link, resumes anything paused, and turns off the daemon. Your data in ~/.agentrm stays.")
        }
    }

    private var daemonDetail: String {
        switch store.loginItemStatus {
        case .enabled: return "Runs at login and keeps running in the background."
        case .requiresApproval: return "Waiting for your approval in System Settings → General → Login Items."
        default: return "Not running. Leases, questions and auto-stop need it."
        }
    }

    private func cliDetail(_ s: SetupCheck?) -> String {
        var d = "Links ~/.local/bin/agentrm to the copy inside this app, for agents and your terminal."
        if let s, !s.cli_link.isEmpty, !s.cli_link_dir_on_path {
            d += " ~/.local/bin is not on your PATH yet; add it to your shell profile."
        }
        return d
    }
}

struct LimitsTab: View {
    @EnvironmentObject var store: Store
    @State private var headroom = 1.0
    @State private var grace = 10
    @State private var idle = 30
    @State private var pause = 30
    @State private var loaded = false

    var body: some View {
        Form {
            Stepper(value: $headroom, in: 0.5...8, step: 0.5) {
                LabeledContent("Keep free after any grant", value: String(format: "%.1f GB", headroom))
            }
            Stepper(value: $grace, in: 1...240, step: 5) {
                LabeledContent("Pause unclaimed things after", value: "\(grace) min")
            }
            Stepper(value: $pause, in: 0...480, step: 10) {
                LabeledContent("Then stop them after", value: pause == 0 ? "at once (no pause)" : "\(pause) min paused")
            }
            Stepper(value: $idle, in: 5...240, step: 5) {
                LabeledContent("Ask a holder after", value: "\(idle) min unused")
            }
            HStack {
                Spacer()
                Button("Apply") { store.setLimits(headroom: headroom, grace: grace, idle: idle, pause: pause) }
                    .keyboardShortcut(.defaultAction)
            }
            Text("Other settings (never-stop list, extra command patterns, dev-server names) are in ~/.agentrm/config.json; AgentRM picks up changes there within a minute and tells you it did.")
                .font(.caption).foregroundStyle(.secondary)
        }
        .formStyle(.grouped)
        .onAppear { load() }
        .onChange(of: store.status?.config.headroom_gb) { _, _ in load() }
    }

    private func load() {
        guard !loaded, let c = store.status?.config else { return }
        (headroom, grace, idle, pause) = (c.headroom_gb, c.grace_minutes, c.idle_minutes, c.pause_minutes ?? 30)
        loaded = true
    }
}

/// What AgentRM did and would have done: events, decisions to review, and what can be restored.
struct HistoryTab: View {
    @EnvironmentObject var store: Store
    @State private var section = 0

    var body: some View {
        VStack(alignment: .leading, spacing: 8) {
            Picker("", selection: $section) {
                Text("Decisions").tag(0)
                Text("Stopped").tag(1)
                Text("Events").tag(2)
            }
            .pickerStyle(.segmented).labelsHidden()
            switch section {
            case 0: decisions
            case 1: stopped
            default: events
            }
        }
        .padding()
        .task { await store.refreshReview() }
    }

    private var decisions: some View {
        VStack(alignment: .leading, spacing: 6) {
            if let r = store.review {
                Text(r.reviewed == 0 ? "Mark each decision right or wrong; this is how you know when auto-stop is safe."
                                     : "\(r.wrong) of \(r.reviewed) reviewed decisions were wrong (\(String(format: "%.1f", 100 * r.false_rate))%).")
                    .font(.caption).foregroundStyle(.secondary)
            }
            List(store.review?.decisions ?? []) { d in
                HStack(alignment: .top) {
                    VStack(alignment: .leading, spacing: 2) {
                        Text("\(d.dry_run ? "Would \(d.action)" : d.action.capitalized) \(d.name.isEmpty ? d.resource : d.name) · \(d.mem_mb.gb)")
                            .font(.callout)
                        Text("\(d.time, style: .date) \(d.time, style: .time) · \(d.why)").font(.caption).foregroundStyle(.secondary)
                    }
                    Spacer()
                    if let v = d.verdict {
                        Text(v).font(.caption.bold()).foregroundStyle(v == "right" ? .green : .red)
                    } else {
                        Button("Right") { store.mark(d, right: true) }.controlSize(.small)
                        Button("Wrong") { store.mark(d, right: false) }.controlSize(.small)
                    }
                }
            }
        }
    }

    private var stopped: some View {
        List((store.status?.stopped ?? []).reversed()) { item in
            HStack {
                VStack(alignment: .leading, spacing: 2) {
                    Text(item.resource.name).font(.callout)
                    Text("\(item.stopped_at, style: .date) \(item.stopped_at, style: .time) · \(item.why)").font(.caption).foregroundStyle(.secondary)
                }
                Spacer()
                Button("Restore") { store.restore(item.resource.id) }.controlSize(.small)
            }
        }
    }

    private var events: some View {
        List((store.status?.events ?? []).reversed()) { e in
            VStack(alignment: .leading, spacing: 1) {
                Text("\(e.time, style: .time) · \(e.kind)").font(.caption.bold())
                Text(e.text).font(.caption).foregroundStyle(.secondary).textSelection(.enabled)
            }
        }
    }
}

struct AboutTab: View {
    let updater: SPUUpdater
    @EnvironmentObject var store: Store

    var body: some View {
        VStack(spacing: 8) {
            Image(systemName: "memorychip").font(.system(size: 40)).accessibilityHidden(true)
            Text("AgentRM").font(.title2.bold())
            Text("Version \(Bundle.main.object(forInfoDictionaryKey: "CFBundleShortVersionString") as? String ?? "")")
                .foregroundStyle(.secondary)
            if let s = store.status, let u = s.selfUsage {
                Text("Daemon \(s.daemon_version ?? "") · \(Int(u.rss_mb)) MB · \(String(format: "%.1f", u.cpu_percent))% CPU · sweep \(Int(u.sweep_ms)) ms")
                    .font(.caption).foregroundStyle(.secondary)
            }
            Text("A resource manager for coding agents: leases for heavy work, questions for what is left running, and a reversible pause and stop for what nobody claims.")
                .multilineTextAlignment(.center).font(.callout).padding(.horizontal, 30)
            HStack {
                Button("Check for Updates…") { updater.checkForUpdates() }
                Link("Source code", destination: URL(string: "https://github.com/Ash20pk/agentrm")!)
            }
        }
        .padding()
    }
}
