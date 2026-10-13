import Combine
import DJOneHubPhoneCore
import Foundation
import SwiftUI

struct PhoneContact: Codable, Identifiable, Equatable {
  var name: String
  var number: String
  var favorite: Bool = false
  var id: String { number }
}

@MainActor final class PhoneContactsStore: ObservableObject {
  @Published private(set) var saved: [PhoneContact] = []
  @Published private(set) var error = ""
  private let url: URL
  private var writable = true

  init(url: URL? = nil) {
    self.url = url ?? FileManager.default.urls(for: .applicationSupportDirectory, in: .userDomainMask)[0]
      .appendingPathComponent("phone-contacts.json")
    guard FileManager.default.fileExists(atPath: self.url.path) else { return }
    do {
      let file = try FileHandle(forReadingFrom: self.url)
      defer { try? file.close() }
      let data = try file.read(upToCount: 262145) ?? Data()
      guard data.count <= 262144 else { throw PhoneError.invalidData }
      let contacts = try JSONDecoder().decode([PhoneContact].self, from: data)
      guard contacts.count <= 500, Set(contacts.map(\.number)).count == contacts.count,
        contacts.allSatisfy({ PhoneCall.dialNumber($0.number) == $0.number && !$0.name.isEmpty && $0.name.count <= 64 })
      else { throw PhoneError.invalidData }
      saved = contacts
    } catch {
      writable = false
      self.error = "本机联系人无法读取，已保留原文件，请检查本机存储"
    }
  }

  func all(history: CallHistory) -> [PhoneContact] {
    var byNumber = Dictionary(uniqueKeysWithValues: saved.map { ($0.number, $0) })
    for entry in history.entries {
      guard let number = PhoneCall.dialNumber(entry.display), byNumber[number] == nil else { continue }
      byNumber[number] = PhoneContact(name: number, number: number)
    }
    return byNumber.values.sorted {
      if $0.favorite != $1.favorite { return $0.favorite }
      return $0.name.localizedStandardCompare($1.name) == .orderedAscending
    }
  }

  func name(for display: String) -> String {
    guard let number = PhoneCall.dialNumber(display) else { return display }
    return saved.first(where: { $0.number == number })?.name ?? display
  }

  @discardableResult func save(_ contact: PhoneContact, replacing oldNumber: String? = nil) -> Bool {
    guard writable else { return false }
    guard let number = PhoneCall.dialNumber(contact.number), !contact.name.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty,
      contact.name.count <= 64 else {
      error = "请输入姓名和有效电话号码"
      return false
    }
    var next = saved.filter { $0.number != number && $0.number != oldNumber }
    next.append(PhoneContact(name: contact.name.trimmingCharacters(in: .whitespacesAndNewlines), number: number, favorite: contact.favorite))
    guard next.count <= 500 else { error = "最多可保存 500 位联系人"; return false }
    return commit(next)
  }

  private func commit(_ next: [PhoneContact]) -> Bool {
    do {
      try FileManager.default.createDirectory(at: url.deletingLastPathComponent(), withIntermediateDirectories: true)
      try JSONEncoder().encode(next).write(to: url, options: [.atomic, .completeFileProtectionUntilFirstUserAuthentication])
      var file = url
      var values = URLResourceValues()
      values.isExcludedFromBackup = true
      try file.setResourceValues(values)
      saved = next
      error = ""
      return true
    } catch {
      self.error = "联系人保存失败，请检查本机存储后重试"
      return false
    }
  }
}

struct PhoneContactsView: View {
  @ObservedObject var runtime: PhoneRuntime
  @ObservedObject var contacts: PhoneContactsStore
  var dial: (String, String?) -> Void
  @State private var query = ""
  @State private var editing: PhoneContact?
  @State private var adding = false
  var allowsDial = true

  private var all: [PhoneContact] { contacts.all(history: runtime.history) }
  private var filtered: [PhoneContact] {
    let search = query.trimmingCharacters(in: .whitespacesAndNewlines)
    return all.filter { search.isEmpty || $0.name.localizedCaseInsensitiveContains(search) || $0.number.contains(search) }
  }

