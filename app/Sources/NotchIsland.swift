import AppKit
import Combine
import SwiftUI

// The notch island: AgentRM's state in two thin wings either side of the camera, approvals that
// drop out of the notch, and the stats when the pointer rests on it. It is black so it reads as
// part of the notch. On a Mac without one it hangs from the middle of the menu bar.
//
// Hovering opens it for a look; clicking it pins it open until you click it again or anywhere
// else. Approving is a click: the panel never becomes key, so a Return or Esc typed into a
// terminal while an approval is showing never answers it.

@MainActor
final class NotchModel: ObservableObject {
    enum Mode: Equatable { case rest, approval, stats }
    struct Flash: Equatable { let text: String; let approved: Bool }

    @Published var mode: Mode = .rest
    /// Opened by a click: stays open when the pointer leaves, until a click closes it.
    @Published var pinned = false
    /// Which waiting approval is showing, when there are several.
    @Published var index = 0
    /// The camera housing: its width (0 without a notch) and the menu bar's height.
    @Published var notch = CGSize(width: 0, height: 32)
    @Published var flash: Flash?
    /// The island as last laid out, for hit testing.
    var islandSize: CGSize = .zero
}

@MainActor
final class NotchController {
    static let enabledKey = "notchIsland"
    static var isEnabled: Bool { UserDefaults.standard.object(forKey: enabledKey) as? Bool ?? true }
    private static var shared: NotchController?

    /// Starts the island once per launch; `openActivity` opens the main window (it needs a scene).
    static func start(store: Store, openActivity: @escaping () -> Void) {
        guard shared == nil else { return }
        let c = NotchController(store: store)
        c.openActivity = openActivity
        shared = c
        c.applyEnabled()
    }

    private let store: Store
    let model = NotchModel()
    private var panel: NSPanel?
    private var subs: Set<AnyCancellable> = []
    private var monitors: [Any] = []
    private var seen: Set<String> = []
    private var hovering = false
    private var hoverTask: Task<Void, Never>?
    private var foldTask: Task<Void, Never>?
    private var flashTask: Task<Void, Never>?
    private var pointerTimer: Timer?
    var openActivity: () -> Void = {}

    /// Room for the largest island and its glow; outside the island the panel lets clicks through.
    private let canvas = CGSize(width: 680, height: 640)

    private init(store: Store) {
        self.store = store
        // Approvals already waiting at launch still pop once: they are why the island exists.
        store.objectWillChange
            .receive(on: RunLoop.main)
            .sink { [weak self] _ in Task { @MainActor in self?.statusChanged() } }
            .store(in: &subs)
        NotificationCenter.default.publisher(for: UserDefaults.didChangeNotification)
            .receive(on: RunLoop.main)
            .sink { [weak self] _ in Task { @MainActor in self?.applyEnabled() } }
            .store(in: &subs)
        NotificationCenter.default.publisher(for: NSApplication.didChangeScreenParametersNotification)
            .receive(on: RunLoop.main)
            .sink { [weak self] _ in Task { @MainActor in self?.place() } }
            .store(in: &subs)
    }

    private func applyEnabled() {
        if Self.isEnabled {
            if panel == nil { show() }
        } else if let p = panel {
            p.orderOut(nil)
            panel = nil
            monitors.forEach(NSEvent.removeMonitor)
            monitors = []
            pointerTimer?.invalidate()
            pointerTimer = nil
        }
    }

