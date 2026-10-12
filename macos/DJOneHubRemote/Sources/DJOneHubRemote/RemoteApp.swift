import AppKit
import DJOneHubPhoneCore
import RemoteManagement
import SwiftUI

@MainActor final class ManagementModel: ObservableObject {
  @Published var profile = "default"
  @Published var status: ProfileStatus?
  @Published var installations: [Installation] = []
  @Published var selected = Set<String>()
  @Published var busy = false
  @Published var message = "读取已登记的 Hub 配置后，选择允许此 iPhone 使用的模块。"
  @Published var qr: NSImage?
  @Published var grant: PairingFile?
  let client = ManagementClient(executable: URL(fileURLWithPath: CommandLine.arguments[0])
    .resolvingSymlinksInPath().deletingLastPathComponent().appendingPathComponent("djonehub-macos"))
  func clearQR() { qr = nil; grant = nil }
  func load() async {
    guard !busy else { return }
    busy = true; clearQR(); status = nil; installations = []; selected = []
    defer { busy = false }
    do {
      let snapshot = try await client.status(profile: profile)
      guard snapshot.phase == "ready" else { throw ManagementError.invalid }
      status = snapshot
      installations = try await client.installations(profile: snapshot.profile)
      message = "已读取安装身份。撤销后，该 iPhone 的远程通话授权将失效。"
    } catch { message = "读取失败。请确认已登记此配置、允许钥匙串访问，并检查 Relay 连接。" }
  }
  func create() async {
    guard !busy, let status, !selected.isEmpty else { return }
    busy = true; clearQR()
    defer { busy = false }
    do {
      let data = try await client.pairing(profile: status.profile, modules: selected.sorted())
      let parsed = try PairingFile.parse(data)
      let image = try PairingQRCode.image(data)
      qr = NSImage(cgImage: image, size: NSSize(width: image.width, height: image.height))
      grant = parsed
      message = "请使用 iPhone App 扫描。二维码是一次性配对凭据，请勿分享截图。"
    } catch { message = "生成失败或二维码容量不足，请重试并减少选择的模块。" }
  }
  func revoke(_ installation: Installation) async {
    guard !busy, let status else { return }
    busy = true; clearQR()
    defer { busy = false }
    do {
      try await client.revoke(profile: status.profile, installation: installation.id)
      installations = try await client.installations(profile: status.profile)
      message = "安装身份已撤销。请同时关闭模块指向该身份的远程接听设置。"
    } catch { message = "撤销或刷新失败，请刷新列表核对结果。" }
  }
}

@main struct DJOneHubRemoteApp: App {
  @StateObject private var model = ManagementModel()
  @State private var revokeTarget: Installation?
  var body: some Scene {
    WindowGroup("DJOneHub 远程配对") {
      ScrollView {
        VStack(alignment: .leading, spacing: 16) {
          HStack {
            TextField("Hub 配置名称", text: $model.profile).disabled(model.busy)
            Button("读取 / 刷新") { Task { await model.load() } }.disabled(model.busy)
          }
          Text(model.message).font(.callout)
          if let status = model.status {
            Text(status.relay_url).font(.headline).textSelection(.enabled)
            Text("选择此 iPhone 可使用的模块")
            ForEach(status.principal.modules, id: \.self) { module in
              Toggle(module, isOn: Binding(get: { model.selected.contains(module) }, set: { enabled in
                model.clearQR()
                if enabled { model.selected.insert(module) } else { model.selected.remove(module) }
              })).disabled(model.busy)
            }
            Button("生成一次性配对二维码") { Task { await model.create() } }
              .disabled(model.busy || model.selected.isEmpty)
            if let qr = model.qr, let grant = model.grant {
              TimelineView(.periodic(from: .now, by: 1)) { context in
                if context.date < grant.grant.expires_at {
                  VStack {
                    Image(nsImage: qr).interpolation(.none).resizable().scaledToFit()
                      .frame(width: 360, height: 360).padding(20).background(.white)
                    Text("有效期至 " + grant.grant.expires_at.formatted())
                    Button("隐藏二维码") { model.clearQR() }
                  }
                } else { Text("二维码已过期，请重新生成") }
              }
            }
            Divider()
            Text("已配对 iPhone").font(.headline)
            ForEach(model.installations) { installation in
              HStack {
                VStack(alignment: .leading) {
                  Text(installation.id).textSelection(.enabled)
                  Text(installation.modules.joined(separator: ", ")).font(.caption)
                }
                Spacer()
                Button("撤销", role: .destructive) { revokeTarget = installation }.disabled(model.busy)
              }
            }
            if model.installations.isEmpty { Text("暂无安装身份。手机配对完成后点击刷新。") }
          }
          Divider()
          Button("打开模块远程接听设置") { NSWorkspace.shared.open(URL(string: "http://127.0.0.1:7575/#calls")!) }
          Text("在模块的远程接听设置填写上方安装 ID。启用 AI 接听时，远程接听不会触发。")
            .font(.caption)
        }.padding(24)
      }.frame(minWidth: 520, minHeight: 540)
      .confirmationDialog("撤销此 iPhone 的授权？现有通话可能中断。", isPresented: Binding(
        get: { revokeTarget != nil }, set: { if !$0 { revokeTarget = nil } }), titleVisibility: .visible) {
        if let target = revokeTarget {
          Button("撤销 " + target.id, role: .destructive) { Task { await model.revoke(target) }; revokeTarget = nil }
        }
      }
      .onChange(of: model.profile) { _ in
        model.clearQR(); model.status = nil; model.installations = []; model.selected = []
      }
      .onReceive(NotificationCenter.default.publisher(for: NSApplication.didResignActiveNotification)) { _ in model.clearQR() }
      .onAppear { NSApplication.shared.setActivationPolicy(.regular); NSApplication.shared.activate(ignoringOtherApps: true) }
    }
  }
}
