import SwiftUI

/// A display snapshot keeps the call layout independent of CallKit side effects.
struct PhoneCallPresentation {
  var display = ""
  var module = ""
  var phase = ""
  var connectedAt: Date?
  var pending = false
  var result = ""
  var dtmfPending = false
  var controlsSupported = false
  var muted = false
  var outgoing = false
  var speaker = false

  init() {}
  @MainActor init(runtime: PhoneRuntime) {
    display = runtime.callDisplay
    module = runtime.moduleName(for: runtime.callModule)
    phase = runtime.callPhase
    connectedAt = runtime.connectedAt
    pending = runtime.callControlPending
    result = runtime.lastCallResult
    dtmfPending = runtime.dtmfPending
    controlsSupported = runtime.callControlsSupported
    muted = runtime.muted
    outgoing = runtime.outgoing
    speaker = runtime.speaker
  }
}

struct PhoneCallView: View {
  @ObservedObject var runtime: PhoneRuntime
  @ObservedObject var contacts: PhoneContactsStore
  @State private var showingContacts = false

  var body: some View {
    PhoneCallScreen(call: PhoneCallPresentation(runtime: runtime), name: contacts.name(for: runtime.callDisplay),
                    answer: runtime.answerCall, end: runtime.endCall, mute: runtime.toggleMuted,
                    speaker: runtime.toggleSpeaker, sendTone: runtime.sendDTMF, showContacts: { showingContacts = true })
      .sheet(isPresented: $showingContacts) {
        NavigationStack {
          ZStack {
            AppBackground()
            PhoneContactsView(runtime: runtime, contacts: contacts, dial: { _, _ in }, allowsDial: false)
          }.toolbar { ToolbarItem(placement: .confirmationAction) { Button("完成") { showingContacts = false } } }
        }.tint(PhoneTheme.violet).foregroundStyle(PhoneTheme.ink).preferredColorScheme(.light)
      }
      .onChange(of: runtime.callUUID) { _, value in if value == nil { showingContacts = false } }
  }
}

struct PhoneCallScreen: View {
  var call: PhoneCallPresentation
  var name: String
  var answer: () -> Void
  var end: () -> Void
  var mute: () -> Void
  var speaker: () -> Void
  var sendTone: (String) -> Void
  var showContacts: () -> Void
  @State private var keyboard = false
  @State private var tones = ""
  private var incoming: Bool { call.phase == "incoming" }
  private var connected: Bool { call.connectedAt != nil }
  private var ending: Bool { call.phase == "ending" }
  private var controlsDisabled: Bool { !connected || call.pending || ending }
  private var state: String {
    switch call.phase {
    case "incoming": return "正在呼叫你…"
    case "dialing": return "正在拨号…"
    case "ringing": return "等待对方接听…"
    case "ending": return "正在挂断…"
    case "connected": return "通话中"
    default: return "正在连接…"
    }
  }

