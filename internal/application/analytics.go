// 中文说明：从只读 inventory 生成 token、消息、存储和维护候选统计，不保存正文内容。
package application

import (
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"codex-chronicle.local/internal/domain"
)

// AnalyticsReport 是工作区统计的只读投影，所有数值都来自最近一次 inventory。
type AnalyticsReport struct {
	ReadOnly          bool              `json:"read_only"`
	GeneratedAt       time.Time         `json:"generated_at"`
	FileCount         int               `json:"file_count"`
	TotalBytes        int64             `json:"total_bytes"`
	ConversationCount int               `json:"conversation_count"`
	StableCount       int               `json:"stable_count"`
	BlockedCount      int               `json:"blocked_count"`
	TokenUsage        domain.TokenUsage `json:"token_usage"`
	MessageCount      int               `json:"message_count"`
	TurnCount         int               `json:"turn_count"`
	Daily             []DailyUsage      `json:"daily"`
	Models            []NamedCount      `json:"models"`
	Projects          []NamedCount      `json:"projects"`
	Statuses          []NamedCount      `json:"statuses"`
}

// DailyUsage 是按对话更新时间聚合的趋势点；未知日期不会伪造为当天。
type DailyUsage struct {
	Date          string `json:"date"`
	Conversations int    `json:"conversations"`
	Messages      int    `json:"messages"`
	Turns         int    `json:"turns"`
	TotalTokens   int64  `json:"total_tokens"`
}

// NamedCount 是模型、项目和状态分布的最小投影。
type NamedCount struct {
	Name        string `json:"name"`
	Count       int    `json:"count"`
	TotalTokens int64  `json:"total_tokens,omitempty"`
	TotalBytes  int64  `json:"total_bytes,omitempty"`
}

// MaintenanceReport 是临时文件、缓存、日志和 SQLite 辅助文件的只读候选清单。
type MaintenanceReport struct {
	ReadOnly       bool                   `json:"read_only"`
	GeneratedAt    time.Time              `json:"generated_at"`
	CandidateCount int                    `json:"candidate_count"`
	CandidateBytes int64                  `json:"candidate_bytes"`
	Candidates     []MaintenanceCandidate `json:"candidates"`
}

// MaintenanceCandidate 描述候选用途和风险，不代表可以安全删除。
type MaintenanceCandidate struct {
	RelativePath string             `json:"relative_path"`
	Category     string             `json:"category"`
	Reason       string             `json:"reason"`
	SizeBytes    int64              `json:"size_bytes"`
	ModTime      time.Time          `json:"mod_time"`
	Status       domain.AssetStatus `json:"status"`
	Risk         string             `json:"risk"`
	Recoverable  bool               `json:"recoverable"`
}

// BuildAnalytics 将 inventory 聚合为适合 UI 下钻的统计结果。
func BuildAnalytics(report domain.InventoryReport) AnalyticsReport {
	result := AnalyticsReport{
		ReadOnly: report.ReadOnly, GeneratedAt: report.GeneratedAt,
		FileCount: len(report.Files), ConversationCount: len(report.Conversations),
		Daily: make([]DailyUsage, 0), Models: make([]NamedCount, 0),
		Projects: make([]NamedCount, 0), Statuses: make([]NamedCount, 0),
	}
	for _, file := range report.Files {
		result.TotalBytes += file.SizeBytes
	}
	assetBytes := make(map[string]int64, len(report.Files))
	for _, file := range report.Files {
		assetBytes[file.RelativePath] = file.SizeBytes
	}
	daily := make(map[string]*DailyUsage)
	models := make(map[string]*NamedCount)
	projects := make(map[string]*NamedCount)
	projectAssets := make(map[string]map[string]struct{})
	statuses := make(map[string]*NamedCount)
	for _, conversation := range report.Conversations {
		if conversation.Status == domain.StatusStable {
			result.StableCount++
		} else {
			result.BlockedCount++
		}
		result.MessageCount += conversation.UserMessages + conversation.AssistantMessages
		result.TurnCount += conversation.TurnCount
		result.TokenUsage = addTokenUsage(result.TokenUsage, conversation.TokenUsage)

		model := AnalyticsModelName(conversation.Model)
		modelCount := ensureNamedCount(models, model)
		modelCount.Count++
		modelCount.TotalTokens += conversation.TokenUsage.TotalTokens

		project := AnalyticsProjectName(conversation.CWD)
		projectCount := ensureNamedCount(projects, project)
		projectCount.Count++
		projectCount.TotalTokens += conversation.TokenUsage.TotalTokens
		assetPaths := append([]string{}, conversation.RolloutPaths...)
		assetPaths = append(assetPaths, conversation.RelatedAssetPaths...)
		seenPaths := projectAssets[project]
		if seenPaths == nil {
			seenPaths = make(map[string]struct{}, len(assetPaths))
			projectAssets[project] = seenPaths
		}
		for _, path := range assetPaths {
			if _, ok := seenPaths[path]; ok {
				continue
			}
			seenPaths[path] = struct{}{}
			projectCount.TotalBytes += assetBytes[path]
		}

		statusCount := ensureNamedCount(statuses, string(conversation.Status))
		statusCount.Count++
		statusCount.TotalTokens += conversation.TokenUsage.TotalTokens

		stamp := conversation.UpdatedAt
		if stamp.IsZero() {
			stamp = conversation.StartedAt
		}
		if stamp.IsZero() {
			continue
		}
		date := stamp.UTC().Format("2006-01-02")
		point := daily[date]
		if point == nil {
			point = &DailyUsage{Date: date}
			daily[date] = point
		}
		point.Conversations++
		point.Messages += conversation.UserMessages + conversation.AssistantMessages
		point.Turns += conversation.TurnCount
		point.TotalTokens += conversation.TokenUsage.TotalTokens
	}
	result.Daily = mapDaily(daily)
	result.Models = mapNamedCounts(models)
	result.Projects = mapNamedCounts(projects)
	result.Statuses = mapNamedCounts(statuses)
	return result
}

