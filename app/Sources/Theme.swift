import SwiftUI

// The "instrument" look: a graphite panel, LED lamps and hardware keys. Colour always means
// state (Lamp), never which agent owns something.

extension Color {
    init(hex: UInt32) {
        self.init(.sRGB, red: Double((hex >> 16) & 0xFF) / 255, green: Double((hex >> 8) & 0xFF) / 255,
                  blue: Double(hex & 0xFF) / 255, opacity: 1)
    }
}

enum Ink {
    static let bg = Color(hex: 0x1C1E22)
    static let panel = Color(hex: 0x24272C)
    static let line = Color(hex: 0x33373D)
    static let hair = Color(hex: 0x2E3238)
    static let off = Color(hex: 0x2A2E34)
    static let text = Color(hex: 0xE8E6E1)
    static let dim = Color(hex: 0xB4BAC2)
    static let label = Color(hex: 0x8A919B)
    static let scale = Color(hex: 0x6B7280)
    static let amberPanel = Color(hex: 0x2A2418)
    static let greenPanel = Color(hex: 0x1E2A22)
}

/// The state of a piece of memory. One colour each, everywhere.
enum Lamp: Int, CaseIterable, Hashable {
    case nobody, paused, waiting, inUse, system

    var color: Color {
        switch self {
        case .inUse: return Color(hex: 0x4ADE80)
        case .waiting: return Color(hex: 0xFBBF24)
        case .nobody: return Color(hex: 0xFF5A5F)
        case .paused: return Color(hex: 0x7DD3FC)
        case .system: return Color(hex: 0x6B7280)
        }
    }

    /// The lighter tint used for a status line in this state.
    var textColor: Color {
        switch self {
        case .inUse, .system: return Ink.dim
        case .waiting: return Color(hex: 0xFCD679)
        case .nobody: return Color(hex: 0xFF8A8D)
        case .paused: return Color(hex: 0xA5DEF7)
        }
    }

    var name: String {
        switch self {
        case .inUse: return "In use"
        case .waiting: return "Waiting"
        case .nobody: return "Nobody"
        case .paused: return "Paused"
        case .system: return "You, macOS"
        }
    }

    var glows: Bool { self != .system }
}

enum Face {
    static func mono(_ size: CGFloat, _ weight: Font.Weight = .regular) -> Font {
        .system(size: size, weight: weight, design: .monospaced)
    }
    static func body(_ size: CGFloat, _ weight: Font.Weight = .regular) -> Font {
        .system(size: size, weight: weight).width(.condensed)
    }
}

/// A small round indicator lamp.
struct LampDot: View {
    let color: Color
    var glow = true
    var size: CGFloat = 8

    init(_ lamp: Lamp, size: CGFloat = 8) { color = lamp.color; glow = lamp.glows; self.size = size }
    init(color: Color, glow: Bool = true, size: CGFloat = 8) { self.color = color; self.glow = glow; self.size = size }

    var body: some View {
        Circle().fill(color).frame(width: size, height: size)
            .shadow(color: glow ? color.opacity(0.8) : .clear, radius: glow ? 4 : 0)
            .accessibilityHidden(true)
    }
}

/// Silkscreen text: the small spaced labels printed on the panel.
struct Silk: View {
    let text: String
    var color: Color = Ink.label
    init(_ text: String, color: Color = Ink.label) { self.text = text; self.color = color }

    var body: some View {
        Text(text.uppercased()).font(Face.mono(10, .medium)).tracking(1).foregroundStyle(color)
    }
}

/// A label with its lamp: "● LIVE 4 S".
struct LampLabel: View {
    let lamp: Color
    let text: String
    var body: some View {
        HStack(spacing: 6) { LampDot(color: lamp); Silk(text) }.accessibilityElement(children: .combine)
    }
}

extension View {
    /// A recessed panel on the faceplate.
    func panel(_ fill: Color = Ink.panel, stroke: Color = Ink.line) -> some View {
        background(fill, in: RoundedRectangle(cornerRadius: 10, style: .continuous))
            .overlay(RoundedRectangle(cornerRadius: 10, style: .continuous).stroke(stroke, lineWidth: 1))
    }
}

/// A hardware key: KEEP, STOP, RELEASE.
struct KeyStyle: ButtonStyle {
    enum Tone { case plain, danger, amber, go }
    var tone: Tone = .plain
    var large = false

