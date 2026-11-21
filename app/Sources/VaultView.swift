import AppKit
import SwiftUI

/// An agent asking the vault for access, in the menu: who, what, for how long, why, and what
/// approving cannot undo. Approving asks for Touch ID.
struct VaultApprovalCard: View {
    @EnvironmentObject var store: Store
    let approval: Approval
    let ask: VaultAsk

    var body: some View {
        VStack(alignment: .leading, spacing: 8) {
            LampLabel(lamp: Lamp.waiting.color, text: "Vault request, expires \(Fmt.rel(approval.expires))")
            Text("\(Store.agentName(ask.agent)) wants \(ask.account)")
                .font(Face.body(17, .semibold)).fixedSize(horizontal: false, vertical: true)
            Text("“\(ask.purpose)”").font(Face.body(13)).italic().fixedSize(horizontal: false, vertical: true)
            VStack(alignment: .leading, spacing: 3) {
                fact("Gives", VaultText.gives(ask, account: account))
                fact("For", "\(ask.minutes) min")
                fact("Session", "\(ask.session.prefix(12))" + (ask.repo.map { " in \($0)" } ?? ""))
                if ask.kind == "apikey", let url = account?.api_url { fact("Sent to", url) }
            }
            if ask.reveal == true {
                warn("It asked to SEE the secret, not only use it: what it is shown goes into its transcript.")
            }
            if ask.kind != "apikey" { warn(VaultText.protection(ask.kind)) }
            HStack(spacing: 8) {
                Spacer()
                Button("Deny") { store.answer(approval.id, approve: false) }.buttonStyle(.key)
                Button("Approve · Touch ID") { store.approveVault(approval) }
                    .buttonStyle(.key(.amber)).keyboardShortcut(.defaultAction)
            }
        }
        .padding(12)
        .panel(Ink.amberPanel, stroke: Color(hex: 0x8A6A12))
        .shadow(color: Lamp.waiting.color.opacity(0.12), radius: 12)
    }

    // As saved in the app: the daemon's copy could have been edited.
    private var account: VaultAccount? { store.savedSettings(ask.account) ?? store.vaultAccounts.first { $0.name == ask.account } }

    private func fact(_ k: String, _ v: String) -> some View {
        HStack(alignment: .firstTextBaseline, spacing: 8) {
            Text(k.uppercased()).font(Face.mono(10, .semibold)).foregroundStyle(Ink.label).frame(width: 58, alignment: .leading)
            Text(v).font(Face.mono(12)).foregroundStyle(Color(hex: 0xC9C0A8)).fixedSize(horizontal: false, vertical: true)
        }
    }

    private func warn(_ s: String) -> some View {
        Text(s).font(Face.body(12, .semibold)).foregroundStyle(Color(hex: 0xFF8A8D)).fixedSize(horizontal: false, vertical: true)
    }
}

enum VaultText {
    /// In order of how well they keep the secret from the agent: an API key never reaches it.
    static let kinds: [(id: String, name: String)] = [("apikey", "API key"), ("github", "GitHub App"), ("aws", "AWS role"), ("password", "Login")]
    static func kindName(_ k: String) -> String { kinds.first { $0.id == k }?.name ?? k }

    /// What the vault can and cannot keep from an agent given this kind of account.
    static func protection(_ k: String) -> String {
        switch k {
        case "apikey": return "The agent never holds the key: AgentRM's proxy adds it. Revoking stops it at once."
        case "github": return "The agent holds a short-lived token it could print or copy while the grant lasts. Revoking ends it at GitHub."
        case "aws": return "The agent holds session keys it could print or copy. They work until they expire (15 minutes at least), revoked or not."
        case "password": return "The agent sees the password and can keep it. Use only logins you could change in a minute."
        default: return ""
        }
    }

