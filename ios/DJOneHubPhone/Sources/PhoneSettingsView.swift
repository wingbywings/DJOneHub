import SwiftUI
import UniformTypeIdentifiers

struct PhoneSettingsView: View {
  @ObservedObject var runtime: PhoneRuntime
  @ObservedObject private var inbox = PhoneSMSInbox.shared
  @Environment(\.openURL) private var openURL
  var clearHistory: () -> Void
  @State private var scanning = false
  @State private var importing = false
  @State private var forgetting = false
  @State private var diagnostics = false
  @State private var copied = false
  @State private var pushChanging = false
  @AppStorage("phoneAppearance") private var appearance = PhoneAppearance.light.rawValue

  private var pairingDisabled: Bool {
    runtime.paired || runtime.working || runtime.online || runtime.callUUID != nil
  }

  private var pairing: PhonePairingPresentation {
    PhonePairingPresentation(paired: runtime.paired, online: runtime.online, identityReady: runtime.identityReady)
  }

  private var notificationDenied: Bool {
    inbox.status.contains("未获系统授权") || inbox.status.contains("系统通知设置")
  }

  private var pairingFailed: Bool {
    runtime.status.contains("配对失败") || runtime.status.contains("无法读取配对")
  }

  private var connectionFailed: Bool {
    runtime.paired && runtime.identityReady && !runtime.online && !runtime.working
      && (runtime.status.contains("连接失败") || runtime.status.contains("无法读取本机凭据"))
  }