    private func show() {
        let p = IslandPanel(contentRect: CGRect(origin: .zero, size: canvas),
                            styleMask: [.borderless, .nonactivatingPanel], backing: .buffered, defer: false)
        p.isOpaque = false
        p.backgroundColor = .clear
        p.hasShadow = false
        p.level = NSWindow.Level(rawValue: NSWindow.Level.statusBar.rawValue + 1)
        p.collectionBehavior = [.canJoinAllSpaces, .stationary, .fullScreenAuxiliary, .ignoresCycle]
        p.hidesOnDeactivate = false
        p.isMovable = false
        p.ignoresMouseEvents = true
        p.acceptsMouseMovedEvents = true
        let view = NotchIsland(model: model, controller: self).environmentObject(store)
        p.contentView = FirstClickHostingView(rootView: AnyView(view))
        panel = p
        place()
        p.orderFrontRegardless()
        // The pointer is followed rather than hovered: the panel is never key, and it only takes
        // mouse events while the pointer is over the island itself.
        let mask: NSEvent.EventTypeMask = [.mouseMoved, .leftMouseDragged]
        if let g = NSEvent.addGlobalMonitorForEvents(matching: mask, handler: { [weak self] _ in
            Task { @MainActor in self?.pointerMoved() }
        }) { monitors.append(g) }
        if let l = NSEvent.addLocalMonitorForEvents(matching: mask, handler: { [weak self] e in
            Task { @MainActor in self?.pointerMoved() }
            return e
        }) { monitors.append(l) }
        // Once AgentRM is the active app (after Touch ID, say) moves over other apps reach
        // neither monitor, and a panel left taking clicks would swallow them: also look often.
        let t = Timer(timeInterval: 0.2, repeats: true) { [weak self] _ in
            Task { @MainActor in self?.pointerMoved() }
        }
        RunLoop.main.add(t, forMode: .common)
        pointerTimer = t
        // A click anywhere else closes an island opened by a click.
        let clicks: NSEvent.EventTypeMask = [.leftMouseDown, .rightMouseDown]
        if let g = NSEvent.addGlobalMonitorForEvents(matching: clicks, handler: { [weak self] _ in
            Task { @MainActor in self?.clickedElsewhere() }
        }) { monitors.append(g) }
        if let l = NSEvent.addLocalMonitorForEvents(matching: clicks, handler: { [weak self] e in
            let mine = e.window != nil && e.window === self?.panel
            if !mine { Task { @MainActor in self?.clickedElsewhere() } }
            return e
        }) { monitors.append(l) }
        statusChanged()
    }

    /// The screen with the notch (the built-in display), else the main one.
    private var screen: NSScreen? {
        NSScreen.screens.first { $0.safeAreaInsets.top > 0 } ?? NSScreen.main ?? NSScreen.screens.first
    }

    private func place() {
        guard let p = panel, let s = screen else { return }
        var notchW: CGFloat = 0, height: CGFloat, center = s.frame.midX
        if s.safeAreaInsets.top > 0, let l = s.auxiliaryTopLeftArea, let r = s.auxiliaryTopRightArea {
            notchW = max(0, s.frame.width - l.width - r.width)
            center = s.frame.minX + l.width + notchW / 2
            height = s.safeAreaInsets.top
        } else {
            let bar = s.frame.maxY - s.visibleFrame.maxY
            height = bar > 0 ? bar : 24
        }
        model.notch = CGSize(width: notchW, height: height)
        p.setFrame(CGRect(x: center - canvas.width / 2, y: s.frame.maxY - canvas.height,
                          width: canvas.width, height: canvas.height), display: true)
    }

    /// Where the island is on screen now.
    private var islandRect: CGRect {
        guard let p = panel else { return .zero }
        let sz = model.islandSize
        return CGRect(x: p.frame.midX - sz.width / 2, y: p.frame.maxY - sz.height, width: sz.width, height: sz.height)
    }

    private func pointerMoved() {
        guard let p = panel else { return }
        let inside = islandRect.insetBy(dx: -2, dy: -2).contains(NSEvent.mouseLocation)
        p.ignoresMouseEvents = !inside
        guard inside != hovering else { return }
        hovering = inside
        hoverTask?.cancel()
        hoverTask = Task { @MainActor in
            // A pointer passing on its way to a menu should not open anything.
            try? await Task.sleep(nanoseconds: inside ? 180_000_000 : 350_000_000)
            guard !Task.isCancelled, hovering == inside else { return }
            if inside {
                foldTask?.cancel()
                if model.mode == .rest { set(store.pendingApprovals.isEmpty ? .stats : .approval) }
            } else if !model.pinned && !busy {
                set(.rest)
            }
        }
    }

