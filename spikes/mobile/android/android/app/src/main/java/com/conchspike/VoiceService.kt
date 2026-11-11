package com.conchspike

import android.app.Notification
import android.app.NotificationChannel
import android.app.NotificationManager
import android.app.PendingIntent
import android.app.Service
import android.content.Intent
import android.content.pm.ServiceInfo
import android.os.IBinder

/**
 * The foreground service that keeps the process, and so the room, alive while
 * the person is in voice (ADR-016 Decision 3; #1109, M-L2, M-L3, M-P3).
 *
 * Its types are microphone and mediaPlayback and never camera (the manifest
 * declares the same), so the OS itself refuses background camera capture.
 * It is started only by [ConchVoiceModule.startService], which JS calls from
 * the Join press while the activity is visible, and it ends on Leave, on
 * swipe-away and on a stop from the module. It holds no audio and no
 * credential: it is a notification and a process priority, nothing else.
 */
class VoiceService : Service() {
  override fun onBind(intent: Intent?): IBinder? = null

  override fun onStartCommand(intent: Intent?, flags: Int, startId: Int): Int {
    if (intent?.action == ACTION_STOP) {
      stopForeground(STOP_FOREGROUND_REMOVE)
      stopSelf()
      report("stopped")
      return START_NOT_STICKY
    }
    try {
      startForeground(
        NOTIFICATION_ID,
        notification(),
        ServiceInfo.FOREGROUND_SERVICE_TYPE_MICROPHONE or ServiceInfo.FOREGROUND_SERVICE_TYPE_MEDIA_PLAYBACK,
      )
      running = true
      report("started")
    } catch (e: Exception) {
      // ForegroundServiceStartNotAllowedException, or a SecurityException when
      // RECORD_AUDIO is not granted: reported by name, never retried.
      running = false
      report("failed:" + e.javaClass.simpleName)
      stopSelf()
    }
    // Not sticky: a killed process must not come back in voice (M-K1).
    return START_NOT_STICKY
  }

  // Swiping the app away ends voice: the next launch does not rejoin.
  override fun onTaskRemoved(rootIntent: Intent?) {
    // Tell JavaScript first, if it is still there, so it leaves the room
    // (ConchVoiceModule emits it as the "service" event task_removed).
    report("task_removed")
    running = false
    stopForeground(STOP_FOREGROUND_REMOVE)
    stopSelf()
    super.onTaskRemoved(rootIntent)
  }

  override fun onDestroy() {
    running = false
    super.onDestroy()
  }

  private fun notification(): Notification {
    val manager = getSystemService(NotificationManager::class.java)
    manager.createNotificationChannel(
      NotificationChannel(CHANNEL_ID, "Voice", NotificationManager.IMPORTANCE_LOW),
    )
    val open = PendingIntent.getActivity(
      this,
      0,
      packageManager.getLaunchIntentForPackage(packageName),
      PendingIntent.FLAG_IMMUTABLE or PendingIntent.FLAG_UPDATE_CURRENT,
    )
    return Notification.Builder(this, CHANNEL_ID)
      .setContentTitle("Conch")
      .setContentText("You are in voice")
      .setSmallIcon(android.R.drawable.ic_btn_speak_now)
      .setOngoing(true)
      .setContentIntent(open)
      .setForegroundServiceBehavior(Notification.FOREGROUND_SERVICE_IMMEDIATE)
      .build()
  }

  private fun report(state: String) {
    listener?.invoke(state)
  }

  companion object {
    const val ACTION_STOP = "com.conchspike.STOP_VOICE"
    private const val CHANNEL_ID = "voice"
    private const val NOTIFICATION_ID = 1

    // The module's callback for "started", "stopped" and "failed:<class>".
    @Volatile var listener: ((String) -> Unit)? = null

    @Volatile var running = false
      private set
  }
}
