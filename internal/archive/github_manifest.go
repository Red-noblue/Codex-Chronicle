// 中文说明：为归档发现只读取和核对源清单，不下载正文分片，也不把清单匹配当作完整归档校验。
package archive

import (
	"context"
	"encoding/json"
	"fmt"
)

// RemoteArchiveManifestReader 是可选能力，不要求现有第三方 provider 改动上传、下载接口。
type RemoteArchiveManifestReader interface {
	ReadArchiveManifest(ctx context.Context, archiveID, expectedSHA256 string) (Manifest, error)
}

func (p *GitHubCredentialArchiveProvider) ReadArchiveManifest(ctx context.Context, archiveID, expectedSHA256 string) (Manifest, error) {
	provider, err := p.provider(ctx)
	if err != nil {
		return Manifest{}, err
	}
	return provider.ReadArchiveManifest(ctx, archiveID, expectedSHA256)
}

func (p *GitHubProvider) ReadArchiveManifest(ctx context.Context, archiveID, expectedSHA256 string) (Manifest, error) {
	var source Manifest
	if err := validateArchiveID(archiveID); err != nil {
		return source, err
	}
	data, _, err := p.getContent(ctx, p.remotePath(archiveID, "remote-manifest.json"))
	if err != nil {
		return source, err
	}
	var remote GitHubRemoteManifest
	if err := json.Unmarshal(data, &remote); err != nil {
		return source, fmt.Errorf("远端归档清单无效: %w", err)
	}
	if err := p.validateRemoteManifest(remote, archiveID); err != nil {
		return source, err
	}
	if remote.SourceManifestSHA256 != expectedSHA256 {
		return source, fmt.Errorf("远端归档在扫描期间变化，请重新刷新")
	}
	for _, file := range remote.Files {
		if file.RelativePath != manifestName {
			continue
		}
		if file.SizeBytes > 16<<20 {
			return source, fmt.Errorf("归档源清单超过 16 MiB，未读取正文")
		}
		var content []byte
		for _, chunk := range file.Chunks {
			part, _, err := p.getContent(ctx, chunk.Path)
			if err != nil {
				return source, err
			}
			if int64(len(part)) != chunk.SizeBytes || sha256Hex(part) != chunk.SHA256 {
				return source, fmt.Errorf("归档源清单分片校验失败")
			}
			content = append(content, part...)
		}
		if int64(len(content)) != file.SizeBytes || sha256Hex(content) != file.SHA256 || sha256Hex(content) != expectedSHA256 {
			return source, fmt.Errorf("归档源清单摘要不匹配")
		}
		if err := json.Unmarshal(content, &source); err != nil {
			return source, fmt.Errorf("解析归档源清单失败: %w", err)
		}
		if source.Version != 1 || source.OperationID == "" || len(source.Assets) == 0 {
			return source, fmt.Errorf("归档源清单缺少有效版本、操作 ID 或资产")
		}
		return source, nil
	}
	return source, fmt.Errorf("远端归档缺少源清单")
}
