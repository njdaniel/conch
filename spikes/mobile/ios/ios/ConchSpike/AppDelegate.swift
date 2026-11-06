import UIKit
import livekit_react_native

@main
class AppDelegate: UIResponder, UIApplicationDelegate {
  func application(
    _ application: UIApplication,
    didFinishLaunchingWithOptions launchOptions: [UIApplication.LaunchOptionsKey: Any]? = nil
  ) -> Bool {
    // LiveKit's WebRTC options (encoder factory, audio processing module) must
    // be set before anything React Native starts, the iOS counterpart of the
    // Android spike's LiveKitReactNative.setup in MainApplication.onCreate
    // (#1391). Call order is the SDK's README for Swift. No camera options are
    // set: the camera is M5-D's.
    LivekitReactNative.setup()
    return true
  }
}
