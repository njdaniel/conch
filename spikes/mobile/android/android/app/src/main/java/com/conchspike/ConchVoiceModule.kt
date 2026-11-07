package com.conchspike

import android.content.BroadcastReceiver
import android.content.Context
import android.content.Intent
import android.content.IntentFilter
import android.media.AudioAttributes
import android.media.AudioDeviceCallback
import android.media.AudioDeviceInfo
import android.media.AudioFocusRequest
import android.media.AudioManager
import android.os.Handler
import android.os.Looper
import com.facebook.react.bridge.Arguments
import com.facebook.react.bridge.Promise
import com.facebook.react.bridge.ReactApplicationContext
import com.facebook.react.bridge.ReactContextBaseJavaModule
import com.facebook.react.bridge.ReactMethod
import com.facebook.react.modules.core.DeviceEventManagerModule

/**
 * The spike's native bridge (#1109, design section 7.4 item 5): what
 * @livekit/react-native 3.1.0 does not hand to JavaScript, written in Kotlin
 * with no library.
 *
 *  - the foreground service (microphone|mediaPlayback), started from Join;
 *  - audio focus, owned here (the SDK is told not to manage it) so that its
 *    loss and gain reach JavaScript as the lifecycle's "interruption";
 *  - the screen-off and unlock broadcasts as the lifecycle's "lock";
 *  - audio device changes, as the "route".
 *
 * Events go to JavaScript as "ConchVoiceEvent" with a "type" and a "state".
 * Nothing here carries a credential, an address or a device identifier.
 */
class ConchVoiceModule(private val context: ReactApplicationContext) : ReactContextBaseJavaModule(context) {
  override fun getName() = NAME

  private val audio = context.getSystemService(Context.AUDIO_SERVICE) as AudioManager
  private val main = Handler(Looper.getMainLooper())
  private var focus: AudioFocusRequest? = null
  private var receiver: BroadcastReceiver? = null
  private var devices: AudioDeviceCallback? = null
  private var pending: Promise? = null

  override fun initialize() {
    super.initialize()
    val screen = object : BroadcastReceiver() {
      override fun onReceive(c: Context?, intent: Intent?) {
        when (intent?.action) {
          Intent.ACTION_SCREEN_OFF -> emit("lock", "locked")
          // The screen on with the keyguard still up is still locked; the
          // unlock is the person getting past it.
          Intent.ACTION_USER_PRESENT -> emit("lock", "unlocked")
          // Screen on without a keyguard (none set, or already dismissed).
          Intent.ACTION_SCREEN_ON -> if (!locked()) emit("lock", "unlocked")
        }
      }
    }
    val filter = IntentFilter().apply {
      addAction(Intent.ACTION_SCREEN_OFF)
      addAction(Intent.ACTION_USER_PRESENT)
      addAction(Intent.ACTION_SCREEN_ON)
    }
    context.registerReceiver(screen, filter, Context.RECEIVER_NOT_EXPORTED)
    receiver = screen

    val callback = object : AudioDeviceCallback() {
      override fun onAudioDevicesAdded(added: Array<out AudioDeviceInfo>?) = emit("route", "changed", kinds(added))
      override fun onAudioDevicesRemoved(removed: Array<out AudioDeviceInfo>?) = emit("route", "changed", kinds(removed))
    }
    audio.registerAudioDeviceCallback(callback, main)
    devices = callback

    VoiceService.listener = { state ->
      main.post {
        val p = pending
        pending = null
        if (state.startsWith("failed:")) p?.reject("service_failed", state.removePrefix("failed:")) else p?.resolve(state)
      }
    }
  }

  override fun invalidate() {
    receiver?.let { context.unregisterReceiver(it) }
    devices?.let { audio.unregisterAudioDeviceCallback(it) }
    abandon()
    VoiceService.listener = null
    super.invalidate()
  }

  // Starts the foreground service. JS calls this from the Join press, with the
  // activity visible and RECORD_AUDIO granted; a start the OS refuses rejects
  // with the exception's class name.
  @ReactMethod
  fun startService(promise: Promise) {
    main.post {
      pending = promise
      try {
        context.startForegroundService(Intent(context, VoiceService::class.java))
      } catch (e: Exception) {
        pending = null
        promise.reject("service_failed", e.javaClass.simpleName)
      }
    }
  }

  @ReactMethod
  fun stopService(promise: Promise) {
    main.post {
      if (VoiceService.running) {
        context.startService(Intent(context, VoiceService::class.java).setAction(VoiceService.ACTION_STOP))
      }
      promise.resolve(null)
    }
  }

