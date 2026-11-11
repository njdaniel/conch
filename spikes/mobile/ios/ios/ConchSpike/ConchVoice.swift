// ConchVoice: the spike's one native module (#1395, M5-C; ADR-016 Decisions 5
// and 10). It owns what only iOS can say and JavaScript cannot see:
//  - the microphone permission, asked at Join and never at launch, and (M5-D,
//    #1396) the camera permission, asked at the first tap of the camera button
//    and never at launch or at Join;
//  - AVAudioSession's interruption notification (a call, Siri, an alarm,
//    another app taking the session) and its route-change notification (a
//    headset or Bluetooth device attached or gone), delivered to JavaScript as
//    "ConchVoiceEvent" events that src/app/bridge.ts feeds to logic/ios.ts;
//  - the output route types now in use, for the diagnostics.
// It does NOT set the session category or activate it: @livekit/react-native
// (registerGlobals and AudioSession) owns that, so the two never fight over
// the category. It only listens. It never reads a device's name, only the port
// type ("Speaker", "BluetoothHFP"), so a name cannot reach a log or an export.
// The interruption's "should resume" option is passed on for the receipt and
// is acted on by nothing (logic/ios.ts).
import AVFoundation
import Foundation
import React

@objc(ConchVoice)
final class ConchVoice: RCTEventEmitter {
  private var hasListeners = false
  private var tokens: [NSObjectProtocol] = []

  override static func requiresMainQueueSetup() -> Bool { false }

  override func supportedEvents() -> [String]! { ["ConchVoiceEvent"] }

  override func startObserving() { hasListeners = true }
  override func stopObserving() { hasListeners = false }

  // ---- permission

  @objc func requestMicrophone(
    _ resolve: @escaping RCTPromiseResolveBlock,
    rejecter reject: @escaping RCTPromiseRejectBlock
  ) {
    if #available(iOS 17.0, *) {
      switch AVAudioApplication.shared.recordPermission {
      case .granted: resolve("granted")
      case .denied: resolve("denied")
      default:
        AVAudioApplication.requestRecordPermission { granted in
          resolve(granted ? "granted" : "denied")
        }
      }
    } else {
      let session = AVAudioSession.sharedInstance()
      switch session.recordPermission {
      case .granted: resolve("granted")
      case .denied: resolve("denied")
      default:
        session.requestRecordPermission { granted in
          resolve(granted ? "granted" : "denied")
        }
      }
    }
  }

  // The camera permission, the same three answers. The prompt appears the
  // first time only; a denial is changed in Settings, not by asking again.
  // AVCaptureDevice only asks. Capture itself is LiveKit's (createLocalVideoTrack
  // in src/app/camera/capture.ts), and nothing here starts a capture session.
  @objc func requestCamera(
    _ resolve: @escaping RCTPromiseResolveBlock,
    rejecter reject: @escaping RCTPromiseRejectBlock
  ) {
    switch AVCaptureDevice.authorizationStatus(for: .video) {
    case .authorized: resolve("granted")
    case .denied, .restricted: resolve("denied")
    default:
      AVCaptureDevice.requestAccess(for: .video) { granted in
        resolve(granted ? "granted" : "denied")
      }
    }
  }

  // ---- observers

  // startSession begins listening for interruptions and route changes. It is
  // idempotent. (The name matches the NativePort of controller.ts; the audio
  // session itself is the SDK's.)
  @objc func startSession(
    _ resolve: @escaping RCTPromiseResolveBlock,
    rejecter reject: @escaping RCTPromiseRejectBlock
  ) {
    if tokens.isEmpty {
      let center = NotificationCenter.default
      let session = AVAudioSession.sharedInstance()
      tokens.append(
        center.addObserver(
          forName: AVAudioSession.interruptionNotification, object: session, queue: .main
        ) { [weak self] note in self?.onInterruption(note) })
      tokens.append(
        center.addObserver(
          forName: AVAudioSession.routeChangeNotification, object: session, queue: .main
        ) { [weak self] note in self?.onRouteChange(note) })
    }
    resolve("ok")
  }

  @objc func stopSession(
    _ resolve: @escaping RCTPromiseResolveBlock,
    rejecter reject: @escaping RCTPromiseRejectBlock
  ) {
    for token in tokens { NotificationCenter.default.removeObserver(token) }
    tokens.removeAll()
    resolve(nil)
  }

  @objc func routes(
    _ resolve: @escaping RCTPromiseResolveBlock,
    rejecter reject: @escaping RCTPromiseRejectBlock
  ) {
    resolve(outputTypes())
  }

  // ---- notifications

  private func onInterruption(_ note: Notification) {
    guard
      let raw = note.userInfo?[AVAudioSessionInterruptionTypeKey] as? UInt,
      let type = AVAudioSession.InterruptionType(rawValue: raw)
    else { return }
    switch type {
    case .began:
      emit(type: "interruption", state: "began", detail: "")
    case .ended:
      let options = (note.userInfo?[AVAudioSessionInterruptionOptionKey] as? UInt)
        .map { AVAudioSession.InterruptionOptions(rawValue: $0) }
      let resume = options?.contains(.shouldResume) == true
      emit(type: "interruption", state: "ended", detail: resume ? "shouldResume" : "")
    @unknown default:
      break
    }
  }

  private func onRouteChange(_ note: Notification) {
    let raw = note.userInfo?[AVAudioSessionRouteChangeReasonKey] as? UInt
    let reason = raw.flatMap { AVAudioSession.RouteChangeReason(rawValue: $0) }
    emit(type: "route", state: name(of: reason), detail: outputTypes().joined(separator: ", "))
  }

  // Names are the ones logic/ios.ts RouteChangeReason lists; anything else
  // reaches JavaScript as "unknown", which still shuts a held press.
  private func name(of reason: AVAudioSession.RouteChangeReason?) -> String {
    switch reason {
    case .newDeviceAvailable: return "newDeviceAvailable"
    case .oldDeviceUnavailable: return "oldDeviceUnavailable"
    case .categoryChange: return "categoryChange"
    case .override: return "override"
    case .wakeFromSleep: return "wakeFromSleep"
    case .noSuitableRouteForCategory: return "noSuitableRouteForCategory"
    case .routeConfigurationChange: return "routeConfigurationChange"
    default: return "unknown"
    }
  }

  private func outputTypes() -> [String] {
    AVAudioSession.sharedInstance().currentRoute.outputs.map { $0.portType.rawValue }
  }

  private func emit(type: String, state: String, detail: String) {
    guard hasListeners else { return }
    sendEvent(withName: "ConchVoiceEvent", body: ["type": type, "state": state, "detail": detail])
  }

  deinit {
    for token in tokens { NotificationCenter.default.removeObserver(token) }
  }
}