  var body: some View {
    ScrollView {
      VStack(spacing: 12) {
        HStack {
          PageHeading(title: "联系人", subtitle: "给家人打个电话吧")
          RoundIconButton(icon: "plus", label: "添加联系人") { adding = true }
        }
        HStack(spacing: 9) {
          Image(systemName: "magnifyingglass").foregroundStyle(PhoneTheme.muted)
          TextField("搜索姓名或号码", text: $query).textInputAutocapitalization(.never)
            .autocorrectionDisabled().font(.system(size: 15))
          if !query.isEmpty {
            Button { query = "" } label: { Image(systemName: "xmark.circle.fill").foregroundStyle(PhoneTheme.muted) }
              .accessibilityLabel("清除搜索")
          }
        }.padding(14).phoneGlass(radius: 30)

        if query.isEmpty && all.contains(where: \.favorite) {
          SectionCaption(title: "常用联系人")
          ScrollView(.horizontal) {
            HStack(spacing: 18) {
              ForEach(all.filter(\.favorite)) { contact in
                Button {
                  if allowsDial { dial(contact.number, nil) } else { editing = contact }
                } label: {
                  VStack(spacing: 7) {
                    PhoneAvatar(name: contact.name, size: 60)
                    Text(contact.name).font(.system(size: 12, weight: .semibold)).lineLimit(1).frame(width: 64)
                  }
                }.buttonStyle(PhonePressStyle()).accessibilityLabel(allowsDial ? "呼叫\(contact.name)" : contact.name)
              }
            }.padding(.vertical, 5).padding(.horizontal, 4)
          }.scrollIndicators(.hidden)
        }

        SectionCaption(title: "全部联系人")
        if filtered.isEmpty {
          EmptyPhoneState(icon: "person.crop.circle.badge.plus", title: all.isEmpty ? "还没有联系人" : "没有匹配的联系人",
                          message: all.isEmpty ? "点击右上角添加家人，通话记录中的号码也会显示在这里。" : "试试其他姓名或号码")
        }
        LazyVStack(spacing: 10) {
          ForEach(filtered) { contact in
            HStack(spacing: 12) {
              Button { editing = contact } label: {
                HStack(spacing: 12) {
                  PhoneAvatar(name: contact.name)
                  VStack(alignment: .leading, spacing: 4) {
                    HStack(spacing: 5) {
                      Text(contact.name).font(.system(size: 16, weight: .bold)).lineLimit(1)
                      if contact.favorite {
                        Image(systemName: "star.fill").font(.system(size: 10)).foregroundStyle(PhoneTheme.amber)
                      }
                    }
                    Text(contact.number).font(.system(size: 12)).foregroundStyle(PhoneTheme.secondary).lineLimit(1)
                  }.frame(maxWidth: .infinity, alignment: .leading)
                }.contentShape(Rectangle())
              }.buttonStyle(PhonePressStyle()).accessibilityLabel("编辑联系人 \(contact.name)")
              if allowsDial {
                RoundIconButton(icon: "phone.fill", label: "呼叫\(contact.name)", color: PhoneTheme.green,
                                background: PhoneTheme.greenSoft) { dial(contact.number, nil) }
              }
            }.padding(14).phoneGlass(radius: 20)
          }
        }
        if !contacts.error.isEmpty { Text(contacts.error).font(.caption).foregroundStyle(PhoneTheme.red) }
        Text("联系人仅保存在此 iPhone").font(.caption).foregroundStyle(PhoneTheme.secondary).padding(.vertical, 10)
      }.padding(.horizontal, 22).padding(.bottom, 18)
    }.scrollIndicators(.hidden).scrollDismissesKeyboard(.interactively)
      .sheet(isPresented: $adding) { ContactEditor(contacts: contacts, contact: nil) }
      .sheet(item: $editing) { ContactEditor(contacts: contacts, contact: $0) }
      .onChange(of: runtime.callUUID) { _, value in
        if value != nil && allowsDial { adding = false; editing = nil }
      }
  }
}

private struct ContactEditor: View {
  @ObservedObject var contacts: PhoneContactsStore
  var contact: PhoneContact?
  @Environment(\.dismiss) private var dismiss
  @State private var name = ""
  @State private var number = ""
  @State private var favorite = true

  var body: some View {
    NavigationStack {
      ZStack {
        AppBackground()
        ScrollView {
          VStack(alignment: .leading, spacing: 18) {
            PhoneAvatar(name: name, size: 80).frame(maxWidth: .infinity).padding(.vertical, 12)
            VStack(alignment: .leading, spacing: 10) {
              Text("姓名").font(.caption).foregroundStyle(PhoneTheme.secondary)
              TextField("例如：妈妈", text: $name).textContentType(.name)
              Divider()
              Text("电话号码").font(.caption).foregroundStyle(PhoneTheme.secondary)
              TextField("电话号码", text: $number).keyboardType(.phonePad).textContentType(.telephoneNumber)
              Divider()
              Toggle("常用联系人", isOn: $favorite).tint(PhoneTheme.green)
            }.padding(20).phoneGlass()
            if !contacts.error.isEmpty { Text(contacts.error).font(.caption).foregroundStyle(PhoneTheme.red) }
            Text("仅保存在此 iPhone，相同号码会合并为一位联系人。")
              .font(.caption).foregroundStyle(PhoneTheme.secondary)
          }.padding(22)
        }
      }.navigationTitle(contact == nil ? "添加联系人" : "编辑联系人").navigationBarTitleDisplayMode(.inline)
        .toolbar {
          ToolbarItem(placement: .cancellationAction) { Button("取消") { dismiss() } }
          ToolbarItem(placement: .confirmationAction) {
            Button("保存") {
              if contacts.save(PhoneContact(name: name, number: number, favorite: favorite), replacing: contact?.number) { dismiss() }
            }.disabled(name.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty || name.count > 64 || PhoneCall.dialNumber(number) == nil)
          }
        }
    }.tint(PhoneTheme.violet).foregroundStyle(PhoneTheme.ink).preferredColorScheme(.light)
      .onAppear { name = contact?.name ?? ""; number = contact?.number ?? ""; favorite = contact?.favorite ?? true }
  }
}
