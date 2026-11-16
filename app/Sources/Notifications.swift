import Foundation
import UserNotifications

/// Posts notifications for daemon events, with actions, and runs the action the user picks.
final class NotificationHandler: NSObject, UNUserNotificationCenterDelegate {
    weak var store: Store?

    private enum Cat {
        static let approval = "APPROVAL", orphan = "ORPHAN", paused = "PAUSED", ask = "ASKYOU", info = "INFO"
    }

    func install() {
        let c = UNUserNotificationCenter.current()
        c.delegate = self
        c.requestAuthorization(options: [.alert, .sound]) { _, _ in }
        let approve = UNNotificationAction(identifier: "approve", title: "Approve", options: [.authenticationRequired])
        let deny = UNNotificationAction(identifier: "deny", title: "Deny", options: [.destructive])
        let claim = UNTextInputNotificationAction(identifier: "claim", title: "Claim…", options: [],
                                                  textInputButtonTitle: "Claim", textInputPlaceholder: "Why do you need it?")
        let stop = UNNotificationAction(identifier: "stop", title: "Stop now", options: [.destructive])
        let resume = UNNotificationAction(identifier: "resume", title: "Resume", options: [])
        let keep = UNNotificationAction(identifier: "keep", title: "Keep it", options: [])
        c.setNotificationCategories([
            UNNotificationCategory(identifier: Cat.approval, actions: [approve, deny], intentIdentifiers: []),
            UNNotificationCategory(identifier: Cat.orphan, actions: [claim, stop], intentIdentifiers: []),
            UNNotificationCategory(identifier: Cat.paused, actions: [resume, stop], intentIdentifiers: []),
            UNNotificationCategory(identifier: Cat.ask, actions: [keep, stop], intentIdentifiers: []),
        ])
    }

    private static let titles: [String: String] = [
        "APPROVAL": "Approve this?", "ORPHAN": "Unclaimed resource", "PAUSED": "Paused (nothing lost)",
        "STOPPED": "Stopped", "STOP": "Could not stop", "ASKYOU": "Still using this?",
        "OVERRIDE": "Changed outside the app", "TAMPER": "AgentRM was tampered with",
        "UNLEASED-USE": "Heavy use without a lease", "HEAVY": "Heavy work without a lease",
    ]

    @MainActor
    func post(_ events: [AgentEvent]) {
        for e in events {
            guard let title = Self.titles[e.kind] else { continue }
            let content = UNMutableNotificationContent()
            content.title = title
            content.body = e.text
            if e.kind == "TAMPER" || e.kind == "APPROVAL" { content.sound = .default }
            switch e.kind {
            case "APPROVAL": content.categoryIdentifier = Cat.approval
            case "ORPHAN": content.categoryIdentifier = Cat.orphan
            case "PAUSED": content.categoryIdentifier = Cat.paused
            case "ASKYOU": content.categoryIdentifier = Cat.ask
            default: content.categoryIdentifier = Cat.info
            }
            content.userInfo = ["ref": e.ref ?? ""]
            UNUserNotificationCenter.current().add(
                UNNotificationRequest(identifier: "agentrm-\(e.seq)", content: content, trigger: nil))
        }
    }

    func userNotificationCenter(_ center: UNUserNotificationCenter, didReceive response: UNNotificationResponse,
                                withCompletionHandler done: @escaping () -> Void) {
        let ref = response.notification.request.content.userInfo["ref"] as? String ?? ""
        let action = response.actionIdentifier
        let text = (response as? UNTextInputNotificationResponse)?.userText ?? ""
        Task { @MainActor in
            guard let store = self.store, !ref.isEmpty else { return done() }
            switch action {
            case "approve": store.answer(ref, approve: true)
            case "deny": store.answer(ref, approve: false)
            case "claim": store.claim(ref, purpose: text.isEmpty ? "kept by you" : text, hours: 4)
            case "keep": store.claim(ref, purpose: "kept by you", hours: 4)
            case "stop": store.stop(ref)
            case "resume": store.resume(ref)
            default: break
            }
            done()
        }
    }

    // Show notifications even while the menu is open.
    func userNotificationCenter(_ center: UNUserNotificationCenter, willPresent notification: UNNotification,
                                withCompletionHandler done: @escaping (UNNotificationPresentationOptions) -> Void) {
        done([.banner, .sound])
    }
}