    /// Touch ID is up for an approval: nothing folds away under it.
    private var busy: Bool { !store.authenticating.isEmpty }

    private func clickedElsewhere() {
        guard model.pinned, !busy, !islandRect.contains(NSEvent.mouseLocation) else { return }
        close()
    }

    /// A click on the wings: open and pin, or close.
    func toggle() {
        if model.mode == .rest || !model.pinned {
            foldTask?.cancel()
            model.pinned = true
            set(store.pendingApprovals.isEmpty ? .stats : .approval)
        } else {
            close()
        }
    }

    func close() {
        model.pinned = false
        set(.rest)
    }

    /// From the stats to the approvals and back.
    func show(_ mode: NotchModel.Mode) {
        foldTask?.cancel()
        model.pinned = true
        set(mode)
    }

    /// Between the approvals waiting.
    func step(_ by: Int) {
        let n = store.pendingApprovals.count
        guard n > 1 else { return }
        store.lastActionError = nil
        withAnimation(.easeOut(duration: 0.15)) { model.index = ((model.index + by) % n + n) % n }
    }

    private func statusChanged() {
        let pending = store.pendingApprovals
        let fresh = pending.filter { !seen.contains($0.id) }
        seen.formUnion(pending.map(\.id))
        if model.index >= pending.count { model.index = max(0, pending.count - 1) }
        if !fresh.isEmpty {
            // Show the new one, unless you are already looking at another.
            if model.mode != .approval, let first = fresh.first, let i = pending.firstIndex(where: { $0.id == first.id }) {
                model.index = i
            }
            set(.approval)
            scheduleFold()
        } else if pending.isEmpty && model.mode == .approval && !busy {
            if model.pinned && hovering { set(.stats) } else { close() }
        }
    }

    /// An approval nobody looks at folds back into the wings ("APPROVE 2") after a while.
    private func scheduleFold() {
        foldTask?.cancel()
        foldTask = Task { @MainActor in
            try? await Task.sleep(nanoseconds: 8_000_000_000)
            guard !Task.isCancelled, !hovering, !model.pinned, !busy, model.mode == .approval else { return }
            set(.rest)
        }
    }

    private func set(_ mode: NotchModel.Mode) {
        guard model.mode != mode else { return }
        withAnimation(.spring(response: 0.38, dampingFraction: 0.82)) { model.mode = mode }
    }

    // MARK: actions from the island

    func answer(_ a: Approval, approve: Bool) {
        foldTask?.cancel()
        store.lastActionError = nil
        if approve && a.vault != nil {
            // Touch ID first: the island stays open, shows it is waiting, and says how it went.
            model.pinned = true
            let task = store.approveVault(a)
            Task { @MainActor in
                if await task.value {
                    done(a, approved: true)
                } else {
                    flash(NotchModel.Flash(text: "NOT APPROVED", approved: false))
                }
            }
            return
        }
        store.answer(a.id, approve: approve)
        done(a, approved: approve)
    }

    private func done(_ a: Approval, approved: Bool) {
        flash(NotchModel.Flash(text: approved ? "APPROVED" : "DENIED", approved: approved))
        if store.pendingApprovals.filter({ $0.id != a.id }).isEmpty { close() }
    }

    private func flash(_ f: NotchModel.Flash) {
        flashTask?.cancel()
        withAnimation { model.flash = f }
        flashTask = Task { @MainActor in
            try? await Task.sleep(nanoseconds: 2_500_000_000)
            guard !Task.isCancelled else { return }
            withAnimation { model.flash = nil }
        }
    }

    func open() {
        close()
        NSApp.activate(ignoringOtherApps: true)
        openActivity()
    }
}

/// Never key, never main: keys typed elsewhere stay elsewhere.
private final class IslandPanel: NSPanel {
    override var canBecomeKey: Bool { false }
    override var canBecomeMain: Bool { false }
}

/// The first click on a key works even though the panel never becomes key.
private final class FirstClickHostingView: NSHostingView<AnyView> {
    override func acceptsFirstMouse(for event: NSEvent?) -> Bool { true }
}

