import Foundation

extension Store {
    /// Let everything through for a while (0 ends it): every request is granted and nothing is
    /// paused or stopped automatically. The escape hatch when AgentRM is in the way.
    func bypass(minutes: Int) { act(["bypass", minutes > 0 ? String(minutes) : "off"]) }
}