    static func gives(_ ask: VaultAsk, account: VaultAccount?) -> String {
        switch ask.kind {
        case "github":
            let repos = (ask.repos ?? []).isEmpty ? "every repo the app can reach" : (ask.repos ?? []).joined(separator: ", ")
            return "\(ask.scope.replacingOccurrences(of: ",", with: ", ")) on \(repos)"
        case "apikey": return ask.scope == "read" ? "read-only (GET) use of the API, through AgentRM" : "full use of the API, through AgentRM (never the key)"
        case "aws": return "the role \(ask.scope)" // the whole ARN: the account is in it
        case "password": return "the login" + (account?.username.map { " as \($0)" } ?? "")
        default: return ask.scope
        }
    }

    static func summary(_ a: VaultAccount) -> String {
        switch a.kind {
        case "github":
            let perms = (a.permissions ?? [:]).isEmpty ? "the app's permissions" : (a.permissions ?? [:]).sorted { $0.key < $1.key }.map { "\($0.key):\($0.value)" }.joined(separator: ", ")
            let repos = (a.repos ?? []).isEmpty ? "all repos" : (a.repos ?? []).joined(separator: ", ")
            return "App \(a.app_id ?? "?") · up to \(perms) · \(repos)"
        case "apikey":
            let ro = (a.scopes ?? []) == ["read"] ? " · read-only" : ""
            return "\(a.api_url ?? "") · \(a.key_env ?? "AGENTRM_API_KEY")\(ro)"
        case "aws": return "\(a.region ?? "us-east-1") · " + (a.scopes ?? []).map { String($0.split(separator: "/").last ?? "") }.joined(separator: ", ")
        case "password": return [a.username, a.url].compactMap { $0 }.joined(separator: " · ")
        default: return ""
        }
    }
}

/// The Vault tab: the accounts agents may ask for, and the access given.
struct VaultTab: View {
    @EnvironmentObject var store: Store
    @State private var editing: VaultEdit?
    @State private var error: String?

    struct VaultEdit: Identifiable {
        var account: VaultAccount
        var isNew: Bool
        var id: String { (isNew ? "new:" : "") + account.name + account.kind }
    }

    var body: some View {
        ScrollView {
            VStack(alignment: .leading, spacing: 14) {
                Text("Agents ask for an account with `agentrm vault run`, say why, and wait. You approve with Touch ID; they get access that expires on its own (30 minutes unless they ask for less or more). The secrets stay in your Keychain: the daemon holds what it hands out in memory only, and only until the grant ends.")
                    .font(Face.body(13)).foregroundStyle(Ink.dim).fixedSize(horizontal: false, vertical: true)
                if let error { Text(error).font(Face.body(12, .semibold)).foregroundStyle(Color(hex: 0xFF8A8D)) }
                accounts
                grants
            }
            .padding(22)
        }
        .sheet(item: $editing) { e in
            VaultAccountEditor(original: e.isNew ? nil : e.account, account: e.account) { editing = nil }
                .environmentObject(store)
        }
    }

    private func add(_ kind: String) { editing = VaultEdit(account: VaultAccountEditor.blank(kind), isNew: true) }

    /// No accounts yet: start with an API key, the kind whose secret the agent never sees and
    /// the quickest to set up.
    private var firstRun: some View {
        VStack(alignment: .leading, spacing: 10) {
            Text("Start with an API key").font(Face.body(15, .semibold))
            Text("Pick the API (OpenAI, Anthropic, Gemini, Stripe and 50 more, or your own) and paste the key. Agents get a token for AgentRM's local proxy instead of the key, so they never see it, and revoking stops them at once.")
                .font(Face.body(13)).foregroundStyle(Ink.dim).fixedSize(horizontal: false, vertical: true)
            HStack(spacing: 8) {
                Button("Add an API key") { add("apikey") }.buttonStyle(.key(.amber))
                Spacer()
            }
            Text("GitHub Apps, AWS roles and logins work too, under Add account. With those the agent holds the credential while the grant lasts, and could print it.")
                .font(Face.body(12)).foregroundStyle(Ink.label).fixedSize(horizontal: false, vertical: true)
        }
        .padding(14).frame(maxWidth: .infinity, alignment: .leading)
    }

