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

/// The menu-bar icon. It also opens the welcome window on first launch.
struct MenuIcon: View {
    @EnvironmentObject var store: Store
    @Environment(\.openWindow) private var openWindow

    var body: some View {
        HStack(spacing: 3) {
            Image(systemName: icon)
            // A count only when something needs a decision; a quiet icon otherwise.
            if attention > 0 { Text("\(attention)").monospacedDigit() }
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

    private var attention: Int { store.pendingApprovals.count + store.orphans.count }

    private var icon: String {
        if store.status == nil { return "memorychip.fill" }
        if !store.pendingApprovals.isEmpty || store.underPressure { return "exclamationmark.triangle.fill" }
        if !store.orphans.isEmpty { return "memorychip.fill" }
        return "memorychip"
    }

    private var label: String {
        if store.status == nil { return "AgentRM: daemon not running" }
        if !store.pendingApprovals.isEmpty { return "AgentRM: approval waiting" }
        if store.underPressure { return "AgentRM: memory pressure" }
        if !store.orphans.isEmpty { return "AgentRM: \(store.orphans.count) unclaimed" }
        return "AgentRM"
    }
}
