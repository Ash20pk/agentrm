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
                let material = try VaultKeychain.read(ask.account)
                let body = try JSONEncoder().encode(["material": material])
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
        let isNew = !VaultKeychain.has(acct.name)
        if let material {
            try VaultKeychain.save(acct.name, material)
        } else if let from = renamedFrom {
            try VaultKeychain.save(acct.name, try VaultKeychain.read(from))
        }
        var list = vaultAccounts.filter { $0.name != (old ?? acct.name) && $0.name != acct.name }
        list.append(acct)
        do {
            try await syncVaultAccounts(list)
        } catch {
            if isNew && (material != nil || renamedFrom != nil) { VaultKeychain.delete(acct.name) }
            throw error
        }
        if let from = renamedFrom { VaultKeychain.delete(from) }
    }

    func deleteVaultAccount(_ name: String) async throws {
        guard await VaultAuth.confirm("remove the vault account \(name)") else {
            throw CLI.Failure(message: "Not removed: authentication was cancelled.")
        }
        try await syncVaultAccounts(vaultAccounts.filter { $0.name != name })
        VaultKeychain.delete(name)
    }

    private func syncVaultAccounts(_ list: [VaultAccount]) async throws {
        let data = try JSONEncoder().encode(list)
        _ = try await CLI.run(["vault", "accounts-set"], input: data)
        await refresh()
    }

    func revokeGrant(_ g: VaultGrant) { act(["vault", "revoke", g.id]) }

    static func agentName(_ agent: String) -> String {
        switch agent {
        case "claude": return "Claude Code"
        case "codex": return "Codex"
        case "you": return "you"
        default: return agent.capitalized
        }
    }
}