    private var accounts: some View {
        VStack(alignment: .leading, spacing: 8) {
            HStack {
                Text("ACCOUNTS").font(Face.mono(11, .semibold)).tracking(1).foregroundStyle(Ink.label)
                Spacer()
                Menu("Add account") {
                    ForEach(VaultText.kinds, id: \.id) { k in
                        Button(k.id == "apikey" ? "API key (safest)" : k.name) { add(k.id) }
                    }
                }
                .menuStyle(.borderlessButton).fixedSize()
            }
            VStack(spacing: 0) {
                if store.vaultAccounts.isEmpty { firstRun }
                ForEach(store.vaultAccounts) { a in
                    HStack(spacing: 12) {
                        LampDot(VaultKeychain.has(a.name) ? Lamp.inUse : Lamp.nobody)
                        VStack(alignment: .leading, spacing: 3) {
                            HStack(spacing: 8) {
                                Text(a.name).font(Face.mono(14, .semibold))
                                Text(VaultText.kindName(a.kind)).font(Face.mono(11)).foregroundStyle(Ink.label)
                            }
                            Text(VaultText.summary(a)).font(Face.body(12)).foregroundStyle(Ink.dim).lineLimit(1)
                            if !VaultKeychain.has(a.name) {
                                Text("No secret saved: edit it to add one").font(Face.body(12)).foregroundStyle(Color(hex: 0xFF8A8D))
                            } else if store.vaultTampered(a) {
                                Text("Changed outside the app: requests for it are refused until you edit it and save it again").font(Face.body(12, .semibold)).foregroundStyle(Color(hex: 0xFF8A8D))
                            }
                        }
                        Spacer()
                        Button("Edit") { editing = VaultEdit(account: store.savedSettings(a.name) ?? a, isNew: false) }.buttonStyle(.key)
                        Button("Remove") {
                            Task {
                                do { try await store.deleteVaultAccount(a.name); error = nil } catch { self.error = error.localizedDescription }
                            }
                        }.buttonStyle(.key(.danger))
                    }
                    .padding(.horizontal, 14).padding(.vertical, 10)
                    Rectangle().fill(Ink.hair).frame(height: 1)
                }
            }
            .panel()
        }
    }

    private var grants: some View {
        let all = store.vaultGrants.reversed()
        let live = all.filter(\.live)
        let past = all.filter { !$0.live }.prefix(25)
        return VStack(alignment: .leading, spacing: 8) {
            Text("ACCESS GIVEN").font(Face.mono(11, .semibold)).tracking(1).foregroundStyle(Ink.label)
            VStack(spacing: 0) {
                if live.isEmpty && past.isEmpty {
                    Text("Nothing yet.").font(Face.body(13)).foregroundStyle(Ink.label).padding(14).frame(maxWidth: .infinity, alignment: .leading)
                }
                ForEach(live) { g in grantRow(g, live: true) }
                ForEach(Array(past)) { g in grantRow(g, live: false) }
            }
            .panel()
        }
    }

    private func grantRow(_ g: VaultGrant, live: Bool) -> some View {
        VStack(spacing: 0) {
            HStack(alignment: .top, spacing: 12) {
                LampDot(live ? Lamp.inUse : Lamp.system)
                VStack(alignment: .leading, spacing: 3) {
                    HStack(spacing: 8) {
                        Text(g.account).font(Face.mono(13, .semibold))
                        Text(g.scope).font(Face.mono(11)).foregroundStyle(Ink.label).lineLimit(1)
                        if g.reveal == true { Text("SHOWN").font(Face.mono(10, .bold)).foregroundStyle(Color(hex: 0xFF8A8D)) }
                    }
                    Text("“\(g.purpose)”").font(Face.body(12)).italic().foregroundStyle(Ink.dim).lineLimit(2)
                    Text(detail(g, live: live)).font(Face.mono(11)).foregroundStyle(Ink.label)
                    if let note = g.note, live { Text(note).font(Face.body(11)).foregroundStyle(Ink.label) }
                }
                Spacer()
                if live { Button("Revoke") { store.revokeGrant(g) }.buttonStyle(.key(.danger)) }
            }
            .padding(.horizontal, 14).padding(.vertical, 10)
            .opacity(live ? 1 : 0.6)
            Rectangle().fill(Ink.hair).frame(height: 1)
        }
    }

