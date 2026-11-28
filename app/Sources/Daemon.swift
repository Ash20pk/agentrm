import Foundation
import ServiceManagement

/// Runs the daemon as a child of the app, so it lives exactly as long as the app: it starts
/// when AgentRM opens and stops when AgentRM quits. On a quit it gets SIGTERM and thaws
/// whatever it paused before exiting. If the app crashes or is force-quit, the daemon notices
/// its parent is gone (AGENTRM_PARENT_PID) and exits on its own. If the daemon dies while the
/// app is open, it is started again, waiting longer each time it keeps failing.
@MainActor
final class Daemon {
    /// Called whenever the daemon starts or exits.
    var onChange: (() -> Void)?
    /// Why the last run ended, when it ended on an error (its stderr, or its exit status).
    private(set) var lastError: String?

    private var process: Process?
    private var stopping = false
    private var exits: [Date] = []
    private var pendingStart: DispatchWorkItem?

    var running: Bool { process?.isRunning == true }

    /// Where the daemon's stderr goes: it logs to agentrm.log, so this only holds a fatal error.
    private static var errURL: URL {
        FileManager.default.homeDirectoryForCurrentUser.appendingPathComponent(".agentrm/daemon.err")
    }

    func start() {
        pendingStart?.cancel()
        pendingStart = nil
        stopping = false
        guard !running else { return }
        let p = Process()
        p.executableURL = URL(fileURLWithPath: CLI.path)
        p.arguments = ["daemon"]
        var env = ProcessInfo.processInfo.environment
        env["AGENTRM_NOTIFY"] = "app"
        env["AGENTRM_PARENT_PID"] = String(getpid())
        // An app opened from Finder has a bare PATH; the daemon runs docker, brew and xcrun.
        env["PATH"] = "/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin:/Applications/Docker.app/Contents/Resources/bin"
        p.environment = env
        p.standardInput = FileHandle.nullDevice
        p.standardOutput = FileHandle.nullDevice
        try? FileManager.default.createDirectory(at: Self.errURL.deletingLastPathComponent(), withIntermediateDirectories: true)
        FileManager.default.createFile(atPath: Self.errURL.path, contents: nil)
        p.standardError = (try? FileHandle(forWritingTo: Self.errURL)) ?? FileHandle.nullDevice
        p.terminationHandler = { [weak self] proc in
            let status = proc.terminationStatus
            Task { @MainActor in self?.exited(proc, status: status) }
        }
        do {
            try p.run()
            process = p
            log.notice("daemon started, pid \(p.processIdentifier)")
        } catch {
            lastError = "Could not start the daemon: \(error.localizedDescription)"
            log.error("daemon start failed: \(error.localizedDescription, privacy: .public)")
            scheduleRestart()
        }
        onChange?()
    }

    /// Stops the daemon and starts it again: after an update it may still run old code.
    func restart() {
        guard let p = process, p.isRunning else { return start() }
        // The exit handler sees `stopping` false and starts it again at once.
        exits.removeAll()
        p.terminate()
    }

    /// Stops the daemon and waits for it to exit, so it has thawed what it paused before the
    /// app goes. Used on quit, so it blocks; a daemon slower than `wait` finishes on its own.
    func stop(wait: TimeInterval = 10) {
        stopping = true
        pendingStart?.cancel()
        pendingStart = nil
        guard let p = process, p.isRunning else { return }
        p.terminate()
        let deadline = Date().addingTimeInterval(wait)
        while p.isRunning && Date() < deadline { usleep(50_000) }
        if p.isRunning { log.notice("daemon still shutting down after \(wait)s; leaving it to finish") }
    }

    private func exited(_ p: Process, status: Int32) {
        guard p === process else { return }
        process = nil
        let err = (try? String(contentsOf: Self.errURL, encoding: .utf8))?
            .trimmingCharacters(in: .whitespacesAndNewlines) ?? ""
        if stopping {
            log.notice("daemon stopped")
        } else {
            lastError = status == 0 ? nil : (err.isEmpty ? "The daemon exited with status \(status)." : err)
            log.notice("daemon exited (status \(status)): \(err, privacy: .public)")
            scheduleRestart()
        }
        onChange?()
    }

    /// Starts it again: at once the first time, then waiting 2, 4, 8 … up to 60 s while it keeps
    /// exiting (another daemon holding the lock, a broken install) so it does not spin.
    private func scheduleRestart() {
        let now = Date()
        exits = exits.filter { now.timeIntervalSince($0) < 300 } + [now]
        let delay = exits.count <= 1 ? 0 : min(60, pow(2, Double(exits.count - 1)))
        let work = DispatchWorkItem { [weak self] in
            Task { @MainActor in
                guard let self, !self.stopping else { return }
                self.start()
            }
        }
        pendingStart = work
        DispatchQueue.main.asyncAfter(deadline: .now() + delay, execute: work)
    }

    /// Versions up to 0.2 ran the daemon as a launchd login agent that started at login and kept
    /// running after the app quit. Remove it, which also stops it (and it thaws what it paused).
    static func removeLoginAgent() async {
        let agent = SMAppService.agent(plistName: "dev.agentrm.daemon.plist")
        guard agent.status == .enabled || agent.status == .requiresApproval else { return }
        do {
            try await agent.unregister()
            log.notice("removed the old login-agent daemon")
        } catch {
            log.error("could not remove the old login-agent daemon: \(error.localizedDescription, privacy: .public)")
        }
    }
}
