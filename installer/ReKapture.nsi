; ReKapture installer (NSIS 3, Modern UI 2)
; Build: makensis -DVERSION=2.3.0 -DSRC=<folder with ReKapture.exe, README.txt, ffmpeg\> ReKapture.nsi

Unicode true
ManifestDPIAware true
SetCompressor /SOLID lzma

!ifndef VERSION
  !define VERSION "0.0.0"
!endif
!ifndef SRC
  !define SRC "..\build\app"
!endif
!ifndef OUTFILE
  !define OUTFILE "ReKapture-Setup-${VERSION}.exe"
!endif

!define APP      "ReKapture"
!define EXE      "ReKapture.exe"
!define UNKEY    "Software\Microsoft\Windows\CurrentVersion\Uninstall\ReKapture"
!define APPKEY   "Software\ReKapture"
!define MUTEX    "Local\ReKaptureApp"

Name "${APP}"
Caption "${APP} ${VERSION} Setup"
BrandingText "${APP} ${VERSION}"
OutFile "${OUTFILE}"
InstallDir "$PROGRAMFILES32\${APP}"
InstallDirRegKey HKLM "${APPKEY}" "InstallDir"
RequestExecutionLevel admin

VIProductVersion "${VERSION}.0"
VIAddVersionKey "ProductName"     "${APP}"
VIAddVersionKey "ProductVersion"  "${VERSION}"
VIAddVersionKey "FileVersion"     "${VERSION}"
VIAddVersionKey "FileDescription" "${APP} Setup"
VIAddVersionKey "CompanyName"     "${APP}"
VIAddVersionKey "LegalCopyright"  "${APP}"

!include "MUI2.nsh"
!include "LogicLib.nsh"
!include "FileFunc.nsh"
!include "x64.nsh"

!define MUI_ICON   "ReKapture.ico"
!define MUI_UNICON "ReKapture.ico"
!define MUI_ABORTWARNING
!define MUI_WELCOMEFINISHPAGE_BITMAP   "sidebar.bmp"
!define MUI_UNWELCOMEFINISHPAGE_BITMAP "sidebar.bmp"
!define MUI_HEADERIMAGE
!define MUI_HEADERIMAGE_RIGHT
!define MUI_HEADERIMAGE_BITMAP   "header.bmp"
!define MUI_HEADERIMAGE_UNBITMAP "header.bmp"
!define MUI_COMPONENTSPAGE_NODESC

; ---- Pages: welcome, shortcuts, install folder, install, finish ----
!define MUI_WELCOMEPAGE_TITLE "Welcome to ${APP} ${VERSION}"
!define MUI_WELCOMEPAGE_TEXT "This will install ${APP}, Instant Replay and screen recording for NVIDIA GPUs.$\r$\n$\r$\nIf ${APP} is already running, quit it from its tray icon first. Your settings and clips are kept when you upgrade.$\r$\n$\r$\nClick Next to continue."
!insertmacro MUI_PAGE_WELCOME

!define MUI_PAGE_HEADER_TEXT "Shortcuts"
!define MUI_PAGE_HEADER_SUBTEXT "Choose where you want shortcuts to ${APP}."
!define MUI_COMPONENTSPAGE_TEXT_TOP "${APP} itself is always installed. Untick any shortcuts you don't want."
!insertmacro MUI_PAGE_COMPONENTS

!define MUI_PAGE_HEADER_TEXT "Install location"
!define MUI_PAGE_HEADER_SUBTEXT "Choose the folder to install ${APP} in."
!define MUI_DIRECTORYPAGE_TEXT_TOP "${APP} will be installed in the folder below. To use a different folder, click Browse and pick one, then click Install."
!insertmacro MUI_PAGE_DIRECTORY

!insertmacro MUI_PAGE_INSTFILES

!define MUI_FINISHPAGE_TITLE "${APP} is installed"
!define MUI_FINISHPAGE_TEXT "${APP} ${VERSION} has been installed on your computer.$\r$\n$\r$\nInstant Replay starts automatically. Press Alt+Shift+F10 to save the last 2 minutes.$\r$\n$\r$\nClick Finish to close Setup."
!define MUI_FINISHPAGE_RUN
!define MUI_FINISHPAGE_RUN_TEXT "Start ${APP}"
!define MUI_FINISHPAGE_RUN_FUNCTION LaunchApp
!define MUI_FINISHPAGE_SHOWREADME "$INSTDIR\README.txt"
!define MUI_FINISHPAGE_SHOWREADME_TEXT "Open the README"
!insertmacro MUI_PAGE_FINISH

; ---- Uninstaller pages ----
!insertmacro MUI_UNPAGE_CONFIRM
!insertmacro MUI_UNPAGE_INSTFILES
!insertmacro MUI_UNPAGE_FINISH

!insertmacro MUI_LANGUAGE "English"

; ---- Helpers ----
; Asks the user to quit ReKapture if it's running (it may be recording, so
; Setup never closes it by force).
!macro WaitForAppClosed un
  Function ${un}WaitForAppClosed
    StrCpy $1 0
    check:
      System::Call 'kernel32::OpenMutexW(i 0x00100000, i 0, w "${MUTEX}") i .r0'
      ${If} $0 <> 0
        System::Call 'kernel32::CloseHandle(i r0)'
        ; A silent update is started by ReKapture as it closes: wait for it.
        ${If} ${Silent}
        ${AndIf} $1 < 60
          IntOp $1 $1 + 1
          Sleep 500
          Goto check
        ${EndIf}
        MessageBox MB_RETRYCANCEL|MB_ICONEXCLAMATION "${APP} is running.$\r$\n$\r$\nRight-click its icon in the system tray (by the clock) and choose Quit, then click Retry." /SD IDCANCEL IDRETRY check
        Abort
      ${EndIf}
  FunctionEnd
