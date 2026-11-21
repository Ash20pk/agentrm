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
    var owner_gone_grace_minutes: Int?
    var auto_stop: [String: Bool]?
    var max_swap_mb_per_min: Double?
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
    var expected_end: Date?
    var likely_done: Bool?
    var branch: String?
    var task_done_why: String?
    var backfill: Bool?
    /// What a lease with nothing discoverable behind it (a build) uses now, measured.
    var used_mb: Double?
    /// The holder chose the size itself (otherwise AgentRM's estimate, or inferred).
    var declared: Bool?
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
    var unseen: Date?
    var source: String?
    var restart: String?
    var owner: String?
    var owner_gone_at: Date?
    var first_seen: Date?
    var ask_only: Bool?
    var pid: Int?
    /// What the daemon will do next if nothing changes ("pause" or "stop"), when, and since when
    /// it has been waiting; next_dry: in dry run, it only would. Nil: nothing is coming.
    var next_step: String?
    var next_at: Date?
    var next_from: Date?
    var next_dry: Bool?

    /// Comes back by itself once stopped (Docker's VM, Xcode's helpers): not leased, decided
    /// about on its own path.
    var selfRestarting: Bool { kind == "docker-vm" || kind == "xcode-helper" }
}

struct AgentSession: Codable, Identifiable {
    var id: String
    var agent: String
    var cwd: String
    var last_seen: Date
    var footprint_mb: Double?
    var priority: Int?
    var ended_at: Date?
    var end_reason: String?
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
    var path: String?
    /// Set for an agent asking the vault for access to an account (approved with Touch ID).
    var vault: VaultAsk?
    var error: String?
}

/// An account in the vault, without its secret (that is in the Keychain). Mirrors
/// internal/model/vault.go; nil fields are left out when sent back.
struct VaultAccount: Codable, Identifiable, Hashable {
    var name: String
    var kind: String
    var note: String?
    var max_minutes: Int?
    var scopes: [String]?
    var app_id: String?
    var installation_id: String?
    var repos: [String]?
    var permissions: [String: String]?
    var api_url: String?
    var header: String?
    var prefix: String?
    var raw_key: Bool?
    var key_env: String?
    var base_env: String?
    var region: String?
    var external_id: String?
    var username: String?
    var url: String?
    var user_env: String?
    var pass_env: String?
    var id: String { name }
}

/// A well-known API an API key account can be filled in for: `agentrm vault templates --json`,
/// from internal/vault/templates.json.
struct VaultTemplate: Codable, Identifiable, Hashable {
    var id: String
    var name: String
    var category: String
    var api_url: String
    var header: String?
    var prefix: String?
    var raw_key: Bool?
    var key_env: String
    var base_env: String
    var key_url: String?
    var note: String?
}

/// What an agent asked the vault for.
struct VaultAsk: Codable, Hashable {
    var account: String
    var kind: String
    var scope: String
    var repos: [String]?
    var minutes: Int
    var purpose: String
    var reveal: Bool?
    var session: String
    var agent: String
    var repo: String?
}

/// Access given (a record: never the secret).
struct VaultGrant: Codable, Identifiable {
    var id: String
    var account: String
    var kind: String
    var scope: String
    var repos: [String]?
    var minutes: Int
    var purpose: String
    var reveal: Bool?
    var session: String
    var agent: String
    var repo: String?
    var created: Date
    var expires: Date
    var ended: Date?
    var end_reason: String?
    var uses: Int?
    var last_used: Date?
    var note: String?
    var live: Bool { ended == nil && expires > Date() }
}

/// A process whose memory keeps climbing with nothing to explain it (status "leaks"; absent
/// from older daemons). Every field but the id is optional so a changed daemon still decodes.
struct Leak: Codable, Identifiable {
    var id: String
    var pid: Int?
    var name: String?
    var resource: String?
    var since: Date?
    var start_mb: Double?
    var now_mb: Double?
    var rate_mb_per_hour: Double?
    var action: String?
    var approval: String?
    var dismissed: Bool?
}

