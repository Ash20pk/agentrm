import Foundation
import LocalAuthentication
import Security

/// The vault's secrets, one Keychain item per account, created by this app. The login keychain
/// trusts the app that made an item: AgentRM reads them without a prompt, and anything else (an
/// agent running `security find-generic-password`) makes macOS ask you first. A secret leaves
/// the Keychain only when you approve a request with Touch ID, on its way to the daemon, which
/// keeps what it mints in memory for as long as the grant lasts.
enum VaultKeychain {
    static let service = "dev.agentrm.vault"

    struct Failure: Error, LocalizedError {
        let status: OSStatus
        let what: String
        var errorDescription: String? {
            let msg = SecCopyErrorMessageString(status, nil) as String? ?? "error \(status)"
            return "\(what): \(msg)"
        }
    }

    private static func query(_ account: String) -> [String: Any] {
        [kSecClass as String: kSecClassGenericPassword, kSecAttrService as String: service, kSecAttrAccount as String: account]
    }

    static func save(_ account: String, _ material: [String: String]) throws {
        let data = try JSONEncoder().encode(material)
        let update = SecItemUpdate(query(account) as CFDictionary, [kSecValueData as String: data] as CFDictionary)
        if update == errSecSuccess { return }
        guard update == errSecItemNotFound else { throw Failure(status: update, what: "Could not update \(account) in the Keychain") }
        var add = query(account)
        add[kSecValueData as String] = data
        add[kSecAttrLabel as String] = "AgentRM vault: \(account)"
        add[kSecAttrDescription as String] = "Handed to agents only when you approve, with Touch ID"
        let status = SecItemAdd(add as CFDictionary, nil)
        guard status == errSecSuccess else { throw Failure(status: status, what: "Could not save \(account) in the Keychain") }
    }

    static func read(_ account: String) throws -> [String: String] {
        var q = query(account)
        q[kSecReturnData as String] = true
        q[kSecMatchLimit as String] = kSecMatchLimitOne
        var out: CFTypeRef?
        let status = SecItemCopyMatching(q as CFDictionary, &out)
        guard status == errSecSuccess, let data = out as? Data else {
            throw Failure(status: status, what: "No secret for \(account) in the Keychain (add it under Vault)")
        }
        return try JSONDecoder().decode([String: String].self, from: data)
    }

    /// Whether a secret is saved, without reading it.
    static func has(_ account: String) -> Bool {
        var q = query(account)
        q[kSecReturnAttributes as String] = true
        return SecItemCopyMatching(q as CFDictionary, nil) == errSecSuccess
    }

    static func delete(_ account: String) {
        SecItemDelete(query(account) as CFDictionary)
    }

    /// The account's settings are kept in its item, beside the secret, as you saved them. The
    /// daemon's copy lives in a file anything running as you can edit; the daemon uses the
    /// secret only if its copy matches this one.
    static let settingsKey = "_account"

    /// The item's secret and the settings saved with it.
    static func readAll(_ account: String) throws -> (material: [String: String], settings: VaultAccount?) {
        var m = try read(account)
        let raw = m.removeValue(forKey: settingsKey)
        let settings = raw.flatMap { try? JSONDecoder().decode(VaultAccount.self, from: Data($0.utf8)) }
        return (m, settings)
    }

    static func saveAll(_ account: String, material: [String: String], settings: VaultAccount) throws {
        var m = material
        m[settingsKey] = String(decoding: try JSONEncoder().encode(settings), as: UTF8.self)
        try save(account, m)
    }
}

enum VaultAuth {
    /// Touch ID, or your Mac password where there is none. False if cancelled or failed.
    static func confirm(_ reason: String) async -> Bool {
        let ctx = LAContext()
        var err: NSError?
        guard ctx.canEvaluatePolicy(.deviceOwnerAuthentication, error: &err) else { return false }
        return (try? await ctx.evaluatePolicy(.deviceOwnerAuthentication, localizedReason: reason)) ?? false
    }
}

extension Store {
    var vaultAccounts: [VaultAccount] { (status?.vault_accounts ?? []).sorted { $0.name < $1.name } }
    var vaultGrants: [VaultGrant] { status?.vault_grants ?? [] }

