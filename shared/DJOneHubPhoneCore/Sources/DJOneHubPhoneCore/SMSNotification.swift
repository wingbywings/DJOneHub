import Foundation

public struct SMSNotification: Codable, Identifiable, Equatable, Sendable {
  public let id: String
  public let hub_id: String
  public let module_id: String
  public let installation_id: String
  public let sender: String
  public var content: String
  public let timestamp: Date

  public static func decode(_ data: Data, principal: Principal) throws -> Self {
    struct Payload: Decodable {
      struct APS: Decodable {
        struct Alert: Decodable { let body: String }
        let alert: Alert
      }
      let aps: APS
      var sms: SMSNotification
    }
    guard data.count <= 8192 else { throw PhoneError.invalidData }
    var payload = try Wire.decoder().decode(Payload.self, from: data)
    payload.sms.content = payload.aps.alert.body
    let sms = payload.sms
    guard sms.id.utf8.count == 64,
      sms.id.utf8.allSatisfy({ (48...57).contains($0) || (97...102).contains($0) }),
      sms.hub_id == principal.hub_id, sms.installation_id == principal.id,
      principal.modules.contains(sms.module_id), sms.sender.utf8.count <= 256,
      sms.content.utf8.count <= 4096
    else { throw PhoneError.denied }
    return sms
  }
}
