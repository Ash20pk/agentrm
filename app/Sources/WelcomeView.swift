import SwiftUI

/// First run: a first reading of this Mac, then what connecting the agents would change, before
/// anything is changed.
struct WelcomeView: View {
    @EnvironmentObject var store: Store
    @Environment(\.dismissWindow) private var dismissWindow
    @State private var hooks = true
    @State private var shim = false
    @State private var cli = true
    @State private var showChange = false

    var body: some View {
        VStack(alignment: .leading, spacing: 14) {
            HStack(spacing: 10) {
                Text("AGENTRM").font(Face.mono(13, .bold)).tracking(2)
                Silk("First reading")
            }
            reading
            options
            Spacer(minLength: 0)
            HStack(spacing: 8) {
                LampLabel(lamp: Lamp.waiting.color, text: "Starts in dry run: reports, pauses nothing")
                Spacer()
                Button("Not now") { finish(install: false) }.buttonStyle(.key(.plain, large: true))
                Button("Connect") { finish(install: true) }.buttonStyle(.key(.go, large: true)).keyboardShortcut(.defaultAction)
            }
        }
        .padding(22)
        .frame(width: 640, height: showChange ? 640 : 520)
        .background(Ink.bg)
        .foregroundStyle(Ink.text)
        .font(Face.body(14))
        .environment(\.colorScheme, .dark)
        .task { await store.loadHookPreview() }
    }

    private var reading: some View {
        VStack(alignment: .leading, spacing: 10) {
            if let r = store.reading {
                Text(headline(r)).font(Face.body(22, .semibold)).fixedSize(horizontal: false, vertical: true)
                LEDMeter(segments: r.segments, height: 40, showScale: false)
                Text(detail(r)).font(Face.body(13)).foregroundStyle(Ink.dim).fixedSize(horizontal: false, vertical: true)
            } else {
                Text("Taking the first reading…").font(Face.body(22, .semibold))
                LEDMeter(segments: Array(repeating: .off, count: 16), height: 40, showScale: false)
                Text("The background daemon is starting.").font(Face.body(13)).foregroundStyle(Ink.dim)
            }
        }
        .padding(14).panel()
    }

    private func headline(_ r: Reading) -> String {
        r.nobodyMB >= 100 ? "\(Fmt.gb(r.nobodyMB)) GB of this Mac is running with nobody behind it."
                          : "Nothing heavy is running unowned right now."
    }

    private func detail(_ r: Reading) -> String {
        let names = r.items.filter { $0.lamp == .nobody }.prefix(3).map(\.name)
        if names.isEmpty { return "When an agent leaves a cluster, a build daemon or a server behind, AgentRM asks it who needs it." }
        return "\(names.joined(separator: ", ")). Connect your agents and AgentRM asks them who needs these."
    }

    private var options: some View {
        VStack(spacing: 0) {
            option($hooks, "Claude Code", "Four hooks in ~/.claude/settings.json. Heavy commands ask for memory first.", first: true) {
                Button(showChange ? "Hide change" : "Show change") { showChange.toggle() }.buttonStyle(.key)
            }
            if showChange && hooks && !store.hookPreview.isEmpty {
                ScrollView([.vertical, .horizontal]) {
                    Text(store.hookPreview).font(Face.mono(11)).foregroundStyle(Ink.dim).textSelection(.enabled)
                        .frame(maxWidth: .infinity, alignment: .leading)
                }
                .frame(height: 110).padding(10).background(Ink.off)
            }
            option($shim, "Codex, Gemini CLI, Aider, OpenCode, Cursor", "A PATH shim in front of docker, cargo and kind. You and Claude pass straight through.") { EmptyView() }
            option($cli, "The agentrm command", "A link in ~/.local/bin for agents and your terminal.") { EmptyView() }
        }
        .panel()
    }

    private func option<T: View>(_ on: Binding<Bool>, _ title: String, _ detail: String, first: Bool = false, @ViewBuilder trailing: () -> T) -> some View {
        HStack(alignment: .top, spacing: 10) {
            Toggle(isOn: on) { EmptyView() }.toggleStyle(.checkbox).tint(Lamp.inUse.color).labelsHidden()
            VStack(alignment: .leading, spacing: 2) {
                Text(title).font(Face.body(14, .semibold))
                Text(detail).font(Face.body(12)).foregroundStyle(Ink.dim).fixedSize(horizontal: false, vertical: true)
            }
            Spacer()
            trailing()
        }
        .padding(.horizontal, 12).padding(.vertical, 11)
        .overlay(alignment: .top) { if !first { Rectangle().fill(Ink.hair).frame(height: 1) } }
        .contentShape(Rectangle())
        .onTapGesture { on.wrappedValue.toggle() }
        .accessibilityElement(children: .combine)
        .accessibilityAddTraits(.isButton)
        .accessibilityValue(on.wrappedValue ? "on" : "off")
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
