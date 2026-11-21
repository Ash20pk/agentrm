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
    var step: String = ""
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
        // Docker's VM and Xcode's helpers belong to no one by design: grey, not "nobody".
        if r.pinned == true || r.selfRestarting { return .system }
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
        // Leases nothing discoverable runs behind (a build in progress): shown by what they use,
        // measured; what they asked for and have not used yet is not memory in use.
        let backed = Set(resources.compactMap(\.lease_id))
        for l in leases where !backed.contains(l.id) {
            let lamp: Lamp = l.verified == false ? .waiting : .inUse
            let mb = l.used_mb ?? 0
            // What a Docker or Gradle build adds to Docker's VM or a Gradle daemon is in their rows
            // already: the meter counts only the rest (the processes it started itself).
            byLamp[lamp, default: 0] += max(0, mb - (l.inside_mb ?? 0))
            let evidence = l.verified == false ? "no use seen yet" : (l.evidence ?? "in use")
            var detail = "\(l.agent.capitalized), \(evidence)"
            if l.declared == true {
                detail += ", asked for \(Fmt.size(l.mem_gb * 1024))"
            } else if !l.purpose.hasPrefix("inferred") {
                detail += ", charged \(Fmt.size(l.mem_gb * 1024))"
            }
            if let inside = l.inside_mb, inside > 0 {
                // Docker's VM grows for others too: its growth is only taken off what is held back.
                detail += l.kind == "gradle" ? ", \(Fmt.size(inside)) of it in its Gradle daemon"
                    : ", \(Fmt.size(inside)) of it is Docker's VM growing since"
            }
            items.append(Item(id: l.id, name: l.purpose.isEmpty ? Kind.name(l.kind) : l.purpose,
                              detail: detail, mb: mb, lamp: lamp, resource: nil, actions: [.release(l.id)]))
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
        // The countdown is the daemon's: what it will do next and when (nil: nothing is coming).
        let next = r.next_at
        let window = max(1, (r.next_at ?? Date()).timeIntervalSince(r.next_from ?? r.next_at ?? Date()))
        let would = r.next_dry == true
        let step = r.next_step ?? ""
        if let unseen = r.unseen {
            // Its source stopped answering: this is the last view, not a live one, and nothing is
            // done to it (or counted on from it) until it is seen again.
            let src = r.source == "docker" ? "Docker" : (r.source ?? "Its source")
            return Item(id: r.id, name: r.name, detail: "\(src) not answering since \(Fmt.rel(unseen)), may be gone",
                        mb: r.mem_mb, lamp: lamp, resource: r, actions: [])
        }
        switch lamp {
        case .paused:
            var detail = "Paused, nothing lost"
            if step == "stop", let at = next {
                detail += at < Date() ? (would ? ", would stop now" : ", stopping now") : (would ? ", would stop \(Fmt.rel(at))" : ", stops \(Fmt.rel(at))")
            }
            return Item(id: r.id, name: r.name, detail: detail, mb: r.mem_mb, lamp: lamp, resource: r, actions: [.resume],
                        deadline: step == "stop" && !would ? next : nil, window: window, step: step)
        case .nobody:
            var detail = "\(Kind.name(r.kind)), nobody claims it"
            if let gone = r.owner_gone_at, let owner = r.owner {
                // The collector's fast path: what an exited agent left behind.
                let who = agentTitle(status?.sessions?[owner]?.agent)
                detail = "\(who) exited \(Fmt.rel(gone)), left this running"
            } else if let since = r.orphan_since {
                detail = "Nobody for \(Fmt.since(since))"
            }
            if r.started_by == "you" {
                detail += ". You started it: AgentRM leaves it to you"
            } else if step != "", let at = next {
                let verb = step // "pause", "stop" or "restart"
                let ing = ["pause": "pausing", "stop": "stopping", "restart": "restarting"][step] ?? step
                if would {
                    detail += at < Date() ? ", would \(verb) now" : ", would \(verb) \(Fmt.rel(at))"
                } else {
                    detail += at < Date() ? ", \(ing) now" : ", \(verb)s \(Fmt.rel(at))"
                }
            } else if r.orphan_since != nil && (r.kind == "vm" || r.ask_only == true) {
                // Never stopped on AgentRM's own (a VM, an app): yours to stop.
                detail += ". AgentRM will not stop it: stop it yourself if you are done"
            }
            return Item(id: r.id, name: r.name, detail: detail, mb: r.mem_mb, lamp: lamp, resource: r,
                        actions: [.keep, .stop], deadline: step != "" && !would ? next : nil, window: window, step: step)
        case .system:
            if r.selfRestarting {
                // Nobody's by design; it comes back by itself once stopped. Its countdown is the
                // daemon's too (when its own auto-stop level is on).
                var detail = r.detail.isEmpty ? Kind.name(r.kind) : r.detail
                if r.pinned == true { detail += ", pinned by you" }
                if r.pinned != true, step == "stop", let at = next {
                    let verb = r.kind == "docker-vm" ? "restart" : "end"
                    detail += would ? ", would \(verb) \(at < Date() ? "now" : Fmt.rel(at))" : ", \(verb)s \(at < Date() ? "now" : Fmt.rel(at))"
                }
                return Item(id: r.id, name: r.name, detail: detail, mb: r.mem_mb, lamp: lamp, resource: r,
                            // Restart offered only when the daemon would consider it (nothing runs in
                            // it): otherwise it is refused.
                            actions: r.pinned == true ? [.unpin] : (r.orphan_since != nil ? [.stop] : []),
                            deadline: step == "stop" && !would && r.pinned != true ? next : nil, window: window, step: step)
            }
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
        // A small item bumped to one segment has had its share: it takes no remainder as well.
        var bumped = Set<Int>()
        for i in counts.indices where counts[i] == 0 && order[i].1 >= 256 && i < 5 { counts[i] = 1; bumped.insert(i) }
        var left = n - counts.reduce(0, +)
        if left > 0 {
            let rema = order.indices.filter { !bumped.contains($0) }
                .sorted { (order[$0].1 / perSeg).truncatingRemainder(dividingBy: 1) > (order[$1].1 / perSeg).truncatingRemainder(dividingBy: 1) }
            for i in rema where left > 0 && order[i].1 > 0 { counts[i] += 1; left -= 1 }
            if left > 0 { counts[6] += left }
        }
        while counts.reduce(0, +) > n {
            // Take back from the largest unlit or system run first, then from any run of more
            // than one, then from the smallest lit run: the count always ends at n.
            if let i = [6, 5, 4, 0, 1, 2, 3].first(where: { counts[$0] > 1 }) {
                counts[i] -= 1
            } else if let i = counts.indices.filter({ counts[$0] > 0 }).min(by: { order[$0].1 < order[$1].1 }) {
                counts[i] -= 1
            } else { break }
        }
        return zip(order, counts).flatMap { Array(repeating: $0.0.0, count: $0.1) }
    }
}
