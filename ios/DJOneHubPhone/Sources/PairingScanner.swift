import AVFoundation
import DJOneHubPhoneCore
import SwiftUI
import VisionKit

struct PairingScanner: View {
  @ObservedObject var runtime: PhoneRuntime
  @Environment(\.dismiss) private var dismiss
  @Environment(\.scenePhase) private var phase
  @State private var permitted = false
  @State private var message = "正在请求相机权限"
  @State private var data: Data?
  @State private var preview: PairingFile?
  var body: some View {
    NavigationStack {
      VStack(spacing: 16) {
        if let preview, let data {
          Text("确认配对服务器").font(.headline)
          Text(preview.relay_url).textSelection(.enabled)
          Text("授权模块：" + preview.grant.modules.joined(separator: ", "))
          Text("有效期至：" + preview.grant.expires_at.formatted()).font(.caption)
          Text("请与 Mac 上的服务器和模块核对后再配对。")
          Button("确认配对") {
            Task { await runtime.pair(data: data); dismiss() }
          }.buttonStyle(.borderedProminent).disabled(runtime.working || runtime.paired)
          Button("重新扫描") { self.data = nil; self.preview = nil }.disabled(runtime.working)
        } else if permitted && phase == .active {
          QRReader { value in
            guard self.preview == nil, value.utf8.count <= 8192 else { return }
            let candidate = Data(value.utf8)
            do {
              preview = try PairingFile.parse(candidate)
              data = candidate
            } catch { message = "二维码无效或已过期，请在 Mac 重新生成" }
          } onFailure: { message = "相机不可用，请使用配对文件导入" }
          Text(message).font(.caption)
        } else { Text(message) }
      }
      .padding().navigationTitle("扫描 Hub 配对码")
      .toolbar { Button("取消") { dismiss() }.disabled(runtime.working) }
    }
    .interactiveDismissDisabled(runtime.working)
    .task {
      guard DataScannerViewController.isSupported else {
        message = "此设备不支持扫码，请使用配对文件导入"
        return
      }
      let allowed = await AVCaptureDevice.requestAccess(for: .video)
      permitted = allowed && DataScannerViewController.isAvailable
      message = permitted ? "将 Mac 上的配对二维码放入取景框" : "相机不可用，请在系统设置允许相机或导入配对文件"
    }
    .onChange(of: phase) { _, value in
      if value != .active { data = nil; preview = nil }
    }
  }
}

private struct QRReader: UIViewControllerRepresentable {
  var onValue: (String) -> Void
  var onFailure: () -> Void
  func makeCoordinator() -> Coordinator { Coordinator(onValue: onValue, onFailure: onFailure) }
  func makeUIViewController(context: Context) -> DataScannerViewController {
    let scanner = DataScannerViewController(recognizedDataTypes: [.barcode(symbologies: [.qr])],
      qualityLevel: .balanced, recognizesMultipleItems: false, isHighFrameRateTrackingEnabled: false,
      isHighlightingEnabled: true)
    scanner.delegate = context.coordinator
    do { try scanner.startScanning() } catch { DispatchQueue.main.async { onFailure() } }
    return scanner
  }
  func updateUIViewController(_ uiViewController: DataScannerViewController, context: Context) {}
  static func dismantleUIViewController(_ controller: DataScannerViewController, coordinator: Coordinator) {
    controller.stopScanning()
    controller.delegate = nil
  }
  final class Coordinator: NSObject, DataScannerViewControllerDelegate {
    let onValue: (String) -> Void
    let onFailure: () -> Void
    init(onValue: @escaping (String) -> Void, onFailure: @escaping () -> Void) {
      self.onValue = onValue; self.onFailure = onFailure
    }
    func dataScanner(_ scanner: DataScannerViewController, didAdd addedItems: [RecognizedItem], allItems: [RecognizedItem]) {
      read(addedItems)
    }
    func dataScanner(_ scanner: DataScannerViewController, didUpdate updatedItems: [RecognizedItem], allItems: [RecognizedItem]) {
      read(updatedItems)
    }
    private func read(_ items: [RecognizedItem]) {
      for case .barcode(let barcode) in items {
        if let value = barcode.payloadStringValue { onValue(value) }
      }
    }
    func dataScanner(_ scanner: DataScannerViewController, becameUnavailableWithError error: DataScannerViewController.ScanningUnavailable) { onFailure() }
  }
}