    func makeBody(configuration: Configuration) -> some View {
        let (top, bottom, fg, edge): (Color, Color, Color, Color) = {
            switch tone {
            case .plain: return (Color(hex: 0x34383F), Color(hex: 0x2A2E34), Ink.text, Color(hex: 0x454A52))
            case .danger: return (Color(hex: 0x34383F), Color(hex: 0x2A2E34), Color(hex: 0xFF8A8D), Color(hex: 0x6A3236))
            case .amber: return (Color(hex: 0xFCD34D), Color(hex: 0xF5B90F), Ink.bg, Color(hex: 0xC99400))
            case .go: return (Color(hex: 0x6EE7A0), Color(hex: 0x3FCB72), Color(hex: 0x10261A), Color(hex: 0x2E9E58))
            }
        }()
        return configuration.label
            .font(Face.mono(large ? 12 : 11, .semibold)).tracking(0.5)
            .textCase(.uppercase)
            .foregroundStyle(fg)
            .padding(.horizontal, large ? 14 : 10)
            .frame(height: large ? 30 : 24)
            .background(LinearGradient(colors: configuration.isPressed ? [bottom, top] : [top, bottom], startPoint: .top, endPoint: .bottom),
                        in: RoundedRectangle(cornerRadius: 5, style: .continuous))
            .overlay(RoundedRectangle(cornerRadius: 5, style: .continuous).stroke(edge, lineWidth: 1))
            .shadow(color: .black.opacity(0.5), radius: 0, y: 1)
            .offset(y: configuration.isPressed ? 1 : 0)
            .contentShape(Rectangle())
    }
}

extension ButtonStyle where Self == KeyStyle {
    static var key: KeyStyle { KeyStyle() }
    static func key(_ tone: KeyStyle.Tone, large: Bool = false) -> KeyStyle { KeyStyle(tone: tone, large: large) }
}

/// One segment of the LED meter.
enum Segment: Hashable {
    case lit(Lamp), kept, off
}

/// The LED meter: one segment per GB, lit in its state colour.
struct LEDMeter: View {
    let segments: [Segment]
    var height: CGFloat = 56
    var showScale = true

    var body: some View {
        VStack(spacing: 4) {
            HStack(spacing: 4) {
                ForEach(Array(segments.enumerated()), id: \.offset) { _, s in cell(s) }
            }
            .frame(height: height)
            if showScale {
                HStack {
                    ForEach(scaleMarks, id: \.self) { m in
                        Text("\(m)").font(Face.mono(9)).foregroundStyle(Ink.scale)
                        if m != scaleMarks.last { Spacer() }
                    }
                }
            }
        }
        .accessibilityElement()
        .accessibilityLabel("Memory meter")
        .accessibilityValue(summary)
    }

    /// Evenly spaced labels that end at the whole: the marks are spread across the full width,
    /// so the step must divide the count (18 GB: 0, 6, 12, 18; with a step of 4 "16" stood
    /// where 18 is).
    private var scaleMarks: [Int] {
        let n = segments.count
        let parts = [4, 3, 5, 6, 2].first { n % $0 == 0 } ?? 1
        return (0...parts).map { n * $0 / parts }
    }

    @ViewBuilder
    private func cell(_ s: Segment) -> some View {
        let shape = RoundedRectangle(cornerRadius: 3, style: .continuous)
        switch s {
        case .lit(let lamp):
            shape.fill(lamp.color)
                .overlay(alignment: .top) { shape.fill(.white.opacity(0.3)).frame(height: 1).padding(.horizontal, 1) }
                .shadow(color: lamp.glows ? lamp.color.opacity(0.55) : .clear, radius: lamp.glows ? 5 : 0)
        case .kept:
            shape.fill(Ink.off).overlay(
                Canvas { ctx, size in
                    var x: CGFloat = -size.height
                    while x < size.width {
                        var p = Path()
                        p.move(to: CGPoint(x: x, y: size.height))
                        p.addLine(to: CGPoint(x: x + size.height, y: 0))
                        ctx.stroke(p, with: .color(Color(hex: 0x3A3F46)), lineWidth: 2)
                        x += 6
                    }
                }.clipShape(shape))
        case .off:
            shape.fill(Ink.off).overlay(shape.stroke(.black.opacity(0.35), lineWidth: 1))
        }
    }

    private var summary: String {
        var counts: [String: Int] = [:]
        for s in segments {
            switch s {
            case .lit(let l): counts[l.name, default: 0] += 1
            case .kept: counts["held back", default: 0] += 1
            case .off: counts["free", default: 0] += 1
            }
        }
        return counts.sorted { $0.key < $1.key }.map { "\($0.value) GB \($0.key)" }.joined(separator: ", ")
    }
}