// MARK: the view

struct NotchIsland: View {
    @EnvironmentObject var store: Store
    @ObservedObject var model: NotchModel
    /// nil in snapshots: the keys do nothing there.
    var controller: NotchController?

    private var notchW: CGFloat { model.notch.width }
    private var barH: CGFloat { model.notch.height }
    private var current: Approval? {
        let p = store.pendingApprovals
        return p.isEmpty ? nil : p[min(model.index, p.count - 1)]
    }

    private var mode: NotchModel.Mode {
        if model.mode == .approval && current == nil { return .rest }
        return model.mode
    }

    private var width: CGFloat {
        let rest = notchW + 2 * 112
        switch mode {
        case .rest: return rest
        case .approval: return max(rest, 540)
        case .stats: return max(rest, 580)
        }
    }

    var body: some View {
        island
            .frame(maxWidth: .infinity, maxHeight: .infinity, alignment: .top)
            .environment(\.colorScheme, .dark)
    }

    private var island: some View {
        let radius: CGFloat = mode == .rest ? 12 : 28
        let shape = UnevenRoundedRectangle(bottomLeadingRadius: radius, bottomTrailingRadius: radius, style: .continuous)
        let glow = rest.glow
        return VStack(spacing: 0) {
            wings
            switch mode {
            case .rest: EmptyView()
            case .approval: if let a = current { approvalBody(a).transition(.opacity) }
            case .stats: stats.transition(.opacity)
            }
        }
        .frame(width: width)
        .foregroundStyle(Ink.text)
        .background(shape.fill(Color.black))
        .clipShape(shape)
        .overlay(shape.stroke(glow?.opacity(0.22) ?? .clear, lineWidth: 1))
        .shadow(color: glow?.opacity(mode == .rest ? 0.30 : 0.16) ?? .clear, radius: mode == .rest ? 10 : 22)
        .shadow(color: .black.opacity(mode == .rest ? 0 : 0.45), radius: 24, y: 14)
        // The black shape itself, before the room left for its glow. Not a preference: one set in
        // a background never arrives on macOS 27, which left the island with no size, so the
        // pointer was never over it and it neither opened nor took a click.
        .onGeometryChange(for: CGSize.self) { $0.size } action: { model.islandSize = $0 }
        .padding(.horizontal, 30).padding(.bottom, 40) // room for the glow
        .accessibilityElement(children: .contain)
        .accessibilityLabel("AgentRM")
    }

    // MARK: wings

    /// The row level with the camera: lamp and meter on the left, one word on the right.
    private var wings: some View {
        HStack(spacing: 0) {
            Group {
                if mode == .stats {
                    Text("AGENTRM").font(Face.mono(12, .bold)).tracking(2)
                } else {
                    HStack(spacing: 6) {
                        LampDot(color: rest.lamp, glow: rest.lampGlows)
                        NotchMeter(segments: store.reading?.segments ?? Array(repeating: .off, count: 16))
                    }
                }
            }
            .frame(maxWidth: .infinity, alignment: .leading)
            Color.clear.frame(width: notchW)
            Group {
                if mode == .stats {
                    LampLabel(lamp: headerLamp.color, text: headerLamp.text)
                } else {
                    Text(rest.word).font(Face.mono(11, .semibold)).tracking(0.5).foregroundStyle(rest.wordColor)
                        .lineLimit(1).fixedSize()
                        .contentTransition(.numericText())
                }
            }
            .frame(maxWidth: .infinity, alignment: .trailing)
        }
        .padding(.horizontal, mode == .rest ? 14 : 22)
        .frame(height: barH)
        .contentShape(Rectangle())
        .onTapGesture { controller?.toggle() }
        .accessibilityElement(children: .combine)
        .accessibilityLabel(rest.spoken)
        .accessibilityHint(mode == .rest ? "Opens the island" : "Closes the island")
        .accessibilityAddTraits(.isButton)
    }

    private struct Rest {
        var word: String
        var wordColor: Color
        var lamp: Color
        var lampGlows = true
        var glow: Color?
        var spoken: String
    }

