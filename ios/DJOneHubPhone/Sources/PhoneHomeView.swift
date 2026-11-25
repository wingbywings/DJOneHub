import DJOneHubPhoneCore
import SwiftUI

enum PhoneTab: String, CaseIterable {
  case dialer = "拨号", recents = "记录", sms = "短信", settings = "设置"
  var icon: String {
    switch self {
    case .dialer: return "circle.grid.3x3.fill"
    case .recents: return "clock"
    case .sms: return "message"
    case .settings: return "gearshape"
    }
  }
}

struct PhoneHomeView: View {
  @ObservedObject var runtime: PhoneRuntime
  @ObservedObject private var inbox = PhoneSMSInbox.shared
  @StateObject private var contacts = PhoneContactsStore()
  @State private var tab: PhoneTab = .dialer
  @State private var clearingHistory = false
  @State private var presentingCall = false
  @State private var showingContacts = false
  @AppStorage("preferredDialModule") private var preferredModule = ""
  @AppStorage("phoneAppearance") private var appearance = PhoneAppearance.light.rawValue
  @ScaledMetric(relativeTo: .caption2) private var tabIconSize: CGFloat = 20
  @ScaledMetric(relativeTo: .caption2) private var tabBarHeight: CGFloat = 68

  private var pairing: PhonePairingPresentation {
    PhonePairingPresentation(paired: runtime.paired, online: runtime.online, identityReady: runtime.identityReady)
  }

  var body: some View {
    ZStack {
      AppBackground()
      Group {
        if showingContacts {
          PhoneContactsView(runtime: runtime, contacts: contacts, dial: prepareDial, onBack: { showingContacts = false })
        } else {
          switch tab {
          case .dialer:
            DialerView(runtime: runtime, openSettings: { tab = .settings }, openContacts: { showingContacts = true })
          case .recents:
            PhoneRecentsView(runtime: runtime, contacts: contacts, clear: { clearingHistory = true },
                             dial: prepareDial, openContacts: { showingContacts = true })
          case .sms:
            PhoneSMSView(runtime: runtime)
          case .settings:
            PhoneSettingsView(runtime: runtime, clearHistory: { clearingHistory = true })
          }
        }
      }
      .frame(maxWidth: .infinity, maxHeight: .infinity)
    }
    .safeAreaInset(edge: .bottom, spacing: 0) {
      if !showingContacts { tabBar }
    }
    .fontDesign(.rounded).foregroundStyle(PhoneTheme.ink).tint(PhoneTheme.violet)
    .phonePreferredColorScheme(appearance)
    .fullScreenCover(isPresented: $presentingCall) {
      PhoneCallView(runtime: runtime, contacts: contacts)
        .phonePreferredColorScheme(appearance).interactiveDismissDisabled()
    }
    .confirmationDialog("清除本机已结束的通话记录？", isPresented: $clearingHistory, titleVisibility: .visible) {
      Button("清除记录", role: .destructive) { runtime.clearHistory() }
    }
    .onChange(of: inbox.openedNotification, initial: true) { _, value in
      if value != nil { showingContacts = false; tab = .sms; inbox.openedNotification = nil }
    }
    .onChange(of: runtime.callUUID) { _, value in
      if value != nil { clearingHistory = false; showingContacts = false }
    }
    .task(id: runtime.callUUID) {
      guard runtime.callUUID != nil else { presentingCall = false; return }
      // Let pairing/contact sheets dismiss before presenting the incoming call.
      do { try await Task.sleep(for: .milliseconds(500)) } catch { return }
      if runtime.callUUID != nil { presentingCall = true }
    }
    .onChange(of: runtime.modules, initial: true) { _, modules in
      if modules.contains(preferredModule) { runtime.selectedModule = preferredModule }
    }
    .onChange(of: runtime.selectedModule) { _, module in
      if !module.isEmpty { preferredModule = module }
    }
  }

  private func prepareDial(_ number: String, _ module: String?) {
    guard let number = PhoneCall.dialNumber(number) else { return }
    runtime.dialNumber = number
    if let module, runtime.modules.contains(module) { runtime.selectedModule = module }
    showingContacts = false
    tab = .dialer
    if runtime.canDial { runtime.dialCall() }
  }

