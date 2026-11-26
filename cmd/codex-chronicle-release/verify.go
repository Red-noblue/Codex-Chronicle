// 中文说明：用仓库固定公钥验证最终 ZIP 的实际内容、版本与构建提交；验证过程只使用独立临时目录。
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"

	updatepkg "codex-chronicle.local/internal/update"
)

func verifyArchiveCommand(ctx context.Context, arguments []string) (updatepkg.Manifest, error) {
	flags := flag.NewFlagSet("verify-archive", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	archive := flags.String("archive", "", "最终发布 ZIP 路径")
	public := flags.String("trusted-public-key-file", "", "仓库固定验签公钥")
	version := flags.String("version", "", "预期发布版本")
	build := flags.String("build-id", "", "预期源码提交")
	startup := flags.Bool("startup-check", false, "验签后使用包内启动器进行隔离启动检查")
	if err := flags.Parse(arguments); err != nil || flags.NArg() != 0 || *archive == "" || *public == "" || *version == "" || *build == "" {
		return updatepkg.Manifest{}, errors.New("verify-archive 必须明确提供 archive、trusted-public-key-file、version 和 build-id")
	}
	keys, err := updatepkg.LoadTrustedPublicKeys(*public)
	if err != nil {
		return updatepkg.Manifest{}, err
	}
	work, err := os.MkdirTemp("", "chronicle-final-archive-")
	if err != nil {
		return updatepkg.Manifest{}, err
	}
	defer os.RemoveAll(work)
	payload := filepath.Join(work, "payload")
	if err := updatepkg.ExtractArchive(ctx, *archive, payload, updatepkg.DefaultExtractionLimits); err != nil {
		return updatepkg.Manifest{}, err
	}
	manager, err := updatepkg.NewManagerWithOptions(filepath.Join(work, "verification"), updatepkg.ManagerOptions{RequireSignature: true, TrustedPublicKeys: keys})
	if err != nil {
		return updatepkg.Manifest{}, err
	}
	if err := manager.VerifyVersionRoot(ctx, payload); err != nil {
		return updatepkg.Manifest{}, fmt.Errorf("最终安装包验签失败：%w", err)
	}
	data, err := os.ReadFile(filepath.Join(payload, updatepkg.ManifestName))
	if err != nil {
		return updatepkg.Manifest{}, err
	}
	var manifest updatepkg.Manifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return updatepkg.Manifest{}, err
	}
	if manifest.Version != *version || manifest.BuildID != *build {
		return updatepkg.Manifest{}, errors.New("最终安装包版本或源码提交与预期不符")
	}
	if *startup {
		name := "codex-chronicle-desktop"
		if runtime.GOOS == "windows" {
			name += ".exe"
		}
		checkCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
		defer cancel()
		output, err := exec.CommandContext(checkCtx, filepath.Join(payload, "bin", name), "--verify").CombinedOutput()
		if err != nil {
			if len(output) > 4096 {
				output = output[len(output)-4096:]
			}
			return updatepkg.Manifest{}, fmt.Errorf("最终 ZIP 的隔离启动检查失败：%w：%s", err, output)
		}
	}
	return manifest, nil
}
