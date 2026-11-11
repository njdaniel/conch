package com.conchspike

import android.app.Application
import com.facebook.react.PackageList
import com.facebook.react.ReactApplication
import com.facebook.react.ReactHost
import com.facebook.react.ReactNativeApplicationEntryPoint.loadReactNative
import com.facebook.react.defaults.DefaultReactHost.getDefaultReactHost
import com.livekit.reactnative.LiveKitReactNative
import com.livekit.reactnative.audio.AudioType

class MainApplication : Application(), ReactApplication {

  override val reactHost: ReactHost by lazy {
    getDefaultReactHost(
      context = applicationContext,
      packageList =
        PackageList(this).packages.apply {
          // The spike's own native bridge: the foreground service, audio focus,
          // the lock broadcast and the route (ConchVoiceModule).
          add(ConchVoicePackage())
        },
    )
  }

  override fun onCreate() {
    // The SDK's native setup (its README; JS cannot do this): the audio device
    // module and the recording-samples tap. Before React Native starts.
    LiveKitReactNative.setup(this, AudioType.CommunicationAudioType())
    super.onCreate()
    loadReactNative(this)
  }
}