    /// What the wings say when nothing is open, most urgent first.
    private var rest: Rest {
        guard let s = store.status else {
            return Rest(word: "OFF", wordColor: Ink.label, lamp: Ink.scale, lampGlows: false, spoken: "AgentRM is not running")
        }
        if let f = model.flash {
            let c = f.approved ? Lamp.inUse : Lamp.nobody
            return Rest(word: f.text, wordColor: c.color, lamp: c.color, glow: c.color, spoken: f.text.capitalized)
        }
        let n = store.pendingApprovals.count
        if n > 0 {
            return Rest(word: n == 1 ? "APPROVE" : "APPROVE \(n)", wordColor: Lamp.waiting.textColor, lamp: Lamp.waiting.color,
                        glow: Lamp.waiting.color, spoken: n == 1 ? "An approval is waiting" : "\(n) approvals waiting")
        }
        if s.fail_open != nil {
            return Rest(word: "OPEN", wordColor: Lamp.waiting.textColor, lamp: Lamp.waiting.color,
                        spoken: "Letting everything through")
        }
        if store.underPressure {
            let rate = Int(s.swap_rate_mb_min ?? 0)
            return Rest(word: "SWAP \(rate)", wordColor: Lamp.nobody.textColor, lamp: Lamp.nobody.color, glow: Lamp.nobody.color,
                        spoken: "Swapping \(rate) megabytes a minute")
        }
        if store.runningHot {
            return Rest(word: "HOT", wordColor: Lamp.nobody.textColor, lamp: Lamp.nobody.color, glow: Lamp.nobody.color,
                        spoken: "Running hot: builds wait")
        }
        let g = Fmt.gb(max(0, s.headroom_mb))
        return Rest(word: "\(g)G", wordColor: Ink.text, lamp: Lamp.inUse.color, spoken: "\(g) gigabytes free to grant")
    }

    private var headerLamp: (color: Color, text: String) {
        guard store.status != nil else { return (Ink.scale, "Offline") }
        if store.underPressure { return (Lamp.nobody.color, "Overload") }
        if store.runningHot { return (Lamp.nobody.color, "Running hot") }
        if !store.pendingApprovals.isEmpty || (store.reading?.nobodyMB ?? 0) > 0 { return (Lamp.waiting.color, "Needs you") }
        return (Lamp.inUse.color, "All clear")
    }

    // MARK: approval

    private func approvalBody(_ a: Approval) -> some View {
        VStack(alignment: .leading, spacing: 10) {
            HStack(spacing: 8) {
                LampLabel(lamp: Lamp.waiting.color, text: "Approval, expires \(Fmt.rel(a.expires))")
                Spacer()
                let n = store.pendingApprovals.count
                if n > 1 {
                    IslandIcon("chevron.left", help: "Previous approval") { controller?.step(-1) }
                    Silk("\(min(model.index, n - 1) + 1) of \(n)")
                    IslandIcon("chevron.right", help: "Next approval") { controller?.step(1) }
                }
                IslandIcon("chart.bar.fill", help: "Show the stats") { controller?.show(.stats) }
                IslandIcon("xmark", help: "Close; the approval stays waiting") { controller?.close() }
            }
            if let err = store.lastActionError { errorLine(err) }
            if let ask = a.vault {
                // The vault's own card: it carries the warnings about what approving cannot undo.
                VaultApprovalCard(approval: a, ask: ask) { controller?.answer(a, approve: $0) }
                    .id(a.id)
            } else {
                Text(sentence(a.what)).font(Face.body(21, .semibold)).fixedSize(horizontal: false, vertical: true)
                let tags = tags(for: a)
                if !tags.isEmpty {
                    HStack(spacing: 6) {
                        ForEach(tags, id: \.self) { t in
                            Text(t).font(Face.mono(11)).foregroundStyle(Ink.dim)
                                .padding(.horizontal, 7).padding(.vertical, 3)
                                .background(Color(hex: 0x16181B), in: RoundedRectangle(cornerRadius: 5, style: .continuous))
                                .overlay(RoundedRectangle(cornerRadius: 5, style: .continuous).stroke(Ink.line, lineWidth: 1))
                        }
                    }
                }
                Text(a.path == "/leak"
                     ? "AgentRM is asking: its memory keeps climbing and nothing explains it. Nothing happens unless you approve."
                     : "Asked from \(a.from ?? "a terminal"). If you didn't type it, an agent did.")
                    .font(Face.body(13)).foregroundStyle(Color(hex: 0xC9C0A8)).fixedSize(horizontal: false, vertical: true)
                HStack(spacing: 8) {
                    Spacer()
                    Button("Deny") { controller?.answer(a, approve: false) }.buttonStyle(.key(.plain, large: true))
                    Button("Approve") { controller?.answer(a, approve: true) }.buttonStyle(.key(.amber, large: true))
                }
                .padding(.top, 4)
            }
        }
        .padding(.horizontal, 22).padding(.top, 10).padding(.bottom, 20)
    }

