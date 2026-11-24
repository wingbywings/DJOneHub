import AVFAudio
import SwiftUI
import UserNotifications

final class PhoneApplicationDelegate: NSObject, UIApplicationDelegate {
  func application(
    _ application: UIApplication,
    didFinishLaunchingWithOptions options: [UIApplication.LaunchOptionsKey: Any]? = nil
  ) -> Bool {
    UNUserNotificationCenter.current().delegate = PhoneSMSInbox.shared
    PhoneRuntime.shared.startPushIfEnabled()
    return true
  }
  func application(_ application: UIApplication, didRegisterForRemoteNotificationsWithDeviceToken deviceToken: Data) {
    PhoneSMSInbox.shared.registered(deviceToken)
  }
  func application(_ application: UIApplication, didFailToRegisterForRemoteNotificationsWithError error: Error) {
    PhoneSMSInbox.shared.registrationFailed()
  }
}

@main struct DJOneHubPhoneApp: App {
  @UIApplicationDelegateAdaptor(PhoneApplicationDelegate.self) private var delegate
  @StateObject private var runtime = PhoneRuntime.shared
  @Environment(\.scenePhase) private var phase

  var body: some Scene {
    WindowGroup {
      PhoneHomeView(runtime: runtime)
        .onReceive(NotificationCenter.default.publisher(for: AVAudioSession.routeChangeNotification)) { _ in
          runtime.refreshAudioRoute()
        }
        .onChange(of: phase, initial: true) { _, value in
          if value == .active { runtime.foregrounded() }
          else if value == .background { runtime.backgrounded() }
        }
    }
  }
}
