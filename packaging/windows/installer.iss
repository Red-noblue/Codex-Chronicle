; 中文说明：Windows 每用户安装器模板；升级或卸载前请求安全退出，仅移除安装文件，保留用户数据和归档。
#define ChronicleVersion GetEnv("CHRONICLE_VERSION")
#define ChroniclePayload GetEnv("CHRONICLE_PAYLOAD")
#define ChronicleArch GetEnv("CHRONICLE_ARCH")
#if ChronicleVersion == "" || ChroniclePayload == ""
  #error 请设置 CHRONICLE_VERSION、CHRONICLE_PAYLOAD 和 CHRONICLE_ARCH 后再调用 ISCC
#endif

[Setup]
AppId={{5FA52307-E1AC-4396-8C46-3F2D56214CF4}
AppName=Codex Chronicle
AppVersion={#ChronicleVersion}
DefaultDirName={localappdata}\Programs\Codex Chronicle
DefaultGroupName=Codex Chronicle
PrivilegesRequired=lowest
#if ChronicleArch == "arm64"
ArchitecturesAllowed=arm64
ArchitecturesInstallIn64BitMode=arm64
#else
ArchitecturesAllowed=x64compatible
ArchitecturesInstallIn64BitMode=x64compatible
#endif
OutputDir=..\..\dist\{#ChronicleVersion}\windows-{#ChronicleArch}
OutputBaseFilename=Codex-Chronicle-{#ChronicleVersion}-{#ChronicleArch}-Setup
Compression=lzma2
SolidCompression=yes
CloseApplications=no
RestartApplications=no
UninstallDisplayIcon={app}\bin\codex-chronicle-desktop.exe

[Files]
Source: "{#ChroniclePayload}\*"; DestDir: "{app}"; Flags: ignoreversion recursesubdirs createallsubdirs

[Icons]
Name: "{group}\Codex Chronicle"; Filename: "{app}\bin\codex-chronicle-desktop.exe"
Name: "{group}\退出 Codex Chronicle 后台"; Filename: "{app}\bin\codex-chronicle-desktop.exe"; Parameters: "--quit"

[Run]
Filename: "{app}\bin\codex-chronicle-desktop.exe"; Description: "打开 Codex Chronicle"; Flags: nowait postinstall skipifsilent

[Code]
function StopBackend(): Boolean;
var
  ResultCode: Integer;
  Launcher: String;
begin
  Launcher := ExpandConstant('{app}\bin\codex-chronicle-desktop.exe');
  Result := True;
  if FileExists(Launcher) then
    Result := Exec(Launcher, '--quit', '', SW_HIDE, ewWaitUntilTerminated, ResultCode) and (ResultCode = 0);
end;

function PrepareToInstall(var NeedsRestart: Boolean): String;
begin
  Result := '';
  if not StopBackend() then
    Result := '后台任务尚未完成，未强制退出。请等待任务结束后重试安装。';
end;

function InitializeUninstall(): Boolean;
begin
  Result := StopBackend();
  if Result then
    { 卸载仅清理本应用的当前用户登录启动项；覆盖安装保留用户选择。 }
    RegDeleteValue(HKCU, 'Software\Microsoft\Windows\CurrentVersion\Run', 'CodexChronicle')
  else
    MsgBox('后台任务尚未完成，请等待任务结束后重试卸载。', mbError, MB_OK);
end;
