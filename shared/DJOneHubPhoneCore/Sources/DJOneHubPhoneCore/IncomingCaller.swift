import Foundation

/// Presentation only: a push hint's display never authorizes call control.
public struct IncomingCaller: Sendable {
  public let display: String
  public let phoneNumber: String?

  public func localizedName(moduleName raw: String?) -> String {
    let line = (raw ?? "").trimmingCharacters(in: .whitespacesAndNewlines)
    // A disambiguated line may include both the 80-scalar remark and module ID.
    guard !line.isEmpty, line.utf8.count <= 512,
      !line.unicodeScalars.contains(where: { CharacterSet.controlCharacters.contains($0) })
    else { return display }
    return "\(display) · \(line)"
  }

  public init(display raw: String?) {
    let text = (raw ?? "").trimmingCharacters(in: .whitespacesAndNewlines)
    if text.isEmpty || text.utf8.count > 128
      || text.unicodeScalars.contains(where: { CharacterSet.controlCharacters.contains($0) })
    {
      display = "DJOneHub"
      phoneNumber = nil
    } else {
      display = text
      phoneNumber = PhoneCall.dialNumber(text)
    }
  }
}