// AnalyticsModelName 统一统计分组与精确下钻使用的模型名称。
func AnalyticsModelName(value string) string {
	if value = strings.TrimSpace(value); value != "" {
		return value
	}
	return "未知模型"
}

// AnalyticsProjectName 统一统计分组与精确下钻使用的项目路径，保留未知项。
func AnalyticsProjectName(value string) string {
	if value = strings.TrimSpace(value); value != "" {
		return path.Clean(strings.ReplaceAll(value, `\`, "/"))
	}
	return "未知项目"
}

// BuildMaintenanceReport 只标记名称和扩展名明确表明为维护候选的普通文件。
func BuildMaintenanceReport(report domain.InventoryReport) MaintenanceReport {
	result := MaintenanceReport{ReadOnly: report.ReadOnly, GeneratedAt: report.GeneratedAt, Candidates: make([]MaintenanceCandidate, 0)}
	for _, file := range report.Files {
		category, reason, risk, ok := maintenanceCategory(file.RelativePath)
		if !ok {
			continue
		}
		candidate := MaintenanceCandidate{
			RelativePath: file.RelativePath, Category: category, Reason: reason,
			SizeBytes: file.SizeBytes, ModTime: file.ModTime, Status: file.Status,
			Risk: risk, Recoverable: true,
		}
		result.Candidates = append(result.Candidates, candidate)
		result.CandidateBytes += file.SizeBytes
	}
	result.CandidateCount = len(result.Candidates)
	sort.Slice(result.Candidates, func(i, j int) bool {
		if result.Candidates[i].SizeBytes == result.Candidates[j].SizeBytes {
			return result.Candidates[i].RelativePath < result.Candidates[j].RelativePath
		}
		return result.Candidates[i].SizeBytes > result.Candidates[j].SizeBytes
	})
	return result
}

func maintenanceCategory(path string) (category, reason, risk string, ok bool) {
	normalized := filepath.ToSlash(path)
	parts := strings.Split(strings.ToLower(normalized), "/")
	base := parts[len(parts)-1]
	for _, part := range parts[:len(parts)-1] {
		switch part {
		case "tmp", ".tmp", "temp", ".temp":
			return "temporary", "路径包含临时目录名称", "high", true
		case "cache", "caches", ".cache":
			return "cache", "路径包含缓存目录名称", "medium", true
		case "logs", "log":
			return "log", "路径包含日志目录名称", "medium", true
		}
	}
	switch {
	case strings.HasSuffix(base, ".tmp") || strings.HasSuffix(base, ".temp"):
		return "temporary", "文件扩展名表明可能是临时文件", "high", true
	case strings.HasSuffix(base, ".log"):
		return "log", "文件扩展名表明可能是日志", "medium", true
	case strings.HasSuffix(base, "-wal") || strings.HasSuffix(base, "-shm"):
		return "sqlite_auxiliary", "SQLite WAL/SHM 辅助文件需要先确认是否仍在使用", "high", true
	default:
		return "", "", "", false
	}
}

func addTokenUsage(left, right domain.TokenUsage) domain.TokenUsage {
	return domain.TokenUsage{
		InputTokens:           left.InputTokens + right.InputTokens,
		CachedInputTokens:     left.CachedInputTokens + right.CachedInputTokens,
		OutputTokens:          left.OutputTokens + right.OutputTokens,
		ReasoningOutputTokens: left.ReasoningOutputTokens + right.ReasoningOutputTokens,
		TotalTokens:           left.TotalTokens + right.TotalTokens,
	}
}

func ensureNamedCount(values map[string]*NamedCount, name string) *NamedCount {
	if value := values[name]; value != nil {
		return value
	}
	value := &NamedCount{Name: name}
	values[name] = value
	return value
}

func mapNamedCounts(values map[string]*NamedCount) []NamedCount {
	result := make([]NamedCount, 0, len(values))
	for _, value := range values {
		result = append(result, *value)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Count == result[j].Count {
			return result[i].Name < result[j].Name
		}
		return result[i].Count > result[j].Count
	})
	return result
}

func mapDaily(values map[string]*DailyUsage) []DailyUsage {
	result := make([]DailyUsage, 0, len(values))
	for _, value := range values {
		result = append(result, *value)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Date < result[j].Date })
	return result
}
