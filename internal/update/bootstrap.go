// 中文说明：把已校验的首次安装载荷登记为独立版本基线，识别安装器替换，并保留已有在线更新和人工回滚决定。
package update

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// RegisterInstallation 只在首次安装或安装包版本变化时登记；同一安装包再次打开不会覆盖活动版本。
func (m *Manager) RegisterInstallation(ctx context.Context, bundleRoot, expectedVersion string) error {
	if m == nil {
		return errors.New("更新管理器未初始化")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	lock, err := m.operationLock()
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := m.recoverLocked(ctx); err != nil {
		return err
	}
	bundle, err := safeExistingDirectory(bundleRoot)
	if err != nil {
		return err
	}
	manifest, digest, err := m.readAndValidateManifest(ctx, bundle)
	if err != nil {
		return err
	}
	if expectedVersion != "" && manifest.Version != expectedVersion {
		return fmt.Errorf("安装清单版本 %s 与启动器版本 %s 不一致", manifest.Version, expectedVersion)
	}
	state, err := m.readState()
	if err != nil {
		return err
	}
	if state.InstalledVersion == manifest.Version {
		return nil
	}
	finalRoot := filepath.Join(m.root, VersionDir, safeVersionName(manifest.Version))
	if info, err := os.Lstat(finalRoot); err == nil {
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("安装基线的同名路径不是普通版本目录")
		}
		existing, _, err := m.readAndValidateManifest(ctx, finalRoot)
		// ZIP、目录和 .app 分别签名打包时生成时间可不同；版本身份与实际文件必须完全相同。
		existing.CreatedAt, existing.Signature = manifest.CreatedAt, nil
		comparison := manifest
		comparison.Signature = nil
		if err != nil || !manifestsEqual(existing, comparison) {
			return fmt.Errorf("版本 %s 已存在不同或损坏的载荷，拒绝覆盖", manifest.Version)
		}
	} else if !os.IsNotExist(err) {
		return err
	} else {
		id, err := newID("baseline")
		if err != nil {
			return err
		}
		stage := filepath.Join(m.root, VersionDir, "."+id)
		if err := os.Mkdir(stage, 0o700); err != nil {
			return err
		}
		defer os.RemoveAll(stage)
		if err := copyBundle(ctx, Plan{BundleRoot: bundle, Manifest: manifest}, stage); err != nil {
			return fmt.Errorf("保存安装基线失败：%w", err)
		}
		if _, copiedDigest, err := m.readAndValidateManifest(ctx, stage); err != nil || copiedDigest != digest {
			return fmt.Errorf("安装基线复制后校验失败：%v", err)
		}
		if err := os.Rename(stage, finalRoot); err != nil {
			return err
		}
		if err := syncDirectory(filepath.Dir(finalRoot)); err != nil {
			return err
		}
	}
	// 兼容旧版已有活动指针的用户：补齐安装基线，但不撤销已激活的在线更新。
	if state.ActiveVersion == "" || state.InstalledVersion != "" {
		if state.ActiveVersion != manifest.Version {
			state.PreviousVersion = state.ActiveVersion
		}
		state.ActiveVersion = manifest.Version
		state.LastStatus, state.LastError = "installed", ""
	} else if state.PreviousVersion == "" && state.ActiveVersion != manifest.Version {
		state.PreviousVersion = manifest.Version
	}
	state.InstalledVersion = manifest.Version
	state.UpdatedAt = time.Now().UTC()
	return m.writeState(ctx, state)
}

// recoverBaselines 仅在持有更新事务锁时清理本管理器中断留下的基线副本，不触碰已发布版本。
func (m *Manager) recoverBaselines() error {
	root := filepath.Join(m.root, VersionDir)
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		name := entry.Name()
		if len(name) != len(".baseline-")+24 || name[:len(".baseline-")] != ".baseline-" {
			continue
		}
		if decoded, err := hex.DecodeString(name[len(".baseline-"):]); err != nil || len(decoded) != 12 {
			continue
		}
		if !entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			return errors.New("中断的基线副本不是普通目录，停止自动清理")
		}
		if err := os.RemoveAll(filepath.Join(root, name)); err != nil {
			return err
		}
	}
	return nil
}
