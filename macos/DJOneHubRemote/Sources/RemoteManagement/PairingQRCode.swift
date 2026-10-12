import CoreImage.CIFilterBuiltins
import DJOneHubPhoneCore
import Foundation

public enum PairingQRCode {
  public static func image(_ data: Data) throws -> CGImage {
    _ = try PairingFile.parse(data)
    let filter = CIFilter.qrCodeGenerator()
    filter.message = data
    filter.correctionLevel = "M"
    guard let output = filter.outputImage else { throw ManagementError.invalid }
    let scaled = output.transformed(by: .init(scaleX: 4, y: 4))
    guard let image = CIContext().createCGImage(scaled, from: scaled.extent) else { throw ManagementError.invalid }
    return image
  }
}
