import SwiftUI

/// The auto-stop levels, each with its evidence: owner-gone first, then unclaimed, then idle
/// leases. A level can be turned on when its own reviewed record says so (or anyway, with a
/// warning), and only after the one above it; a wrong stop turns it off again by itself.
struct TrustLevelsView: View {
    @EnvironmentObject var store: Store

    var body: some View {
        let levels = store.status?.trust_levels ?? []
        VStack(alignment: .leading, spacing: 10) {
            Text("AUTO-STOP, LEVEL BY LEVEL").font(Face.mono(11, .semibold)).tracking(1).foregroundStyle(Ink.label)
            if store.status?.config.dry_run == false {
                Text("Auto-stop is on for everything. Turn it off in the menu to go back to levels.")
                    .font(Face.body(13)).foregroundStyle(Ink.dim)
            }
            VStack(spacing: 0) {
                ForEach(Array(levels.enumerated()), id: \.element.id) { i, lv in
                    row(lv, number: i + 1)
                        .overlay(alignment: .top) { if i > 0 { Rectangle().fill(Ink.hair).frame(height: 1) } }
                }
            }
            .panel()
            Text("A level is ready when at least 5 of its decisions, over at least 7 days, have been reviewed and under 2% were wrong. If you mark one of its stops wrong, it turns itself off.")
                .font(Face.body(12)).foregroundStyle(Ink.label).fixedSize(horizontal: false, vertical: true)
        }
    }

    private func row(_ lv: TrustLevel, number: Int) -> some View {
        HStack(alignment: .top, spacing: 12) {
            Text("\(number)").font(Face.mono(15, .bold)).foregroundStyle(lv.on ? Lamp.inUse.color : Ink.label).frame(width: 18)
            VStack(alignment: .leading, spacing: 3) {
                HStack(spacing: 8) {
                    Text(lv.title).font(Face.body(15, .semibold))
                    if lv.on {
                        LampLabel(lamp: Lamp.inUse.color, text: "Stops on its own")
                    } else if lv.eligible {
                        LampLabel(lamp: Lamp.waiting.color, text: "Ready")
                    }
                }
                Text(lv.detail.prefix(1).uppercased() + lv.detail.dropFirst() + ".").font(Face.body(12)).foregroundStyle(Ink.dim)
                Text(lv.evidence).font(Face.mono(11)).foregroundStyle(Ink.label)
                if let b = lv.blocked, !lv.on { Text(b.prefix(1).uppercased() + b.dropFirst() + ".").font(Face.body(11)).foregroundStyle(Ink.label) }
            }
            Spacer()
            if store.status?.config.dry_run != false {
                if lv.on {
                    Button("Turn off") { store.setLevel(lv.category, on: false) }.buttonStyle(.key)
                } else if lv.blocked == nil {
                    Button(lv.eligible ? "Turn on" : "Turn on anyway") { store.setLevel(lv.category, on: true) }
                        .buttonStyle(.key(lv.eligible ? .go : .danger))
                }
            }
        }
        .padding(.horizontal, 14).padding(.vertical, 12)
        .accessibilityElement(children: .combine)
    }
}

/// The week in one panel: what AgentRM freed, found, made wait and restored.
struct WeekTab: View {
    @EnvironmentObject var store: Store

    var body: some View {
        ScrollView {
            VStack(alignment: .leading, spacing: 14) {
                if let r = store.receipt {
                    Text(r.text).font(Face.body(20, .semibold)).fixedSize(horizontal: false, vertical: true)
                    HStack(spacing: 10) {
                        Readout(label: "Freed", value: Fmt.gb(r.freed_gb * 1024), unit: "GB", color: Lamp.inUse.color)
                        Readout(label: "Would free (dry run)", value: Fmt.gb(r.would_free_gb * 1024), unit: "GB")
                        Readout(label: "Left behind found", value: "\(r.leftovers)", unit: "processes",
                                color: r.leftovers > 0 ? Lamp.nobody.color : Ink.text)
                        Readout(label: "Waited for memory", value: "\(r.queued)", unit: r.queued > 0 ? "avg \(seconds(r.wait_mean))" : "requests")
                    }
                    VStack(spacing: 0) {
                        line("Sessions that ended", "\(r.sessions_ended)")
                        line("Paused", "\(r.paused)")
                        line("Stopped", "\(r.stopped)")
                        line("Restored afterwards", "\(r.restored)")
                        line("Decisions you marked wrong", "\(r.wrong)", warn: r.wrong > 0)
                        line("Agents' answers that proved untrue", "\(r.misreports)", warn: r.misreports > 0)
                        line("Longest wait for memory", seconds(r.wait_max))
                    }
                    .panel()
                    Text("\(r.since.formatted(date: .abbreviated, time: .omitted)) to \(r.until.formatted(date: .abbreviated, time: .omitted)). Counted from AgentRM's decision log and events log; once a week it arrives as a notification.")
                        .font(Face.body(12)).foregroundStyle(Ink.label)
                } else {
                    Text("Reading the week…").font(Face.body(15)).foregroundStyle(Ink.dim)
                }
            }
            .padding(22)
        }
        .task { await store.refreshReceipt() }
    }

    private func seconds(_ ns: Int64) -> String {
        let s = Int(ns / 1_000_000_000)
        return s >= 60 ? "\(s / 60) min \(s % 60) s" : "\(s) s"
    }

    private func line(_ label: String, _ value: String, warn: Bool = false) -> some View {
        HStack {
            Text(label).font(Face.body(14))
            Spacer()
            Text(value).font(Face.mono(14, .semibold)).foregroundStyle(warn ? Lamp.nobody.color : Ink.text)
        }
        .padding(.horizontal, 14).padding(.vertical, 9)
        .overlay(alignment: .top) { Rectangle().fill(Ink.hair).frame(height: 1) }
    }
}