    private func sentence(_ s: String) -> String { s.prefix(1).uppercased() + s.dropFirst() }

    /// Why the last thing you did there did not work: Touch ID cancelled, the daemon refusing.
    private func errorLine(_ err: String) -> some View {
        HStack(alignment: .top, spacing: 8) {
            Image(systemName: "exclamationmark.triangle.fill").font(.system(size: 11)).foregroundStyle(Lamp.nobody.color)
            Text(err).font(Face.body(12, .semibold)).foregroundStyle(Color(hex: 0xFF8A8D))
                .fixedSize(horizontal: false, vertical: true)
            Spacer(minLength: 4)
            IslandIcon("xmark", help: "Dismiss") { store.lastActionError = nil }
        }
        .padding(8)
        .background(Lamp.nobody.color.opacity(0.10), in: RoundedRectangle(cornerRadius: 8, style: .continuous))
    }

    /// For a request about a lease: what the lease is, so "release lease d55c6b97" means something.
    private func tags(for a: Approval) -> [String] {
        guard let l = store.leases.first(where: { a.what.contains($0.id) }) else { return [] }
        var t = [Kind.name(l.kind), String(format: "%.1f GB", l.mem_gb)]
        let who = Store.agentName(l.agent)
        if let s = store.status?.sessions?[l.session], !s.cwd.isEmpty {
            t.append("\(who) · \((s.cwd as NSString).lastPathComponent)")
        } else {
            t.append(who)
        }
        return t
    }

    // MARK: stats

