// 中文说明：定义可选项目关系扫描的只读报告，不保存项目源文件正文。
package domain

import "time"

// ProjectEvidence 是项目关系判断使用的可解释证据。
type ProjectEvidence struct {
	Kind       string `json:"kind"`
	Value      string `json:"value"`
	Confidence string `json:"confidence"`
}

// ProjectFileSummary 是项目文件的元数据摘要，不包含文件内容。
type ProjectFileSummary struct {
	RelativePath string    `json:"relative_path"`
	Kind         string    `json:"kind"`
	SizeBytes    int64     `json:"size_bytes"`
	ModTime      time.Time `json:"mod_time"`
}

// ProjectRelationReport 是一次按需项目扫描的只读结果。
type ProjectRelationReport struct {
	ConversationID           string               `json:"conversation_id"`
	RequestedPath            string               `json:"requested_path,omitempty"`
	ProjectRoot              string               `json:"project_root,omitempty"`
	GitRoot                  string               `json:"git_root,omitempty"`
	GitBranch                string               `json:"git_branch,omitempty"`
	GitRemote                string               `json:"git_remote,omitempty"`
	WorkingDirectoryRelative string               `json:"working_directory_relative,omitempty"`
	Status                   string               `json:"status"`
	Confidence               string               `json:"confidence"`
	FileCount                int                  `json:"file_count"`
	IgnoredFileCount         int                  `json:"ignored_file_count"`
	SymlinkCount             int                  `json:"symlink_count"`
	TotalBytes               int64                `json:"total_bytes"`
	Truncated                bool                 `json:"truncated"`
	TopLevelDirs             []string             `json:"top_level_dirs,omitempty"`
	KeyFiles                 []ProjectFileSummary `json:"key_files,omitempty"`
	RecentFiles              []ProjectFileSummary `json:"recent_files,omitempty"`
	Evidence                 []ProjectEvidence    `json:"evidence,omitempty"`
	Limitations              []string             `json:"limitations,omitempty"`
	ReadOnly                 bool                 `json:"read_only"`
	GeneratedAt              time.Time            `json:"generated_at"`
}
