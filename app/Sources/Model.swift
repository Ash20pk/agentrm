import Foundation

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
}

struct Config: Codable {
    var reserve_gb: Double
    var min_free_percent: Int
    var grace_minutes: Int
    var default_ttl_minutes: Int
    var dry_run: Bool
    var never_stop: [String]?
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
}

struct AgentSession: Codable, Identifiable {
    var id: String
    var agent: String
    var cwd: String
    var last_seen: Date
}

struct AgentEvent: Codable, Identifiable {
    var seq: Int
    var time: Date
    var kind: String
    var text: String
    var id: Int { seq }
}

struct Status: Codable {
    var mem: MemStats
    var committed_mb: Double
    var budget_mb: Double
    var config: Config
    var leases: [String: Lease]?
    var resources: [String: Resource]?
    var sessions: [String: AgentSession]?
    var events: [AgentEvent]?
    var last_sweep: Date
}

struct SetupCheck: Codable {
    var binary: String
    var claude_hooks: Bool
    var claude_hooks_current: Bool
    var codex_note: Bool
    var cli_link: String
    var cli_link_dir_on_path: Bool
    var daemon_running: Bool
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
    static func run(_ args: [String]) async throws -> Data {
        try await withCheckedThrowingContinuation { cont in
            DispatchQueue.global(qos: .userInitiated).async {
                let p = Process()
                p.executableURL = URL(fileURLWithPath: path)
                p.arguments = args
                p.environment = ProcessInfo.processInfo.environment.merging(["AGENTRM_AGENT": "app"]) { $1 }
                let out = Pipe(), err = Pipe()
                p.standardOutput = out
                p.standardError = err
                do {
                    try p.run()
                } catch {
                    cont.resume(throwing: error)
                    return
                }
                let data = out.fileHandleForReading.readDataToEndOfFile()
                let errData = err.fileHandleForReading.readDataToEndOfFile()
                p.waitUntilExit()
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
