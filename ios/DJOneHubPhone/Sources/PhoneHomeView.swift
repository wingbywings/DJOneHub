import DJOneHubPhoneCore
import SwiftUI

enum PhoneTab: String, CaseIterable {
  case dialer = "拨号", contacts = "联系人", recents = "通话记录", settings = "设置"
  var icon: String {
    switch self {
    case .dialer: return "square.grid.2x2"
    case .contacts: return "person"
    case .recents: return "clock"
    case .settings: return "gearshape"
    }
  }
}

struct PhoneHomeView: View {
  @ObservedObject var runtime: PhoneRuntime
  @StateObject private var contacts = PhoneContactsStore()
  @State private var tab: PhoneTab = .dialer
  @State private var clearingHistory = false
  @State private var presentingCall = false
  @AppStorage("preferredDialModule") private var preferredModule = ""

  var body: some View {
    ZStack {
      AppBackground()
      Group {
        switch tab {
        case .dialer:
          DialerView(runtime: runtime) { tab = .settings }
        case .contacts:
          PhoneContactsView(runtime: runtime, contacts: contacts, dial: prepareDial)
        case .recents:
          PhoneRecentsView(runtime: runtime, contacts: contacts, clear: { clearingHistory = true }, dial: prepareDial)
        case .settings:
          PhoneSettingsView(runtime: runtime, clearHistory: { clearingHistory = true })
        }
      }
      .frame(maxWidth: .infinity, maxHeight: .infinity)
    }
    .safeAreaInset(edge: .bottom, spacing: 0) { tabBar }
    .fontDesign(.rounded).foregroundStyle(PhoneTheme.ink).tint(PhoneTheme.violet)
    .preferredColorScheme(.light)
    .fullScreenCover(isPresented: $presentingCall) {
      PhoneCallView(runtime: runtime, contacts: contacts)
        .preferredColorScheme(.light).interactiveDismissDisabled()
    }
    .confirmationDialog("清除本机已结束的通话记录？", isPresented: $clearingHistory, titleVisibility: .visible) {
      Button("清除记录", role: .destructive) { runtime.clearHistory() }
    }
    .onChange(of: runtime.callUUID) { _, value in
      if value != nil { clearingHistory = false }
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
    tab = .dialer
    if runtime.canDial { runtime.dialCall() }
  }

  private var tabBar: some View {
    HStack(spacing: 0) {
      ForEach(PhoneTab.allCases, id: \.self) { item in
        Button { tab = item } label: {
          VStack(spacing: 4) {
            Image(systemName: item.icon).font(.system(size: 22, weight: .medium))
              .frame(width: 46, height: 32)
              .background(tab == item ? PhoneTheme.violetSoft : .clear, in: RoundedRectangle(cornerRadius: 14))
            Text(item.rawValue).font(.system(size: 11, weight: .semibold))
          }.foregroundStyle(tab == item ? PhoneTheme.violet : PhoneTheme.muted)
            .frame(maxWidth: .infinity).frame(height: 80).contentShape(Rectangle())
        }.buttonStyle(PhonePressStyle())
          .accessibilityAddTraits(tab == item ? [.isSelected] : [])
      }
    }.phoneGlass(radius: 30, strong: true).padding(.horizontal, 14).padding(.bottom, 4)
  }
}

struct DialerView: View {
  @ObservedObject var runtime: PhoneRuntime
  var settings: () -> Void
  @State private var inputMessage = ""

  private var hint: String {
    if !runtime.paired { return "先在设置中配对 Hub，即可开始通话" }
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
      let compact = geometry.size.height < 610
      ScrollView {
        VStack(spacing: 0) {
          HStack(alignment: .center, spacing: 10) {
            VStack(alignment: .leading, spacing: 4) {
              Text("拨号").font(.system(size: 24, weight: .heavy, design: .rounded)).accessibilityAddTraits(.isHeader)
              if !compact {
                Text("高清音质 · 安心通话").font(.system(size: 12)).foregroundStyle(PhoneTheme.secondary)
              }
            }
            Spacer(minLength: 0)
            Button(action: settings) {
              HStack(spacing: 7) {
                Circle().fill(runtime.online ? PhoneTheme.green : PhoneTheme.muted.opacity(0.5))
                  .frame(width: 8, height: 8)
                Text(runtime.online ? "已连接 · 在线" : runtime.working ? "连接中…" : "未连接")
                  .font(.system(size: 12, weight: .semibold))
              }.foregroundStyle(PhoneTheme.secondary).padding(.horizontal, 12).padding(.vertical, 9)
                .phoneGlass(radius: 30)
            }.buttonStyle(PhonePressStyle()).accessibilityLabel("连接状态，打开设置")
          }.padding(.top, 12)

          Spacer(minLength: compact ? 8 : 24)
          VStack(spacing: compact ? 8 : 12) {
            Text(runtime.dialNumber.isEmpty ? "输入电话号码" : runtime.dialNumber)
              .font(.system(size: runtime.dialNumber.isEmpty ? 26 : 34, weight: .bold, design: .rounded))
              .foregroundStyle(runtime.dialNumber.isEmpty ? PhoneTheme.muted : PhoneTheme.ink)
              .lineLimit(1).minimumScaleFactor(0.45).frame(maxWidth: .infinity).frame(minHeight: compact ? 34 : 46)
              .accessibilityLabel("电话号码").accessibilityValue(runtime.dialNumber.isEmpty ? "空" : runtime.dialNumber)
            LineMenu(runtime: runtime)
            PhoneKeypad(compact: compact, press: append, longPressZero: {
              if runtime.dialNumber.isEmpty { append("+") }
              else if runtime.dialNumber == "0" { runtime.dialNumber = "+" }
            }).padding(.horizontal, 8)
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
          Spacer(minLength: compact ? 8 : 16)
        }.padding(.horizontal, 22).frame(minHeight: geometry.size.height)
      }.scrollIndicators(.hidden)
    }
  }

  private func append(_ digit: String) {
    guard runtime.dialNumber.count < 32 else { return }
    runtime.dialNumber += digit
    inputMessage = ""
  }
}
