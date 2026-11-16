import Foundation

// Mirrors of the daemon's JSON (internal/api and internal/model). Fields added later are
// optional so an older daemon still decodes.

struct MemStats: Codable {
    var total_mb: Double
    var available_mb: Double
    var compressed_mb: Double
    var wired_mb: Double
    var swap_total_mb: Double
    var swap_used_mb: Double
    var free_percent: Int
    var disk_free_gb: Double?
    var load1: Double?
    var cores: Int?
    var pressure_level: Int?
}

struct Config: Codable {
    var headroom_gb: Double
    var idle_minutes: Int
    var grace_minutes: Int
    var default_ttl_minutes: Int
    var dry_run: Bool
    var pause_minutes: Int?
    var never_stop: [String]?
    var require_app_approval: Bool?
}

struct Lease: Codable, Identifiable {
    var id: String
    var session: String
    var agent: String
    var kind: String
    var mem_gb: Double
    var purpose: String
    var created: Date
    var expires: Date
    var resources: [String]?
    var verified: Bool?
    var evidence: String?
    var last_used: Date?
    var until: Date?
}

struct Resource: Codable, Identifiable {
    var id: String
    var kind: String
    var name: String
    var detail: String
    var mem_mb: Double
    var lease_id: String?
    var orphan_since: Date?
    var pinned: Bool?
    var started_by: String?
    var paused_at: Date?
    var source: String?
    var restart: String?
}

struct AgentSession: Codable, Identifiable {
    var id: String
    var agent: String
    var cwd: String
    var last_seen: Date
    var footprint_mb: Double?
    var priority: Int?
}

struct AgentEvent: Codable, Identifiable {
    var seq: Int
    var time: Date
    var kind: String
    var text: String
    var id: Int { seq }

    /// The resource or approval an event is about: the word after the kind ("PAUSED srv:42 (...)").
    var ref: String? {
        let words = text.split(separator: " ", maxSplits: 2)
        guard words.count >= 2 else { return nil }
        return String(words[1]).trimmingCharacters(in: CharacterSet(charactersIn: ",.:;()"))
    }
}

struct Waiter: Codable {
    var session: String
    var kind: String
    var need_mb: Double
    var since: Date
    var priority: Int?
}

struct Approval: Codable, Identifiable {
    var id: String
    var what: String
    var created: Date
    var expires: Date
    var from: String?
    var state: String
}

struct StoppedItem: Codable, Identifiable {
    var resource: Resource
    var stopped_at: Date
    var why: String
    var by: String?
    var id: String { resource.id + "@" + String(stopped_at.timeIntervalSince1970) }
}

struct SelfUsage: Codable {
    var rss_mb: Double
    var cpu_percent: Double
    var sweep_ms: Double
}

struct Status: Codable {
    var mem: MemStats
    var headroom_mb: Double
    var pending_mb: Double
    var swap_rate_mb_min: Double?
    var waiters: [Waiter]?
    var config: Config
    var leases: [String: Lease]?
    var resources: [String: Resource]?
    var sessions: [String: AgentSession]?
    var events: [AgentEvent]?
    var last_sweep: Date
    var daemon_version: String?
    var sources: [String: String]?
    var stopped: [StoppedItem]?
    var approvals: [String: Approval]?
    var notes: [String]?
    var selfUsage: SelfUsage?

    enum CodingKeys: String, CodingKey {
        case mem, headroom_mb, pending_mb, swap_rate_mb_min, waiters, config, leases, resources, sessions,
             events, last_sweep, daemon_version, sources, stopped, approvals, notes
        case selfUsage = "self"
    }
}

struct SetupCheck: Codable {
    var binary: String
    var claude_hooks: Bool
    var claude_hooks_current: Bool
    var codex_note: Bool
    var cli_link: String
    var cli_link_dir_on_path: Bool
    var daemon_running: Bool
    var shim: Bool?
    var problems: [String]?
}

struct Decision: Codable, Identifiable {
    var id: String
    var time: Date
    var action: String
    var resource: String
    var name: String
    var mem_mb: Double
    var why: String
    var dry_run: Bool
    var verdict: String?
}

struct ReviewResp: Codable {
    var decisions: [Decision]?
    var reviewed: Int
    var wrong: Int
    var false_rate: Double
}

enum CLI {
    /// The Go helper bundled inside the app.
    static var path: String {
        Bundle.main.bundleURL.appendingPathComponent("Contents/Helpers/agentrm").path
    }

    struct Failure: Error, LocalizedError {
        let message: String
        var errorDescription: String? { message }
    }

    /// Runs the helper off the main thread and returns stdout.
    static func run(_ args: [String], timeout: TimeInterval = 15) async throws -> Data {
        try await withCheckedThrowingContinuation { cont in
            DispatchQueue.global(qos: .userInitiated).async {
                let p = Process()
                p.executableURL = URL(fileURLWithPath: path)
                p.arguments = args
                let out = Pipe(), err = Pipe()
                p.standardOutput = out
                p.standardError = err
                do {
                    try p.run()
                } catch {
                    cont.resume(throwing: error)
                    return
                }
                let killer = DispatchWorkItem { if p.isRunning { p.terminate() } }
                DispatchQueue.global().asyncAfter(deadline: .now() + timeout, execute: killer)
                let data = out.fileHandleForReading.readDataToEndOfFile()
                let errData = err.fileHandleForReading.readDataToEndOfFile()
                p.waitUntilExit()
                killer.cancel()
                if p.terminationStatus == 0 {
                    cont.resume(returning: data)
                } else {
                    let msg = String(data: errData, encoding: .utf8)?
                        .trimmingCharacters(in: .whitespacesAndNewlines) ?? "agentrm failed"
                    cont.resume(throwing: Failure(message: msg.replacingOccurrences(of: "agentrm: ", with: "")))
                }
            }
        }
    }

    static func text(_ args: [String]) async -> String {
        guard let d = try? await run(args) else { return "" }
        return String(data: d, encoding: .utf8) ?? ""
    }

    static let decoder: JSONDecoder = {
        let d = JSONDecoder()
        let frac = ISO8601DateFormatter()
        frac.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
        let plain = ISO8601DateFormatter()
        plain.formatOptions = [.withInternetDateTime]
        d.dateDecodingStrategy = .custom { dec in
            let s = try dec.singleValueContainer().decode(String.self)
            if let date = frac.date(from: s) ?? plain.date(from: s) { return date }
            // Go writes nanoseconds; ISO8601DateFormatter wants at most milliseconds.
            if let dot = s.firstIndex(of: "."), let tz = s[dot...].firstIndex(where: { $0 == "Z" || $0 == "+" || $0 == "-" }) {
                let trimmed = String(s[..<dot]) + "." + s[s.index(after: dot)..<tz].prefix(3) + s[tz...]
                if let date = frac.date(from: trimmed) { return date }
            }
            return Date.distantPast
        }
        return d
    }()
}

extension Double {
    var gb: String { String(format: "%.1f GB", self / 1024) }
}