    private func detail(_ g: VaultGrant, live: Bool) -> String {
        var s = "\(Store.agentName(g.agent)) \(g.session.prefix(12))"
        if let r = g.repo { s += " in \(r)" }
        s += " · granted \(Fmt.rel(g.created))"
        s += live ? " · ends \(Fmt.rel(g.expires))" : " · ended: \(g.end_reason ?? "expired")"
        if let u = g.uses, u > 0 { s += " · used \(u)×" }
        return s
    }
}

/// Adds or edits an account. Secret fields left empty keep the saved secret.
struct VaultAccountEditor: View {
    @EnvironmentObject var store: Store
    let original: VaultAccount?
    @State var account: VaultAccount
    let done: () -> Void

    @State private var secret = ""         // API key, password, AWS secret access key
    @State private var secondary = ""      // AWS access key ID
    @State private var pem = ""            // GitHub App private key
    @State private var sessionToken = ""   // AWS, optional
    @State private var reposText = ""
    @State private var permsText = ""
    @State private var rolesText = ""
    @State private var readOnly = false
    @State private var error: String?
    @State private var saving = false
    // API key: the template it was filled in from; a new one starts on the list of them.
    @State private var template: VaultTemplate?
    @State private var picking: Bool
    @State private var details = false

    init(original: VaultAccount?, account: VaultAccount, done: @escaping () -> Void) {
        self.original = original
        _account = State(initialValue: account)
        self.done = done
        _picking = State(initialValue: original == nil && account.kind == "apikey")
        _reposText = State(initialValue: (account.repos ?? []).joined(separator: ", "))
        _permsText = State(initialValue: (account.permissions ?? [:]).sorted { $0.key < $1.key }.map { "\($0.key):\($0.value)" }.joined(separator: ", "))
        _rolesText = State(initialValue: account.kind == "aws" ? (account.scopes ?? []).joined(separator: "\n") : "")
        _readOnly = State(initialValue: (account.scopes ?? []) == ["read"])
    }

    static func blank(_ kind: String) -> VaultAccount {
        var a = VaultAccount(name: "", kind: kind)
        a.max_minutes = 60
        if kind == "apikey" { a.api_url = "https://"; a.scopes = ["full", "read"] }
        if kind == "aws" { a.region = "us-east-1" }
        return a
    }

    var body: some View {
        if picking {
            VaultTemplatePicker(pick: use, cancel: done)
        } else {
            form
        }
    }

    private var title: String {
        if let original { return "Edit \(original.name)" }
        if let template { return "Add \(template.name)" }
        return "Add \(VaultText.kindName(account.kind))"
    }

    private var form: some View {
        VStack(alignment: .leading, spacing: 12) {
            HStack {
                Text(title).font(Face.body(18, .semibold))
                Spacer()
                if original == nil && account.kind == "apikey" {
                    Button("Choose another API") { picking = true }.buttonStyle(.link)
                }
            }
            Form {
                TextField("Name agents ask for", text: $account.name, prompt: Text("github-openbox"))
                TextField("Note for agents", text: opt(\.note), prompt: Text("what it is for"))
                Stepper("Longest grant: \(account.max_minutes ?? 60) min", value: Binding(
                    get: { account.max_minutes ?? 60 }, set: { account.max_minutes = $0 }), in: 15...(account.kind == "github" ? 60 : 720), step: 15)
                switch account.kind {
                case "github": github
                case "apikey": apikey
                case "aws": aws
                default: password
                }
            }
            .formStyle(.grouped)
            if let error { Text(error).font(Face.body(12, .semibold)).foregroundStyle(Color(hex: 0xFF8A8D)).fixedSize(horizontal: false, vertical: true) }
            HStack {
                Text(original == nil ? "Saving asks for Touch ID." : "Saving asks for Touch ID and ends live grants of this account.")
                    .font(Face.body(12)).foregroundStyle(Ink.label)
                Spacer()
                Button("Cancel") { done() }.buttonStyle(.key).keyboardShortcut(.cancelAction)
                Button(saving ? "Saving…" : "Save") { save() }.buttonStyle(.key(.go)).disabled(saving)
            }
        }
        .padding(20)
        .frame(width: 560)
        .background(Ink.bg)
        .environment(\.colorScheme, .dark)
    }

