// 中文说明：提供 Codex 运行期间的 JSONL 活动读取，只消费捕获时已经存在的字节前缀。
package catalog

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"codex-chronicle.local/internal/domain"
)

// LiveConsistency 描述一次活动读取得到的内容一致性边界。
type LiveConsistency string

const (
	LiveStable          LiveConsistency = "stable"
	LiveAppending       LiveConsistency = "active_append"
	LivePendingLine     LiveConsistency = "active_pending_line"
	LiveRewriteConflict LiveConsistency = "rewrite_conflict"
	LiveTruncated       LiveConsistency = "truncated"
	LivePrefixLimited   LiveConsistency = "prefix_limited"
)

// LiveFileState 是一个文件在活动读取窗口内的可审阅证据。
type LiveFileState struct {
	BeforeSizeBytes int64           `json:"before_size_bytes"`
	SourceSizeBytes int64           `json:"source_size_bytes"`
	CapturedBytes   int64           `json:"captured_bytes"`
	BeforeModTime   time.Time       `json:"before_mod_time"`
	SourceModTime   time.Time       `json:"source_mod_time"`
	Identity        string          `json:"identity,omitempty"`
	SHA256          string          `json:"sha256"`
	Complete        bool            `json:"complete"`
	Active          bool            `json:"active"`
	PendingLine     bool            `json:"pending_line,omitempty"`
	Consistency     LiveConsistency `json:"consistency"`
	Diagnostics     []string        `json:"diagnostics,omitempty"`
}

// LiveJSONLSnapshot 是 JSONL framing 摘要和活动文件状态的组合结果。
type LiveJSONLSnapshot struct {
	State   LiveFileState           `json:"state"`
	Summary domain.JSONLScanSummary `json:"summary"`
}

// ScanLiveJSONL 在不锁定、不修改源文件的情况下扫描捕获时存在的 JSONL 前缀。
// 如果文件在读取期间追加内容，只返回初始大小范围，并把结果标记为 active_append。
func ScanLiveJSONL(ctx context.Context, path string) (LiveJSONLSnapshot, error) {
	return ScanLiveJSONLContent(ctx, path, nil)
}

// ScanLiveJSONLContent 在活动 JSONL 的捕获前缀上执行 framing，并把已完成行交给回调。
// 回调不会收到源文件捕获范围之外的数据；源文件追加、截断或替换只会体现在 State 中。
func ScanLiveJSONLContent(ctx context.Context, path string, handler ContentFrameHandler) (LiveJSONLSnapshot, error) {
	before, err := regularFileState(path)
	if err != nil {
		return LiveJSONLSnapshot{}, err
	}
	input, err := os.Open(path)
	if err != nil {
		return LiveJSONLSnapshot{}, fmt.Errorf("打开活动 JSONL 失败：%w", err)
	}
	defer input.Close()

	reader := &contextReader{ctx: ctx, reader: io.LimitReader(input, before.size)}
	summary, scanErr := ScanReaderContent(reader, handler)
	captured := reader.bytes
	prefixLimited := errors.Is(scanErr, ErrLimitReached)
	if scanErr != nil && !prefixLimited {
		return LiveJSONLSnapshot{}, scanErr
	}
	after, statErr := regularFileState(path)
	if statErr != nil {
		return LiveJSONLSnapshot{}, statErr
	}
	state := classifyLiveState(before, after, captured, summary.PendingLines > 0, summary.ContentSHA256)
	if prefixLimited {
		state.Complete = false
		state.Active = true
		state.Consistency = LivePrefixLimited
		state.Diagnostics = append(state.Diagnostics, "搜索达到结果上限，只保留已读取前缀并继续标记为只读")
	}
	return LiveJSONLSnapshot{State: state, Summary: summary}, nil
}

// CaptureLiveFile 读取普通文件的捕获前缀，用于活动副本的非 JSONL 资产。
func CaptureLiveFile(ctx context.Context, path string) (LiveFileState, error) {
	before, err := regularFileState(path)
	if err != nil {
		return LiveFileState{}, err
	}
	input, err := os.Open(path)
	if err != nil {
		return LiveFileState{}, fmt.Errorf("打开活动文件失败：%w", err)
	}
	defer input.Close()
	hash := sha256.New()
	reader := &contextReader{ctx: ctx, reader: io.LimitReader(input, before.size), hash: hash}
	if _, err := io.Copy(io.Discard, reader); err != nil {
		return LiveFileState{}, err
	}
	after, err := regularFileState(path)
	if err != nil {
		return LiveFileState{}, err
	}
	return classifyLiveState(before, after, reader.bytes, false, hex.EncodeToString(hash.Sum(nil))), nil
}

type fileState struct {
	info     os.FileInfo
	size     int64
	modTime  time.Time
	identity string
}

func regularFileState(path string) (fileState, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return fileState{}, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return fileState{}, fmt.Errorf("活动文件必须是普通文件：%s", path)
	}
	return fileState{info: info, size: info.Size(), modTime: info.ModTime().UTC(), identity: fileIdentity(info)}, nil
}

func classifyLiveState(before, after fileState, captured int64, pending bool, digest string) LiveFileState {
	state := LiveFileState{
		BeforeSizeBytes: before.size,
		SourceSizeBytes: after.size,
		CapturedBytes:   captured,
		BeforeModTime:   before.modTime,
		SourceModTime:   after.modTime,
		Identity:        before.identity,
		SHA256:          digest,
		Complete:        false,
		Active:          true,
		PendingLine:     pending,
		Consistency:     LiveRewriteConflict,
	}
	if captured < before.size || !os.SameFile(before.info, after.info) || after.size < before.size {
		state.Consistency = LiveTruncated
		state.Diagnostics = append(state.Diagnostics, "读取期间文件被截断、替换或无法读取完整捕获前缀")
		return state
	}
	if after.size > before.size {
		state.Consistency = LiveAppending
		state.Diagnostics = append(state.Diagnostics, "文件在读取后继续追加，快照只覆盖捕获前缀")
		return state
	}
	if !before.modTime.Equal(after.modTime) {
		state.Diagnostics = append(state.Diagnostics, "文件修改时间在读取窗口内变化，内容未被宣称为稳定")
		return state
	}
	if pending {
		state.Consistency = LivePendingLine
		state.Diagnostics = append(state.Diagnostics, "文件末尾存在尚未完成的 JSONL 记录")
		return state
	}
	state.Consistency = LiveStable
	state.Complete = true
	state.Active = false
	return state
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
	hash   io.Writer
	bytes  int64
}

func (reader *contextReader) Read(buffer []byte) (int, error) {
	if err := reader.ctx.Err(); err != nil {
		return 0, err
	}
	read, err := reader.reader.Read(buffer)
	if read > 0 {
		reader.bytes += int64(read)
		if reader.hash != nil {
			_, _ = reader.hash.Write(buffer[:read])
		}
	}
	return read, err
}

func fileIdentity(info os.FileInfo) string {
	return fmt.Sprintf("%d:%d", info.ModTime().UnixNano(), info.Size())
}
