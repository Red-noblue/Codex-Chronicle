// 中文说明：阅读器只保存来源与游标，正文来自有界只读窗口；不代表删除授权。
package reader

import "codex-chronicle.local/internal/domain"

const MaxLineBytes = 8 << 20
const PageReadBytes = 2 << 20
const PageTextBytes = 512 << 10

type Source struct {
	Relative string
	Path     string
	ID       string
}

type Segment struct {
	Source
	Start    int64
	End      int64
	ThreadID string
	Version  string
	Guard    string
}

type Plan struct {
	Segments    []Segment        `json:"-"`
	Diagnostics []string         `json:"diagnostics"`
	Complete    bool             `json:"complete"`
	Projection  *Projection      `json:"-"`
	Versions    []HistoryVersion `json:"versions,omitempty"`
	Leaf        string           `json:"source,omitempty"`
	Mainline    string           `json:"mainline,omitempty"`
}

type HistoryVersion struct {
	Path     string `json:"path"`
	Mainline bool   `json:"mainline"`
}

type Position struct {
	Source          string `json:"source,omitempty"`
	Path            string `json:"path"`
	Offset          int64  `json:"offset"`
	Line            uint64 `json:"line"`
	Turn            string `json:"turn,omitempty"`
	Inferred        bool   `json:"inferred,omitempty"`
	Version         string `json:"version,omitempty"`
	Guard           string `json:"guard,omitempty"`
	Size            int64  `json:"size,omitempty"`
	LastHash        string `json:"last_hash,omitempty"`
	LastType        string `json:"last_type,omitempty"`
	LastRole        string `json:"last_role,omitempty"`
	Skipping        bool   `json:"skipping,omitempty"`
	ProjectionStamp string `json:"projection_stamp,omitempty"`
}

type Page struct {
	Items         []domain.ConversationEvent `json:"items"`
	Next          string                     `json:"next_cursor"`
	Previous      string                     `json:"previous_cursor"`
	HasMore       bool                       `json:"has_more"`
	Complete      bool                       `json:"complete"`
	Diagnostics   []string                   `json:"diagnostics"`
	BytesRead     int64                      `json:"bytes_read"`
	SourceChanged bool                       `json:"source_changed"`
	Pending       bool                       `json:"pending"`
	SourceStamp   string                     `json:"source_stamp"`
	Indexing      bool                       `json:"indexing,omitempty"`
	IndexedBytes  int64                      `json:"indexed_bytes,omitempty"`
	TotalBytes    int64                      `json:"total_bytes,omitempty"`
	Versions      []HistoryVersion           `json:"versions,omitempty"`
	Source        string                     `json:"source,omitempty"`
	Mainline      string                     `json:"mainline,omitempty"`
}

type FrameLocator func(path string, offset int64, previous int) (int64, uint64, error)
