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

/// The menu-bar item: the app's logo. It also
/// starts the notch island and opens the welcome window on first launch.
struct MenuIcon: View {
    @EnvironmentObject var store: Store
    @Environment(\.openWindow) private var openWindow

    var body: some View {
        // Just the logo: the numbers are in the notch.
        Image(nsImage: MenuLogo.image(alert: store.underPressure || store.runningHot || !store.pendingApprovals.isEmpty))
        .accessibilityElement(children: .ignore)
        .accessibilityLabel(label)
        .task {
            NotchController.start(store: store) { openWindow(id: "activity") }
            if !store.onboarded {
                NSApp.activate(ignoringOtherApps: true)
                openWindow(id: "welcome")
            }
        }
    }

    private var label: String {
        guard let s = store.status, let r = store.reading else { return "AgentRM: daemon not running" }
        var parts = ["AgentRM", "\(Fmt.gb(max(0, s.headroom_mb))) GB free to grant"]
        if !store.pendingApprovals.isEmpty { parts.append("approval waiting") }
        if let open = s.fail_open { parts.append("not managing memory: \(open)") }
        if store.underPressure { parts.append("memory pressure") }
        if store.runningHot { parts.append("running hot, builds wait") }
        if r.nobodyMB > 0 { parts.append("\(Fmt.gb(r.nobodyMB)) GB owned by nobody") }
        return parts.joined(separator: ", ")
    }
}

/// The logo drawn for the menu bar: the meter's window with its seven bars, four lit, as in the
/// app icon. A template image, so macOS draws it white on a dark menu bar like its own icons.
/// When something needs you a dot sits on its corner (a template has one colour, so the dot is
/// its shape, not red).
enum MenuLogo {
    static func image(alert: Bool) -> NSImage {
        let w: CGFloat = 26, h: CGFloat = 18
        let img = NSImage(size: NSSize(width: w, height: h), flipped: false) { _ in
            NSColor.black.set()
            let window = NSBezierPath(roundedRect: NSRect(x: 0.75, y: 1.75, width: w - 1.5, height: h - 3.5), xRadius: 4, yRadius: 4)
            window.lineWidth = 1.5
            window.stroke()
            for i in 0..<7 {
                NSColor.black.withAlphaComponent(i < 4 ? 1 : 0.35).setFill()
                let bar = NSRect(x: 3.75 + CGFloat(i) * 2.75, y: 4.5, width: 2, height: h - 9)
                NSBezierPath(roundedRect: bar, xRadius: 0.6, yRadius: 0.6).fill()
            }
            if alert {
                let dot = NSRect(x: w - 8, y: h - 8, width: 7, height: 7)
                NSGraphicsContext.current?.compositingOperation = .clear
                NSBezierPath(ovalIn: dot.insetBy(dx: -1.5, dy: -1.5)).fill()
                NSGraphicsContext.current?.compositingOperation = .sourceOver
                NSColor.black.setFill()
                NSBezierPath(ovalIn: dot).fill()
            }
            return true
        }
        img.isTemplate = true
        return img
    }
}
