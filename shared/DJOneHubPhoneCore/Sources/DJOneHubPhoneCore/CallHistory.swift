import Foundation

/// Local display history only. Never used to authorize or resume a call.
public struct CallHistory: Codable, Sendable {
  public struct Entry: Codable, Identifiable, Equatable, Sendable {
    public let id: UUID
    public let module: String
    public let display: String
    public let outgoing: Bool
    public let started: Date
    public var connected: Date?
    public var ended: Date?
    public var result: String?
    public var duration: Int {
      let seconds = (ended ?? started).timeIntervalSince(connected ?? ended ?? started)
      return seconds.isFinite ? Int(min(31536000, max(0, seconds))) : 0
    }
  }
  public private(set) var entries: [Entry] = []
  public init() {}
  public mutating func begin(id: UUID, module: String, display: String, outgoing: Bool, now: Date = Date()) {
    guard !entries.contains(where: { $0.id == id }) else { return }
    entries.insert(Entry(id: id, module: String(module.prefix(128)), display: String(display.prefix(128)),
      outgoing: outgoing, started: now), at: 0)
    entries = Array(entries.prefix(100))
  }
  public mutating func identify(id: UUID, module: String, display: String) {
    guard let i = entries.firstIndex(where: { $0.id == id }), entries[i].ended == nil,
      entries[i].module == module else { return }
    let e = entries[i]
    entries[i] = Entry(id: e.id, module: e.module, display: String(display.prefix(128)), outgoing: e.outgoing,
      started: e.started, connected: e.connected, ended: e.ended, result: e.result)
  }
  public mutating func connected(id: UUID, now: Date = Date()) {
    guard let i = entries.firstIndex(where: { $0.id == id }), entries[i].ended == nil,
      entries[i].connected == nil else { return }
    entries[i].connected = max(now, entries[i].started)
  }
  public mutating func finish(id: UUID, result: String, now: Date = Date()) {
    guard let i = entries.firstIndex(where: { $0.id == id }), entries[i].ended == nil else { return }
    entries[i].ended = max(now, entries[i].connected ?? entries[i].started)
    entries[i].result = String(result.prefix(128))
  }
  public mutating func clearCompleted() { entries.removeAll { $0.ended != nil } }
  public static func restore(_ data: Data, now: Date = Date()) throws -> Self {
    guard data.count <= 262144 else { throw PhoneError.invalidData }
    var value = try JSONDecoder().decode(Self.self, from: data)
    guard value.entries.count <= 100, Set(value.entries.map(\.id)).count == value.entries.count,
      value.entries.allSatisfy({ entry in
        entry.module.count <= 128 && entry.display.count <= 128 && (entry.result?.count ?? 0) <= 128
          && (entry.connected == nil || entry.connected! >= entry.started)
          && (entry.ended == nil || entry.ended! >= (entry.connected ?? entry.started))
      }) else { throw PhoneError.invalidData }
    for entry in value.entries where entry.ended == nil {
      value.finish(id: entry.id, result: "App 退出，结束状态未确认", now: now)
    }
    return value
  }
}