  var body: some View {
    ScrollView {
      VStack(spacing: 12) {
        PageHeading(title: "设置", subtitle: "连接、配对与通话偏好")
        connectionCard

        if pairingFailed {
          PhoneActionBanner(
            title: "配对失败",
            message: runtime.status,
            buttonTitle: "重新扫描配对码",
            action: { scanning = true }
          )
        } else if !runtime.identityReady && runtime.paired {
          PhoneActionBanner(
            title: "凭据异常",
            message: runtime.status.isEmpty
              ? "配对凭据不完整。请先在 Hub/Relay 撤销旧身份，再清理本机凭据后重新配对。"
              : runtime.status,
            buttonTitle: "清理本机凭据",
            action: { forgetting = true }
          )
        } else if connectionFailed {
          PhoneActionBanner(
            title: "连接失败",
            message: connectionFailureReason,
            buttonTitle: runtime.microphoneDenied ? "打开系统设置允许麦克风" : "重新连接",
            tint: PhoneTheme.amber,
            soft: PhoneTheme.amberSoft,
            action: {
              if runtime.microphoneDenied, let url = URL(string: UIApplication.openSettingsURLString) {
                openURL(url)
              } else {
                Task { await runtime.retryForegroundConnection() }
              }
            }
          )
        }

        if notificationDenied {
          PhoneActionBanner(
            title: "通知权限未开启",
            message: "短信推送与部分来电提醒需要系统通知。请在 iPhone 设置中允许 DJOneHub Phone 通知。",
            buttonTitle: "打开系统设置",
            action: {
              if let url = URL(string: UIApplication.openSettingsURLString) { openURL(url) }
            }
          )
        }

        SectionCaption(title: "配对与设备")
        VStack(spacing: 0) {
          Button { scanning = true } label: {
            SettingsRow(icon: "qrcode.viewfinder", title: "扫描 Hub 配对二维码", subtitle: "与新 Hub 建立配对")
          }.disabled(pairingDisabled)
          Divider().overlay(.white.opacity(0.7))
          Button { importing = true } label: {
            SettingsRow(icon: "square.and.arrow.down", title: "导入 Hub 配对文件", subtitle: "使用 JSON 配对文件", color: PhoneTheme.green)
          }.disabled(pairingDisabled)
          if runtime.paired {
            Divider().overlay(.white.opacity(0.7))
            HStack(spacing: 8) {
              SettingsRow(icon: "key", title: "公钥指纹", subtitle: copied ? "已复制公钥指纹" : runtime.fingerprint,
                          color: PhoneTheme.amber, chevron: false)
              RoundIconButton(icon: copied ? "checkmark" : "doc.on.doc", label: "复制公钥指纹") {
                UIPasteboard.general.string = runtime.fingerprint
                copied = true
              }.disabled(runtime.fingerprint.isEmpty)
            }
            Divider().overlay(.white.opacity(0.7))
            Button { forgetting = true } label: {
              SettingsRow(icon: "trash", title: "清理本机配对凭据", subtitle: "需先在 Hub/Relay 撤销旧身份", color: PhoneTheme.red)
            }.disabled(runtime.online || runtime.working || runtime.callUUID != nil)
          }
        }.padding(.horizontal, 14).phoneGlass()
          .buttonStyle(PhonePressStyle())

        SectionCaption(title: "通话")
        VStack(spacing: 0) {
          HStack(spacing: 8) {
            SettingsRow(icon: "bell", title: "后台来电", subtitle: runtime.pushStatus, chevron: false)
            Toggle("后台来电", isOn: Binding(get: { runtime.pushEnabled }, set: { enabled in
              guard !pushChanging else { return }
              pushChanging = true
              Task {
                if enabled { await runtime.enablePush() } else { await runtime.disablePush() }
                pushChanging = false
              }
            })).labelsHidden().tint(PhoneTheme.green)
              .disabled(!runtime.paired || runtime.working || runtime.callUUID != nil || pushChanging)
          }
          Divider().overlay(.white.opacity(0.7))
          VStack(alignment: .leading, spacing: 0) {
            SettingsRow(icon: "terminal", title: "默认拨号线路",
                        subtitle: "对应 Hub「当前模块」，拨号时优先使用", color: PhoneTheme.green, chevron: false)
            LineMenu(runtime: runtime).padding(.leading, 54).padding(.bottom, 14)
          }
        }.padding(.horizontal, 14).phoneGlass()

        SectionCaption(title: "外观")
        VStack(spacing: 0) {
          SettingsRow(icon: "circle.lefthalf.filled", title: "外观",
                      subtitle: "暖阳色板以浅色为主；也可跟随系统", chevron: false)
          Picker("外观", selection: $appearance) {
            ForEach(PhoneAppearance.allCases) { option in
              Text(option.title).tag(option.rawValue)
            }
          }.pickerStyle(.segmented).padding(.leading, 54).padding(.trailing, 4).padding(.bottom, 14)
        }.padding(.horizontal, 14).phoneGlass()

        SectionCaption(title: "数据与支持")
        VStack(spacing: 0) {
          Button(action: clearHistory) {
            SettingsRow(icon: "clock", title: "清除通话记录", subtitle: "仅删除本机已结束的记录", color: PhoneTheme.red)
          }.disabled(!runtime.history.entries.contains(where: { $0.ended != nil }) && runtime.historyError.isEmpty)
          Divider().overlay(.white.opacity(0.7))
          Button { diagnostics = true } label: {
            SettingsRow(icon: "info.circle", title: "联调诊断", subtitle: "查看本次通话的调试日志", color: .blue)
          }
        }.padding(.horizontal, 14).phoneGlass().buttonStyle(PhonePressStyle())

        SectionCaption(title: "线路接听")
        VStack(alignment: .leading, spacing: 10) {
          if runtime.modules.isEmpty {
            Text("配对并授权线路后，这里按线路显示接听方式。")
              .font(.system(size: 12)).foregroundStyle(PhoneTheme.secondary)
          } else {
            ForEach(runtime.modules, id: \.self) { id in
              let route = runtime.lineRoute(for: id)
              HStack(spacing: 10) {
                Text(runtime.moduleName(for: id))
                  .font(.system(size: 14, weight: .semibold))
                  .foregroundStyle(PhoneTheme.ink)
                  .lineLimit(1)
                Spacer(minLength: 0)
                if let title = route.badgeTitle ?? route.shortTitle {
                  LineRouteBadge(title: title, route: route)
                } else {
                  Text("以 Hub 为准")
                    .font(.system(size: 11, weight: .semibold))
                    .foregroundStyle(PhoneTheme.muted)
                }
              }
              .padding(.vertical, 4)
              if id != runtime.modules.last {
                Divider().overlay(.white.opacity(0.7))
              }
            }
          }
          Text(lineRouteLegend)
            .font(.system(size: 11)).foregroundStyle(PhoneTheme.secondary)
            .fixedSize(horizontal: false, vertical: true)
        }.padding(14).phoneGlass()

        Text("可通过系统通话界面或 App 内通话面板接听、拒接、挂断和静音。\n某线路在 Hub 开启 APP 接听（\(PhoneTheme.routePhone)）后，该线路短信通知接入本机，Bark 提示暂停；请允许系统通知。\n后台来电需要正确的 App 签名和 Relay APNs 配置；跨网络通话需要 TURN 中继。")
          .font(.system(size: 11)).foregroundStyle(PhoneTheme.secondary).multilineTextAlignment(.center).lineSpacing(4)
          .padding(.top, 4)
        Text("DJOneHub Phone · \(Bundle.main.object(forInfoDictionaryKey: "CFBundleShortVersionString") as? String ?? "1.0")")
          .font(.system(size: 11)).foregroundStyle(PhoneTheme.muted)
      }.padding(.horizontal, PhoneLayout.contentInset).padding(.bottom, 24)
    }.scrollIndicators(.hidden)
      .sheet(isPresented: $scanning) { PairingScanner(runtime: runtime).tint(PhoneTheme.violet) }
      .sheet(isPresented: $diagnostics) { diagnosticsView }
      .fileImporter(isPresented: $importing, allowedContentTypes: [.json]) { result in
        if case .success(let url) = result { Task { await runtime.importPairing(url) } }
      }
      .confirmationDialog(
        "请先在 Hub/Relay 撤销旧安装身份。本操作仅删除此 iPhone 的凭据，不能撤销服务端身份。",
        isPresented: $forgetting, titleVisibility: .visible
      ) {
        Button("已撤销旧身份，清理本机凭据", role: .destructive) { runtime.forgetLocalIdentity() }
      }
      .onChange(of: runtime.callUUID) { _, value in
        if value != nil { scanning = false; importing = false; diagnostics = false; forgetting = false }
      }
      .onChange(of: runtime.fingerprint) { _, _ in copied = false }
  }