    private var stats: some View {
        VStack(alignment: .leading, spacing: 10) {
            if let s = store.status, let r = store.reading {
                if let err = store.lastActionError { errorLine(err) }
                let n = store.pendingApprovals.count
                if n > 0 {
                    IslandRow(action: { controller?.show(.approval) }) {
                        HStack(spacing: 8) {
                            LampDot(color: Lamp.waiting.color, glow: true)
                            Text(n == 1 ? "An approval is waiting" : "\(n) approvals waiting")
                                .font(Face.body(13, .semibold)).foregroundStyle(Lamp.waiting.textColor)
                            Spacer()
                            Image(systemName: "chevron.right").font(.system(size: 10, weight: .bold)).foregroundStyle(Ink.dim)
                        }
                        .padding(.horizontal, 12).padding(.vertical, 9)
                    }
                    .panel(Ink.amberPanel, stroke: Color(hex: 0x8A6A12))
                }
                IslandRow(action: { controller?.open() }) {
                    VStack(alignment: .leading, spacing: 8) {
                        HStack {
                            Silk("Memory")
                            Spacer()
                            Text("\(Fmt.gb(r.totalMB - r.freeMB)) / \(Fmt.gb(r.totalMB)) GB").font(Face.mono(12)).foregroundStyle(Ink.dim)
                        }
                        LEDMeter(segments: r.segments, height: 36, showScale: false)
                    }
                    .padding(12)
                }
                .panel(Ink.bg)
                .help("Open AgentRM")
                HStack(spacing: 8) {
                    let grant = max(0, s.headroom_mb)
                    Readout(label: "Grant", value: Fmt.gb(grant), unit: "GB", color: grant < 512 ? Lamp.nobody.color : Lamp.inUse.color)
                    tile("Pressure") {
                        PressureLamps(level: s.mem.pressure_level ?? 1)
                        Text(["", "Normal", "Warning", "", "Critical"][min(s.mem.pressure_level ?? 1, 4)])
                            .font(Face.body(12)).foregroundStyle((s.mem.pressure_level ?? 1) >= 4 ? Lamp.nobody.textColor : Ink.dim)
                    }
                    Readout(label: "Swap", value: "\(Int(s.swap_rate_mb_min ?? 0))", unit: "MB/m",
                            color: (s.swap_rate_mb_min ?? 0) > s.swapLimit ? Lamp.nobody.color : Ink.text)
                    tile("Heat") {
                        HeatLamps(level: s.mem.thermal ?? -1)
                        Text(heatName(s.mem.thermal)).font(Face.body(12))
                            .foregroundStyle(store.runningHot ? Lamp.nobody.textColor : Ink.dim)
                    }
                }
                .fixedSize(horizontal: false, vertical: true)
                sessions(s)
            }
            HStack(spacing: 8) {
                Silk(model.pinned ? "Click outside to close" : "Move away to close")
                Spacer()
                if store.status?.fail_open != nil {
                    Button("Manage again") { store.bypass(minutes: 0) }.buttonStyle(.key(.plain, large: true))
                } else {
                    Button("Let through 1 h") { store.bypass(minutes: 60) }.buttonStyle(.key(.plain, large: true))
                        .help("Let every request through for an hour: nothing is held back, paused or stopped.")
                }
                Button("Open AgentRM") { controller?.open() }.buttonStyle(.key(.plain, large: true))
            }
        }
        .padding(.horizontal, 16).padding(.top, 8).padding(.bottom, 16)
    }

    private func tile<C: View>(_ label: String, @ViewBuilder _ content: () -> C) -> some View {
        VStack(alignment: .leading, spacing: 6) {
            Silk(label)
            content()
        }
        .frame(maxWidth: .infinity, maxHeight: .infinity, alignment: .topLeading)
        .padding(10).panel()
    }

    private func heatName(_ t: Int?) -> String {
        guard let t, t >= 0 else { return "Unknown" }
        return ["Nominal", "Moderate", "Heavy", "Critical", "Critical"][min(t, 4)]
    }

    private func sessions(_ s: Status) -> some View {
        let live = (s.sessions ?? [:]).values
            .filter { $0.ended_at == nil && $0.agent != "you" }
            .sorted { ($0.cpu_percent ?? 0, $0.footprint_mb ?? 0) > ($1.cpu_percent ?? 0, $1.footprint_mb ?? 0) }
        let leases = store.leases
        return VStack(spacing: 0) {
            HStack {
                Silk("Agents")
                Spacer()
                Silk("\(live.count) \(live.count == 1 ? "session" : "sessions") · \(leases.count) \(leases.count == 1 ? "lease" : "leases")")
            }
            .padding(.horizontal, 12).padding(.top, 10).padding(.bottom, 6)
            if live.isEmpty {
                Text("No agents running.").font(Face.body(13)).foregroundStyle(Ink.dim)
                    .frame(maxWidth: .infinity, alignment: .leading)
                    .padding(.horizontal, 12).padding(.vertical, 10)
                    .overlay(alignment: .top) { Rectangle().fill(Ink.hair).frame(height: 1) }
            }
            ForEach(live.prefix(4)) { sess in
                let lease = leases.first { $0.session == sess.id }
                let recent = -sess.last_seen.timeIntervalSinceNow < 300
                IslandRow(action: { controller?.open() }) { HStack(spacing: 10) {
                    LampDot(color: lease != nil || recent ? Lamp.inUse.color : Ink.scale, glow: lease != nil || recent)
                    VStack(alignment: .leading, spacing: 1) {
                        Text("\(Store.agentName(sess.agent)) · \((sess.cwd as NSString).lastPathComponent)")
                            .font(Face.body(14, .semibold)).lineLimit(1)
                        Text(lease.map { "\(Kind.name($0.kind)), lease \($0.id), \(Fmt.since($0.created))" }
                             ?? "seen \(Fmt.since(sess.last_seen)) ago")
                            .font(Face.body(12)).foregroundStyle(Ink.dim).lineLimit(1)
                    }
                    Spacer(minLength: 8)
                    if let cpu = sess.cpu_percent, cpu >= 1 {
                        Text("\(Int(cpu))%").font(Face.mono(12)).foregroundStyle(Ink.dim)
                    }
                    Text(Fmt.size(sess.footprint_mb ?? 0)).font(Face.mono(13)).frame(width: 64, alignment: .trailing)
                }
                .padding(.horizontal, 12).padding(.vertical, 7) }
                .overlay(alignment: .top) { Rectangle().fill(Ink.hair).frame(height: 1) }
                .help("Open AgentRM")
            }
            if live.count > 4 {
                IslandRow(action: { controller?.open() }) {
                    Text("\(live.count - 4) more in AgentRM").font(Face.body(12)).foregroundStyle(Ink.dim)
                        .frame(maxWidth: .infinity, alignment: .leading)
                        .padding(.horizontal, 12).padding(.vertical, 8)
                }
                .overlay(alignment: .top) { Rectangle().fill(Ink.hair).frame(height: 1) }
            }
        }
        .panel(Ink.bg)
    }
}

