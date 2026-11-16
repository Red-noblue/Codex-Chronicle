// 中文说明：生成可解释的发布门槛报告；报告只读，不会把 fixture 证据升级为真实 Codex 写能力。
package api

import (
	"context"
	"fmt"
	"runtime"
	"strconv"

	resourcepkg "codex-chronicle.local/internal/resource"
)

// ReadinessCheck 是一个可审计的发布门槛结果。
type ReadinessCheck struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Status   string `json:"status"`
	Blocking bool   `json:"blocking"`
	Evidence string `json:"evidence"`
	NextStep string `json:"next_step,omitempty"`
}

// ReadinessReport 明确当前实现处于可读、fixture 或真实写入的哪一层。
type ReadinessReport struct {
	Status                    string           `json:"status"`
	RealCodexWriteReady       bool             `json:"real_codex_write_ready"`
	LocalRealExecutionEnabled bool             `json:"local_real_execution_enabled"`
	RuntimeOS                 string           `json:"runtime_os"`
	RuntimeArch               string           `json:"runtime_arch"`
	Checks                    []ReadinessCheck `json:"checks"`
}

func (s *Server) readinessReport(ctx context.Context) ReadinessReport {
	checks := make([]ReadinessCheck, 0, 12)
	checks = append(checks, ReadinessCheck{ID: "readonly-inventory", Name: "只读 inventory 与字节范围读取", Status: "pass", Evidence: "Go scanner、JSONL framing、SQLite 只读解析、canonical conversation asset graph 和 source-range API 已实现"})
	checks = append(checks, ReadinessCheck{ID: "stable-copy-creation", Name: "真实 .codex 稳定副本制作", Status: "implemented", Evidence: "已实现源目录活动/锁/WAL-SHM 门禁、分块哈希、staging、文件级同步、源前后复核、目标原子发布和副本 manifest；当前接口仍要求人工确认执行", NextStep: "在 Windows/Linux/macOS 真机和 80GB 级目录完成活动 writer、长路径、Unicode、临时空间和中断恢复验收"})
	checks = append(checks, ReadinessCheck{ID: "live-copy-read", Name: "Codex 运行期间活动读取与副本", Status: "implemented", Evidence: "LiveBestEffort 已支持 JSONL 捕获前缀、半截记录、追加/截断/重写证据、活动资产排队；SQLite 在隔离的主库/WAL 输入上使用 WAL 感知只读事务和 online backup，源主库、WAL、SHM 不作为发布快照且不被读取流程写入；破坏性操作仍按活动资产阻断", NextStep: "继续完成真实 macOS/Windows/Linux writer、SQLite sidecar 竞态和大容量流式验收"})
	checks = append(checks, ReadinessCheck{ID: "workspace-isolation", Name: "多工作区隔离", Status: "pass", Evidence: "工作区注册表、独立 archive.db 和切换回归已实现"})
	resourceSnapshot := s.inspectResources(ctx)
	resourceStatus := "pass"
	resourceNext := ""
	if !resourceSnapshot.WithinBudget {
		resourceStatus = "blocked"
		resourceNext = "清理或调整本项目派生数据预算后重新检查"
	}
	checks = append(checks, ReadinessCheck{ID: "resource-budget", Name: "资源预算", Status: resourceStatus, Blocking: resourceStatus == "blocked", Evidence: resourceEvidence(resourceSnapshot), NextStep: resourceNext})

	fixtureStatus := "blocked"
	fixtureEvidence := "未启用 fixture 写模式"
	if s.lifecycle != nil && s.lifecycle.Enabled && s.lifecycle.FixtureOnly {
		fixtureStatus = "pass"
		fixtureEvidence = "仅对明确合成/副本根目录开放维护与生命周期测试写入"
	}
	checks = append(checks, ReadinessCheck{ID: "fixture-lifecycle", Name: "fixture 生命周期 saga", Status: fixtureStatus, Evidence: fixtureEvidence})
	checks = append(checks, ReadinessCheck{
		ID: "real-copy-mutation", Name: "codex-real-copy-v1 副本 mutation adapter", Status: "implemented",
		Evidence: "已实现明确副本标记、固定 schema/语义摘要、SQLite 行级快照、触发器顺序、session index 原子重写、删除前预检、恢复和冲突拒绝；真实 .codex 仍不进入写路径",
		NextStep: "在从真实 .codex 制作的稳定副本上完成跨平台复制、活动 writer、WAL 和生命周期 saga 联调",
	})

	localRealEnabled := s.lifecycle != nil && s.lifecycle.AuthorizedLive != nil && s.lifecycle.AuthorizedLive.AllConversations && s.executionAllowed(s.currentRoot())
	realCheck := ReadinessCheck{ID: "real-codex-mutation", Name: "真实 Codex mutation adapter", Status: "blocked", Blocking: true,
		Evidence: "真实会话写执行未启用；副本与旧四 ID 实验不会自动授予全会话权限",
		NextStep: "本机已审阅 schema 可显式启用 --real-conversation-write；其他平台和复杂关联仍需单独适配"}
	if localRealEnabled {
		realCheck.Status, realCheck.Blocking = "implemented", false
		realCheck.Evidence = "已显式启用本机 macOS 闲置会话的原生废纸篓及永久删除；任意所选 UUID 均可预览，无固定四 ID 限制。执行仍核对已审阅的六库 schema、目标活动/锁、完整资产、备份、dry-run 和一次性数字；新通用路径使用合成源验证，没有新增真实删除证据"
		realCheck.NextStep = "复杂附件、spawn/共享记忆、未知 schema、Windows/Linux 写执行和物理断电尚未验收；整产品发布就绪与本机执行开关分别报告"
	}
	checks = append(checks, realCheck)
	checks = append(checks, ReadinessCheck{
		ID: "webdav-provider", Name: "WebDAV 归档 provider", Status: "implemented",
		Evidence: "已实现 HTTP/HTTPS、Basic/Bearer、代理、断点重试、分片校验、PROPFIND 列表、下载 staging 和删除；已有 httptest 回归",
		NextStep: "在用户实际 WebDAV 服务上完成认证、长路径、大文件和网络中断验收",
	})
	checks = append(checks, ReadinessCheck{
		ID: "gitee-provider", Name: "Gitee 归档 provider", Status: "implemented",
		Evidence: "已实现 Gitee Contents API 兼容 provider、token 认证、分片 manifest、列表、校验、下载和删除；已有 httptest 回归",
		NextStep: "在用户实际 Gitee 私有仓库上完成令牌权限、大小限制和网络中断验收",
	})
	checks = append(checks, ReadinessCheck{
		ID: "s3-provider", Name: "S3 归档 provider", Status: "implemented",
		Evidence: "已实现 AWS SigV4、path-style/custom endpoint、代理、逻辑分片、列表、校验、下载 staging 和删除；已有 fake S3 httptest 回归",
		NextStep: "在真实 AWS S3、MinIO 或其他 S3-compatible 服务上完成凭证、区域、长路径、大对象和网络中断验收",
	})
	checks = append(checks, ReadinessCheck{
		ID: "custom-http-provider", Name: "自定义 HTTP provider", Status: "implemented",
		Evidence: "已实现受限 PUT/GET/DELETE 对象协议、archives.json 索引、50 MiB 以内逻辑分片、重试、manifest 校验、staging 下载和删除；已有 httptest 回归",
		NextStep: "在用户实际 HTTP 对象服务上完成认证、代理、并发写入、大小限制和网络中断验收",
	})
	remoteAuthStatus := "blocked"
	remoteAuthBlocking := true
	remoteAuthEvidence := "当前 daemon 默认回环监听，尚未启用一次性绑定码和会话令牌"
	remoteAuthNext := "在显式远程监听中启用一次性绑定码、会话令牌、撤销、TLS 反向代理边界和访问审计"
	if s.authManager != nil {
		remoteAuthStatus = "implemented"
		remoteAuthBlocking = false
		remoteAuthEvidence = "已实现一次性绑定码、密码/会话登录、令牌哈希持久化、撤销和最小访问审计；仍需真实局域网/反向代理验收"
		remoteAuthNext = "在用户实际局域网和 TLS 反向代理环境完成绑定、过期、撤销和审计验收"
	}
	checks = append(checks, ReadinessCheck{
		ID: "remote-auth", Name: "局域网/公网访问认证", Status: remoteAuthStatus, Blocking: remoteAuthBlocking,
		Evidence: remoteAuthEvidence,
		NextStep: remoteAuthNext,
	})
	installerStatus := "blocked"
	installerEvidence := "daemon 未配置独立安装根，当前没有可用的离线更新控制面"
	installerNext := "为 macOS/Windows 首发平台建立安装包、daemon 重启健康检查、数据库迁移和失败回滚验收"
	if s.updater != nil {
		updateStatus, statusErr := s.updater.Status(ctx)
		if statusErr != nil {
			installerEvidence = "离线更新管理器已配置，但状态读取失败：" + statusErr.Error()
			installerNext = "先修复更新管理器状态读取，再进行安装包和真实重启验收"
		} else {
			installerStatus = "implemented"
			signatureMode := "Ed25519 清单签名校验可用但当前未强制"
			if updateStatus.SignatureRequired {
				signatureMode = fmt.Sprintf("Ed25519 清单签名校验已强制，当前信任根 %d 个", updateStatus.TrustedKeyCount)
			}
			onlineMode := "在线版本 feed 未配置"
			if updateStatus.OnlineConfigured {
				onlineMode = "在线版本检查和流式下载已配置"
			}
			installerEvidence = "已实现离线清单哈希校验、平台校验、版本目录、原子状态指针、健康检查、旧版本保留、回滚、archive.db 迁移前 SQLite online backup 与失败自动恢复、" + signatureMode + "、" + onlineMode + "；代码和 fixture 回归已通过，但仍未完成各平台安装包、真实 daemon 重启和发布密钥流程验收"
			installerNext = "为 macOS/Windows 首发平台建立安装包、固定发布密钥流程、daemon 重启健康检查，并完成真实数据库迁移和失败回滚验收"
		}
	}
	checks = append(checks, ReadinessCheck{
		ID: "installer-update", Name: "安装、离线更新和失败回滚", Status: installerStatus, Blocking: true,
		Evidence: installerEvidence,
		NextStep: installerNext,
	})
	checks = append(checks, ReadinessCheck{
		ID: "capacity-platform-acceptance", Name: "真实平台与 80–100GB 容量验收", Status: "blocked", Blocking: true,
		Evidence: "当前仅有合成 fixture、1/10GB 既有证据和交叉编译；没有 Windows/Linux 真机闭环或 80GB 实测",
		NextStep: "在足够存储的真实设备/虚拟机上完成锁、回收站、Unicode、长路径、查询 p95 和容量测试",
	})

	status := "not_ready"
	if !hasBlockingCheck(checks) {
		status = "ready"
	}
	return ReadinessReport{Status: status, RealCodexWriteReady: false, LocalRealExecutionEnabled: localRealEnabled, RuntimeOS: runtime.GOOS, RuntimeArch: runtime.GOARCH, Checks: checks}
}

func hasBlockingCheck(checks []ReadinessCheck) bool {
	for _, check := range checks {
		if check.Blocking && check.Status != "pass" {
			return true
		}
	}
	return false
}

func resourceEvidence(snapshot resourcepkg.Snapshot) string {
	return "持久空间 " + formatBytes(snapshot.PersistentBytes) + "/" + formatBytes(snapshot.PersistentLimitBytes) + "；临时空间 " + formatBytes(snapshot.TemporaryBytes) + "/" + formatBytes(snapshot.TemporaryLimitBytes) + "；堆内存 " + formatBytes(snapshot.MemoryBytes) + "/" + formatBytes(snapshot.MemoryLimitBytes)
}

func formatBytes(value int64) string {
	if value < 1024 {
		return strconv.FormatInt(value, 10) + " B"
	}
	units := []string{"B", "KB", "MB", "GB", "TB"}
	valueFloat := float64(value)
	unit := 0
	for valueFloat >= 1024 && unit < len(units)-1 {
		valueFloat /= 1024
		unit++
	}
	return strconv.FormatFloat(valueFloat, 'f', 1, 64) + " " + units[unit]
}
