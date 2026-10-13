import SwiftUI

enum PhoneTheme {
  static let ink = Color(hex: 0x3D2C4F)
  static let secondary = Color(hex: 0x6E5F82)
  static let muted = Color(hex: 0x9A8CAD)
  static let violet = Color(hex: 0x8B6FF0)
  static let violetSoft = Color(hex: 0xEFE9FF)
  static let green = Color(hex: 0x2FBF83)
  static let greenSoft = Color(hex: 0xE2F8EE)
  static let red = Color(hex: 0xFF5C5C)
  static let redSoft = Color(hex: 0xFFEDED)
  static let amber = Color(hex: 0xFFB020)
  static let shadow = Color(hex: 0x5D3F82)
  static let coral = gradient(0xFF9A5A, 0xFF5C74)
  static let accept = gradient(0x4ED99A, 0x1FB87A)
  static let decline = gradient(0xFF7E6B, 0xF4445A)
  static let connection = gradient(0x8B6FF0, 0xFF6C8C)

  static func gradient(_ first: UInt32, _ last: UInt32) -> LinearGradient {
    LinearGradient(colors: [Color(hex: first), Color(hex: last)], startPoint: .topLeading, endPoint: .bottomTrailing)
  }
}

private extension Color {
  init(hex: UInt32) {
    self.init(.sRGB, red: Double((hex >> 16) & 255) / 255,
              green: Double((hex >> 8) & 255) / 255, blue: Double(hex & 255) / 255, opacity: 1)
  }
}

struct AppBackground: View {
  var body: some View {
    LinearGradient(stops: [
      .init(color: Color(hex: 0xFFEAD3), location: 0),
      .init(color: Color(hex: 0xFFD9E3), location: 0.46),
      .init(color: Color(hex: 0xE3D7FB), location: 1),
    ], startPoint: .topLeading, endPoint: .bottomTrailing).ignoresSafeArea()
  }
}

private struct GlassSurface: ViewModifier {
  var radius: CGFloat
  var strong: Bool
  func body(content: Content) -> some View {
    content
      .background(.white.opacity(strong ? 0.72 : 0.60), in: RoundedRectangle(cornerRadius: radius))
      .background(.ultraThinMaterial, in: RoundedRectangle(cornerRadius: radius))
      .overlay(RoundedRectangle(cornerRadius: radius).strokeBorder(.white.opacity(0.85), lineWidth: 1))
      .shadow(color: PhoneTheme.shadow.opacity(0.10), radius: 11, x: 0, y: 6)
  }
}

extension View {
  func phoneGlass(radius: CGFloat = 28, strong: Bool = false) -> some View {
    modifier(GlassSurface(radius: radius, strong: strong))
  }
}

struct PhonePressStyle: ButtonStyle {
  @Environment(\.isEnabled) private var enabled
  func makeBody(configuration: Configuration) -> some View {
    configuration.label
      .opacity(enabled ? (configuration.isPressed ? 0.75 : 1) : 0.55)
      .scaleEffect(configuration.isPressed ? 0.96 : 1)
      .animation(.easeOut(duration: 0.15), value: configuration.isPressed)
  }
}

struct PageHeading: View {
  var title: String
  var subtitle: String
  var body: some View {
    VStack(alignment: .leading, spacing: 5) {
      Text(title).font(.system(size: 32, weight: .heavy, design: .rounded)).accessibilityAddTraits(.isHeader)
      Text(subtitle).font(.subheadline).foregroundStyle(PhoneTheme.secondary)
    }.frame(maxWidth: .infinity, alignment: .leading).padding(.top, 10).padding(.bottom, 8)
  }
}

struct SectionCaption: View {
  var title: String
  var body: some View {
    Text(title).font(.system(size: 13, weight: .bold)).foregroundStyle(PhoneTheme.secondary)
      .frame(maxWidth: .infinity, alignment: .leading).padding(.leading, 4).padding(.top, 8)
      .accessibilityAddTraits(.isHeader)
  }
}

struct LineChip: View {
  var title: String
  var body: some View {
    Label(title, systemImage: "terminal")
      .font(.system(size: 13, weight: .bold)).foregroundStyle(PhoneTheme.violet)
      .padding(.horizontal, 14).padding(.vertical, 7).background(PhoneTheme.violetSoft, in: Capsule())
  }
}

