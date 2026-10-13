import DJOneHubPhoneCore
import SwiftUI

extension CallHistory.Entry {
  var isMissed: Bool { !outgoing && connected == nil && ended != nil && result != "已拒接" }
  var isUnanswered: Bool { connected == nil && ended != nil }
  var summary: String {
    let direction = outgoing ? "呼出" : "呼入"
    guard ended != nil else { return "\(direction) · 通话进行中" }
    let result = result ?? "通话已结束"
    guard connected != nil else { return "\(direction) · \(result)" }
    let duration = duration < 60 ? "\(duration) 秒" : "\(duration / 60) 分 \(duration % 60) 秒"
    return "\(direction) · \(result) · \(duration)"
  }
}

struct PhoneRecentsView: View {
  @ObservedObject var runtime: PhoneRuntime
  @ObservedObject var contacts: PhoneContactsStore
  var clear: () -> Void
  var dial: (String, String?) -> Void
  @State private var missedOnly = false

  private var groups: [(date: Date, entries: [CallHistory.Entry])] {
    let entries = runtime.history.entries.filter { !missedOnly || $0.isMissed }
    return Dictionary(grouping: entries) { Calendar.current.startOfDay(for: $0.started) }
      .map { (date: $0.key, entries: $0.value.sorted { $0.started > $1.started }) }.sorted { $0.date > $1.date }
  }

  var body: some View {
    ScrollView {
      VStack(spacing: 12) {
        PageHeading(title: "通话记录", subtitle: "仅保存在此 iPhone")
        HStack(spacing: 4) {
          filterButton("全部", missed: false)
          filterButton("未接", missed: true)
        }.padding(4).phoneGlass(radius: 30)
        if groups.isEmpty {
          EmptyPhoneState(icon: "clock", title: missedOnly ? "没有未接来电" : "暂无通话记录",
                          message: "你的通话记录会显示在这里")
        }
        LazyVStack(spacing: 10) {
          ForEach(groups, id: \.date) { group in
            SectionCaption(title: dayTitle(group.date))
            ForEach(group.entries) { entry in recentRow(entry) }
          }
        }
        if !runtime.historyError.isEmpty {
          Text(runtime.historyError).font(.caption).foregroundStyle(PhoneTheme.red)
        }
        if runtime.history.entries.contains(where: { $0.ended != nil }) || !runtime.historyError.isEmpty {
          Button(action: clear) {
            Label("清除已结束的通话记录", systemImage: "trash")
              .font(.system(size: 15, weight: .bold)).foregroundStyle(PhoneTheme.red)
              .frame(maxWidth: .infinity).padding(.vertical, 15).background(PhoneTheme.redSoft, in: Capsule())
          }.buttonStyle(PhonePressStyle()).padding(.top, 12)
        }
      }.padding(.horizontal, 22).padding(.bottom, 22)
    }.scrollIndicators(.hidden)
  }

  private func filterButton(_ title: String, missed: Bool) -> some View {
    Button { missedOnly = missed } label: {
      Text(title).font(.system(size: 14, weight: .bold)).frame(maxWidth: .infinity).padding(.vertical, 10)
        .background(missedOnly == missed ? .white : .clear, in: Capsule())
        .foregroundStyle(missedOnly == missed ? PhoneTheme.ink : PhoneTheme.secondary)
    }.buttonStyle(PhonePressStyle()).accessibilityAddTraits(missedOnly == missed ? [.isSelected] : [])
  }

  private func recentRow(_ entry: CallHistory.Entry) -> some View {
    let color = entry.isMissed ? PhoneTheme.red : entry.isUnanswered && entry.outgoing ? PhoneTheme.amber
      : entry.outgoing ? PhoneTheme.green : PhoneTheme.violet
    return HStack(spacing: 12) {
      Image(systemName: entry.outgoing ? "arrow.up.right" : "arrow.down.left")
        .font(.system(size: 19, weight: .semibold)).foregroundStyle(color).frame(width: 42, height: 42)
        .background(color.opacity(0.1), in: RoundedRectangle(cornerRadius: 14))
      VStack(alignment: .leading, spacing: 5) {
        Text(contacts.name(for: entry.display)).font(.system(size: 16, weight: .bold))
          .foregroundStyle(entry.isMissed ? PhoneTheme.red : PhoneTheme.ink).lineLimit(1)
        Text(runtime.moduleName(for: entry.module)).font(.system(size: 10, weight: .semibold)).foregroundStyle(PhoneTheme.violet)
          .lineLimit(1).padding(.horizontal, 8).padding(.vertical, 3).background(PhoneTheme.violetSoft, in: Capsule())
        Text(entry.summary).font(.system(size: 12)).foregroundStyle(PhoneTheme.secondary).fixedSize(horizontal: false, vertical: true)
      }.frame(maxWidth: .infinity, alignment: .leading)
      VStack(spacing: 7) {
        Text(entry.started, format: .dateTime.hour().minute()).font(.system(size: 12)).foregroundStyle(PhoneTheme.secondary)
        RoundIconButton(icon: "phone.fill", label: "回拨\(contacts.name(for: entry.display))", color: PhoneTheme.green,
                        background: PhoneTheme.greenSoft) { dial(entry.display, entry.module) }
          .disabled(PhoneCall.dialNumber(entry.display) == nil || entry.ended == nil)
      }
    }.padding(14).phoneGlass(radius: 20)
  }

  private func dayTitle(_ date: Date) -> String {
    if Calendar.current.isDateInToday(date) { return "今天" }
    if Calendar.current.isDateInYesterday(date) { return "昨天" }
    return date.formatted(.dateTime.year().month().day())
  }
}