struct StoppedItem: Codable, Identifiable {
    var resource: Resource
    var stopped_at: Date
    var why: String
    var by: String?
    /// What the stop gave back (part of what Docker counted for a container). Older records
    /// lack it: then what it held.
    var freed_mb: Double?
    var freed: Double { freed_mb ?? resource.mem_mb }
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
    var fail_open: String?
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
    var prediction: String?
    var trust_levels: [TrustLevel]?
    var leaks: [Leak]?
    var vault_accounts: [VaultAccount]?
    var vault_grants: [VaultGrant]?
    var freed_today_mb: Double?
    var freed_week_mb: Double?

    /// The swap-out rate above which the daemon refuses work (its config, default 300 MB/min).
    /// 0 or less: the daemon does not refuse for swapping, so neither does the app warn.
    var swapLimit: Double {
        let l = config.max_swap_mb_per_min ?? 300
        return l > 0 ? l : .infinity
    }

    enum CodingKeys: String, CodingKey {
        case mem, headroom_mb, pending_mb, swap_rate_mb_min, waiters, config, leases, resources, sessions,
             events, last_sweep, daemon_version, sources, stopped, approvals, notes, prediction, trust_levels, leaks, fail_open,
             vault_accounts, vault_grants, freed_today_mb, freed_week_mb
        case selfUsage = "self"
    }
}

/// One auto-stop level and the evidence for turning it on.
struct TrustLevel: Codable, Identifiable {
    var category: String
    var title: String
    var detail: String
    var on: Bool
    var decided: Int
    var reviewed: Int
    var wrong: Int
    var days: Int
    var eligible: Bool
    var blocked: String?
    var evidence: String
    var id: String { category }
}

/// What AgentRM did over a period (agentrm receipt --json). Durations are nanoseconds.
struct Receipt: Codable {
    var since: Date
    var until: Date
    var sessions_ended: Int
    var leftovers: Int
    var paused: Int
    var stopped: Int
    var freed_gb: Double
    var would_stop: Int
    var would_free_gb: Double
    var wrong: Int
    var restored: Int
    var queued: Int
    var wait_mean: Int64
    var wait_max: Int64
    var misreports: Int
    var text: String
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
    /// `input` is written to the helper's stdin and closed: the way a secret reaches it, never argv
    /// (any process can read another's arguments).
    static func run(_ args: [String], timeout: TimeInterval = 15, input: Data? = nil) async throws -> Data {
        try await withCheckedThrowingContinuation { cont in
            DispatchQueue.global(qos: .userInitiated).async {
                let p = Process()
                p.executableURL = URL(fileURLWithPath: path)
                p.arguments = args
                let out = Pipe(), err = Pipe()
                p.standardOutput = out
                p.standardError = err
                let inPipe = input.map { _ in Pipe() }
                if let inPipe { p.standardInput = inPipe }
                do {
                    try p.run()
                } catch {
                    cont.resume(throwing: error)
                    return
                }
                if let inPipe, let input {
                    inPipe.fileHandleForWriting.write(input)
                    try? inPipe.fileHandleForWriting.close()
                }
                let killer = DispatchWorkItem { if p.isRunning { p.terminate() } }
                DispatchQueue.global().asyncAfter(deadline: .now() + timeout, execute: killer)
                // Drain both pipes at once: read one to the end first and a child that fills the
                // other's buffer (64 KB) blocks on it forever.
                let errBox = DataBox()
                let errDone = DispatchSemaphore(value: 0)
                DispatchQueue.global(qos: .userInitiated).async {
                    errBox.data = err.fileHandleForReading.readDataToEndOfFile()
                    errDone.signal()
                }
                let data = out.fileHandleForReading.readDataToEndOfFile()
                errDone.wait()
                let errData = errBox.data
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

    /// Holds what one reader thread read, handed over through a semaphore.
    private final class DataBox: @unchecked Sendable { var data = Data() }

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
