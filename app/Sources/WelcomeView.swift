import SwiftUI

/// First run: what AgentRM does, what is running right now, and what installing it would change,
/// before anything is changed.
struct WelcomeView: View {
    @EnvironmentObject var store: Store
    @Environment(\.dismissWindow) private var dismissWindow
    @State private var step = 0
    @State private var hooks = true
    @State private var shim = false
    @State private var cli = true

    var body: some View {
        VStack(alignment: .leading, spacing: 14) {
            switch step {
            case 0: intro
            case 1: running
            default: install
            }
            Spacer(minLength: 0)
            HStack {
                if step > 0 { Button("Back") { step -= 1 } }
                Spacer()
                if step < 2 {
                    Button("Next") { step += 1 }.keyboardShortcut(.defaultAction)
                } else {
                    Button("Skip for now") { finish(install: false) }
                    Button("Install and start") { finish(install: true) }.keyboardShortcut(.defaultAction)
                }
            }
        }
        .padding(24)
        .frame(width: 560, height: 460)
        .task { await store.loadHookPreview() }
    }

    private var intro: some View {
        VStack(alignment: .leading, spacing: 12) {
            Label("Welcome to AgentRM", systemImage: "memorychip").font(.title2.bold())
            Text("Coding agents start clusters, builds, simulators and servers as if they had this Mac to themselves. AgentRM makes them ask first, say why, and clean up after themselves.")
            VStack(alignment: .leading, spacing: 6) {
                Label("Heavy work gets a lease only if the memory is really there.", systemImage: "checkmark.seal")
                Label("Anything nobody claims is asked about, then paused (nothing lost), then stopped.", systemImage: "pause.circle")
                Label("Everything stopped can be restored.", systemImage: "arrow.uturn.backward.circle")
            }
            .font(.callout)
            Text("AgentRM starts in **dry run**: it asks and reports, but never pauses or stops anything. Turn on auto-stop when its decisions look right to you.")
                .font(.callout).foregroundStyle(.secondary)
        }
    }

    private var running: some View {
        VStack(alignment: .leading, spacing: 10) {
            Text("Running right now").font(.title3.bold())
            if let s = store.status {
                Text("\(s.mem.available_mb.gb) of \(s.mem.total_mb.gb) available · swap \(s.mem.swap_used_mb.gb) used")
                    .font(.callout).foregroundStyle(.secondary)
                if store.resources.isEmpty {
                    Text("Nothing AgentRM manages is running.").font(.callout)
                } else {
                    ScrollView {
                        VStack(alignment: .leading, spacing: 4) {
                            ForEach(store.resources) { r in
                                HStack {
                                    Text(r.name).lineLimit(1)
                                    Spacer()
                                    Text(r.mem_mb.gb).foregroundStyle(.secondary)
                                }
                                .font(.callout)
                            }
                        }
                    }
                    .frame(maxHeight: 200)
                    Text("In dry run these are only reported. Pin anything you always want left alone.")
                        .font(.caption).foregroundStyle(.secondary)
                }
            } else {
                Text("The daemon is starting…").foregroundStyle(.secondary)
            }
        }
    }

    private var install: some View {
        VStack(alignment: .leading, spacing: 10) {
            Text("Connect your agents").font(.title3.bold())
            Toggle("Claude Code hooks (recommended)", isOn: $hooks)
            if hooks && !store.hookPreview.isEmpty {
                ScrollView(.horizontal) {
                    Text(store.hookPreview).font(.system(.caption, design: .monospaced)).textSelection(.enabled)
                }
                .frame(maxHeight: 110)
                .padding(6)
                .background(.quaternary.opacity(0.5), in: RoundedRectangle(cornerRadius: 6))
            }
            Toggle("Other agents: PATH shim for Codex, Gemini CLI, Aider, OpenCode, Cursor", isOn: $shim)
            Toggle("The agentrm command in ~/.local/bin", isOn: $cli)
            Text("You can change any of these later in Settings → Setup, and remove them all with Uninstall.")
                .font(.caption).foregroundStyle(.secondary)
        }
    }

    private func finish(install: Bool) {
        if install {
            if hooks { store.setup("claude", install: true) }
            if shim { store.setup("shim", install: true) }
            if cli { store.setup("path", install: true) }
        }
        store.onboarded = true
        _ = store.dryRunSince // start the dry-run clock
        dismissWindow(id: "welcome")
    }
}
