import Sparkle
import SwiftUI

@main
struct AgentRMApp: App {
    @StateObject private var store = Store()
    // Sparkle checks GitHub releases for signed updates (SUFeedURL / SUPublicEDKey in Info.plist).
    private let updates = SPUStandardUpdaterController(startingUpdater: true, updaterDelegate: nil, userDriverDelegate: nil)

    init() {
        #if DEBUG
        Snapshot.runIfAsked()
        #endif
    }

    var body: some Scene {
        MenuBarExtra {
            MenuView(updater: updates.updater)
                .environmentObject(store)
        } label: {
            MenuIcon().environmentObject(store)
        }
        .menuBarExtraStyle(.window)

        Window("Welcome to AgentRM", id: "welcome") {
            WelcomeView().environmentObject(store)
        }
        .windowResizability(.contentSize)

        Window("AgentRM", id: "activity") {
            ActivityView(updater: updates.updater)
                .environmentObject(store)
        }
        .defaultSize(width: 1060, height: 720)
    }
}

/// The menu-bar item: a miniature of the LED meter, one bar per GB lit in its state colour, and
/// the memory a new lease could get. It also opens the welcome window on first launch.
struct MenuIcon: View {
    @EnvironmentObject var store: Store
    @Environment(\.openWindow) private var openWindow

    var body: some View {
        HStack(spacing: 4) {
            Image(nsImage: MiniMeter.image(store.reading?.segments ?? Array(repeating: .off, count: 16),
                                           alert: store.underPressure || !store.pendingApprovals.isEmpty))
            Text(text).monospacedDigit()
        }
        .accessibilityElement(children: .ignore)
        .accessibilityLabel(label)
        .task {
            if !store.onboarded {
                NSApp.activate(ignoringOtherApps: true)
                openWindow(id: "welcome")
            }
        }
    }

    /// The reading next to the meter: what a lease could get now, or why there is nothing.
    private var text: String {
        guard let s = store.status else { return "off" }
        if !store.pendingApprovals.isEmpty { return "approve?" }
        // Let through: nothing is being managed, so a headroom figure would mislead.
        if s.fail_open != nil { return "open" }
        if store.underPressure { return "swap \(Int(s.swap_rate_mb_min ?? 0))" }
        return "\(Fmt.gb(max(0, s.headroom_mb)))G"
    }

    private var label: String {
        guard let s = store.status, let r = store.reading else { return "AgentRM: daemon not running" }
        var parts = ["AgentRM", "\(Fmt.gb(max(0, s.headroom_mb))) GB free to grant"]
        if !store.pendingApprovals.isEmpty { parts.append("approval waiting") }
        if let open = s.fail_open { parts.append("not managing memory: \(open)") }
        if store.underPressure { parts.append("memory pressure") }
        if r.nobodyMB > 0 { parts.append("\(Fmt.gb(r.nobodyMB)) GB owned by nobody") }
        return parts.joined(separator: ", ")
    }
}

/// Draws the meter small enough for the menu bar. Colours are kept (not a template image) because
/// the colour is the information; unlit bars are a grey that shows on light and dark menu bars.
enum MiniMeter {
    static func image(_ segments: [Segment], alert: Bool) -> NSImage {
        let bar: CGFloat = 2.5, gap: CGFloat = 1, h: CGFloat = 14
        let w = CGFloat(segments.count) * bar + CGFloat(max(0, segments.count - 1)) * gap
        let img = NSImage(size: NSSize(width: w + (alert ? 6 : 0), height: h), flipped: false) { _ in
            for (i, seg) in segments.enumerated() {
                let rect = NSRect(x: CGFloat(i) * (bar + gap), y: 1, width: bar, height: h - 2)
                let color: NSColor
                switch seg {
                case .lit(let l): color = NSColor(l == .system ? Color(hex: 0x8E949C) : l.color)
                case .kept: color = NSColor(white: 0.55, alpha: 0.45)
                case .off: color = NSColor(white: 0.55, alpha: 0.28)
                }
                color.setFill()
                NSBezierPath(roundedRect: rect, xRadius: 0.8, yRadius: 0.8).fill()
            }
            if alert {
                NSColor(Lamp.nobody.color).setFill()
                NSBezierPath(ovalIn: NSRect(x: w + 2, y: h - 5, width: 4, height: 4)).fill()
            }
            return true
        }
        img.isTemplate = false
        return img
    }
}