  var body: some View {
    GeometryReader { geometry in
      ZStack {
        AppBackground()
        ScrollView {
          VStack(spacing: 0) {
            LineChip(title: "\(incoming ? "来电 · " : "")\(call.module.isEmpty ? "DJOneHub" : call.module)")
              .padding(.top, 16)
            PhoneAvatar(name: name, size: keyboard ? 72 : incoming ? 118 : 104).padding(.top, keyboard ? 18 : 30)
            Text(name).font(.system(size: incoming ? 30 : 27, weight: .heavy, design: .rounded))
              .multilineTextAlignment(.center).lineLimit(2).minimumScaleFactor(0.65).padding(.top, 18)
            if name != call.display {
              Text(call.display).font(.subheadline).foregroundStyle(PhoneTheme.secondary).padding(.top, 5)
            }
            if let started = call.connectedAt {
              Text(started, style: .timer).monospacedDigit().font(.system(size: 16, weight: .semibold))
                .foregroundStyle(PhoneTheme.secondary).padding(.top, 6).accessibilityLabel("通话时长")
            } else {
              Text(state).font(.system(size: 15)).foregroundStyle(PhoneTheme.secondary).padding(.top, 6)
            }
            if ending || call.pending { ProgressView().tint(PhoneTheme.violet).padding(.top, 12) }
            if !call.result.isEmpty {
              Text(call.result).font(.caption).foregroundStyle(PhoneTheme.red).multilineTextAlignment(.center).padding(.top, 10)
            }

            if incoming {
              HStack(spacing: 10) {
                ForEach(0..<3) { index in Circle().fill(PhoneTheme.violet.opacity(Double(index + 1) / 3)).frame(width: 8, height: 8) }
              }.padding(.top, 22).accessibilityHidden(true)
              Spacer(minLength: 60)
              HStack {
                callAction(title: "拒接", icon: "phone.down.fill", gradient: PhoneTheme.decline, action: end)
                Spacer()
                callAction(title: "接听", icon: "phone.fill", gradient: PhoneTheme.accept, action: answer)
              }.padding(.horizontal, 32).padding(.bottom, 30)
            } else {
              Spacer(minLength: 24)
              if keyboard {
                Text(tones.isEmpty ? "输入分机号码" : tones).font(.system(size: 22, weight: .bold, design: .rounded))
                  .lineLimit(1).truncationMode(.head).padding(.bottom, 14)
                PhoneKeypad(compact: true, press: { digit in
                  tones = String((tones + digit).suffix(32))
                  sendTone(digit)
                }).disabled(call.dtmfPending || call.phase != "connected" || !call.controlsSupported)
                Button { keyboard = false } label: {
                  Label("收起键盘", systemImage: "chevron.down").font(.subheadline.bold()).padding(14)
                }.padding(.bottom, 10)
              } else {
                LazyVGrid(columns: Array(repeating: GridItem(.flexible(), spacing: 18), count: 3), spacing: 20) {
                  control(title: call.muted ? "取消静音" : "静音", icon: call.muted ? "mic.slash.fill" : "mic",
                          selected: call.muted, disabled: controlsDisabled, action: mute)
                  control(title: "键盘", icon: "square.grid.2x2", disabled: controlsDisabled || !call.controlsSupported) { keyboard = true }
                  control(title: "扬声器", icon: "speaker.wave.2", selected: call.speaker, disabled: !connected || ending, action: speaker)
                  control(title: "添加通话", icon: "plus", disabled: true) {}
                  control(title: "视频", icon: "video", disabled: true) {}
                  control(title: "联系人", icon: "person", disabled: ending) { showContacts() }
                }.padding(.bottom, 26)
              }
              callAction(title: ending ? "正在挂断…" : call.outgoing && !connected ? "取消拨号" : "挂断",
                         icon: "phone.down.fill", gradient: PhoneTheme.decline, action: end)
                .padding(.bottom, 26)
            }
          }.padding(.horizontal, 26).frame(minHeight: geometry.size.height)
        }.scrollIndicators(.hidden)
      }
    }.fontDesign(.rounded).foregroundStyle(PhoneTheme.ink).tint(PhoneTheme.violet)

  }

  private func control(title: String, icon: String, selected: Bool = false, disabled: Bool = false,
                       action: @escaping () -> Void) -> some View {
    Button(action: action) {
      VStack(spacing: 8) {
        Image(systemName: icon).font(.system(size: 24, weight: .medium))
          .foregroundStyle(selected ? PhoneTheme.violet : PhoneTheme.ink).frame(width: 66, height: 66)
          .phoneGlass(radius: 33, strong: selected)
        Text(title).font(.system(size: 12, weight: .semibold)).foregroundStyle(PhoneTheme.secondary)
      }
    }.buttonStyle(PhonePressStyle()).disabled(disabled)
      .accessibilityAddTraits(selected ? [.isSelected] : [])
      .accessibilityHint(title == "添加通话" || title == "视频" ? "当前服务暂不支持" : "")
  }

  private func callAction(title: String, icon: String, gradient: LinearGradient, action: @escaping () -> Void) -> some View {
    Button(action: action) {
      VStack(spacing: 9) {
        Image(systemName: icon).font(.system(size: 30)).foregroundStyle(.white)
          .frame(width: 74, height: 74).background(gradient, in: Circle())
          .shadow(color: (icon == "phone.fill" ? PhoneTheme.green : PhoneTheme.red).opacity(0.3), radius: 12, y: 8)
        Text(title).font(.system(size: 14, weight: .bold)).foregroundStyle(PhoneTheme.ink)
      }
    }.buttonStyle(PhonePressStyle()).disabled(call.pending || ending)
  }
}