  private var connectionFailureReason: String {
    if runtime.microphoneDenied { return "麦克风权限被拒绝，无法建立通话连接。" }
    if runtime.foregroundPaused { return "前台连接已暂停。可恢复自动连接，或依赖后台来电推送。" }
    if runtime.status.contains("连接失败") { return "网络或 Relay 不可用。可重试，或检查配对与 Hub 在线状态。" }
    return runtime.status
  }

  private var connectionCard: some View {
    VStack(spacing: 14) {
      HStack(spacing: 12) {
        Image(systemName: "phone.fill").font(.system(size: 24)).frame(width: 46, height: 46)
          .background(.white.opacity(0.22), in: RoundedRectangle(cornerRadius: 16))
        VStack(alignment: .leading, spacing: 4) {
          Text(pairing.title)
            .font(.system(size: 17, weight: .heavy))
            .accessibilityValue(pairing.shortValue)
          Text(connectionDetail).font(.system(size: 12)).opacity(0.9).fixedSize(horizontal: false, vertical: true)
          if !runtime.installation.isEmpty {
            Text("安装 ID：\(runtime.installation)").font(.system(size: 11)).opacity(0.85)
              .lineLimit(1).truncationMode(.middle).textSelection(.enabled)
          }
        }.frame(maxWidth: .infinity, alignment: .leading)
        if runtime.working { ProgressView().tint(.white).accessibilityLabel("正在连接") }
        else {
          Circle().fill(pairing.color.opacity(runtime.online ? 1 : 0.85)).frame(width: 10, height: 10)
            .padding(4).background(.white.opacity(0.15), in: Circle())
            .accessibilityHidden(true)
        }
      }.foregroundStyle(.white)
      Button {
        if runtime.online { runtime.pauseForegroundConnection() }
        else if runtime.microphoneDenied, let url = URL(string: UIApplication.openSettingsURLString) { openURL(url) }
        else { Task { await runtime.retryForegroundConnection() } }
      } label: {
        Text(runtime.online ? "暂停前台连接" : runtime.working ? "正在连接…"
          : runtime.microphoneDenied ? "打开系统设置允许麦克风"
          : runtime.foregroundPaused ? "恢复自动连接"
          : runtime.microphoneGranted ? "重新连接" : "允许麦克风并连接")
          .font(.system(size: 15, weight: .bold)).foregroundStyle(PhoneTheme.violet)
          .frame(maxWidth: .infinity).padding(.vertical, 12).background(.white.opacity(0.94), in: Capsule())
      }.buttonStyle(PhonePressStyle()).disabled(!runtime.canChangeForegroundConnection)
      Text("已配对后打开 App 自动连接。后台接听需开启“后台来电”，无需保持前台连接。")
        .font(.system(size: 11)).foregroundStyle(.white.opacity(0.9)).multilineTextAlignment(.center)
    }.padding(18).background(PhoneTheme.connection, in: RoundedRectangle(cornerRadius: 28))
      .shadow(color: PhoneTheme.shadow.opacity(0.14), radius: 12, y: 8)
      .accessibilityElement(children: .contain)
  }

