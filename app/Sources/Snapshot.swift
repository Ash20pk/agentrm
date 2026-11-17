#if DEBUG
import AppKit
import Sparkle
import SwiftUI

/// Debug builds only: render the app's views to PNG files from a status fixture, to review the
/// design without clicking through the menu bar.
///
///   AgentRM.app/Contents/MacOS/AgentRM --snapshot <out-dir> --fixture <status.json> [--name busy]
///
/// Writes <name>-menu-light.png, <name>-menu-dark.png and, with --all, the settings tabs and the
/// welcome steps. A fixture of "none" renders the daemon-down state.
enum Snapshot {
    static func runIfAsked() {
        let args = CommandLine.arguments
        guard let i = args.firstIndex(of: "--snapshot"), i + 1 < args.count else { return }
        let out = URL(fileURLWithPath: args[i + 1], isDirectory: true)
        let fixture = args.firstIndex(of: "--fixture").flatMap { $0 + 1 < args.count ? args[$0 + 1] : nil }
        let name = args.firstIndex(of: "--name").flatMap { $0 + 1 < args.count ? args[$0 + 1] : nil } ?? "menu"
        let all = args.contains("--all")
        DispatchQueue.main.async {
            MainActor.assumeIsolated {
                render(out: out, fixture: fixture, name: name, all: all)
                exit(0)
            }
        }
    }

    @MainActor
    private static func render(out: URL, fixture: String?, name: String, all: Bool) {
        try? FileManager.default.createDirectory(at: out, withIntermediateDirectories: true)
        var status: Status?
        if let fixture, fixture != "none" {
            do {
                status = try CLI.decoder.decode(Status.self, from: Data(contentsOf: URL(fileURLWithPath: fixture)))
            } catch {
                FileHandle.standardError.write("fixture: \(error)\n".data(using: .utf8)!)
                exit(1)
            }
        }
        let store = Store(preview: status, setup: sampleSetup, review: sampleReview)
        store.receipt = Receipt(since: Date().addingTimeInterval(-7 * 86400), until: Date(), sessions_ended: 23, leftovers: 9,
                                paused: 14, stopped: 11, freed_gb: 18.4, would_stop: 0, would_free_gb: 0, wrong: 1, restored: 2,
                                queued: 6, wait_mean: 74_000_000_000, wait_max: 310_000_000_000, misreports: 1,
                                text: "Stopped 11 things, freeing 18.4 GB; found 9 processes that ended sessions left running; 6 requests waited for memory, 1 min 14 s on average; 2 were restored afterwards; 1 decision you marked wrong.")
        let updater = SPUStandardUpdaterController(startingUpdater: false, updaterDelegate: nil, userDriverDelegate: nil).updater

        // A plausible hour for the waterfall: agents busy, then one walks away and its memory turns red.
        if let r = store.reading {
            let base = r.segments
            var hour: [[Segment]] = []
            for m in 0..<45 {
                var col = base
                if m < 20 { col = col.map { $0 == .lit(.nobody) || $0 == .lit(.paused) ? .lit(.inUse) : $0 } }
                else if m < 40 { col = col.map { $0 == .lit(.paused) ? .lit(.nobody) : $0 } }
                hour.append(col)
            }
            store.setPreviewWaterfall(hour)
        }
        let dark = true
        let suffix = "dark"
        shoot(MenuView(updater: updater), store: store, dark: dark, size: nil, to: out.appendingPathComponent("\(name)-menu-\(suffix).png"))
        for d in [false, true] {
            shoot(MenuIcon().padding(.horizontal, 10).frame(height: 24), store: store, dark: d, size: nil,
                  to: out.appendingPathComponent("\(name)-menubar-\(d ? "dark" : "light").png"))
        }
        guard all else { return }
        for tab in ActivityView.Tab.allCases {
            shoot(ActivityView(updater: updater, tab: tab), store: store, dark: dark, size: CGSize(width: 1060, height: 720),
                  to: out.appendingPathComponent("activity-\(tab.rawValue.lowercased()).png"))
        }
        shoot(WelcomeView(), store: store, dark: dark, size: nil, to: out.appendingPathComponent("welcome.png"))
        shoot(AutoStopGate(isPresented: .constant(true)), store: store, dark: dark, size: nil, to: out.appendingPathComponent("gate.png"))
        if let r = status?.resources?.values.first(where: { $0.lease_id == nil }) {
            shoot(KeepSheet(resource: r) {}, store: store, dark: dark, size: nil, to: out.appendingPathComponent("keep.png"))
        }
    }

    @MainActor
    private static func shoot<V: View>(_ view: V, store: Store, dark: Bool, size: CGSize?, to url: URL) {
        let appearance = NSAppearance(named: dark ? .darkAqua : .aqua)
        let root = view.environmentObject(store)
            .background(Color(nsColor: .windowBackgroundColor))
            .environment(\.colorScheme, dark ? .dark : .light)
        let host = NSHostingView(rootView: root)
        host.appearance = appearance
        let window = NSWindow(contentRect: CGRect(x: -10_000, y: -10_000, width: 100, height: 100),
                              styleMask: [.borderless], backing: .buffered, defer: false)
        window.appearance = appearance
        window.contentView = host
        // Lay out twice: views that measure themselves (the menu's scroll height) settle on the second pass.
        for _ in 0..<3 {
            host.frame.size = size ?? host.fittingSize
            window.setContentSize(host.frame.size)
            host.layoutSubtreeIfNeeded()
            RunLoop.main.run(until: Date().addingTimeInterval(0.25))
        }
        guard let rep = host.bitmapImageRepForCachingDisplay(in: host.bounds) else { return }
        host.cacheDisplay(in: host.bounds, to: rep)
        try? rep.representation(using: .png, properties: [:])?.write(to: url)
        print(url.path)
    }

    private static var sampleSetup: SetupCheck {
        SetupCheck(binary: "/Applications/AgentRM.app/Contents/Helpers/agentrm", claude_hooks: true, claude_hooks_current: true,
                   codex_note: false, cli_link: "~/.local/bin/agentrm", cli_link_dir_on_path: true, daemon_running: true,
                   shim: false, problems: [])
    }

    private static var sampleReview: ReviewResp {
        let now = Date()
        return ReviewResp(decisions: [
            Decision(id: "d1", time: now.addingTimeInterval(-3600), action: "pause", resource: "kind:obx-openshell", name: "obx-openshell",
                     mem_mb: 520, why: "unclaimed 10 min; no agent touched it", dry_run: true, verdict: nil),
            Decision(id: "d2", time: now.addingTimeInterval(-7200), action: "stop", resource: "c:pg", name: "supabase_db_claimwell",
                     mem_mb: 380, why: "paused 30 min, nobody claimed it", dry_run: true, verdict: "right"),
            Decision(id: "d3", time: now.addingTimeInterval(-86400), action: "pause", resource: "srv:72525", name: "node :8282",
                     mem_mb: 60, why: "unclaimed 10 min", dry_run: true, verdict: "wrong"),
        ], reviewed: 2, wrong: 1, false_rate: 0.5)
    }
}
#endif
