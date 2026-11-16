import SwiftUI

@main
struct AgentRMApp: App {
    @StateObject private var store = Store()

    var body: some Scene {
        MenuBarExtra {
            MenuView()
                .environmentObject(store)
        } label: {
            Image(systemName: icon)
        }
        .menuBarExtraStyle(.window)

        Settings {
            SettingsView()
                .environmentObject(store)
        }
    }

    private var icon: String {
        if store.status == nil { return "memorychip.fill" }
        if store.underPressure { return "exclamationmark.triangle.fill" }
        if !store.orphans.isEmpty { return "memorychip.fill" }
        return "memorychip"
    }
}