    private var keep: String { original == nil ? "" : "leave empty to keep the saved one" }

    @ViewBuilder private var github: some View {
        Section("GitHub App") {
            TextField("App ID", text: opt(\.app_id))
            TextField("Installation ID", text: opt(\.installation_id), prompt: Text("from github.com/settings/installations/<id>"))
            TextField("Repositories (empty: all it can reach)", text: $reposText, prompt: Text("agentrm, openbox-backend"))
            TextField("Most that may be asked", text: $permsText, prompt: Text("contents:write, pull_requests:write"))
            TextField("API (GitHub Enterprise only)", text: opt(\.api_url), prompt: Text("https://api.github.com"))
        }
        Section("Private key (.pem)") {
            TextEditor(text: $pem).font(Face.mono(11)).frame(height: 90)
            HStack {
                Text(keep).font(Face.body(11)).foregroundStyle(Ink.label)
                Spacer()
                Button("Choose file…") { choosePEM() }
            }
        }
    }

    @ViewBuilder private var apikey: some View {
        Section {
            SecureField(keep.isEmpty ? "API key" : keep, text: $secret)
            if let t = template, let k = t.key_url, let url = URL(string: k) {
                Link("Get a \(t.name) key ↗", destination: url).font(Face.body(12))
            }
            if let note = template?.note {
                Text(note).font(Face.body(12)).foregroundStyle(Ink.dim).fixedSize(horizontal: false, vertical: true)
            }
            Toggle("Read-only (GET requests only)", isOn: $readOnly)
        } header: {
            Text("Key")
        }
        if template == nil || details {
            apikeyDetails
        } else {
            Section {
                Button("Show details: \(account.api_url ?? "") · \(account.key_env ?? "")") { details = true }.buttonStyle(.link)
            }
        }
    }

    @ViewBuilder private var apikeyDetails: some View {
        Section {
            TextField("API address", text: opt(\.api_url), prompt: Text("https://api.example.com/v1"))
            TextField("Key variable", text: opt(\.key_env), prompt: Text("EXAMPLE_API_KEY"))
            TextField("Address variable", text: opt(\.base_env), prompt: Text("EXAMPLE_BASE_URL"))
            TextField("Header", text: opt(\.header), prompt: Text("Authorization"))
            TextField("Before the key", text: opt(\.prefix), prompt: Text("Bearer  (for Authorization)"))
            Toggle("The key alone in Authorization, no \"Bearer\"", isOn: Binding(
                get: { account.raw_key ?? false }, set: { account.raw_key = $0 ? true : nil }))
        } header: {
            Text("Details")
        } footer: {
            Text("Agents get a token for AgentRM's local proxy and its address in these variables; the proxy adds the real key. Tools must let the API address be set.")
                .font(Face.body(11)).foregroundStyle(Ink.label)
        }
    }

    @ViewBuilder private var aws: some View {
        Section("AWS") {
            TextField("Region", text: opt(\.region))
            VStack(alignment: .leading) {
                Text("Roles that may be assumed (ARNs, one per line)").font(Face.body(12))
                TextEditor(text: $rolesText).font(Face.mono(11)).frame(height: 60)
            }
            TextField("External ID (if the roles need one)", text: opt(\.external_id))
        }
        Section("Keys that may assume them") {
            TextField(keep.isEmpty ? "Access key ID" : keep, text: $secondary)
            SecureField(keep.isEmpty ? "Secret access key" : keep, text: $secret)
            SecureField("Session token (only for temporary keys)", text: $sessionToken)
        }
    }