/// A three-lamp gauge for memory pressure.
struct PressureLamps: View {
    let level: Int // 1 normal, 2 warning, 4 critical

    var body: some View {
        let lit = level >= 4 ? 3 : level >= 2 ? 2 : 1
        HStack(spacing: 3) {
            ForEach(0..<3) { i in
                let c: Color = [Lamp.inUse.color, Lamp.waiting.color, Lamp.nobody.color][i]
                RoundedRectangle(cornerRadius: 2).fill(i < lit ? c : Ink.off).frame(height: 8)
                    .shadow(color: i == lit - 1 ? c.opacity(0.7) : .clear, radius: 3)
            }
        }
        .accessibilityElement().accessibilityLabel("Pressure").accessibilityValue(["", "normal", "warning", "", "critical"][min(level, 4)])
    }
}

/// A readout tile: GRANT 2.4 GB.
struct Readout: View {
    let label: String
    let value: String
    var unit: String = ""
    var color: Color = Ink.text

    var body: some View {
        VStack(alignment: .leading, spacing: 2) {
            Silk(label)
            HStack(alignment: .firstTextBaseline, spacing: 3) {
                Text(value).font(Face.mono(22, .semibold)).foregroundStyle(color).contentTransition(.numericText())
                if !unit.isEmpty { Text(unit).font(Face.mono(11)).foregroundStyle(Ink.label) }
            }
            .lineLimit(1).minimumScaleFactor(0.7)
        }
        .frame(maxWidth: .infinity, alignment: .leading)
        .padding(10).panel()
        .accessibilityElement(children: .combine)
    }
}

enum Kind {
    static func name(_ kind: String) -> String {
        switch kind {
        case "kind-cluster", "cluster": return "Kind cluster"
        case "containers": return "Containers"
        case "compose": return "Compose stack"
        case "docker-build": return "Docker build"
        case "cargo-build": return "Cargo build"
        case "xcode-build": return "Xcode build"
        case "gradle-daemon": return "Gradle daemon"
        case "gradle": return "Gradle build"
        case "simulator": return "Simulator"
        case "android-emulator", "emulator": return "Android emulator"
        case "server": return "Dev server"
        case "vm": return "Virtual machine"
        case "model-server", "model": return "Model server"
        case "database": return "Database"
        case "headless-browser", "browser": return "Headless browser"
        case "loadtest": return "Load test"
        case "heavy": return "Heavy process"
        case "leftover-process": return "Left behind"
        case "docker-vm": return "Docker's VM"
        case "xcode-helper": return "Xcode helper"
        default: return kind.capitalized
        }
    }
}

enum Fmt {
    private static let relative: RelativeDateTimeFormatter = {
        let f = RelativeDateTimeFormatter()
        f.unitsStyle = .abbreviated
        return f
    }()

    /// "2 min ago", "in 12 min". Static text, so it does not tick every second.
    static func rel(_ d: Date) -> String {
        if abs(d.timeIntervalSinceNow) < 45 { return d < Date() ? "just now" : "in under a minute" }
        return relative.localizedString(for: d, relativeTo: Date())
    }

    /// "14 min", "2 h" since a date.
    static func since(_ d: Date) -> String {
        let s = max(0, -d.timeIntervalSinceNow)
        if s < 60 { return "\(Int(s)) s" }
        if s < 3600 { return "\(Int(s / 60)) min" }
        return "\(Int(s / 3600)) h"
    }

    /// "0:40", "6:00" until a date.
    static func clock(_ d: Date) -> String {
        let s = max(0, Int(d.timeIntervalSinceNow))
        return s >= 3600 ? "\(s / 3600):\(String(format: "%02d", (s % 3600) / 60)) h" : "\(s / 60):\(String(format: "%02d", s % 60))"
    }

    static func gb(_ mb: Double) -> String { String(format: "%.1f", max(0, mb) / 1024) }

    /// A size with its unit: "30 MB" under a GB, "1.2 GB" from there.
    static func size(_ mb: Double) -> String {
        let v = max(0, mb)
        return v.rounded() < 1000 ? "\(Int(v.rounded())) MB" : String(format: "%.1f GB", v / 1024)
    }

    static func when(_ d: Date) -> String {
        if Calendar.current.isDateInToday(d) { return "Today " + d.formatted(date: .omitted, time: .shortened) }
        if Calendar.current.isDateInYesterday(d) { return "Yesterday " + d.formatted(date: .omitted, time: .shortened) }
        return d.formatted(.dateTime.day().month(.abbreviated).hour().minute())
    }
}