  private var tabBar: some View {
    HStack(spacing: 0) {
      ForEach(PhoneTab.allCases, id: \.self) { item in
        Button { tab = item } label: {
          VStack(spacing: 3) {
            Image(systemName: item.icon).font(.system(size: tabIconSize, weight: .medium))
              .frame(width: 42, height: 28)
              .background(tab == item ? PhoneTheme.violetSoft : .clear, in: RoundedRectangle(cornerRadius: 12))
            Text(item.rawValue).font(.system(size: 11, weight: .semibold)).lineLimit(1).minimumScaleFactor(0.8)
          }.foregroundStyle(tab == item ? PhoneTheme.violet : PhoneTheme.muted)
            .frame(maxWidth: .infinity).frame(height: tabBarHeight).contentShape(Rectangle())
        }.buttonStyle(PhonePressStyle())
          .accessibilityAddTraits(tab == item ? [.isSelected] : [])
          .accessibilityLabel(item.rawValue)
      }
    }
    .phoneGlass(radius: 28, strong: true)
    .padding(.horizontal, 14).padding(.bottom, 2)
    .accessibilityElement(children: .contain)
    .accessibilityLabel("标签栏")
  }
}

struct DialerView: View {
  @ObservedObject var runtime: PhoneRuntime
  var openSettings: () -> Void
  var openContacts: () -> Void
  @State private var inputMessage = ""

  private var pairing: PhonePairingPresentation {
    PhonePairingPresentation(paired: runtime.paired, online: runtime.online, identityReady: runtime.identityReady)
  }

  private var hint: String {
    if !runtime.paired { return "先配对 Hub，即可开始通话" }
    if !runtime.identityReady { return "配对凭据需要处理，请查看设置" }
    if runtime.working { return "正在连接，请稍候…" }
    if !runtime.online {
      if runtime.foregroundPaused { return "前台连接已暂停，可在设置中恢复" }
      if !runtime.microphoneGranted { return "请在设置中允许麦克风，即可自动连接" }
      return "打开 App 后自动连接；连接失败可在设置中重试"
    }
    if !runtime.dialingSupported { return "当前服务仅支持接听" }
    if runtime.modules.isEmpty { return "暂无可用线路，请检查 Hub 授权" }
    if !runtime.dialNumber.isEmpty && PhoneCall.dialNumber(runtime.dialNumber) == nil {
      return "请输入有效号码，可长按 0 输入 +"
    }
    return "高清音质 · 安心通话"
  }