    @ViewBuilder private var password: some View {
        Section("Login") {
            TextField("Username", text: opt(\.username))
            TextField("Where (URL)", text: opt(\.url))
            SecureField(keep.isEmpty ? "Password" : keep, text: $secret)
            TextField("Username variable", text: opt(\.user_env), prompt: Text("AGENTRM_USERNAME"))
            TextField("Password variable", text: opt(\.pass_env), prompt: Text("AGENTRM_PASSWORD"))
        }
    }

    private func opt(_ kp: WritableKeyPath<VaultAccount, String?>) -> Binding<String> {
        Binding(get: { account[keyPath: kp] ?? "" }, set: { account[keyPath: kp] = $0.isEmpty ? nil : $0 })
    }

    /// Fills the account in from a template (nil: another API, filled in by hand).
    private func use(_ t: VaultTemplate?) {
        let was = template
        template = t
        picking = false
        details = false
        var a = VaultAccountEditor.blank("apikey")
        a.name = account.name
        a.note = account.note
        a.max_minutes = account.max_minutes
        if let t {
            a.api_url = t.api_url
            a.header = t.header
            a.prefix = t.prefix
            a.raw_key = t.raw_key == true ? true : nil
            a.key_env = t.key_env
            a.base_env = t.base_env
            // A name or note the template gave is replaced by the new one's; one you typed is kept.
            if a.name.isEmpty || a.name.hasPrefix(was?.id ?? "\u{0}") { a.name = freeName(t.id) }
            if (a.note ?? "").isEmpty || a.note == was?.name { a.note = t.name }
        }
        account = a
    }

    /// The template's name, or name-2, name-3… if an account already has it.
    private func freeName(_ base: String) -> String {
        let taken = Set(store.vaultAccounts.map(\.name))
        if !taken.contains(base) { return base }
        var n = 2
        while taken.contains("\(base)-\(n)") { n += 1 }
        return "\(base)-\(n)"
    }

    private func choosePEM() {
        let p = NSOpenPanel()
        p.allowedContentTypes = [.init(filenameExtension: "pem") ?? .data]
        p.allowsMultipleSelection = false
        if p.runModal() == .OK, let url = p.url, let s = try? String(contentsOf: url, encoding: .utf8) { pem = s }
    }

    private func list(_ s: String, _ sep: CharacterSet = CharacterSet(charactersIn: ",\n")) -> [String] {
        s.components(separatedBy: sep).map { $0.trimmingCharacters(in: .whitespaces) }.filter { !$0.isEmpty }
    }

    /// The secret to save, or nil to keep the saved one. Throws if a new account has none.
    private func material() throws -> [String: String]? {
        var m: [String: String] = [:]
        switch account.kind {
        case "github": if !pem.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty { m["private_key"] = pem }
        case "aws":
            if !secondary.isEmpty || !secret.isEmpty {
                guard !secondary.isEmpty, !secret.isEmpty else { throw CLI.Failure(message: "Give both the access key ID and the secret access key.") }
                m["access_key_id"] = secondary
                m["secret_access_key"] = secret
                if !sessionToken.isEmpty { m["session_token"] = sessionToken }
            }
        case "apikey": if !secret.isEmpty { m["key"] = secret }
        default: if !secret.isEmpty { m["password"] = secret }
        }
        if m.isEmpty {
            if original == nil { throw CLI.Failure(message: "Add the secret: it is what agents get access to.") }
            return nil
        }
        return m
    }

