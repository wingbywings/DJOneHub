import Foundation

public struct OutgoingSMS: Codable, Equatable, Identifiable, Sendable {
  public let id: String
  public let module_id: String
  public let phone: String
  public let message: String
  public let created_at: Date

  public init(module: String, phone: String, message: String, now: Date = Date()) throws {
    guard let number = Self.normalizePhone(phone), Wire.id(module), Self.segmentCount(message) > 0,
      Self.segmentCount(message) <= 10, !message.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty,
      !message.contains("\0") else { throw PhoneError.invalidData }
    self.id = UUID().uuidString.lowercased(); self.module_id = module
    self.phone = number; self.message = message
    self.created_at = Date(timeIntervalSince1970: floor(now.timeIntervalSince1970))
  }

  public static func normalizePhone(_ value: String) -> String? {
    guard let number = cleanPhone(value) else { return nil }
    if number.hasPrefix("+") { return number }
    var mobile = number
    if number.count == 15 && number.hasPrefix("0086") { mobile = String(number.dropFirst(4)) }
    else if number.count == 13 && number.hasPrefix("86") { mobile = String(number.dropFirst(2)) }
    let digits = Array(mobile.utf8)
    if digits.count == 11 && digits[0] == 49 && (51...57).contains(digits[1]) { return "+86" + mobile }
    return number
  }

  private static func cleanPhone(_ value: String) -> String? {
    let number = value.filter { !" ()-\t\r\n".contains($0) }
    let digits = number.hasPrefix("+") ? String(number.dropFirst()) : number
    guard (1...20).contains(digits.utf8.count), digits.utf8.allSatisfy({ (48...57).contains($0) }) else { return nil }
    return number
  }

  // Matches Hub policy: non-ASCII is UTF-16; GSM extension characters use two
  // septets. The Hub verifies the actual PDU count before accepting the job.
  public static func segmentCount(_ message: String) -> Int {
    guard !message.isEmpty else { return 0 }
    let extensions = Set("^{}\\[~]|\u{000C}".unicodeScalars)
    var septets = 0
    let gsm = message.unicodeScalars.allSatisfy { scalar in
      if extensions.contains(scalar) { septets += 2; return true }
      if scalar.value == 10 || scalar.value == 13 || (32...126).contains(scalar.value) && scalar.value != 96 {
        septets += 1; return true
      }
      return false
    }
    if gsm { return septets <= 160 ? 1 : (septets + 152) / 153 }
    let units = message.utf16.count
    return units <= 70 ? 1 : (units + 66) / 67
  }

  public func validate() throws {
    // Previously saved requests must retain their original number and digest
    // when querying a result; only newly created requests are normalized here.
    guard UUID(uuidString: id)?.uuidString.lowercased() == id, Wire.id(module_id),
      Self.cleanPhone(phone) == phone, message.utf8.count <= 8192,
      !message.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty, !message.contains("\0"),
      (1...10).contains(Self.segmentCount(message)) else { throw PhoneError.invalidData }
  }
}

public struct OutgoingSMSResult: Codable, Equatable, Sendable {
  public let id: String
  public let installation_id: String
  public let state: String
  public let segments: Int
  public let code: String

  public func validate(request: OutgoingSMS, principal: Principal) throws {
    guard id == request.id, installation_id == principal.id, (0...10).contains(segments),
      ["queued", "sending", "sent", "failed", "unknown"].contains(state), code.utf8.count <= 80,
      state != "sent" || (segments > 0 && code.isEmpty) else { throw PhoneError.invalidData }
  }
}
