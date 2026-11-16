// 中文说明：处理 GitHub 大文件元数据、真实目录边界、限流重试与可恢复上传进度。
package archive

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"
)

type transferProgressKey struct{}

// WithTransferProgress 接收本次已完成或已核对可复用的字节数；回调不包含正文。
func WithTransferProgress(ctx context.Context, report func(int64)) context.Context {
	return context.WithValue(ctx, transferProgressKey{}, report)
}

func reportTransfer(ctx context.Context, size int64) {
	if report, ok := ctx.Value(transferProgressKey{}).(func(int64)); ok {
		report(size)
	}
}

func gitBlobSHA(content []byte) string {
	h := sha1.New()
	fmt.Fprintf(h, "blob %d\x00", len(content))
	h.Write(content)
	return hex.EncodeToString(h.Sum(nil))
}

type githubContentMetadata struct {
	Type     string `json:"type"`
	Encoding string `json:"encoding"`
	Content  string `json:"content"`
	SHA      string `json:"sha"`
}

func (p *GitHubProvider) contentMetadata(ctx context.Context, remotePath string) (githubContentMetadata, error) {
	var value githubContentMetadata
	request, err := p.newRequest(ctx, http.MethodGet, remotePath, nil)
	if err != nil {
		return value, err
	}
	if p.config.ProviderID != "gitee" {
		request.Header.Set("Accept", "application/vnd.github.object+json")
	}
	query := request.URL.Query()
	if p.config.Branch != "" {
		query.Set("ref", p.config.Branch)
	}
	request.URL.RawQuery = query.Encode()
	response, err := p.doRetry(request)
	if err != nil {
		return value, err
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotFound {
		return value, errGitHubContentNotFound
	}
	if response.StatusCode != http.StatusOK {
		return value, p.responseError(response, "读取", remotePath)
	}
	limit := int64(4 << 20)
	if p.config.ProviderID == "gitee" {
		limit = 160 << 20
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, limit)).Decode(&value); err != nil {
		return value, err
	}
	if value.Type != "file" || value.SHA == "" {
		return value, fmt.Errorf("GitHub 对象不是有效文件: %s", remotePath)
	}
	return value, nil
}

// Contents 不支持分页；达到 1000 项时逐层读取非递归 Git tree，truncated 时明确失败。
func (p *GitHubProvider) listTreeDirectory(ctx context.Context, remotePath string) ([]githubDirectoryEntry, error) {
	ref := p.config.Branch
	if ref == "" {
		ref = "HEAD"
	}
	components := []string{}
	if strings.Trim(remotePath, "/") != "" {
		components = strings.Split(strings.Trim(remotePath, "/"), "/")
	}
	for level := 0; ; level++ {
		request, err := p.newRequest(ctx, http.MethodGet, "", nil)
		if err != nil {
			return nil, err
		}
		request.URL, err = url.Parse(strings.TrimRight(p.baseURL.String(), "/") + "/repos/" + url.PathEscape(p.config.Owner) + "/" + url.PathEscape(p.config.Repository) + "/git/trees/" + url.PathEscape(ref))
		if err != nil {
			return nil, err
		}
		response, err := p.doRetry(request)
		if err != nil {
			return nil, err
		}
		if response.StatusCode != http.StatusOK {
			err = p.responseError(response, "读取目录树", remotePath)
			response.Body.Close()
			return nil, err
		}
		var tree struct {
			Truncated bool `json:"truncated"`
			Tree      []struct {
				Path string `json:"path"`
				Type string `json:"type"`
				SHA  string `json:"sha"`
			} `json:"tree"`
		}
		err = json.NewDecoder(io.LimitReader(response.Body, 32<<20)).Decode(&tree)
		response.Body.Close()
		if err != nil {
			return nil, err
		}
		if tree.Truncated {
			return nil, fmt.Errorf("GitHub 目录树不完整，停止扫描")
		}
		if level == len(components) {
			entries := make([]githubDirectoryEntry, 0, len(tree.Tree))
			for _, entry := range tree.Tree {
				if strings.ContainsAny(entry.Path, "/\\") || entry.Path == ".." {
					return nil, fmt.Errorf("GitHub 目录树含无效路径")
				}
				kind := "file"
				if entry.Type == "tree" {
					kind = "dir"
				} else if entry.Type != "blob" {
					continue
				}
				entries = append(entries, githubDirectoryEntry{Type: kind, Path: path.Join(remotePath, entry.Path)})
			}
			return entries, nil
		}
		found := false
		for _, entry := range tree.Tree {
			if entry.Type == "tree" && entry.Path == components[level] {
				ref, found = entry.SHA, true
				break
			}
		}
		if !found {
			return nil, errGitHubContentNotFound
		}
	}
}

// 写请求可在网络超时后已提交，因此每次重试前由 putContent 的 SHA 检查保证幂等。
// HTTP 重试仅针对明确未成功的限流/服务错误；网络异常交给持久任务续传。
func (p *GitHubProvider) doRetry(request *http.Request) (*http.Response, error) {
	for attempt := 0; ; attempt++ {
		current := request.Clone(request.Context())
		if attempt > 0 && request.GetBody != nil {
			body, err := request.GetBody()
			if err != nil {
				return nil, err
			}
			current.Body = body
		}
		response, err := p.client.Do(current)
		if err != nil {
			return nil, fmt.Errorf("GitHub 网络请求失败，请重试续传")
		}
		retry := response.StatusCode == 429 || response.StatusCode >= 500 || response.StatusCode == 403 && (response.Header.Get("Retry-After") != "" || response.Header.Get("X-RateLimit-Remaining") == "0")
		if !retry || attempt >= 3 {
			return response, nil
		}
		delay := time.Duration(1<<attempt) * time.Second
		if seconds, err := strconv.Atoi(response.Header.Get("Retry-After")); err == nil && time.Duration(seconds)*time.Second > delay {
			delay = time.Duration(seconds) * time.Second
		}
		if response.Header.Get("X-RateLimit-Remaining") == "0" {
			if reset, err := strconv.ParseInt(response.Header.Get("X-RateLimit-Reset"), 10, 64); err == nil && time.Until(time.Unix(reset, 0)) > delay {
				delay = time.Until(time.Unix(reset, 0))
			}
		}
		// 长时间限流交给用户稍后续传，避免请求无限挂起。
		if delay > 60*time.Second {
			return response, nil
		}
		io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		response.Body.Close()
		timer := time.NewTimer(delay)
		select {
		case <-request.Context().Done():
			timer.Stop()
			return nil, request.Context().Err()
		case <-timer.C:
		}
	}
}
