import SwiftUI

/// First run: a first reading of this Mac, then what connecting the agents would change, before
/// anything is changed.
struct WelcomeView: View {
    @EnvironmentObject var store: Store
    @Environment(\.dismissWindow) private var dismissWindow
    @State private var hooks = true
    @State private var shim = false
    @State private var others = true
    @State private var cli = true
    @State private var showChange = false

    var body: some View {
        VStack(alignment: .leading, spacing: 14) {
            HStack(spacing: 10) {
                Text("AGENTRM").font(Face.mono(13, .bold)).tracking(2)
                Silk("First reading")
            }
            reading
            if !store.leftBehind.isEmpty || !store.askOnlyLeftBehind.isEmpty { leaking }
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
        .frame(width: 640, height: (showChange ? 640 : 520) + (store.leftBehind.isEmpty && store.askOnlyLeftBehind.isEmpty ? 0 : 210))
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

    /// What agents that already exited left running, found from their environments before any
    /// setting is touched: the first thing worth seeing.
    private var leaking: some View {
        let left = store.leftBehind
        let mb = left.reduce(0) { $0 + $1.mem_mb }
        return VStack(alignment: .leading, spacing: 8) {
            HStack {
                Text(left.isEmpty ? "Agents that already exited left these running"
                                  : "Leaking right now: \(Fmt.gb(mb)) GB left by agents that already exited")
                    .font(Face.body(15, .semibold))
                Spacer()
                if !left.isEmpty {
                    Button("Collect \(left.count)") { store.collect() }.buttonStyle(.key(.go))
                        .help("Stops them now. Each can be restored from the Stopped tab.")
                }
            }
            ScrollView {
                VStack(spacing: 0) {
                    ForEach(left + store.askOnlyLeftBehind) { r in
                        HStack(spacing: 10) {
                            LampDot(color: r.ask_only == true ? Ink.scale : Lamp.nobody.color, glow: false)
                            VStack(alignment: .leading, spacing: 1) {
                                Text(r.name).font(Face.body(13, .semibold))
                                Text(r.ask_only == true ? "An app an agent launched: AgentRM only asks about it." : r.detail)
                                    .font(Face.body(11)).foregroundStyle(Ink.dim).lineLimit(1)
                            }
                            Spacer()
                            Text("\(Fmt.gb(r.mem_mb)) GB").font(Face.mono(12))
                        }
                        .padding(.vertical, 5)
                    }
                }
            }
            .frame(maxHeight: 120)
            Text("Found from what each agent leaves in its processes' environment. Collecting stops them now; everything stopped can be restored. Nothing else is touched in dry run.")
                .font(Face.body(11)).foregroundStyle(Ink.label).fixedSize(horizontal: false, vertical: true)
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
            option($others, "Codex, Gemini CLI, Cursor", "Their hooks too, where installed: heavy commands ask first, and questions reach them.") { EmptyView() }
            option($shim, "Aider, OpenCode and older agents", "A PATH shim in front of docker, cargo and kind. Agents with hooks pass straight through.") { EmptyView() }
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
            if others {
                let home = FileManager.default.homeDirectoryForCurrentUser
                for (agent, dir) in [("codex", ".codex"), ("gemini", ".gemini"), ("cursor", ".cursor")]
                    where FileManager.default.fileExists(atPath: home.appendingPathComponent(dir).path) {
                    store.setup(agent, install: true)
                }
            }
            if shim { store.setup("shim", install: true) }
            if cli { store.setup("path", install: true) }
        }
        store.onboarded = true
        _ = store.dryRunSince // start the dry-run clock
        dismissWindow(id: "welcome")
    }
}