    private func save() {
        error = nil
        account.name = account.name.trimmingCharacters(in: .whitespaces).lowercased()
        switch account.kind {
        case "github":
            account.repos = list(reposText).isEmpty ? nil : list(reposText)
            var perms: [String: String] = [:]
            for p in list(permsText) {
                let parts = p.split(whereSeparator: { $0 == ":" || $0 == "=" }).map { $0.trimmingCharacters(in: .whitespaces) }
                guard parts.count == 2 else { error = "Write permissions as name:level, like contents:write"; return }
                perms[parts[0]] = parts[1]
            }
            account.permissions = perms.isEmpty ? nil : perms
        case "apikey": account.scopes = readOnly ? ["read"] : ["full", "read"]
        case "aws": account.scopes = list(rolesText, .newlines)
        default: break
        }
        let mat: [String: String]?
        do { mat = try material() } catch { self.error = error.localizedDescription; return }
        saving = true
        Task {
            do {
                try await store.saveVaultAccount(account, material: mat, replacing: original?.name)
                done()
            } catch {
                self.error = error.localizedDescription
            }
            saving = false
        }
    }
}

/// The APIs an API key account can be filled in for, searchable, by category. The list is
/// internal/vault/templates.json, read through the helper.
struct VaultTemplatePicker: View {
    let pick: (VaultTemplate?) -> Void
    let cancel: () -> Void
    @State private var all: [VaultTemplate] = []
    @State private var query = ""
    @State private var failed: String?

    var body: some View {
        VStack(alignment: .leading, spacing: 12) {
            Text("Add an API key").font(Face.body(18, .semibold))
            Text("Pick the API and paste the key: the rest is filled in.").font(Face.body(13)).foregroundStyle(Ink.dim)
            TextField("Search \(all.count) APIs", text: $query).textFieldStyle(.roundedBorder)
            if let failed { Text(failed).font(Face.body(12, .semibold)).foregroundStyle(Color(hex: 0xFF8A8D)) }
            ScrollView {
                LazyVStack(alignment: .leading, spacing: 0) {
                    ForEach(groups, id: \.0) { cat, ts in
                        Text(cat.uppercased()).font(Face.mono(10, .semibold)).tracking(1).foregroundStyle(Ink.label)
                            .padding(.top, 10).padding(.bottom, 4)
                        ForEach(ts) { t in row(t.name, t.api_url) { pick(t) } }
                    }
                    Text("NOT LISTED").font(Face.mono(10, .semibold)).tracking(1).foregroundStyle(Ink.label)
                        .padding(.top, 10).padding(.bottom, 4)
                    row("Another API", "fill in the address, variables and header yourself") { pick(nil) }
                }
            }
            .frame(height: 380)
            HStack {
                Spacer()
                Button("Cancel") { cancel() }.buttonStyle(.key).keyboardShortcut(.cancelAction)
            }
        }
        .padding(20)
        .frame(width: 560)
        .background(Ink.bg)
        .environment(\.colorScheme, .dark)
        .task { await load() }
    }

    private var groups: [(String, [VaultTemplate])] {
        let q = query.trimmingCharacters(in: .whitespaces).lowercased()
        let hits = q.isEmpty ? all : all.filter {
            $0.name.lowercased().contains(q) || $0.id.contains(q) || $0.api_url.lowercased().contains(q) || $0.category.lowercased().contains(q)
        }
        var order: [String] = []
        var by: [String: [VaultTemplate]] = [:]
        for t in hits {
            if by[t.category] == nil { order.append(t.category) }
            by[t.category, default: []].append(t)
        }
        return order.map { ($0, by[$0]!) }
    }

    private func row(_ name: String, _ detail: String, _ action: @escaping () -> Void) -> some View {
        Button(action: action) {
            HStack(spacing: 10) {
                Text(name).font(Face.body(14, .semibold))
                Text(detail).font(Face.mono(11)).foregroundStyle(Ink.label).lineLimit(1)
                Spacer()
            }
            .padding(.vertical, 7).padding(.horizontal, 8)
            .contentShape(Rectangle())
        }
        .buttonStyle(.plain)
    }

    private func load() async {
        do {
            let d = try await CLI.run(["vault", "templates", "--json"])
            all = try JSONDecoder().decode([VaultTemplate].self, from: d)
        } catch {
            failed = "Could not read the list of APIs: \(error.localizedDescription). Choose Another API to fill it in yourself."
        }
    }
}
