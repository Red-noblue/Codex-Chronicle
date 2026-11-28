; 中文说明：Windows 每用户安装器；签名载荷与卸载元数据分开，安全迁移旧布局，保留用户数据和归档。
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
UninstallDisplayIcon={app}\app\bin\codex-chronicle-desktop.exe

[Files]
Source: "{#ChroniclePayload}\*"; DestDir: "{app}\app"; Flags: ignoreversion recursesubdirs createallsubdirs

[InstallDelete]
Type: filesandordirs; Name: "{app}\bin"; Check: HasLegacyPayload
Type: filesandordirs; Name: "{app}\web"; Check: HasLegacyPayload
Type: filesandordirs; Name: "{app}\update"; Check: HasLegacyPayload
Type: filesandordirs; Name: "{app}\licenses"; Check: HasLegacyPayload
Type: files; Name: "{app}\codex-chronicle-release.json"; Check: HasLegacyPayload

[Icons]
Name: "{group}\Codex Chronicle"; Filename: "{app}\app\bin\codex-chronicle-desktop.exe"
Name: "{group}\退出 Codex Chronicle 后台"; Filename: "{app}\app\bin\codex-chronicle-desktop.exe"; Parameters: "--quit"

[Run]
Filename: "{app}\app\bin\codex-chronicle-desktop.exe"; Description: "打开 Codex Chronicle"; Flags: nowait postinstall skipifsilent

[Code]
var
  LegacyOwned: Boolean;
  LegacyDirectory: String;

function HasLegacyPayload(): Boolean;
var
  CurrentDirectory, PreviousLocation: String;
begin
  CurrentDirectory := AddBackslash(ExpandConstant('{app}'));
  if CompareText(LegacyDirectory, CurrentDirectory) <> 0 then
  begin
    LegacyDirectory := CurrentDirectory;
    { 只清理同一 AppId 已登记目录里的旧程序文件，不按文件夹名称猜测其他目录的归属。 }
    LegacyOwned := FileExists(ExpandConstant('{app}\codex-chronicle-release.json')) and
      FileExists(ExpandConstant('{app}\bin\codex-chronicle-desktop.exe')) and
      RegQueryStringValue(HKCU, 'Software\Microsoft\Windows\CurrentVersion\Uninstall\{5FA52307-E1AC-4396-8C46-3F2D56214CF4}_is1', 'InstallLocation', PreviousLocation) and
      (CompareText(AddBackslash(PreviousLocation), CurrentDirectory) = 0);
  end;
  Result := LegacyOwned;
end;

function StopBackend(): Boolean;
var
  ResultCode: Integer;
  Launcher: String;
begin
  Launcher := ExpandConstant('{app}\app\bin\codex-chronicle-desktop.exe');
  if not FileExists(Launcher) then
    Launcher := ExpandConstant('{app}\bin\codex-chronicle-desktop.exe');
  Result := True;
  if FileExists(Launcher) then
    Result := Exec(Launcher, '--quit', '', SW_HIDE, ewWaitUntilTerminated, ResultCode) and (ResultCode = 0);
end;

function PrepareToInstall(var NeedsRestart: Boolean): String;
begin
  Result := '';
  if not StopBackend() then
  begin
    Result := '后台任务尚未完成，未强制退出。请等待任务结束后重试安装。';
    Exit;
  end;
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
