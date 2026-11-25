// 中文说明：从 GitHub 公开发布列表发现预览更新，按版本号选择已上传 feed 的版本，并严格绑定原仓库的下载地址。
package update

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

const githubFeedName = "codex-chronicle-feed.json"

var githubReleaseTag = regexp.MustCompile(`^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-preview\.(0|[1-9][0-9]*))?$`)

type githubOnlineRelease struct {
	TagName    string `json:"tag_name"`
	Draft      bool   `json:"draft"`
	Prerelease bool   `json:"prerelease"`
	Assets     []struct {
		Name  string `json:"name"`
		State string `json:"state"`
		URL   string `json:"browser_download_url"`
	} `json:"assets"`
}

func githubReleaseRepository(address string) string {
	parsed, err := url.Parse(address)
	if err != nil || parsed.Scheme != "https" || parsed.Host != "api.github.com" {
		return ""
	}
	parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	if len(parts) != 4 || parts[0] != "repos" || parts[3] != "releases" || parts[1] == "" || parts[2] == "" {
		return ""
	}
	return parts[1] + "/" + parts[2]
}

func githubStableFeed(address string) bool {
	parsed, err := url.Parse(address)
	if err != nil || parsed.Scheme != "https" || parsed.Host != "github.com" {
		return false
	}
	parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	return len(parts) == 6 && strings.Join(parts[2:], "/") == "releases/latest/download/"+githubFeedName
}

func resolveGitHubPreviewFeed(ctx context.Context, client *http.Client, config OnlineConfig, repository, current string) (string, string, error) {
	address, _ := url.Parse(config.FeedURL)
	remaining := config.MaxFeedBytes
	selectedURL, selectedVersion := "", ""
	for page := 1; page <= 10; page++ {
		body, header, err := readOnlineJSON(ctx, client, address.String(), remaining)
		if err != nil {
			return "", "", err
		}
		remaining -= int64(len(body))
		var releases []githubOnlineRelease
		if err := json.Unmarshal(body, &releases); err != nil || strings.TrimSpace(string(body)) == "null" {
			return "", "", errors.New("GitHub 公开发布列表不是有效 JSON 数组")
		}
		for _, release := range releases {
			version := strings.TrimPrefix(release.TagName, "v")
			if release.Draft || !githubReleaseTag.MatchString(release.TagName) || release.Prerelease != strings.Contains(version, "-") {
				continue
			}
			if current != "" && !versionLess(current, version) || selectedVersion != "" && !versionLess(selectedVersion, version) {
				continue
			}
			expectedURL := "https://github.com/" + repository + "/releases/download/" + release.TagName + "/" + githubFeedName
			for _, asset := range release.Assets {
				if asset.Name == githubFeedName && asset.State == "uploaded" && asset.URL == expectedURL {
					selectedURL, selectedVersion = expectedURL, version
				}
			}
		}
		if !strings.Contains(header.Get("Link"), `rel="next"`) {
			return selectedURL, selectedVersion, nil
		}
		// 不跟随服务端提供的任意 URL；分页始终留在同一公开仓库，并限制总页数和累计字节数。
		query := address.Query()
		query.Set("page", strconv.Itoa(page+1))
		address.RawQuery = query.Encode()
	}
	return "", "", errors.New("GitHub 公开发布列表超过 10 页，请缩小更新来源范围")
}
