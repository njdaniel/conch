# Conch for Linux desktop

The Conch desktop app is a window for Conch on your Linux computer. It is
shipped as a `.deb` package. This page tells you how to check it, install it,
update it, and remove it, and what works on which systems.

**Status: technical preview.** No real-device acceptance run is recorded yet
(issue #1603). Read the [support matrix](#what-works-where) before you rely on
anything.

Building the app yourself is a developer task. See
[desktop/README.md](../../desktop/README.md).

## Before you start

You need:

- An Ubuntu 24.04-class system with AppArmor 4. Pop!_OS 24.04 and Ubuntu
  24.04 are the targets. The package says so in its description.
- An x86_64 (amd64) computer. There is no ARM64 package.
- A Conch server that the app has been tested with (see
  [Troubleshooting](#troubleshooting)).

## Download and verify

Download these files from the Conch release page into one empty folder:

- `conch-desktop_<version>_amd64.deb`
- `checksums.txt`

The release page is <https://github.com/njdaniel/conch/releases>.

> **Availability.** The `.deb`, the checksums and the attestations are
> available from the first release that ships the `.deb`. Earlier releases do
> not have them, and these steps do not work on them. They are specified in
> PR #1634, which is not merged at the time of writing.

Check the file is the one that was published:

```sh
sha256sum -c --ignore-missing checksums.txt
```

You should see `conch-desktop_<version>_amd64.deb: OK`. If you see `FAILED`,
stop. Do not install the file. Download it again.

Then check where it was built. This needs the GitHub CLI (`gh`). Replace `<tag>`
with the release tag, for example `v0.1.0-alpha.19`:

```sh
gh attestation verify conch-desktop_*_amd64.deb --repo njdaniel/conch --signer-workflow njdaniel/conch/.github/workflows/release.yml --source-ref refs/tags/<tag>
```

This proves the file was built by Conch's release workflow from that tag. If
the command fails, do not install the file.

There is no detached signature and no apt repository in the first release.

## Install

```sh
sudo apt install ./conch-desktop_<version>_amd64.deb
```

Use the real file name. Keep the `./`, so that apt reads the local file.

What the package does:

- It puts the app in `/opt/conch-desktop`, owned by root.
- It adds the command `conch-desktop`, a link in `/usr/bin`. (`conch` is
  the command-line tool, so the app does not use that name.)
- It adds a menu entry and icons.
- It adds a rule for `conch://invite?...` links, so the app opens them.
- It adds an AppArmor profile, `/etc/apparmor.d/conch-desktop`, and loads it.
  The profile lets the installed app use the sandbox that Chromium needs on
  Ubuntu 24.04. The app's sandbox stays on.

What the package does not do:

- It does not install a setuid `chrome-sandbox` program.
- It does not add a udev rule, change a group, or give any program extra
  powers.
- The app does not run as root.

If AppArmor cannot load the profile (an older system), the install says so and
goes on. On a system that restricts user namespaces, the app will then not
start.

## First run

Start **Conch** from your menu, or run:

```sh
conch-desktop
```

1. Enter your server's address. A server on `https://` is needed, except for
   `localhost`.
2. Sign in. You can add more servers later and switch between them.
3. Where it is possible, the app keeps your sign-in on this computer, locked
   by your desktop's password keyring. If there is no usable keyring, the app
   says why. It then keeps your sign-in only until you close the app, and you
   sign in again next time.
4. To talk with a key, set the talk key in the app's voice settings. The app
   shows the permission it needs and asks first. The package never sets it up
   for you.

If someone sent you an invite link that starts with `conch://`, click it. The
app opens and starts from the link.

To see what works on your computer, open the app's settings and find
**What works on this desktop**. Each row says Working, Not available here, or
Unknown, with one sentence why. **Copy this list** copies it for a problem
report.

### Open at login and the tray

- **Open Conch when I log in** is in the app's **Desktop app** settings. It
  works only if Conch is installed where only you or root can change it, which
  the package does. It writes `~/.config/autostart/chat.conch.desktop.desktop`.
- **Keep running in the tray** works only if your desktop shows a tray. If you
  cannot see the Conch icon, answer "No" when the app asks, and closing the
  window quits the app.

## Update

Updates are manual. The app does not download anything.

1. Download the new `.deb` and `checksums.txt`.
2. Verify them as above.
3. Quit Conch. If you are in a voice call, leave it first.
4. Install the new file:

   ```sh
   sudo apt install ./conch-desktop_<version>_amd64.deb
   ```

5. Start Conch again.

If you install while Conch is running, the app notices. It checks when a window
gains focus and every ten minutes. It shows "Conch was updated to X. Restart
when you're ready" with a **Restart** button. Nothing restarts by itself. A
call goes on, but Join and Retry say "Restart Conch to finish the update".

Your saved servers and sign-ins stay.

## Downgrade

Going back to an older version is **not tested and not supported**. Install a
newer version, or the same one, if you can.

What we know:

- Your saved servers are in `profiles.json`, version 1. An app that does not
  understand the file treats it as unreadable.
- If the app cannot read that file, it shows **The list of servers could not
  be read**. Nothing is changed or deleted. Choose **Start an empty list**. The
  old file is kept beside it, renamed. You add your servers and sign in again.
- We have not recorded whether an older app can read a newer app's saved
  sign-ins. Expect to sign in again.

To go back anyway, install the older `.deb` over the new one. Apt may need
`--allow-downgrades`:

```sh
sudo apt install --allow-downgrades ./conch-desktop_<older-version>_amd64.deb
```

Also match your server. The app accepts only the server release that was
shipped with it, and a few tested patch releases.

## Remove

Remove the program and keep its settings files in `/etc`:

```sh
sudo apt remove conch-desktop
```

Remove the program and the package's files in `/etc` (the AppArmor profile):

```sh
sudo apt purge conch-desktop
```

Neither command touches files in your home folder. The package's own scripts do
not remove them. Your data stays until you delete it:

| What | Where |
| --- | --- |
| Saved servers and sign-ins | `~/.config/conch-desktop` |
| Open-at-login entry, if you turned it on | `~/.config/autostart/chat.conch.desktop.desktop` |

To delete them, after you quit Conch:

```sh
rm -rf ~/.config/conch-desktop
rm -f ~/.config/autostart/chat.conch.desktop.desktop
```

If you only want to forget your servers, remove one in the app, or delete
`~/.config/conch-desktop`. Turn off **Open Conch when I log in** in the app
before you remove the package, or delete the file above afterwards.

If `$XDG_CONFIG_HOME` is set, your files are under it instead of `~/.config`.

## What works where

Each row is **supported**, **experimental**, **unsupported** or **not tried**.
"Not tried" means nobody has run it and recorded the result yet. Issue #1603
fills in the rest.

### Systems

| System | Status |
| --- | --- |
| Pop!_OS 24.04, COSMIC, Wayland, x86_64 | Supported after #1602 and #1603 pass. Until then, preview. |
| Ubuntu 24.04, GNOME | Experimental |
| Any desktop on X11 | Push-to-talk only. No screen sharing. |
| ARM64 | Not offered |
| AppImage, Flatpak, RPM | Not offered |

### Features

| Feature | Status | Notes |
| --- | --- | --- |
| Push-to-talk with a keyboard key | Not tried | Set up in the app. |
| Push-to-talk with Mouse 4 | Not tried | Set up in the app. |
| Screen sharing | Not tried | Wayland only, through the system's picker. Not on X11. |
| Sound when sharing | Unsupported | Silent on Linux for now (#1585, #1586). |
| Tray icon | Not tried | Needs a desktop with a tray. |
| Notifications | Not tried | |
| Lock and suspend | Not tried | Where the app can tell the screen locked, it mutes you and hides notification text. Otherwise it says it can't tell. |
| Saved sign-ins | Not tried | Needs a working password keyring. Without one, sign-ins last until the app closes. |

The app's own **What works on this desktop** list is the truth for your
computer.

## Troubleshooting

**The app does not start.** On Ubuntu 24.04 the AppArmor profile must be
loaded. Run:

```sh
sudo apparmor_parser -r /etc/apparmor.d/conch-desktop
```

If that fails, your AppArmor is older than version 4, and this package does not
support your system.

**No tray icon.** Your desktop may show no tray. Closing the window then quits
the app. This is expected. Open the app from the menu again.

**Muting does not follow the screen lock.** The app may not be able to tell
when your screen locks on this desktop. The settings list says so. Mute by hand.

**No screen sharing.** You can watch shares but not start one when your system
has no screen-share service, or when you are on X11. Log in to a Wayland
session. The settings list says which.

**"Server version not tested".** The app signs in only to server versions it
was tested with. Use the server release that matches the app. Ask the server's
operator.

**A private certificate authority.** If your server uses one, install it on this
computer. See "Private CA" in [desktop/README.md](../../desktop/README.md).

**The server list could not be read.** See [Downgrade](#downgrade).

**Report a problem.** Include the version (`dpkg -s conch-desktop | grep
Version`) and the copied **What works on this desktop** list.