    /// Approves a vault request: Touch ID, then the account's secret from the Keychain to the
    /// daemon on the helper's stdin.
    func approveVault(_ a: Approval, minutes: Int? = nil) {
        guard let ask = a.vault else { return }
        Task {
            let reason = "give \(Self.agentName(ask.agent)) access to \(ask.account) for \(minutes ?? ask.minutes) minutes"
            guard await VaultAuth.confirm(reason) else {
                lastActionError = "Not approved: authentication was cancelled. The request stays open until it expires."
                return
            }
            do {
                let (material, settings) = try VaultKeychain.readAll(ask.account)
                guard let settings, settings.name == ask.account, settings.kind == ask.kind else {
                    throw CLI.Failure(message: "\(ask.account) has no saved settings in the Keychain: open Vault, edit it and save it again.")
                }
                // The request as shown to you: the daemon gives nothing unless it holds this one.
                struct Answer: Encodable { let material: [String: String]; let account: VaultAccount; let shown: VaultAsk }
                let body = try JSONEncoder().encode(Answer(material: material, account: settings, shown: ask))
                var args = ["vault", "answer", a.id, "--approve"]
                if let minutes { args += ["--for", String(minutes)] }
                _ = try await CLI.run(args, timeout: 50, input: body)
                lastActionError = nil
            } catch {
                lastActionError = "Vault: \(error.localizedDescription)"
            }
            await refresh()
        }
    }

    /// Saves an account: its secret (when given) to the Keychain, its settings to the daemon.
    /// Changing an account ends its live grants. Touch ID first: an agent that could click
    /// through the app could otherwise point an API key's account at its own server.
    func saveVaultAccount(_ acct: VaultAccount, material: [String: String]?, replacing old: String?) async throws {
        guard await VaultAuth.confirm("save the vault account \(acct.name)") else {
            throw CLI.Failure(message: "Not saved: authentication was cancelled.")
        }
        let renamedFrom = old.flatMap { $0 == acct.name ? nil : $0 }
        // What the Keychain held, to put back if the daemon refuses the account.
        let previous = try? VaultKeychain.read(acct.name)
        // The secret: the new one, or the one saved (under the old name, if renamed). The
        // settings are saved with it every time, so that the item always holds the account as
        // you last saved it.
        let secret = try material ?? VaultKeychain.readAll(renamedFrom ?? acct.name).material
        try VaultKeychain.saveAll(acct.name, material: secret, settings: acct)
        var list = vaultAccounts.filter { $0.name != (old ?? acct.name) && $0.name != acct.name }.map { savedSettings($0.name) ?? $0 }
        list.append(acct)
        do {
            try await syncVaultAccounts(list)
        } catch {
            if let previous { try? VaultKeychain.save(acct.name, previous) } else { VaultKeychain.delete(acct.name) }
            throw error
        }
        if let from = renamedFrom { VaultKeychain.delete(from) }
    }

    func deleteVaultAccount(_ name: String) async throws {
        guard await VaultAuth.confirm("remove the vault account \(name)") else {
            throw CLI.Failure(message: "Not removed: authentication was cancelled.")
        }
        try await syncVaultAccounts(vaultAccounts.filter { $0.name != name }.map { savedSettings($0.name) ?? $0 })
        VaultKeychain.delete(name)
    }

    private func syncVaultAccounts(_ list: [VaultAccount]) async throws {
        let data = try JSONEncoder().encode(list)
        _ = try await CLI.run(["vault", "accounts-set"], input: data)
        await refresh()
    }

    func revokeGrant(_ g: VaultGrant) { act(["vault", "revoke", g.id]) }

    /// The account as you saved it in the app, from its Keychain item (nil before it was saved
    /// with its settings). Preferred over the daemon's copy, which lives in an editable file.
    func savedSettings(_ name: String) -> VaultAccount? { try? VaultKeychain.readAll(name).settings }

    /// Whether the daemon's copy of an account differs from the one saved in the app.
    func vaultTampered(_ a: VaultAccount) -> Bool {
        guard let saved = savedSettings(a.name) else { return false }
        return saved.normalized != a.normalized
    }

    static func agentName(_ agent: String) -> String {
        switch agent {
        case "claude": return "Claude Code"
        case "codex": return "Codex"
        case "you": return "you"
        default: return agent.capitalized
        }
    }
}

extension VaultAccount {
    /// Empty lists, maps and strings as absent, as the daemon writes them.
    var normalized: VaultAccount {
        var a = self
        func n(_ s: String?) -> String? { (s ?? "").isEmpty ? nil : s }
        a.note = n(a.note); a.app_id = n(a.app_id); a.installation_id = n(a.installation_id); a.api_url = n(a.api_url)
        a.header = n(a.header); a.prefix = n(a.prefix); a.key_env = n(a.key_env); a.base_env = n(a.base_env)
        a.region = n(a.region); a.external_id = n(a.external_id); a.username = n(a.username); a.url = n(a.url)
        a.user_env = n(a.user_env); a.pass_env = n(a.pass_env)
        if (a.scopes ?? []).isEmpty { a.scopes = nil }
        if (a.repos ?? []).isEmpty { a.repos = nil }
        if (a.permissions ?? [:]).isEmpty { a.permissions = nil }
        if a.max_minutes == 0 { a.max_minutes = nil }
        return a
    }
}
