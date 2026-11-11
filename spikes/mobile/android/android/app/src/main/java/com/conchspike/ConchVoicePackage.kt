package com.conchspike

import com.facebook.react.BaseReactPackage
import com.facebook.react.bridge.NativeModule
import com.facebook.react.bridge.ReactApplicationContext
import com.facebook.react.module.model.ReactModuleInfo
import com.facebook.react.module.model.ReactModuleInfoProvider

class ConchVoicePackage : BaseReactPackage() {
  override fun getModule(name: String, reactContext: ReactApplicationContext): NativeModule? =
    if (name == ConchVoiceModule.NAME) ConchVoiceModule(reactContext) else null

  override fun getReactModuleInfoProvider() = ReactModuleInfoProvider {
    mapOf(
      ConchVoiceModule.NAME to ReactModuleInfo(
        ConchVoiceModule.NAME, ConchVoiceModule.NAME, false, false, false, false,
      ),
    )
  }
}
