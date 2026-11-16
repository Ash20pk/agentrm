import Foundation

/// Something on the panel: a running resource, or a lease with nothing running behind it yet
/// (a build). Its lamp decides its colour everywhere.
struct Item: Identifiable {
    enum Action { case keep, stop, release(String), resume, unpin }

    let id: String
    let name: String
    let detail: String
    let mb: Double
    let lamp: Lamp
    let resource: Resource?
    let actions: [Action]
    /// For a countdown: when the next step (pause or stop) happens, and how long the wait is in all.
    var deadline: Date? = nil
    var window: TimeInterval = 0
}

/// What the meter, the gauges and the rows show, worked out once from the daemon's status.
struct Reading {
    var mbByLamp: [Lamp: Double]
    var keptMB: Double
    var freeMB: Double
    var totalMB: Double
    var segments: [Segment]
    var items: [Item]

    var nobodyMB: Double { mbByLamp[.nobody] ?? 0 }
}

extension Store {
    func lamp(for r: Resource) -> Lamp {
        if r.paused_at != nil { return .paused }
        if r.pinned == true { return .system }
        if let lid = r.lease_id {
            return status?.leases?[lid]?.verified == false ? .waiting : .inUse
        }
        return .nobody
    }

    var reading: Reading? {
        guard let s = status else { return nil }
        var items: [Item] = []
        var byLamp: [Lamp: Double] = [:]

        for r in resources {
            let lamp = lamp(for: r)
            byLamp[lamp, default: 0] += r.mem_mb
            items.append(item(for: r, lamp: lamp, s))
        }
        // Leases nothing is running behind yet (a build in progress): shown by what they asked for.
        let backed = Set(resources.compactMap(\.lease_id))
        for l in leases where !backed.contains(l.id) {
            let lamp: Lamp = l.verified == false ? .waiting : .inUse
            let mb = l.mem_gb * 1024
            byLamp[lamp, default: 0] += mb
            let evidence = l.verified == false ? "no use seen yet" : (l.evidence ?? "in use")
            items.append(Item(id: l.id, name: l.purpose.isEmpty ? Kind.name(l.kind) : l.purpose,
                              detail: "\(l.agent.capitalized), \(evidence)", mb: mb, lamp: lamp, resource: nil,
                              actions: [.release(l.id)]))
        }

        let total = max(s.mem.total_mb, 1)
        let used = max(0, total - s.mem.available_mb)
        let tracked = byLamp.values.reduce(0, +)
        byLamp[.system, default: 0] += max(0, used - tracked)
        let kept = min(s.config.headroom_gb * 1024, max(0, s.mem.available_mb))
        let free = max(0, s.mem.available_mb - kept)

        // Most urgent first: nobody, paused (soonest to stop), waiting, in use, pinned.
        items.sort { ($0.lamp.rawValue, -$0.mb) < ($1.lamp.rawValue, -$1.mb) }

        return Reading(mbByLamp: byLamp, keptMB: kept, freeMB: free, totalMB: total,
                       segments: Self.segments(byLamp: byLamp, kept: kept, free: free, total: total), items: items)
    }