struct LineMenu: View {
  @ObservedObject var runtime: PhoneRuntime
  var body: some View {
    Menu {
      Picker("拨号线路", selection: $runtime.selectedModule) {
        ForEach(runtime.modules, id: \.self) { id in Text(runtime.moduleName(for: id)).tag(id) }
      }
    } label: {
      HStack(spacing: 6) {
        Image(systemName: "terminal")
        Text(runtime.selectedModule.isEmpty ? "暂无可用线路" : "线路：\(runtime.moduleName(for: runtime.selectedModule))").lineLimit(1)
        Image(systemName: "chevron.down").font(.system(size: 10, weight: .bold))
      }
      .font(.system(size: 13, weight: .bold)).foregroundStyle(PhoneTheme.violet)
      .padding(.horizontal, 14).padding(.vertical, 8).background(PhoneTheme.violetSoft, in: Capsule())
    }.disabled(runtime.modules.isEmpty || runtime.callUUID != nil)
      .accessibilityLabel("选择拨号线路")
  }
}

struct PhoneAvatar: View {
  var name: String
  var size: CGFloat = 48
  var body: some View {
    let palettes: [(UInt32, UInt32)] = [
      (0xFF9A5A, 0xFF5C74), (0x8B6FF0, 0x5E9BFF), (0x4ED99A, 0x1FB87A),
      (0xFFB020, 0xFF7A59), (0xFF6FB5, 0xB565F0),
    ]
    let index = name.unicodeScalars.reduce(0) { ($0 + Int($1.value)) % palettes.count }
    ZStack {
      Circle().fill(PhoneTheme.gradient(palettes[index].0, palettes[index].1))
      if name.allSatisfy({ $0.isNumber || "+ -()".contains($0) }) || name.isEmpty {
        Image(systemName: "person.fill").font(.system(size: size * 0.38))
      } else {
        Text(String(name.prefix(1))).font(.system(size: size * 0.38, weight: .bold, design: .rounded))
      }
    }.foregroundStyle(.white).frame(width: size, height: size)
      .overlay(Circle().strokeBorder(.white.opacity(0.9), lineWidth: size > 80 ? 4 : 2.5))
      .shadow(color: PhoneTheme.shadow.opacity(0.13), radius: 10, y: 5).accessibilityHidden(true)
  }
}

struct RoundIconButton: View {
  var icon: String
  var label: String
  var color: Color = PhoneTheme.violet
  var background: Color = PhoneTheme.violetSoft
  var size: CGFloat = 44
  var action: () -> Void
  var body: some View {
    Button(action: action) {
      Image(systemName: icon).font(.system(size: 18, weight: .semibold))
        .foregroundStyle(color).frame(width: size, height: size).background(background, in: Circle())
    }.buttonStyle(PhonePressStyle()).accessibilityLabel(label)
  }
}

struct EmptyPhoneState: View {
  var icon: String
  var title: String
  var message: String
  var body: some View {
    VStack(spacing: 12) {
      Image(systemName: icon).font(.system(size: 36, weight: .light)).foregroundStyle(PhoneTheme.violet)
      Text(title).font(.headline)
      Text(message).font(.subheadline).foregroundStyle(PhoneTheme.secondary).multilineTextAlignment(.center)
    }.frame(maxWidth: .infinity).padding(.vertical, 34).padding(.horizontal, 22).phoneGlass()
  }
}

struct PhoneKeypad: View {
  var compact = false
  var press: (String) -> Void
  var longPressZero: (() -> Void)? = nil
  private let digits = ["1", "2", "3", "4", "5", "6", "7", "8", "9", "*", "0", "#"]
  private let letters = ["", "ABC", "DEF", "GHI", "JKL", "MNO", "PQRS", "TUV", "WXYZ", "", "+", ""]
  var body: some View {
    LazyVGrid(columns: Array(repeating: GridItem(.flexible(), spacing: compact ? 8 : 12), count: 3), spacing: compact ? 8 : 12) {
      ForEach(Array(digits.enumerated()), id: \.offset) { index, digit in
        if digit == "0", let longPressZero {
          key(digit, letters: letters[index])
            .highPriorityGesture(LongPressGesture().exclusively(before: TapGesture()).onEnded { value in
              switch value {
              case .first: longPressZero()
              case .second: press(digit)
              }
            }).accessibilityHint("长按输入加号")
        } else {
          key(digit, letters: letters[index])
        }
      }
    }
  }

  private func key(_ digit: String, letters: String) -> some View {
    Button { press(digit) } label: {
      VStack(spacing: 1) {
        Text(digit).font(.system(size: compact ? 24 : 28, weight: .bold, design: .rounded))
        Text(letters.isEmpty ? " " : letters)
          .font(.system(size: 10, weight: .semibold)).tracking(2.5).foregroundStyle(PhoneTheme.muted)
      }.frame(maxWidth: .infinity).frame(height: compact ? 46 : 74).phoneGlass(radius: compact ? 18 : 24)
    }.buttonStyle(PhonePressStyle()).accessibilityLabel("拨号键 \(digit)")
  }
}
