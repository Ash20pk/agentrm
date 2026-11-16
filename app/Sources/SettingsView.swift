import ServiceManagement
import SwiftUI

struct SettingsView: View {
    @EnvironmentObject var store: Store

    var body: some View {
        TabView {
            SetupTab().tabItem { Label("Setup", systemImage: "checklist") }
            LimitsTab().tabItem { Label("Limits", systemImage: "gauge.with.dots.needle.33percent") }
            AboutTab().tabItem { Label("About", systemImage: "info.circle") }
        }
        .frame(width: 520, height: 400)
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
            VStack(alignment: .leading, spacing: 2) {
                Text(title).font(.body.bold())
                Text(detail).font(.caption).foregroundStyle(.secondary).fixedSize(horizontal: false, vertical: true)
            }
            Spacer()
            trailing
        }
    }
}

struct SetupTab: View {
    @EnvironmentObject var store: Store

    var body: some View {
        let s = store.setup
        Form {
            Row(ok: store.loginItemStatus == .enabled, title: "Background daemon",
                detail: daemonDetail) {
                if store.loginItemStatus == .enabled {
                    Button("Turn off") { store.unregisterDaemon() }
                } else if store.loginItemStatus == .requiresApproval {
                    Button("Open Login Items") { SMAppServiceOpen.loginItems() }
                } else {
                    Button("Turn on") { store.registerDaemon() }
                }
            }
            Row(ok: s?.claude_hooks_current == true, title: "Claude Code hooks",
                detail: "Heavy commands ask for a lease first; sessions get orphan questions. Adds four hooks to ~/.claude/settings.json and keeps a backup. Your other settings and hooks are untouched.") {
                if s?.claude_hooks == true {
                    if s?.claude_hooks_current == false { Button("Repair") { store.setup("claude", install: true) } }
                    Button("Remove") { store.setup("claude", install: false) }
                } else {
                    Button("Install") { store.setup("claude", install: true) }
                }
            }
            Row(ok: s?.codex_note == true, title: "Codex instructions",
                detail: "Codex has no hooks, so this adds a marked section to ~/.codex/AGENTS.md telling it to acquire and release leases.") {
                Button(s?.codex_note == true ? "Remove" : "Install") { store.setup("codex", install: s?.codex_note != true) }
            }
            Row(ok: !(s?.cli_link ?? "").isEmpty, title: "agentrm command line",
                detail: cliDetail(s)) {
                Button((s?.cli_link ?? "").isEmpty ? "Install" : "Remove") {
                    store.setup("path", install: (s?.cli_link ?? "").isEmpty)
                }
            }
        }
        .formStyle(.grouped)
    }

    private var daemonDetail: String {
        switch store.loginItemStatus {
        case .enabled: return "Runs at login and keeps running in the background."
        case .requiresApproval: return "Waiting for your approval in System Settings → General → Login Items."
        default: return "Not running. Leases, orphan checks and auto-stop need it."
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
    @State private var reserve = 4.0
    @State private var grace = 10
    @State private var minFree = 10
    @State private var loaded = false

    var body: some View {
        Form {
            Stepper(value: $reserve, in: 1...32, step: 1) {
                LabeledContent("Keep back for macOS and apps", value: "\(Int(reserve)) GB")
            }
            Stepper(value: $grace, in: 1...240, step: 5) {
                LabeledContent("Stop unclaimed resources after", value: "\(grace) min")
            }
            Stepper(value: $minFree, in: 1...50, step: 1) {
                LabeledContent("Refuse leases below", value: "\(minFree)% free memory")
            }
            HStack {
                Spacer()
                Button("Apply") { store.setLimits(reserve: reserve, grace: grace, minFree: minFree) }
                    .keyboardShortcut(.defaultAction)
            }
            Text("Advanced settings (never-stop list, dev-server names, extra command patterns) live in ~/.agentrm/config.json.")
                .font(.caption).foregroundStyle(.secondary)
        }
        .formStyle(.grouped)
        .onAppear { load() }
        .onChange(of: store.status?.config.reserve_gb) { _, _ in load() }
    }

    private func load() {
        guard !loaded, let c = store.status?.config else { return }
        reserve = c.reserve_gb
        grace = c.grace_minutes
        minFree = c.min_free_percent
        loaded = true
    }
}

struct AboutTab: View {
    var body: some View {
        VStack(spacing: 8) {
            Image(systemName: "memorychip").font(.system(size: 40))
            Text("AgentRM").font(.title2.bold())
            Text("Version \(Bundle.main.object(forInfoDictionaryKey: "CFBundleShortVersionString") as? String ?? "")")
                .foregroundStyle(.secondary)
            Text("A resource manager for coding agents: leases for heavy work, questions for whatever is left running, and a reversible stop for what nobody claims.")
                .multilineTextAlignment(.center).font(.callout).padding(.horizontal, 30)
            Link("Source code", destination: URL(string: "https://github.com/agentrm/agentrm")!)
        }
        .padding()
    }
}
