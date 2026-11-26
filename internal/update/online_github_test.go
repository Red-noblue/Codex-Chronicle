// 中文说明：模拟 GitHub 公开发布列表与静态 feed，验证渠道隔离、语义版本排序、分页、标签绑定和网络失败边界。
package update

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"runtime"
	"strings"
	"testing"
)

const githubTestIndex = "https://api.github.com/repos/Red-noblue/Codex-Chronicle/releases?per_page=100"
const githubTestStable = "https://github.com/Red-noblue/Codex-Chronicle/releases/latest/download/codex-chronicle-feed.json"

type githubTestTransport func(*http.Request) (*http.Response, error)

func (transport githubTestTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return transport(request)
}

func githubTestFeedURL(version string) string {
	return "https://github.com/Red-noblue/Codex-Chronicle/releases/download/v" + version + "/codex-chronicle-feed.json"
}

func githubTestEntry(version string) map[string]any {
	return map[string]any{
		"tag_name": "v" + version, "prerelease": strings.Contains(version, "-"), "draft": false,
		"assets": []map[string]string{{"name": "codex-chronicle-feed.json", "state": "uploaded", "browser_download_url": githubTestFeedURL(version)}},
	}
}

func githubTestFeed(version string) OnlineFeed {
	return OnlineFeed{FormatVersion: 1, Product: "codex-chronicle", Releases: []OnlineRelease{
		onlineTestRelease(version, []byte("package"), runtime.GOOS, runtime.GOARCH),
	}}
}

func githubTestConfig(t *testing.T, documents map[string]any, nextPage bool) OnlineConfig {
	t.Helper()
	return OnlineConfig{FeedURL: githubTestIndex, HTTPClient: &http.Client{Transport: githubTestTransport(func(request *http.Request) (*http.Response, error) {
		if request.Header.Get("Authorization") != "" {
			t.Error("公开更新请求携带了账户凭据")
		}
		if request.Header.Get("User-Agent") == "" {
			t.Error("GitHub 更新请求缺少 User-Agent")
		}
		document, ok := documents[request.URL.String()]
		if !ok {
			return nil, fmt.Errorf("出现非预期请求：%s", request.URL)
		}
		body, err := json.Marshal(document)
		if err != nil {
			return nil, err
		}
		header := make(http.Header)
		if nextPage && request.URL.String() == githubTestIndex {
			header.Set("Link", `<https://api.github.com/repos/Red-noblue/Codex-Chronicle/releases?page=2&per_page=100>; rel="next"`)
		}
		return &http.Response{StatusCode: http.StatusOK, Header: header, Body: io.NopCloser(strings.NewReader(string(body))), ContentLength: int64(len(body))}, nil
	})}}
}

func TestGitHubPreviewSelectsHighestVersionAndIgnoresIncompleteReleases(t *testing.T) {
	draft := githubTestEntry("99.0.0")
	draft["draft"] = true
	missing := githubTestEntry("98.0.0")
	missing["assets"] = []any{}
	wrongFlag := githubTestEntry("97.0.0-preview.1")
	wrongFlag["prerelease"] = false
	config := githubTestConfig(t, map[string]any{
		githubTestIndex:                       []any{githubTestEntry("1.2.3-preview.9"), draft, missing, wrongFlag, githubTestEntry("1.2.3-preview.10"), githubTestEntry("1.2.2")},
		githubTestFeedURL("1.2.3-preview.10"): githubTestFeed("1.2.3-preview.10"),
	}, false)
	result, err := CheckOnlineFeed(t.Context(), config, "1.2.3-preview.8")
	if err != nil || result.Release == nil || result.Release.Version != "1.2.3-preview.10" {
		t.Fatalf("预览版本选择错误：%+v %v", result, err)
	}
}

func TestGitHubPreviewCanGraduateToStableAndReadLaterPages(t *testing.T) {
	config := githubTestConfig(t, map[string]any{
		githubTestIndex: []any{githubTestEntry("1.2.3-preview.12")},
		"https://api.github.com/repos/Red-noblue/Codex-Chronicle/releases?page=2&per_page=100": []any{githubTestEntry("1.2.3")},
		githubTestFeedURL("1.2.3"): githubTestFeed("1.2.3"),
	}, true)
	result, err := CheckOnlineFeed(t.Context(), config, "1.2.3-preview.12")
	if err != nil || result.Release == nil || result.Release.Version != "1.2.3" {
		t.Fatalf("预览版未能升级到正式版：%+v %v", result, err)
	}
}

func TestGitHubPreviewDoesNotDowngradeOrFailWhenNoReleaseIsUsable(t *testing.T) {
	for _, entries := range [][]any{{}, {githubTestEntry("1.2.3-preview.1")}, {map[string]any{"tag_name": "unrelated"}}} {
		config := githubTestConfig(t, map[string]any{githubTestIndex: entries}, false)
		result, err := CheckOnlineFeed(t.Context(), config, "1.2.3")
		if err != nil || result.Available || result.Release != nil {
			t.Fatalf("没有可用更新时结果错误：%+v %v", result, err)
		}
	}
}

func TestGitHubFeedMustMatchReleaseTagAndStableChannel(t *testing.T) {
	config := githubTestConfig(t, map[string]any{
		githubTestIndex:            []any{githubTestEntry("1.2.3")},
		githubTestFeedURL("1.2.3"): githubTestFeed("9.0.0"),
		githubTestStable:           githubTestFeed("9.0.0-preview.1"),
	}, false)
	if _, err := CheckOnlineFeed(t.Context(), config, "1.0.0"); err == nil {
		t.Fatal("GitHub 标签与 feed 版本不符时未被拒绝")
	}
	config.FeedURL = githubTestStable
	if _, err := CheckOnlineFeed(t.Context(), config, "1.0.0"); err == nil {
		t.Fatal("稳定渠道接受了预览版本")
	}
}

func TestGitHubAPIFailureDoesNotReportUpToDate(t *testing.T) {
	config := OnlineConfig{FeedURL: githubTestIndex, HTTPClient: &http.Client{Transport: githubTestTransport(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusForbidden, Body: io.NopCloser(strings.NewReader("rate limit"))}, nil
	})}}
	if _, err := CheckOnlineFeed(t.Context(), config, "1.0.0"); err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("API 失败未明确报告：%v", err)
	}
}