/// A row of the island that is a button: it lights up under the pointer.
private struct IslandRow<Content: View>: View {
    let action: () -> Void
    @ViewBuilder let content: () -> Content
    @State private var over = false

    var body: some View {
        Button(action: action) {
            content()
                .contentShape(Rectangle())
                .background(Color.white.opacity(over ? 0.06 : 0))
        }
        .buttonStyle(.plain)
        .onHover { over = $0 }
    }
}

/// A small round icon button in the island's headers.
private struct IslandIcon: View {
    let symbol: String
    let help: String
    let action: () -> Void
    @State private var over = false

    init(_ symbol: String, help: String, action: @escaping () -> Void) {
        self.symbol = symbol; self.help = help; self.action = action
    }

    var body: some View {
        Button(action: action) {
            Image(systemName: symbol).font(.system(size: 10, weight: .bold))
                .foregroundStyle(over ? Ink.text : Ink.dim)
                .frame(width: 22, height: 22)
                .background(Circle().fill(Color.white.opacity(over ? 0.12 : 0.06)))
                .contentShape(Circle())
        }
        .buttonStyle(.plain)
        .onHover { over = $0 }
        .help(help)
        .accessibilityLabel(help)
    }
}

/// The memory meter at the size of the wings: one bar per GB in its state colour.
struct NotchMeter: View {
    let segments: [Segment]
    var body: some View {
        HStack(spacing: 2) {
            ForEach(Array(segments.enumerated()), id: \.offset) { _, s in
                RoundedRectangle(cornerRadius: 1).fill(color(s)).frame(width: 3, height: 12)
            }
        }
        .accessibilityHidden(true)
    }

    private func color(_ s: Segment) -> Color {
        switch s {
        case .lit(let l): return l == .system ? Color(hex: 0x6B7280) : l.color
        case .kept: return Color(white: 0.33)
        case .off: return Color(white: 0.17)
        }
    }
}

/// macOS thermal pressure as four lamps: nominal, moderate, heavy, critical.
struct HeatLamps: View {
    let level: Int
    var body: some View {
        HStack(spacing: 3) {
            ForEach(0..<4, id: \.self) { i in
                RoundedRectangle(cornerRadius: 2).fill(i <= level ? color : Ink.off).frame(height: 6)
            }
        }
        .padding(.top, 4)
        .accessibilityHidden(true)
    }

    private var color: Color { level >= 2 ? Lamp.nobody.color : level == 1 ? Lamp.waiting.color : Lamp.inUse.color }
}
