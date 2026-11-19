; The Windows installer's own steps (issue #900; docs/design/windows-desktop.md
; section 9.3), included by electron-builder's NSIS target (the "include" of
; nsis in electron-builder.config.json). Read against electron-builder 26.17.0's
; templates in desktop/node_modules/app-builder-lib/templates/nsis.
;
; Owns two things the configuration cannot say:
;
;   - per-user only. An assisted installer (oneClick false, perMachine false)
;     shows a page that offers "all users" too, which an administrator can pick,
;     and /allusers on the command line asks for elevation even when
;     allowElevation is false (multiUserUi.nsh, assistedInstaller.nsh). This
;     installer never installs for all users: the choice is made here, and the
;     page is not shown. One gap no hook can close: the uninstaller, started
;     by hand with /allusers, asks for elevation before any hook runs (it then
;     still removes only the current user's install).
;   - the conch:// handler (conch://invite?server=<origin>&code=<code>,
;     linux-desktop.md section 9.1), as the Linux desktop file's
;     MimeType=x-scheme-handler/conch; does. electron-builder's "protocols" is
;     macOS only (PlatformSpecificBuildOptions.d.ts), so the key is written
;     here, under HKCU (per-user), and removed by the uninstaller. The app
;     takes the link from its command line (instance.ts, invites.ts).

; After initMultiUser (installer.nsi .onInit): whatever it read from the
; registry or the command line, this install is the current user's.
!macro customInit
  StrCpy $hasPerMachineInstallation "0"
  StrCpy $hasPerUserInstallation "1"
  !insertmacro setInstallModePerUser
!macroend

; The uninstaller's .onInit, after its initMultiUser (uninstaller.nsh). An
; uninstaller started by hand with /allusers has already asked for elevation by
; then, and nothing before this hook can stop that prompt; what runs is still
; the current user's uninstall, the only kind this installer makes.
!macro customUnInit
  StrCpy $hasPerMachineInstallation "0"
  StrCpy $hasPerUserInstallation "1"
  !insertmacro setInstallModePerUser
!macroend

; The install-mode page's pre-function, in the installer and the uninstaller
; alike (multiUserUi.nsh). The template tests /allusers right after this macro
; and, before it ever looks at isForceCurrentInstall, elevates and goes
; per-machine; so the mode is set here and the page left with Abort, before
; that test runs. A GUI install with /allusers stays the current user's and
; asks for nothing.
!macro customInstallMode
  StrCpy $hasPerMachineInstallation "0"
  StrCpy $hasPerUserInstallation "1"
  !insertmacro setInstallModePerUser
  Abort
!macroend

; After the files and shortcuts (installSection.nsh). An upgrade runs the old
; uninstaller first, which removes the key; this writes it again.
!macro customInstall
  WriteRegStr HKCU "Software\Classes\conch" "" "URL:Conch invite link"
  WriteRegStr HKCU "Software\Classes\conch" "URL Protocol" ""
  WriteRegStr HKCU "Software\Classes\conch\DefaultIcon" "" '"$INSTDIR\${APP_EXECUTABLE_FILENAME}",0'
  WriteRegStr HKCU "Software\Classes\conch\shell\open\command" "" '"$INSTDIR\${APP_EXECUTABLE_FILENAME}" "%1"'
!macroend

; Before the files go (uninstaller.nsh). The user's data under %APPDATA% is
; not touched: deleteAppDataOnUninstall is false.
!macro customUnInstall
  DeleteRegKey HKCU "Software\Classes\conch"
!macroend