  var body: some View {
    GeometryReader { geometry in
      let compact = geometry.size.height < 640
      ScrollView {
        VStack(spacing: 0) {
          HStack(alignment: .center, spacing: 8) {
            VStack(alignment: .leading, spacing: 2) {
              Text("拨号").font(.title2.weight(.heavy)).fontDesign(.rounded).accessibilityAddTraits(.isHeader)
              if !compact {
                Text("高清音质 · 安心通话").font(.system(size: 12)).foregroundStyle(PhoneTheme.secondary)
              }
            }
            Spacer(minLength: 0)
            RoundIconButton(icon: "person", label: "联系人", size: 40, action: openContacts)
            Button(action: openSettings) {
              HStack(spacing: 7) {
                Circle().fill(pairing.color).frame(width: 8, height: 8)
                Text(runtime.working ? "连接中…" : pairing.title)
                  .font(.system(size: 12, weight: .semibold)).lineLimit(1).minimumScaleFactor(0.8)
              }.foregroundStyle(PhoneTheme.secondary).padding(.horizontal, 12).padding(.vertical, 9)
                .phoneGlass(radius: 30)
            }.buttonStyle(PhonePressStyle())
              .accessibilityLabel("连接状态，打开设置")
              .accessibilityValue(runtime.working ? "连接中" : pairing.shortValue)
          }.padding(.top, compact ? 6 : 12)

          if !runtime.paired {
            VStack(spacing: 12) {
              Text("尚未配对 Hub").font(.system(size: 18, weight: .heavy))
              Text("扫码或导入配对文件后，即可拨号与接听。")
                .font(.subheadline).foregroundStyle(PhoneTheme.secondary).multilineTextAlignment(.center)
              Button(action: openSettings) {
                Text("去配对").font(.system(size: 16, weight: .bold)).foregroundStyle(.white)
                  .frame(maxWidth: .infinity).padding(.vertical, 14)
                  .background(PhoneTheme.coral, in: Capsule())
              }.buttonStyle(PhonePressStyle())
            }
            .padding(18).phoneGlass(radius: 24).padding(.top, compact ? 12 : 20)
          }

          Spacer(minLength: compact ? 4 : 16)
          VStack(spacing: compact ? 6 : 12) {
            Text(runtime.dialNumber.isEmpty ? "输入电话号码" : runtime.dialNumber)
              .font(.system(size: runtime.dialNumber.isEmpty ? 24 : (compact ? 30 : 34), weight: .bold, design: .rounded))
              .foregroundStyle(runtime.dialNumber.isEmpty ? PhoneTheme.muted : PhoneTheme.ink)
              .lineLimit(1).minimumScaleFactor(0.45).frame(maxWidth: .infinity).frame(minHeight: compact ? 30 : 46)
              .accessibilityLabel("电话号码").accessibilityValue(runtime.dialNumber.isEmpty ? "空" : runtime.dialNumber)
            if runtime.paired {
              LineMenu(runtime: runtime)
            }
            ZStack {
              PhoneKeypad(compact: compact || !runtime.paired, press: append, longPressZero: {
                if runtime.dialNumber.isEmpty { append("+") }
                else if runtime.dialNumber == "0" { runtime.dialNumber = "+" }
              }).padding(.horizontal, 8)
                .allowsHitTesting(runtime.paired)
                .opacity(runtime.paired ? 1 : 0.35)
              if !runtime.paired {
                Color.clear.contentShape(Rectangle())
                  .onTapGesture(perform: openSettings)
                  .accessibilityLabel("先去设置配对")
              }
            }
            HStack(spacing: 22) {
              PasteButton(payloadType: String.self) { strings in
                guard let value = strings.first, let number = PhoneCall.dialNumber(value) else {
                  inputMessage = "剪贴板中没有有效电话号码"
                  return
                }
                runtime.dialNumber = number
                inputMessage = ""
              }.labelStyle(.iconOnly).tint(PhoneTheme.secondary)
                .buttonBorderShape(.capsule).accessibilityLabel("粘贴电话号码")
                .disabled(!runtime.paired)
              Button { runtime.dialCall() } label: {
                Label("呼叫", systemImage: "phone.fill").font(.system(size: 19, weight: .bold))
                  .foregroundStyle(.white).padding(.horizontal, 34).frame(height: compact ? 54 : 62)
                  .background(runtime.canDial ? PhoneTheme.coral : PhoneTheme.gradient(0xC9BDD6, 0xB7A8C8), in: Capsule())
                  .shadow(color: PhoneTheme.red.opacity(runtime.canDial ? 0.3 : 0), radius: 12, y: 8)
              }.buttonStyle(PhonePressStyle()).disabled(!runtime.canDial)
              RoundIconButton(icon: "delete.left", label: "删除最后一位号码", color: PhoneTheme.secondary,
                              background: .white.opacity(0.5), size: 50) {
                if !runtime.dialNumber.isEmpty { runtime.dialNumber.removeLast() }
                inputMessage = ""
              }.disabled(runtime.dialNumber.isEmpty)
                .simultaneousGesture(LongPressGesture().onEnded { _ in runtime.dialNumber = "" })
            }.padding(.top, 5)
            Text(inputMessage.isEmpty ? hint : inputMessage)
              .font(.system(size: 12)).foregroundStyle(PhoneTheme.secondary).multilineTextAlignment(.center)
            if !runtime.lastCallResult.isEmpty {
              Text(runtime.lastCallResult).font(.caption).foregroundStyle(PhoneTheme.secondary)
                .multilineTextAlignment(.center).padding(10).frame(maxWidth: .infinity).phoneGlass(radius: 16)
            }
          }
          Spacer(minLength: compact ? 4 : 12)
        }.padding(.horizontal, PhoneLayout.contentInset).frame(minHeight: geometry.size.height)
      }.scrollIndicators(.hidden)
    }
  }

  private func append(_ digit: String) {
    guard runtime.dialNumber.count < 32 else { return }
    runtime.dialNumber += digit
    inputMessage = ""
  }
}