    private func item(for r: Resource, lamp: Lamp, _ s: Status) -> Item {
        let grace = Double(s.config.grace_minutes) * 60
        let pause = Double(s.config.pause_minutes ?? 30) * 60
        switch lamp {
        case .paused:
            let stopAt = (r.paused_at ?? Date()).addingTimeInterval(pause)
            return Item(id: r.id, name: r.name, detail: stopAt < Date() ? "Paused, nothing lost, stopping now" : "Paused, nothing lost, stops \(Fmt.rel(stopAt))", mb: r.mem_mb,
                        lamp: lamp, resource: r, actions: [.resume], deadline: stopAt, window: pause)
        case .nobody:
            var detail = "\(Kind.name(r.kind)), nobody claims it"
            var deadline: Date?
            if let gone = r.owner_gone_at, let owner = r.owner {
                // The collector's fast path: what an exited agent left behind.
                let who = agentTitle(status?.sessions?[owner]?.agent)
                let at = (r.orphan_since ?? gone).addingTimeInterval(Double(s.config.owner_gone_grace_minutes ?? 2) * 60)
                detail = "\(who) exited \(Fmt.rel(gone)), left this running"
                deadline = at
            } else if let since = r.orphan_since {
                let at = since.addingTimeInterval(grace)
                deadline = at
                if s.config.dry_run {
                    detail = at < Date() ? "Nobody for \(Fmt.since(since)), would pause now" : "Nobody for \(Fmt.since(since)), would pause \(Fmt.rel(at))"
                } else {
                    detail = at < Date() ? "Nobody for \(Fmt.since(since)), pausing now" : "Nobody for \(Fmt.since(since)), pauses \(Fmt.rel(at))"
                }
            }
            return Item(id: r.id, name: r.name, detail: detail, mb: r.mem_mb, lamp: lamp, resource: r,
                        actions: [.keep, .stop], deadline: deadline, window: grace)
        case .system:
            return Item(id: r.id, name: r.name, detail: "\(Kind.name(r.kind)), pinned by you", mb: r.mem_mb,
                        lamp: lamp, resource: r, actions: [.unpin])
        case .inUse, .waiting:
            let l = r.lease_id.flatMap { s.leases?[$0] }
            let who = l?.agent.capitalized ?? "An agent"
            let evidence = lamp == .waiting ? "no use seen yet" : (l?.evidence ?? l?.purpose ?? "in use")
            return Item(id: r.id, name: r.name, detail: "\(who), \(evidence)", mb: r.mem_mb, lamp: lamp, resource: r,
                        actions: r.lease_id.map { [.release($0)] } ?? [])
        }
    }

    func agentTitle(_ agent: String?) -> String {
        switch agent {
        case "claude": return "Claude Code"
        case "codex": return "Codex"
        case "gemini": return "Gemini CLI"
        case "aider": return "Aider"
        case "opencode": return "OpenCode"
        case "cursor": return "Cursor"
        default: return "Its agent"
        }
    }

    /// One segment per GB, shared out by largest remainder so the segments add up to the whole;
    /// anything of a quarter GB or more gets at least one so it is never invisible.
    static func segments(byLamp: [Lamp: Double], kept: Double, free: Double, total: Double) -> [Segment] {
        let n = max(4, Int((total / 1024).rounded()))
        let order: [(Segment, Double)] = [
            (.lit(.inUse), byLamp[.inUse] ?? 0), (.lit(.waiting), byLamp[.waiting] ?? 0),
            (.lit(.nobody), byLamp[.nobody] ?? 0), (.lit(.paused), byLamp[.paused] ?? 0),
            (.lit(.system), byLamp[.system] ?? 0), (.kept, kept), (.off, free),
        ]
        let perSeg = total / Double(n)
        var counts = order.map { Int(($0.1 / perSeg).rounded(.down)) }
        for i in counts.indices where counts[i] == 0 && order[i].1 >= 256 && i < 5 { counts[i] = 1 }
        var left = n - counts.reduce(0, +)
        if left > 0 {
            let rema = order.indices.sorted { (order[$0].1 / perSeg).truncatingRemainder(dividingBy: 1) > (order[$1].1 / perSeg).truncatingRemainder(dividingBy: 1) }
            for i in rema where left > 0 && order[i].1 > 0 { counts[i] += 1; left -= 1 }
            if left > 0 { counts[6] += left }
        }
        while counts.reduce(0, +) > n {
            // Take back from the largest unlit or system run first.
            if let i = [6, 5, 4, 0, 1].first(where: { counts[$0] > 1 }) { counts[i] -= 1 } else { break }
        }
        return zip(order, counts).flatMap { Array(repeating: $0.0.0, count: $0.1) }
    }
}
