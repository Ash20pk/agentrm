import Sparkle
import SwiftUI

@main
struct AgentRMApp: App {
    @StateObject private var store = Store()
    // Sparkle checks GitHub releases for signed updates (SUFeedURL / SUPublicEDKey in Info.plist).
    private let updates = SPUStandardUpdaterController(startingUpdater: true, updaterDelegate: nil, userDriverDelegate: nil)

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

        Settings {
            SettingsView(updater: updates.updater)
                .environmentObject(store)
        }
    }
}

/// The menu-bar icon. It also opens the welcome window on first launch.
struct MenuIcon: View {
    @EnvironmentObject var store: Store
    @Environment(\.openWindow) private var openWindow

    var body: some View {
        Image(systemName: icon)
            .accessibilityLabel(label)
            .task {
                if !store.onboarded {
                    NSApp.activate(ignoringOtherApps: true)
                    openWindow(id: "welcome")
                }
            }
    }

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
