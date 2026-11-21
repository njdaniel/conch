# Android device run

This is the step-by-step sheet for testing the Android app on a real phone for
M4 ([#1109](https://github.com/njdaniel/conch/issues/1109)). Write what you see
in the [Android receipt](../../../docs/reviews/mobile-android-voice.md). The
[capability matrix](../../../docs/reviews/mobile-capabilities.md#pending-capability-matrix)
has the full pass rule for every row ID (such as **M-F1**) named below. M6
([#1111](https://github.com/njdaniel/conch/issues/1111)) uses the Android and
iPhone receipts to choose how the real app gets built.

There are two sittings. **Run 1** (about 45 minutes) covers what M6 needs to
make that choice, and ends with a check of bug
[#1972](https://github.com/njdaniel/conch/issues/1972). **Run 2** covers every
other row, in groups you can do whenever their setup is ready. Nobody needs to
be a developer to follow it: where a step needs the server's records, it says
to ask the operator.

## Before you start

### Who is who

- **You** hold the Android phone. Only runs on a real phone count as device
  evidence; an emulator never does.
- **The listener** is a second person, signed in with **their own account** in
  a web browser on another device, in the same test channel. They say what they
  hear and see. Use headphones or separate rooms so the two devices don't feed
  back into each other.
- **Your second session** is the same account as the phone, signed in
  somewhere else: a browser, the desktop app or the terminal app. The hand-off
  steps move voice between it and the phone. If it shares a computer with the
  listener, use a separate browser profile for it.
- **The operator** runs the server (usually Nick). They can read the
  server's history: who joined and left voice, start and stop reports, camera
  records and their timestamps. When a step says "ask the operator", write down
  the clock time and send it to them afterwards.

### What to have ready

1. **The app.** Install the APK built by CI
   ([#2007](https://github.com/njdaniel/conch/issues/2007); the spike's README
   gets a "Download from CI" section when it lands), or one built with the
   [standalone APK recipe](README.md#standalone-apk-for-the-device-run-1920).
   Use this standalone APK so the phone can be unplugged. For the bug check at
   the end of Run 1, know whether the APK includes
   [PR #1985](https://github.com/njdaniel/conch/pull/1985) (ask the operator).
2. **Sign-in details.** The server's address (it must start with `https://`),
   a test channel, and your name and password or a fresh one-use sign-in link.
   Nick's Run 1 uses his own alpha server, `https://alpha.gankbears.com`, where
   he is the operator, with a test channel and a second account for the
   listener. Other testers use the server and the test accounts Nick gives
   them. Never use your everyday account for the listener or the second
   session.
3. **A clock.** A stopwatch on another device, or the listener's clock, to time
   things.
4. **A way to call the phone** (another phone or a friend), for the call steps.
5. **A steady sound source** for the open-microphone steps: another device,
   near the phone, playing a sound with no gaps in it, such as pink or white
   noise or music with no breaks. Not speech: after the microphone has been
   held shut, the app sends again once it has heard half a second of quiet and
   then a new sound, and the pauses between sentences are longer than that.
   It must be loud enough that step 14 shows **Open microphone: sending**.
   When a step says "pause and restart", pause it for about a second, then
   play it again. Any gap in the sound longer than half a second counts as a
   pause and restart: if the sound comes back after one, check the source for
   a gap before writing **fail**.
6. **A note of the setup** for the receipt's "Device runs" table: date, APK and
   its commit, phone model, Android version (Settings, About phone), whether
   Google Play services is on the phone, Wi-Fi or cellular, and whether battery
   saver is on.

### The app's screens

- **Sign in**: **Server**, **Name**, **Password**, **Sign in**; or a one-use
  link under **Or a one-use sign-in link**, then **Use the link**.
- **Channels**: your channels, and the buttons **Refresh**, **Diagnostics** and
  **Sign out**. Tap a channel to open it.
- **The channel's voice screen**: the status at the top (**Not in voice**,
  **Joining**, **In voice**, **Reconnecting**, **Waiting for you to confirm**),
  then **Join voice**. In voice it shows **Mute**, **Deafen**, **Use open
  microphone** (or **Use push to talk**), the big **Hold to talk** button (it
  says **Talking** while held), the line **Audio is being sent.** or **Nothing
  is being sent.**, **Turn camera on** (or **Turn camera off**), **Cameras**,
  and **Leave voice**. Under **In this voice room** are the people in voice;
  your own line ends in "(you)". Any warning appears in a box under the status.
  **Back** returns to Channels.

### Reading the Diagnostics screen

From a channel, tap **Back**, then **Diagnostics**. The first lines show the
voice state right now:

| Line | What it means |
| --- | --- |
| `phase` | `idle` (not in voice), `checking` or `joining` (on the way in), `confirm` (the take-over warning is showing), `connected`, `reconnecting` |
| `mode` | `ptt` for Hold to talk, `open` for the open microphone |
| `muted`, `deafened` | `true` when **Mute** or **Deafen** is on |
| `gate` | `open` when sound can leave the phone, `shut` when it cannot |
| `blocked` | `no`, or what is holding the microphone shut: `lock`, `background`, `inactive`, `interruption`, or `stopped` (Android sets `stopped` every time you leave the app). Several show together, joined by `+`, such as `background+stopped`. |

Below them is a log of recent events: the app going to the background or
coming back, voice and connection messages, and each request to the server
with its answer code. **Refresh** reloads the log. **Share export** sends the
log through Android's share sheet (save it to a file or send it to yourself).
Names, addresses, device IDs and room IDs are already removed from it. After
any failure, export straight away, before closing or restarting the app,
because closing the app empties the log. The export does not include the
state lines at the top, so take a screenshot of them too.

### What to screenshot

- The voice screen before and after each change, showing the status and any
  warning or message box.
- Android's microphone or camera indicator (the green dot at the top right) and
  the Conch voice notification, when they are visible.
- The listener's screen when it disagrees with the phone (their list of people
  in voice, or your camera tile).
- The Diagnostics state lines and an export after anything that fails.
- For battery and data: the same Android Settings screens at the start and at
  the end.

Name each file with the row, the build and the time. Before sharing, crop out
names, server addresses, room and device IDs, sign-in details and private
messages. Point the camera at a blank wall. Don't record audio or
conversations for the receipt. If an indicator disappears while the screen is
off, write down when you could see it and ask the operator whether voice stayed
connected.

### Writing results

For each row in the receipt write **pass** when every part of its rule was
checked and held, **fail** when you saw any part fail, **not run** when you
couldn't try it or only checked part of it (say which part in Evidence), or
**unsupported** when the row names a policy or Android limit. Write down real
times and how you measured them. A time limit under a second can't be judged by
eye: write what you saw, and the operator compares timestamps. Fill **Who ran
it** with your name. Nick's runs are the device evidence; other testers send
their notes to Nick.

## Run 1: about 45 minutes

Do the sections in this order. The minutes are a guide. If time runs short,
do fewer repeats rather than skipping a section, and write the count you did.

### Sign in, join and leave (about 4 minutes)

1. **M-F1.** On **Sign in**, type the server's address under **Server**.
   Enter your name and password and tap **Sign in**, or paste a fresh one-use
   link and tap **Use the link**. Expect **Channels** with your test channel
   and no voice running.
2. **M-F1, M-C2.** Tap the test channel. Expect **Not in voice** and **Join
   voice**, with no microphone or camera indicator.
3. **M-F1, M-C2.** Tap **Join voice**, and allow the microphone (and
   notifications) if Android asks. Expect **In voice**, your name with "(you)"
   under **In this voice room**, and the listener seeing you in voice. The
   camera stays off: no camera question, no camera indicator. Ask the listener
   to speak; you should hear them within 2 seconds of **In voice**. (The other
   direction is checked at step 6.)
4. **M-K2.** Tap **Leave voice**. Expect **Not in voice**, the Conch voice
   notification gone, and the microphone indicator gone within 2 seconds. The
   listener should see you leave voice within a few seconds. Note the time
   for the operator.
5. **M-F1, M-C2.** Tap **Join voice** again. Expect **In voice** with the
   camera off. Start step 6 as soon as it says **In voice**.

### Hold to talk and the 30-second limit (about 5 minutes)

6. **M-F1, M-F2.** Press and hold **Hold to talk** and speak. Expect it to say
   **Talking**, **Audio is being sent.**, and the listener to hear you. Ask
   the listener how long after your press they first heard you; the target is
   within 2 seconds of joining and within 200 ms of the press, so give the
   operator the time for an exact figure.
7. **M-F2.** Let go. Expect **Hold to talk** and **Nothing is being sent.**,
   and the listener to hear nothing more from you. Press and let go again until
   you have done 20 presses in all. Ask the operator to check 20 starts and 20
   stops in the server's history, and no `voice_transmit_unreported` record.
8. **M-F6.** Hold the button and keep talking for more than 30 seconds. Expect
   sending to stop at 30 seconds while your finger is still down, the message
   "The microphone shut after 30 s of one press. Release and press again.", and
   the listener to hear you stop. Let go, press again and check that you are
   heard again.

### Five minutes locked, then five minutes in another app (about 12 minutes)

Keep voice joined and don't touch **Hold to talk** in this section.

9. **M-L2.** Lock the phone (power button) and leave it locked for 5 minutes
   while nobody speaks. Look at the lock screen now and then: write down
   whether the Conch voice notification and the microphone indicator show, and
   when. Expect the listener to keep seeing you in voice the whole time, and
   to hear nothing from you.
10. **M-L2.** After 5 minutes, ask the listener to start talking, then unlock.
    Expect to hear them within 1 second of unlocking, the phone still **In
    voice**, and **Nothing is being sent.**
11. **M-L3.** Open another quiet app (for example Settings or a clock) and stay
    in it for 5 minutes, with the phone unlocked. Expect the same as step 9.
12. **M-L3.** Ask the listener to start talking, then switch back to Conch.
    Expect to hear them within 1 second and still send nothing.

### The microphone shuts when you leave the app, and waits for fresh sound (about 8 minutes)

13. **M-L5.** Hold **Hold to talk**, keep talking, and press the power button
    to lock. Expect the listener to hear you cut off at once, with no tail of
    sound. Unlock while still holding: expect nothing to be sent until you let
    go and press again. Do it once more, this time switching to another app
    (Home or Recents) instead of locking. Note the time of the lock and of the
    switch, and ask the operator to check that each made one stop report
    (`stopped`) in the server's history, and how soon after the lock or switch
    it came.
14. **M-L8.** Tap **Use open microphone**. In quiet, expect **Open microphone:
    waiting for you to speak**. Start the steady sound. Expect **Open
    microphone: sending** and the listener to hear it. If it stays on
    **waiting for you to speak**, turn the sound up or move it closer. Keep it
    playing for steps 15 to 22 unless a step says otherwise.
15. **M-L8.** Switch to another app while the sound plays. Expect the listener
    to hear it stop at once and not come back.
16. **M-L9.** Switch back to Conch with the sound still playing. Expect the
    listener to keep hearing nothing. Pause the sound for about a second, then
    restart it. Expect the listener to hear it only after the restart.
17. **M-L8.** Lock the phone while the sound is being sent. Expect it to stop
    at once, as in step 15.
18. **M-L9.** Unlock with the sound still playing. Expect silence until you
    pause and restart the sound.
19. **M-L8.** Have someone call the phone while the sound is being sent, and
    answer. Expect the sound to stop at once, as in step 15. Note the time and
    ask the operator to check that steps 15, 17 and 19 each made one stop
    report and no `voice_transmit_unreported` record.
20. **M-L9.** End the call with the sound still playing and go back to Conch.
    If the screen says "The microphone is held shut: interruption.", wait until
    the call is fully over, press Home, then reopen Conch: the app asks Android
    for the audio again only when it comes back to the screen (going **Back**
    and into the channel again doesn't). If it still says held shut after
    that, write it down, because then nothing is sent even after a restart.
    Expect silence until you pause and restart the sound.
21. **M-L9.** Turn on Airplane mode for about 20 seconds, then turn it off,
    with the sound still playing. Expect **Reconnecting** and then **In voice**
    again, and silence until you pause and restart the sound. If it never says
    **Reconnecting**, or voice ends instead, write down what it said.
22. **M-L9.** Tap **Use push to talk**, then **Use open microphone** again,
    with the sound still playing. Expect silence until you pause and restart
    it. Stop the sound and tap **Use push to talk** for the rest of the run.

### Move voice between your two sessions (about 5 minutes)

23. **M-S1.** Tap **Leave voice** on the phone. On your second session, join
    voice in the same channel. Expect the listener to see you in voice there.
24. **M-S1.** On the phone, tap **Join voice**. Expect **Waiting for you to
    confirm** and the warning "You are already in this voice room on another
    device. Joining here takes your place: the other device will stop.", with
    **Join here** and **Cancel**. Your second session stays in voice while the
    warning shows.
25. **M-S1.** Tap **Join here**. Expect your second session to leave voice
    within 1 second, say that another device took its place, and not rejoin on
    its own. Expect the listener to hear the phone when you press **Hold to
    talk**. Screenshot the phone's warning and the second session's exact
    words.
26. **M-S2.** On your second session, join voice in the same channel again.
    Expect the phone to stop with "Another device took over your place in this
    voice room", the Conch voice notification to go, and the phone not to
    rejoin on its own. Screenshot the phone's exact words.
27. **M-S1, M-C2.** Move voice back to the phone with **Join voice** and
    **Join here**. Expect the camera to be off.

### Camera (about 8 minutes)

Each time the camera stops below, check that Android's camera indicator goes
away and that the listener's tile for you goes away. Note the time: the
operator checks when the server's camera record ended and why. The aim is for
the camera to stop within 1 second of leaving the app or locking. The record
should end because the phone said so (`self`) or because the server noticed
nothing was being sent (`idle`, within 5 seconds of the video stopping plus
one server check).

28. **M-C1.** Tap **Turn camera on**, and allow the camera if Android asks.
    Expect **Your camera is being sent.** and the listener to see you. Time the
    gap from your tap to the listener's first picture. The operator checks the
    order on the server: the camera started, the server recorded it, then the
    video went out.
29. **M-C1.** Tap **Turn camera off**. Expect **Your camera is off.**, the
    indicator gone and the listener's tile gone. Turn it on and off until you
    have done 10 rounds in all. Ask the operator to check that each record
    ended with `self` and that there is no `voice_camera_unclaimed` record.
30. **M-C3, M-C6.** With the camera on, switch to another app. Expect the
    camera to stop. Come back. Expect the camera to stay off, the message "Your
    camera stopped because the app left the screen, the phone locked or a call
    came in. Turn it on again if you want to.", and no camera question.
31. **M-C4, M-C6.** Turn the camera on and lock the phone. Expect the camera
    to stop. Unlock. Expect it to stay off, with the same message.
32. **M-C5, M-C6.** Turn the camera on and have someone call the phone.
    Expect the camera to stop and, back in Conch, the same message and the
    camera off. Repeat with the voice assistant (hold the power button, or say
    "Hey Google") and with Android's own Camera app instead of the call. Write
    down whether each one also took you out of Conch. When you next turn the
    camera on yourself, expect the same camera (front or back) as before.
33. **M-C9.** With the phone's camera on, join voice on your second session
    (it takes voice from the phone). Expect the phone's camera to stop at once.
34. **M-C9.** Turn on your second session's camera, then take voice back with
    the phone (**Join voice**, **Join here**). Write down what happened to
    the second session's camera. Expect the phone's camera to stay off.

### Camera-on return bug, [#1972](https://github.com/njdaniel/conch/issues/1972) (about 4 minutes)

The bug: coming back to the app after about 30 seconds away with the camera on
can join voice twice, so the phone takes its own place and can leave a hidden
connection while the app says **Not in voice**. PR
[#1985](https://github.com/njdaniel/conch/pull/1985) is the fix. Keep the
listener in the room.

35. **M-C3, M-C6.** Turn the phone's camera on and check that the listener sees
    it. Note the time.
36. **M-C3, M-C6.** Press Home and wait about 30 seconds. Expect the camera
    indicator to go and the listener to stop seeing your video.
37. **M-C6, M-L7.** Bring Conch back from Recents. Don't touch anything for at
    least 45 seconds and watch. Expect the camera to stay off, and the phone's
    list of people to match the listener's. If voice reconnects, expect you to
    appear once, with no take-over warning. If voice ended, expect the message
    "Voice ended while the app was away. Join again when you want to talk."
    and your name gone from the listener's list.
38. **M-K2, M-L7.** If **Leave voice** is showing, tap it. Expect nobody from
    your account left in voice. Screenshot the phone's status, the listener's
    list of people in voice, and Diagnostics, and export the log. Even if the
    phone already said **Not in voice**, ask the operator to check for a hidden
    connection.

Send the operator the times of steps 35 to 38. They compare the server's room
history with what the phone showed: before Home, on return and after leaving.
Record in the receipt's bug table: the APK commit, the seconds away, whether
the old room dropped while the app was reconnecting, the order of connections
and disconnections, and any take-over warning. If you can, run steps 35 to 38
with an APK from before the fix and again with the #1985 APK. If 30 seconds
away never drops the room, try 40. If the drop and the return never overlap,
the bug check is **inconclusive**: say so. If they do overlap, the operator
checks that the dropped room ended before the new one started and that no late
connection took over. The bug verdict goes in its own table in the receipt.

## Run 2: the rest, grouped by setup

Do these groups in any order, whenever their setup is ready. Each line names
the rows it fills. Keep the listener in the channel unless the group says
otherwise.

### Voice controls and repeated joins (listener; operator for the counts)

- **M-F3.** Tap **Mute** and talk: the listener hears nothing. Tap **Unmute**,
  then **Deafen** while the listener talks: you hear nothing. Tap **Undeafen**.
  Then sign in with a test account the operator has set to listen only, and
  join: expect **In voice, listening only**, the listener's voice playing, and
  no microphone indicator.
- **M-F4.** Turn the phone's speaker on and tap **Use open microphone**. After
  quiet, speak, then stop. Expect **Open microphone: sending** when you speak
  and **Open microphone: waiting for you to speak** soon after you stop. Ask
  the listener whether they hear their own voice coming back, and write down
  any speaker warning the phone shows.
- **M-F5.** Hold **Hold to talk** and talk, then, still holding, swipe down
  the notification shade (or use a system back gesture, or let a call banner
  appear). Expect sending to stop at once with no tail, and **Hold to talk**
  to need a fresh press.
- **M-F7, M-C2, M-P3.** (Android 14 or later.) Do 20 rounds of **Join voice**
  and **Leave voice** without pressing talk or the camera. Expect the listener
  to hear nothing, no camera question or indicator, and no crash. The operator
  checks: the microphone was muted from the first moment on every join, no
  start reports, no camera records, and once more after forcing a reconnect
  and after rotating the room. Export Diagnostics and look for "exception" in
  it. Then tap **Join voice**, press Home while it says **Joining**, and come
  back. Either of two outcomes passes, because the join came from your tap:
  you are **In voice** and nothing is sent until you press **Hold to talk**;
  or the join failed with "Android would not start the voice service
  (ForegroundServiceStartNotAllowedException)." (this one doesn't count
  against the 20 joins). Write down which. It fails if the app crashes, if
  anything is sent before you press, or if voice ever starts without a tap on
  **Join voice**. Run 1's joins count only if the operator checked the same
  things for them.
- **M-L6.** Lock and unlock 10 times, then switch away and back 10 times,
  never pressing talk. Expect no joining and no sending caused by coming back.
  Then do the same with **Use open microphone** and the steady sound: silence
  each time until you pause and restart the sound.
- **M-L7.** If your phone ever drops voice while Conch is in the background
  (for example with battery saver on, or after a long time away), watch what
  the phone and the listener show when you come back. Expect the message
  "Voice ended while the app was away. Join again when you want to talk.", your
  name gone from the listener's list within a few seconds, and nothing hidden
  left behind (ask the operator). If voice never drops, write that down and
  leave the row **not run**.
- **M-L4.** Look in the app for a setting to keep the open microphone on in
  the background. This build has none, so write **unsupported**, "not offered
  on this OS". If a later build adds one, turn it on only for this check:
  expect sound to keep going out in the background, Android's microphone
  indicator showing the whole time, a call or lock stopping it until you act,
  and the battery and data cost written down. Turn it off again afterwards.

### The phone's web browser (listener)

- **M-L1.** This tests the browser, not the app. Open the server's address
  in Chrome on the phone, sign in, open the channel and join voice. Lock the
  phone for 5 minutes, then switch to another app for 5 minutes. Each time,
  record whether the listener still sees you in voice, whether you still hear
  them, and whether they still hear you while you talk. This is recorded only;
  a pass doesn't mean the browser will be supported.

### Wi-Fi, cellular and how the phone reaches the server (listener, operator)

- **M-N1.** In voice, turn Wi-Fi off so the phone uses mobile data, and later
  turn Wi-Fi back on. Each time expect **In voice** again within 10 seconds,
  nothing sent until a fresh press, and only one of you in the listener's
  list.
- **M-N2.** In voice, with Wi-Fi off, turn Airplane mode on for 60 seconds,
  then off. Expect the app to retry and come back into voice and chat by
  itself, and nothing to be sent until you press. Ask the operator to check
  the new connections.
- **M-N3.** Ask the operator to rotate the test voice room while you are in
  it. Expect you to come back into voice with the microphone and the camera
  off until you act.
- **M-C6.** Repeat Run 1's camera return steps (30 to 32) until you have 10
  rounds in all, including one Wi-Fi to mobile data switch and one Airplane
  mode round. Expect the camera never to start again by itself. The operator
  checks that a camera "continue" request only happened while the camera was
  still on in the foreground, and that a "stale" answer stopped the camera.
- **M-R1.** This needs a development build and a test server on your home
  network over plain `http://`. Sign in over Wi-Fi. Expect it to work, and the
  app to mark it as a development setup: **Channels** and the voice screen
  show "Development server: this connection is not encrypted (plain http).",
  and **Diagnostics** has the line `server development`. Write **pass** only
  if sign-in worked and all three show it; otherwise write **fail** and say
  which part failed. Against an `https://` server none of the three shows it. (The standalone APK only allows `https://`; with it,
  write **not run**.)
- **M-R2.** On mobile data, sign in and join voice through the server's
  public `https://` address. Expect chat and voice to work. The operator
  confirms the connection tickets and the voice server were reached.
- **M-R3.** Ask the operator for a test server whose certificate comes from
  their own certificate authority. Before installing it, expect sign-in to be
  refused with no "continue anyway" option. Install the certificate in
  Android's settings (Security, Encryption and credentials, Install a
  certificate) and expect sign-in and voice to work. Remove it afterwards.
- **M-R4.** Ask the operator to stop LiveKit (the voice server) and leave
  conchd running, at a time nobody else needs voice there; removing the
  server's voice setup instead tests something else. Open the channel and tap **Join voice**. Expect a message that voice is unavailable
  and no crash. Write down whether anything else in the app still works.

### Bluetooth headset, wired headset and buttons (listener)

- **M-B1.** Connect a Bluetooth headset before joining, then join. Expect the
  headset to show under **Audio devices** at the bottom of the voice screen,
  the listener's voice in the headset, and the listener to hear you through
  the headset's microphone.
- **M-B2.** Join on the phone's speaker, then connect the headset: expect the
  sound to move to it within 2 seconds. Hold **Hold to talk** and disconnect
  the headset while holding: expect sending to stop at once and no loud burst
  from the speaker.
- **M-B3.** Try the earpiece, the speaker and a wired headset. On each, check
  you hear the listener and they hear you. Choose one, turn Airplane mode on
  for about 20 seconds and off again, and check the choice is kept. Note your
  Android version (an earpiece bug on Android 12 is being checked).
- **M-B4, M-H1, M-H2.** Headset buttons, a talk button on the lock screen or
  in the notification, and the volume keys are not supported in this phase:
  write **unsupported**. If a test build shows these presses in Diagnostics,
  write down whether both the press and the release arrive while Conch is in
  the background.

### A phone call, permissions and ending a session (listener, someone to call, operator)

- **M-I1.** Get a call while in voice and not talking, and again while holding
  **Hold to talk**. Expect sending to stop at once, voice not rejoining by
  itself if it ended, and nothing sent after the call until a fresh press. If
  the screen says the microphone is held shut by "interruption" after the
  call, do as in Run 1 step 20 (Home, then reopen Conch) before pressing.
- **M-I2.** Repeat with the voice assistant, an alarm going off, and another
  app playing audio (a video or music app). Expect the same, even if Android
  offers to resume.
- **M-I3.** In voice, open Android Settings, Apps, Conch, Permissions, and turn
  off the microphone. Note the time. Android may close Conch when a
  permission is taken away; that is Android's doing, not a crash. Record
  whether the listener sees you leave voice (and how soon), whether Android's
  microphone indicator goes, and ask the operator whether voice left the room
  and any open start report was closed. Go back to Conch. If it is still
  running, expect sending to stop, the message "The microphone stopped. Check
  the app's microphone permission, then leave and join again.", and no
  repeated retrying. If Android closed it, expect **Sign in** (the sign-in is
  kept in memory only) and nothing rejoining voice by itself. Turn the
  permission back on.
- **M-P1, M-P2.** In Settings, Apps, Conch, Permissions, reset the
  microphone and notification permissions. Tap **Join voice** and deny the
  microphone: expect "Voice needs the microphone. Allow it for this app in
  Android settings, then join again." and **Channels** still working. On a
  separate try, allow the microphone and deny notifications: expect voice to
  work anyway.
- **M-K1.** Hold **Hold to talk** while a helper swipes Conch away in Recents
  (or force-stops it in Settings). Note the time. Expect the listener to see
  you leave voice within about 15 seconds, and opening Conch again not to
  rejoin voice. The operator checks the leave and the closed start report.
- **M-K3.** In voice, tap **Back**, then **Sign out**. Expect voice to end
  first, then the sign-in screen. Close and reopen the app: expect **Sign
  in**. The operator checks the order on the server.
- **M-S3.** While the phone is in voice, end the phone's sign-in from your
  second session (its list of your signed-in devices). Expect the phone to leave
  voice and show **Sign in** within 30 seconds.
- **M-S4.** Ask the operator to remove your account from the test channel while
  you are in voice there. Expect voice to end and the channel to disappear from
  **Channels**. The operator checks the server refuses the channel.
- **M-C7.** Reset the camera permission, join voice and tap **Turn camera
  on**. Expect Android's camera question only now, not earlier. Deny it:
  expect a message saying what to do next, no camera, and voice still
  working. Then allow it "only this time", and while the camera is on, turn
  the camera permission off in Settings. Android may close Conch here, as in
  M-I3. Record whether the camera indicator goes, whether the listener's tile
  for you goes and whether they see you leave voice, and ask the operator
  whether the camera claim was released. Back in Conch, expect no camera and
  no retrying; if Android closed it, expect **Sign in** and nothing rejoining
  voice or turning the camera on by itself. Also try Home or Recents while Android's camera question
  is showing, and write down what happened on return. Ask the operator to turn
  off camera use for your account and tap the button: expect "Camera is off for
  you on this server." and no camera. Then have them turn it off while your
  camera is on: expect it to stop with "Camera is not allowed for you."

### Other rooms and other cameras (listener, second session, operator)

- **M-S5.** Ask the operator to check the phone's requests for any use of the
  "companion" route: there should be none. This build has no Watch button, so
  leave the Watch part **not run**.
- **M-S6.** Join voice in channel A on your second session and in channel B on
  the phone. Write down whether both stay in voice with no warning, and
  whether each one can be heard in its own channel (a listener in each
  channel). This is recorded, not judged.
- **M-C8.** Ask the operator to have one, then three, other test accounts send
  camera video from browsers. Expect their tiles under **Cameras** while the
  voice screen is open, each named, and no camera opening by itself. Tap
  **Back**, then also try with Conch in the background: the operator checks
  the phone stopped receiving video and data use dropped. (Mismatched video is
  covered by a unit test, recorded separately.)

### Battery and data (about 2 hours 45 minutes in total)

Use the same phone, brightness and battery settings for every period, off the
charger. At the start and end of each period, screenshot Android's battery
use for Conch (Settings, Battery) and Conch's data use (Settings, Apps, Conch,
Mobile data and Wi-Fi). Write down the difference and how precise the screen
is. There is no pass mark: the figures are recorded.

- **M-E1.** On Wi-Fi: 30 minutes with Conch closed (the baseline), then 30
  minutes in voice with the phone locked and mostly quiet.
- **M-E2.** The same two periods on mobile data.
- **M-E3.** 30 minutes with Conch open on a channel, not in voice. This build
  has no chat feed: write **not run** and "no chat feed in this build".
- **M-C10.** On Wi-Fi with the screen on, three 15-minute periods: sending
  your camera while receiving one camera; only receiving one camera; voice
  only.

### TalkBack and large text

- **M-A1.** Turn on TalkBack (Settings, Accessibility). Sign in, open a
  channel, join voice, hold and release **Hold to talk**, and leave. Expect
  TalkBack to read every button's name, and the listener to hear you only
  while you hold. Write down anything you couldn't reach or operate. (This
  build has no message sending, so the "send" part is **not run**.)
- **M-A2.** Set Android's font size and display size to the largest. Go
  through the voice screen. Expect no cut-off text, and **Mute**, **Deafen**,
  **Use open microphone** and **Hold to talk** still big enough to tap easily
  (the operator measures them against 44 points if you can only judge by eye).
  Put your usual settings back.

### The iOS-only row

**M-H3** (Apple's Push to Talk framework) is iOS only. The matrix has no
Android cell for it; the receipt marks it "not run (iOS only)".

## What M6 decides without Run 2

Run 1 fills M-F1, M-F2, M-F6, M-K2, M-L2, M-L3, M-L5, M-L8, M-L9, M-S1, M-S2,
M-C1, M-C3, M-C4, M-C5 and M-C9, and part of M-C2, M-C6 and M-L7. With only
Run 1, M6 compares those against the iPhone receipt and decides without the
rows below. For each group, this is what M6 would rely on instead.

| Rows Run 1 doesn't fill | What M6 relies on without them |
| --- | --- |
| M-F3, M-F4, M-F5, M-F7, M-P3 (mute, deafen, listen-only, open microphone echo, cancelled touch, mute on join, the Android 14 service start) | The shared gate and disconnect logic's unit tests, the emulator's partial results (3 joins, no exception), and the browser's behaviour. These test the app's own logic more than the stack. |
| M-L1 (the phone's browser) | The design's position that the browser is not the phone app (design §2.1), with no browser evidence behind it. |
| M-L4, M-L6, M-L7 (open microphone in the background, 10 return cycles, the OS dropping voice) | "Not offered on this OS" for M-L4; Run 1's single lock and switch cycles for M-L6; whatever the #1972 check happened to show for M-L7. |
| M-B1, M-B2, M-B3, M-B4, M-H1, M-H2 (Bluetooth, wired, earpiece, buttons) | The LiveKit React Native SDK's documented audio routing. Whether native code is needed for routes stays open (design §7.4 point 5). |
| M-I1, M-I2, M-I3, M-P1, M-P2 (calls, the assistant, alarms, permissions) | Run 1's call checks (steps 19, 20 and 32) and unit tests for the permission messages. |
| M-N1, M-N2, M-N3, M-C6's network cycles (network changes, room rotation) | The SDK's reconnect, seen once in Run 1 step 21, and the desktop and browser behaviour of the same server. |
| M-K1, M-K3, M-S3, M-S4 (force-stop, sign out, sign-in ended, removed from the channel) | The server-side cleanup that the desktop and browser receipts already cover; the phone's part stays untested. |
| M-R1, M-R2, M-R3, M-R4 (reaching the server) | The release APK's HTTPS sign-in in Run 1 (part of M-R2 if done on mobile data) and Android's documented certificate rules. |
| M-S5, M-S6 (companion route, different rooms) | The code reading already in the matrix (both are D); M-S6 feeds Nick's question about one voice owner, not the stack choice. |
| M-E1, M-E2, M-E3, M-C10 (battery and data) | No figures. M6 chooses without costs; M9 and M10 set limits later from Run 2's numbers. |
| M-A1, M-A2 (TalkBack, large text) | React Native's accessibility support as documented, with no device check. |
| M-C2's 20 joins, M-C7, M-C8 (camera off on joining at scale, camera permission, receiving cameras) | Run 1's joins and the emulator's results for M-C2 and M-C7; unit tests for M-C8. |
| M-H3 | Nothing: it is iOS only. |
