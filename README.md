# Codex Chronicle

本地优先的 Codex 对话档案库。把分散在 `.codex` 中的历史记录整理成可阅读、可搜索、可判断去留的会话，再由用户决定归档、移入回收站或删除。

**当前为预览版。** 首次启动默认只读。真实会话写入仅适配 macOS 上已审阅的 Codex 版本和数据库结构；Windows、Linux 的读取与安装包不代表真实删除已获验证。本项目与 OpenAI 无隶属关系。

[下载预览版](https://github.com/Red-noblue/Codex-Chronicle/releases) · [反馈问题](https://github.com/Red-noblue/Codex-Chronicle/issues)

## 可以做什么

- **会话库**：中文界面、项目和时间筛选、全文搜索、消息定位、长对话分段加载、用户与助手阅读视图、独立技术证据视图。
- **会话关系**：识别主对话和子代理，展示关联资产、活动状态和操作影响。
- **批量处理**：刷新后保留选择，影响预览、一次性数字确认、执行进度和失败即停。超过十项增加一次确认，并按每批最多十项处理。
- **归档中心**：本地和 GitHub 私有仓库归档、压缩分片、可选客户端加密、恢复到 `.codex`、本地与云端副本转换、较新版本恢复冲突选择。真实源的恢复仍受相同平台和兼容性门禁约束。
- **存储维护**：临时目录与文件占用扫描、目录级选择、清理预览、执行与恢复材料检查。可执行范围由工作区和服务端权限决定。
- **设置和统计**：亮暗主题、阅读密度、工作区自动发现、账户连接、网络代理、token 与存储统计。

摘要可在本地提取，也可配置云端模型；云端模型会收到所选摘要材料。GitHub 归档需用户主动连接账户并选择仓库；未配置时不会上传会话。

## 下载安装

在 Releases 中选择与设备对应的架构：`arm64` 包括 Apple 芯片和 64 位 ARM 设备，`amd64` 对应 Intel / AMD 64 位设备。

| 系统 | 安装包 | 使用方式 |
| --- | --- | --- |
| macOS 14+ | DMG、ZIP；arm64 / amd64 | 打开 DMG，将应用复制到“应用程序”；更新可运行其中的“安装或更新.command” |
| Windows | 每用户 Setup EXE、便携 ZIP；amd64 / arm64 | 运行安装器，或解压后启动 `bin/codex-chronicle-desktop.exe` |
| Linux | DEB、RPM、tar.gz、ZIP；amd64 / arm64 | 安装对应发行版包，或解压后运行 `bin/codex-chronicle-desktop` |

应用使用系统浏览器显示界面，后台默认监听本机 `127.0.0.1:8787`，端口冲突时安装版会选择空闲端口。首次启动自动检查 `CODEX_HOME` 和 `~/.codex`，也可在“设置 → 工作区”添加目录。

macOS 包当前只有临时签名，尚无 Developer ID 签名和 Apple 公证，系统可能阻止首次打开；请核对下载来源及 SHA256SUMS 后，在系统“隐私与安全性”中批准。Windows 安装器尚无代码签名。各平台的实际验证范围见对应 Release 说明。

### 数据存放和卸载

程序与用户数据分开保存：

- macOS：`~/Library/Application Support/Codex Chronicle/`
- Windows：`%LOCALAPPDATA%\Codex Chronicle\`
- Linux：`$XDG_DATA_HOME/codex-chronicle/`，未配置时为 `~/.local/share/codex-chronicle/`
- 本地归档默认：用户“文稿 / Documents”下的 `Codex Chronicle/archives/`；可另行选择。

先通过菜单栏或启动器 `--quit` 安全退出后台，再卸载应用或移走便携包。卸载保留配置、索引、归档和恢复材料，也不删除 `.codex`。这些数据可在确认不再需要后由用户另行处理。

### 真实数据操作

默认可以读取正在使用的 `.codex`，**禁止修改正在使用或已加载的目标对话**。同一设备仍有其他 Codex 会话运行时，闲置目标能否执行，取决于锁协议、版本、完整资产关联与数据库事务检查。

当前真实写入仅支持 macOS 当前用户精确的 `~/.codex` 根；需要 Python 3、配套的原生废纸篓桥接程序及正在运行的 Codex daemon socket。已审阅的 daemon 版本为 `0.166.0`、`0.167.0`、`0.167.2`、`0.169.0`，数据库 schema 也必须匹配。

安装版高级用户可在用户数据目录创建 `launcher.json` 后重启：

```json
{
  "scan_root": "/absolute/home/.codex",
  "real_conversation_write": true,
  "python": "/absolute/path/to/python3",
  "daemon_socket": "/absolute/path/to/codex-daemon.sock"
}
```

不要照抄示例路径；请使用本机真实的绝对路径。开启开关只允许进入执行流程：每次仍须扫描、检查活动和锁、解析专属资产、预览、准备恢复材料、dry-run、数字确认。未知 schema、关联不完整、共享资产归属不明、文件变化或数据库 busy 都会阻断。

“移入回收站”会将正文及必要恢复材料作为会话包移走；“删除”移除源会话，成功后清理本次临时恢复正文。共享 SQLite 按目标记录处理，不会整库移动或删除；事务由 SQLite 正常维护 WAL/SHM。归档副本的删除是独立操作。

## 从源码运行

依赖 Go（版本见 `go.mod`）、Node.js 22+、Python 3（集成测试）；macOS 桌面菜单栏构建还需要 Xcode Command Line Tools。

```bash
cd web
npm ci
npm run check
npm run build
cd ..
go test ./...
go vet ./...
python3 -m unittest discover -s docs/项目验证 -p test_authorized_live.py
go run ./cmd/codex-chronicle --scan-root /absolute/path/to/codex-home --archive-db /absolute/path/to/chronicle-data/archive.db --web-root web/dist
```

默认只监听本机。需要远程访问时，必须显式启用 `--allow-remote`，通过一次性绑定码建立会话，并自行配置 TLS 反向代理。

构建安装包：

```bash
bash scripts/build-release.sh 0.1.0-preview.11 darwin arm64
```

支持 `darwin / windows / linux` 与 `arm64 / amd64` 组合；macOS 包须在 macOS 构建。Windows EXE、Linux DEB/RPM 由仓库 Actions 工作流进一步封装。

## 工程结构

Go 后端是一个本地进程，前端使用 Svelte 5 + TypeScript，无微服务。兼容层隔离 Codex 文件、SQLite schema 与活动锁协议，便于后续适配新版本。

- `cmd/`：后台、桌面启动器、原生回收站桥接、发布打包命令。
- `internal/compat/`：JSONL / SQLite 兼容及受限真实执行器。
- `internal/catalog/`、`application/`、`store/`：流式读取、检索和独立控制面数据库。
- `internal/api/`：本地 HTTP API；`operation/`、`maintenance/`：生命周期与维护。
- `internal/archive/`：归档及远端存储；`desktop/`、`update/`：安装与更新。
- `web/`：中文工作台；`packaging/`、`scripts/`：安装和验证。
- `docs/项目验证/`：公开的合成测试生成器及兼容入口，不包含真实对话正文。

## 验证与已知限制

macOS 本机验证包括安装、覆盖更新、回退、安装中断恢复、隔离更新源下载、签名校验、坏候选拒绝、强杀后恢复，以及选定真实会话的生命周期实验。合成 fixture、稳定副本、隔离实时实验和真实源实验分别作为不同证据；不把它们当成全部设备均安全的保证。

在线更新底层已实现下载、验证、激活和回滚，但发布版尚未接入完整应用的一键联网替换与自动重启；当前通过下载新安装包更新。异常重启不会自动继续删除任务，需要人工复核。保留规则尚未开放无人值守自动删除。

反馈问题时请提供系统、版本、复现步骤和脱敏错误；不要提交会话正文、认证文件、令牌、数据库或恢复包。

## 许可证

本项目使用 [MIT 许可证](LICENSE)。第三方依赖保留各自许可，见 [THIRD_PARTY_NOTICES.txt](THIRD_PARTY_NOTICES.txt)；发行包也包含这些说明。