!macroend
!insertmacro WaitForAppClosed ""
!insertmacro WaitForAppClosed "un."

; Start the app as the signed-in user, not elevated like Setup.
Function LaunchApp
  Exec '"$WINDIR\explorer.exe" "$INSTDIR\${EXE}"'
FunctionEnd

Function .onInit
  ${IfNot} ${RunningX64}
    MessageBox MB_OK|MB_ICONSTOP "${APP} needs 64-bit Windows 10 or 11." /SD IDOK
    Abort
  ${EndIf}
  Call WaitForAppClosed
  Call KeepShortcutChoicesOnUpdate
FunctionEnd

; After a silent update (started by ReKapture), start ReKapture again.
Function .onInstSuccess
  ${GetParameters} $R0
  ClearErrors
  ${GetOptions} $R0 "/UPDATE" $R1
  ${IfNot} ${Errors}
    Call LaunchApp
  ${EndIf}
FunctionEnd

Function .onVerifyInstDir
  ; Don't allow installing straight into a drive root or the Windows folder.
  StrLen $0 $INSTDIR
  ${If} $0 <= 3
    Abort
  ${EndIf}
FunctionEnd

; ---- Sections ----
Section "-${APP} (required)" SecApp
  SectionIn RO
  SetOutPath "$INSTDIR"
  ; Remove files from an older install that this version doesn't ship.
  Delete "$INSTDIR\portable.txt"
  File "${SRC}\${EXE}"
  File "${SRC}\README.txt"
  SetOutPath "$INSTDIR\ffmpeg"
  File "${SRC}\ffmpeg\ffmpeg.exe"
  File "${SRC}\ffmpeg\LICENSE.txt"
  SetOutPath "$INSTDIR"

  WriteUninstaller "$INSTDIR\Uninstall.exe"

  WriteRegStr HKLM "${APPKEY}" "InstallDir" "$INSTDIR"
  WriteRegStr HKLM "${UNKEY}" "DisplayName" "${APP}"
  WriteRegStr HKLM "${UNKEY}" "DisplayVersion" "${VERSION}"
  WriteRegStr HKLM "${UNKEY}" "Publisher" "${APP}"
  WriteRegStr HKLM "${UNKEY}" "DisplayIcon" "$INSTDIR\${EXE}"
  WriteRegStr HKLM "${UNKEY}" "InstallLocation" "$INSTDIR"
  WriteRegStr HKLM "${UNKEY}" "UninstallString" '"$INSTDIR\Uninstall.exe"'
  WriteRegStr HKLM "${UNKEY}" "QuietUninstallString" '"$INSTDIR\Uninstall.exe" /S'
  WriteRegDWORD HKLM "${UNKEY}" "NoModify" 1
  WriteRegDWORD HKLM "${UNKEY}" "NoRepair" 1
  ${GetSize} "$INSTDIR" "/S=0K" $0 $1 $2
  IntFmt $0 "0x%08X" $0
  WriteRegDWORD HKLM "${UNKEY}" "EstimatedSize" "$0"
SectionEnd

Section "Start menu shortcut" SecStart
  SetShellVarContext all
  CreateShortcut "$SMPROGRAMS\${APP}.lnk" "$INSTDIR\${EXE}" "" "$INSTDIR\${EXE}" 0
SectionEnd

Section "Desktop shortcut" SecDesktop
  SetShellVarContext all
  CreateShortcut "$DESKTOP\${APP}.lnk" "$INSTDIR\${EXE}" "" "$INSTDIR\${EXE}" 0
SectionEnd

; ---- Uninstaller ----
Function un.onInit
  Call un.WaitForAppClosed
FunctionEnd

Section "Uninstall"
  SetShellVarContext all
  Delete "$SMPROGRAMS\${APP}.lnk"
  Delete "$DESKTOP\${APP}.lnk"

  Delete "$INSTDIR\${EXE}"
  Delete "$INSTDIR\README.txt"
  Delete "$INSTDIR\ffmpeg\ffmpeg.exe"
  Delete "$INSTDIR\ffmpeg\LICENSE.txt"
  RMDir  "$INSTDIR\ffmpeg"
  Delete "$INSTDIR\Uninstall.exe"
  RMDir  "$INSTDIR"

  DeleteRegKey HKLM "${UNKEY}"
  DeleteRegKey HKLM "${APPKEY}"

  ; Per-user bits: "Start with Windows", and settings if the user wants them gone.
  SetShellVarContext current
  DeleteRegValue HKCU "Software\Microsoft\Windows\CurrentVersion\Run" "${APP}"
  MessageBox MB_YESNO|MB_ICONQUESTION|MB_DEFBUTTON2 "Also delete your ${APP} settings?$\r$\n$\r$\nYour saved clips are never deleted." /SD IDNO IDNO keep
    RMDir /r "$APPDATA\${APP}"
    RMDir /r "$LOCALAPPDATA\${APP}"
  keep:
SectionEnd

; On an update, don't bring back shortcuts the user deleted.
Function KeepShortcutChoicesOnUpdate
  ${GetParameters} $R0
  ClearErrors
  ${GetOptions} $R0 "/UPDATE" $R1
  ${If} ${Errors}
    Return
  ${EndIf}
  SetShellVarContext all
  ${IfNot} ${FileExists} "$SMPROGRAMS\${APP}.lnk"
    SectionSetFlags ${SecStart} 0
  ${EndIf}
  ${IfNot} ${FileExists} "$DESKTOP\${APP}.lnk"
    SectionSetFlags ${SecDesktop} 0
  ${EndIf}
FunctionEnd
