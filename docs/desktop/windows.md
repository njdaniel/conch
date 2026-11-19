# Conch for Windows

The Conch desktop installer for Windows x64 is an **unsigned preview for
testers**. Use a Conch server that matches the desktop app's release.

## Download and verify

From the Conch release page, download `Conch-<version>-unsigned-preview-setup.exe`
and `checksums.txt` from the same release into one folder. The version in the
filename is the release tag without the leading `v`.
<https://github.com/njdaniel/conch/releases>

In PowerShell, open that folder and set `$installer` to the actual filename.
Compare the SHA-256 printed for the installer with its entry in `checksums.txt`.

```powershell
$installer = 'Conch-0.1.0-alpha.22-unsigned-preview-setup.exe'
Get-FileHash -Algorithm SHA256 -LiteralPath $installer
Select-String -LiteralPath .\checksums.txt -SimpleMatch $installer
```

With the GitHub CLI signed in, set `$tag` to the release tag and verify the
installer's build provenance. Continue only if the hashes match and this
check succeeds.

```powershell
$tag = 'v0.1.0-alpha.22'
gh attestation verify $installer --repo njdaniel/conch --signer-workflow njdaniel/conch/.github/workflows/release.yml --source-ref "refs/tags/$tag"
```

## Install

1. Open the verified installer. If Windows SmartScreen shows **Windows
   protected your PC**, select **More info**, then **Run anyway**. The preview
   is unsigned, so Windows may display an unknown publisher warning.
2. Follow the installer prompts. Conch installs for your Windows account.
3. Open **Conch** from the Start menu and sign in to your server.

## First voice call and screen share

Windows Defender Firewall may ask whether to allow network access the first
time you use voice or watch a screen share.

- **conch-voice** is the native voice helper. Windows may show **Publisher:
  Unknown** for the unsigned preview.
- **Conch.exe** is the desktop app, which also handles watching screen shares.

On the test VM, choosing **Cancel** on both prompts still allowed voice and
screen-share watching to work. The preview installer leaves firewall settings
alone. Installer-managed rules are deferred until the Windows signing
decision ([#780](https://github.com/njdaniel/conch/issues/780)).
