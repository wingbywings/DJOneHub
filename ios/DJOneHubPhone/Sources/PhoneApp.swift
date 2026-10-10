import AVFAudio
import SwiftUI
import UniformTypeIdentifiers

final class PhoneApplicationDelegate: NSObject, UIApplicationDelegate {
  func application(
    _ application: UIApplication,
    didFinishLaunchingWithOptions options: [UIApplication.LaunchOptionsKey: Any]? = nil
  ) -> Bool {
    PhoneRuntime.shared.startPushIfEnabled()
    return true
  }
}
@main struct DJOneHubPhoneApp: App {
  @UIApplicationDelegateAdaptor(PhoneApplicationDelegate.self) private var delegate
  @StateObject private var runtime = PhoneRuntime.shared
  @Environment(\.scenePhase) private var phase
  @State private var importing = false
  @State private var forgetting = false
  var body: some Scene {
    WindowGroup {
      NavigationStack {
        Form {
          Section("连接状态") {
            Label(runtime.status, systemImage: runtime.online ? "phone.connection.fill" : "phone")
            if !runtime.installation.isEmpty {
              Text(runtime.installation).font(.caption).textSelection(.enabled)
            }
          }
          if runtime.callUUID != nil {
            Section("当前通话") {
              Text(runtime.callDisplay).font(.title2.bold())
              Text("模块：\(runtime.callModule)").font(.caption)
              if let started = runtime.connectedAt {
                Text(started, style: .timer).monospacedDigit()
              }
              if runtime.callPhase == "incoming" {
                Button {
                  runtime.answerCall()
                } label: {
                  Label("接听", systemImage: "phone.fill")
                }.buttonStyle(.borderedProminent).tint(.green).disabled(runtime.callControlPending)
              }
              if runtime.connectedAt != nil {
                Button {
                  runtime.toggleMuted()
                } label: {
                  Label(
                    runtime.muted ? "取消静音" : "静音",
                    systemImage: runtime.muted ? "mic.slash.fill" : "mic.fill")
                }.disabled(runtime.callControlPending || runtime.callPhase == "ending")
                Button {
                  runtime.toggleSpeaker()
                } label: {
                  Label(runtime.speaker ? "切回听筒" : "扬声器", systemImage: "speaker.wave.2.fill")
                }.disabled(runtime.callPhase == "ending")
              }
              Button(role: .destructive) {
                runtime.endCall()
              } label: {
                Label(
                  runtime.callPhase == "incoming"
                    ? "拒接"
                    : runtime.callPhase == "ending"
                      ? "正在挂断…" : runtime.outgoing && runtime.connectedAt == nil ? "取消拨号" : "挂断",
                  systemImage: "phone.down.fill")
              }.buttonStyle(.borderedProminent).tint(.red)
                .disabled(runtime.callControlPending || runtime.callPhase == "ending")
            }
          }
          if runtime.callUUID == nil && runtime.paired {
            Section("拨打电话") {
              if runtime.online && !runtime.dialingSupported {
                Text("当前服务尚未支持主动拨号，可继续接听来电。").font(.caption)
              }
              Picker("使用模块", selection: $runtime.selectedModule) {
                ForEach(runtime.modules, id: \.self) { module in Text(module).tag(module) }
              }.disabled(!runtime.online)
              TextField("电话号码", text: $runtime.dialNumber)
                .keyboardType(.phonePad).textContentType(.telephoneNumber)
              Button {
                runtime.dialCall()
              } label: {
                Label("拨打", systemImage: "phone.fill")
              }
              .disabled(!runtime.canDial)
            }
          }
          if !runtime.lastCallResult.isEmpty {
            Section { Text(runtime.lastCallResult) }
          }
          Section("配对与接听") {
            Button("导入 Hub 配对文件") { importing = true }.disabled(
              runtime.paired || runtime.working || runtime.online)
            Button(runtime.online ? "断开连接" : "连接并等待来电") {
              if runtime.online { runtime.disconnect() } else { Task { await runtime.connect() } }
            }.disabled(!runtime.paired || runtime.working || runtime.callUUID != nil)
          }
          Section("后台来电") {
            Text(runtime.pushStatus).font(.caption)
            Button(runtime.pushEnabled ? "关闭后台来电" : "启用后台来电") {
              Task {
                if runtime.pushEnabled {
                  await runtime.disablePush()
                } else {
                  await runtime.enablePush()
                }
              }
            }.disabled(!runtime.paired || runtime.working || runtime.callUUID != nil)
          }
          if !runtime.callDiagnostics.isEmpty {
            Section("本次联调诊断") {
              Text(runtime.callDiagnostics.joined(separator: "\n"))
                .font(.caption.monospaced()).textSelection(.enabled)
            }
          }
          if runtime.paired {
            Section("恢复配对") {
              Text("公钥指纹：\(runtime.fingerprint)").font(.caption).textSelection(.enabled)
              Button("清理本机配对凭据", role: .destructive) { forgetting = true }.disabled(
                runtime.online || runtime.working)
            }
          }
          Section {
            Text("可以通过系统通话界面或 App 内通话面板接听、拒接、挂断和静音。")
            Text("后台来电需要正确的 App 签名和 Relay APNs 配置；跨网络通话需要 TURN 中继。")
          }
        }
        .navigationTitle("DJOneHub")
        .fileImporter(isPresented: $importing, allowedContentTypes: [.json]) { result in
          if case .success(let url) = result { Task { await runtime.importPairing(url) } }
        }
      }
      .confirmationDialog(
        "请先在 Hub/Relay 撤销旧安装身份。本操作仅删除此 iPhone 的凭据，不能撤销服务端身份。", isPresented: $forgetting,
        titleVisibility: .visible
      ) {
        Button("已撤销旧身份，清理本机凭据", role: .destructive) { runtime.forgetLocalIdentity() }
      }
      .onReceive(NotificationCenter.default.publisher(for: AVAudioSession.routeChangeNotification))
      { _ in
        runtime.refreshAudioRoute()
      }
      .onChange(of: phase) { _, value in if value == .background { runtime.backgrounded() } }
    }
  }
}