  private var connectionDetail: String {
    if runtime.working { return "正在连接…" }
    if pairing == .credentialError { return runtime.status }
    if !runtime.status.isEmpty { return runtime.status }
    return pairing.title
  }

  private var lineRouteLegend: String {
    let knownAI = runtime.modules.contains { runtime.lineRoute(for: $0) == .ai }
    if knownAI {
      return "仅启用 Voice Agent 的线路显示「\(PhoneTheme.routeAI)」且不会通知本机；其余走「\(PhoneTheme.routePhone)」/「\(PhoneTheme.routeApp)」。仅此线路由 AI 接听，其余走 App。"
    }
    return "接听方式按线路在 Hub 配置。本机已确认可用来电的线路显示「\(PhoneTheme.routePhone)」；尚未同步到的线路显示「以 Hub 为准」，不会用全局「AI 接听中」误标全部线路。"
  }

  private var diagnosticsView: some View {
    NavigationStack {
      ZStack {
        AppBackground()
        ScrollView {
          if runtime.callDiagnostics.isEmpty {
            EmptyPhoneState(icon: "text.alignleft", title: "暂无诊断日志", message: "通话开始后，这里会显示本次联调信息。")
              .padding(PhoneLayout.contentInset)
          } else {
            Text(runtime.callDiagnostics.joined(separator: "\n"))
              .font(.system(size: 12, design: .monospaced)).textSelection(.enabled)
              .frame(maxWidth: .infinity, alignment: .leading).padding(18).phoneGlass()
              .padding(PhoneLayout.contentInset)
          }
        }
      }.navigationTitle("联调诊断").navigationBarTitleDisplayMode(.inline)
        .toolbar { ToolbarItem(placement: .confirmationAction) { Button("完成") { diagnostics = false } } }
    }.tint(PhoneTheme.violet).foregroundStyle(PhoneTheme.ink).phonePreferredColorScheme(appearance)
  }
}

private struct SettingsRow: View {
  var icon: String
  var title: String
  var subtitle: String
  var color: Color = PhoneTheme.violet
  var chevron = true
  var body: some View {
    HStack(spacing: 12) {
      Image(systemName: icon).font(.system(size: 19, weight: .medium)).foregroundStyle(color)
        .frame(width: 42, height: 42).background(color.opacity(0.1), in: RoundedRectangle(cornerRadius: 14))
      VStack(alignment: .leading, spacing: 4) {
        Text(title).font(.system(size: 15, weight: .bold)).foregroundStyle(title == "清理本机配对凭据" ? PhoneTheme.red : PhoneTheme.ink)
        Text(subtitle).font(.system(size: 12)).foregroundStyle(PhoneTheme.secondary)
          .lineLimit(2).truncationMode(.middle)
      }.frame(maxWidth: .infinity, alignment: .leading)
      if chevron { Image(systemName: "chevron.right").font(.system(size: 12, weight: .bold)).foregroundStyle(PhoneTheme.muted) }
    }.padding(.vertical, 13).contentShape(Rectangle())
  }
}