  private fun locked(): Boolean =
    context.getSystemService(Context.KEYGUARD_SERVICE).let { (it as android.app.KeyguardManager).isKeyguardLocked } ||
      !(context.getSystemService(Context.POWER_SERVICE) as android.os.PowerManager).isInteractive

  // Whether the screen is off or the keyguard is up: JS asks when the app
  // returns to the foreground, so a missed broadcast cannot leave the lock block on.
  @ReactMethod
  fun isLocked(promise: Promise) {
    promise.resolve(locked())
  }

  @ReactMethod
  fun serviceRunning(promise: Promise) {
    promise.resolve(VoiceService.running)
  }

  // Takes the audio focus for voice. A loss is reported as "lost"
  // (a call, the assistant, another app taking the audio) or "lost_transient";
  // the gain after it as "gained". The OS's hint to resume is not a thing the
  // app acts on: JS only lifts the block.
  @ReactMethod
  fun requestFocus(promise: Promise) {
    main.post {
      abandon()
      val request = AudioFocusRequest.Builder(AudioManager.AUDIOFOCUS_GAIN)
        .setAudioAttributes(
          AudioAttributes.Builder()
            .setUsage(AudioAttributes.USAGE_VOICE_COMMUNICATION)
            .setContentType(AudioAttributes.CONTENT_TYPE_SPEECH)
            .build(),
        )
        .setAcceptsDelayedFocusGain(false)
        .setWillPauseWhenDucked(false)
        .setOnAudioFocusChangeListener({ change ->
          when (change) {
            AudioManager.AUDIOFOCUS_LOSS -> emit("focus", "lost")
            AudioManager.AUDIOFOCUS_LOSS_TRANSIENT -> emit("focus", "lost_transient")
            // A duck is a notification sound, not an interruption.
            AudioManager.AUDIOFOCUS_LOSS_TRANSIENT_CAN_DUCK -> emit("focus", "duck")
            AudioManager.AUDIOFOCUS_GAIN -> emit("focus", "gained")
          }
        }, main)
        .build()
      focus = request
      promise.resolve(audio.requestAudioFocus(request) == AudioManager.AUDIOFOCUS_REQUEST_GRANTED)
    }
  }

  @ReactMethod
  fun abandonFocus(promise: Promise) {
    main.post {
      abandon()
      promise.resolve(null)
    }
  }

  // The kinds of the audio devices now attached, not their names or ids.
  @ReactMethod
  fun routes(promise: Promise) {
    val out = Arguments.createArray()
    kinds(audio.getDevices(AudioManager.GET_DEVICES_ALL)).split(",").filter { it.isNotEmpty() }.forEach { out.pushString(it) }
    promise.resolve(out)
  }

  private fun abandon() {
    focus?.let { audio.abandonAudioFocusRequest(it) }
    focus = null
  }

  private fun kinds(list: Array<out AudioDeviceInfo>?): String =
    (list ?: emptyArray()).map {
      when (it.type) {
        AudioDeviceInfo.TYPE_BLUETOOTH_SCO, AudioDeviceInfo.TYPE_BLE_HEADSET -> "bluetooth"
        AudioDeviceInfo.TYPE_BLUETOOTH_A2DP -> "bluetooth_media"
        AudioDeviceInfo.TYPE_WIRED_HEADSET, AudioDeviceInfo.TYPE_WIRED_HEADPHONES, AudioDeviceInfo.TYPE_USB_HEADSET -> "headset"
        AudioDeviceInfo.TYPE_BUILTIN_SPEAKER -> "speaker"
        AudioDeviceInfo.TYPE_BUILTIN_EARPIECE -> "earpiece"
        AudioDeviceInfo.TYPE_BUILTIN_MIC -> "mic"
        else -> "other"
      }
    }.distinct().sorted().joinToString(",")

  private fun emit(type: String, state: String, detail: String = "") {
    val map = Arguments.createMap().apply {
      putString("type", type)
      putString("state", state)
      putString("detail", detail)
    }
    if (context.hasActiveReactInstance()) {
      context.getJSModule(DeviceEventManagerModule.RCTDeviceEventEmitter::class.java).emit(EVENT, map)
    }
  }

  // Required by NativeEventEmitter.
  @ReactMethod fun addListener(eventName: String) {}
  @ReactMethod fun removeListeners(count: Double) {}

  companion object {
    const val NAME = "ConchVoice"
    const val EVENT = "ConchVoiceEvent"
  }
}
