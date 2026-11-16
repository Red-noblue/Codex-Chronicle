// 中文说明：独立验证归档和回收站目的地，探针只移动程序新建的测试文本，目视确认不授予真实对话写权限。
package api

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"codex-chronicle.local/internal/platform"
)

type operationDestinations struct {
	SourceRoot       string                     `json:"source_root"`
	Key              string                     `json:"key"`
	Mode             string                     `json:"mode"`
	TrashPath        string                     `json:"trash_path"`
	TrashVerified    bool                       `json:"trash_verified"`
	Probe            *platform.TrashProbeRecord `json:"probe,omitempty"`
	ArchivePath      string                     `json:"archive_path"`
	ArchiveConfirmed bool                       `json:"archive_confirmed"`
	Reason           string                     `json:"reason,omitempty"`
}

func (s *Server) operationDestinations(ctx context.Context) (operationDestinations, error) {
	root := s.currentWorkspace().Definition.Root
	status := operationDestinations{SourceRoot: root, Mode: "native"}
	settings, err := s.loadTrashSettings(ctx)
	if err != nil {
		return status, err
	}
	if root == "" {
		return status, fmt.Errorf("尚未配置工作区")
	}
	volume, sourceIdentity, err := platform.LocationIdentity(root)
	if err != nil {
		return status, fmt.Errorf("不能确定工作区所在磁盘: %w", err)
	}
	if settings.CustomPath != "" {
		status.Mode, status.TrashPath = "custom", settings.CustomPath
	} else {
		status.TrashPath, err = platform.NativeTrashDirectory(root)
		if err != nil {
			status.TrashPath = platform.DetectSystemTrash().CandidatePath
			status.Reason = "原生位置尚未确认，请发送测试文件: " + err.Error()
		}
	}
	if s.lifecycle != nil && s.lifecycle.Enabled && !s.lifecycle.UseSystemTrash && s.lifecycle.TrashProvider == nil && s.lifecycle.TrashRoot != "" {
		status.Mode, status.TrashPath = "isolation", s.lifecycle.TrashRoot
	}
	_, destinationIdentity, identityErr := platform.LocationIdentity(status.TrashPath)
	if identityErr != nil {
		destinationIdentity = "unverified"
	}
	digest := sha256.Sum256([]byte(root + "\x00" + volume + "\x00" + sourceIdentity + "\x00" + status.Mode + "\x00" + status.TrashPath + "\x00" + destinationIdentity))
	status.Key = hex.EncodeToString(digest[:])
	if verification := settings.Verification; verification != nil {
		status.TrashVerified = identityErr == nil && verification.SourceRoot == root && verification.DestinationKey == status.Key
	}
	if settings.Probe != nil && settings.Probe.SourceRoot == root && settings.Probe.DestinationKey == status.Key {
		status.Probe = settings.Probe
	}
	if s.lifecycle != nil {
		status.ArchivePath = s.lifecycle.ArchiveRoot
	}
	if confirmation := settings.ArchiveConfirmation; confirmation != nil {
		status.ArchiveConfirmed = status.ArchivePath != "" && confirmation.Path == status.ArchivePath && confirmation.SourceRoot == root
	}
	return status, nil
}

func (s *Server) operationDestinationsHandler(writer http.ResponseWriter, request *http.Request) {
	writer.Header().Set("Cache-Control", "no-store")
	if request.Method != http.MethodGet {
		writeError(writer, http.StatusMethodNotAllowed, "只支持 GET")
		return
	}
	status, err := s.operationDestinations(request.Context())
	if err != nil {
		writeError(writer, http.StatusConflict, err.Error())
		return
	}
	writeJSON(writer, http.StatusOK, status)
}

func (s *Server) trashProbeHandler(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		writeError(writer, http.StatusMethodNotAllowed, "只支持 POST")
		return
	}
	s.trashProbeMu.Lock()
	defer s.trashProbeMu.Unlock()
	status, err := s.operationDestinations(request.Context())
	if err != nil {
		writeError(writer, http.StatusConflict, err.Error())
		return
	}
	var input struct {
		DestinationKey string `json:"destination_key"`
	}
	request.Body = http.MaxBytesReader(writer, request.Body, 4096)
	if err := json.NewDecoder(request.Body).Decode(&input); err != nil || input.DestinationKey != status.Key {
		writeError(writer, http.StatusConflict, "工作区或回收站已变化，请重新检查目的地")
		return
	}
	var provider platform.TrashProvider
	if status.Mode == "isolation" {
		if err = validateLifecycleDestinations(status.SourceRoot, *s.lifecycle, operationPreviewRequest{Trash: true}); err == nil {
			provider, err = platform.NewDirectoryTrash(status.TrashPath)
		}
	} else {
		provider, err = s.systemTrashProvider(request.Context(), status.SourceRoot)
	}
	if err != nil {
		writeError(writer, http.StatusConflict, err.Error())
		return
	}
	// 在工作区外、同一磁盘上创建样本，保证原生 API 测试的是目标磁盘的回收站。
	probeRoot, err := os.MkdirTemp(filepath.Dir(status.SourceRoot), ".codex-chronicle-probe-")
	if err != nil {
		writeError(writer, http.StatusConflict, "无法在工作区外创建同盘测试样本: "+err.Error())
		return
	}
	defer os.Remove(probeRoot)
	sourceVolume, _, volumeErr := platform.LocationIdentity(status.SourceRoot)
	probeVolume, _, probeErr := platform.LocationIdentity(probeRoot)
	if volumeErr != nil || probeErr != nil || sourceVolume != probeVolume {
		writeError(writer, http.StatusConflict, "测试样本与工作区不在同一磁盘，不能验证该回收站")
		return
	}
	var nonce [10]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		writeError(writer, http.StatusInternalServerError, "生成测试标识失败")
		return
	}
	id := hex.EncodeToString(nonce[:])
	name := "Codex-Chronicle-回收站测试-" + time.Now().Format("20060102-150405") + "-" + id + ".txt"
	content := []byte("Codex Chronicle 回收站测试\n测试标识：" + id + "\n这是程序新建的模拟样本，不包含任何真实对话。\n确认后可由你手动清理。\n")
	digest := sha256.Sum256(content)
	source := filepath.Join(probeRoot, name)
	file, err := os.OpenFile(source, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		writeError(writer, http.StatusInternalServerError, err.Error())
		return
	}
	defer os.Remove(source)
	_, writeErr := file.Write(content)
	if writeErr == nil {
		writeErr = file.Sync()
	}
	closeErr := file.Close()
	if writeErr != nil || closeErr != nil {
		writeError(writer, http.StatusInternalServerError, "测试样本写入失败")
		return
	}
	settings, err := s.loadTrashSettings(request.Context())
	if err != nil {
		writeError(writer, http.StatusInternalServerError, err.Error())
		return
	}
	probe := &platform.TrashProbeRecord{ID: id, SourceRoot: status.SourceRoot, DestinationKey: status.Key, FileName: name, SHA256: hex.EncodeToString(digest[:]), State: "prepared", CreatedAt: time.Now().UTC()}
	settings.Probe, settings.Verification = probe, nil
	if err := s.trashSettings.Save(request.Context(), settings); err != nil {
		writeError(writer, http.StatusInternalServerError, err.Error())
		return
	}
	receipt, moveErr := provider.Move(request.Context(), "probe-"+id, probeRoot, name)
	probe.State = "failed"
	if moveErr == nil {
		probe.TrashPath = receipt.TrashPath
		if status.Mode != "native" {
			probe.FileName = filepath.Base(receipt.TrashPath)
		}
		current, locationErr := s.operationDestinations(context.Background())
		if locationErr == nil {
			probe.DestinationKey = current.Key
		}
		if err := verifyTrashProbe(probe); err == nil {
			probe.State = "moved"
		} else {
			moveErr = err
		}
	}
	if err := s.trashSettings.Save(context.Background(), settings); err != nil {
		writeError(writer, http.StatusInternalServerError, "探针可能已移动，但结果保存失败；请按测试文件名检查: "+name)
		return
	}
	if moveErr != nil {
		writeJSON(writer, http.StatusConflict, map[string]any{"error": moveErr.Error(), "probe": probe})
		return
	}
	current, err := s.operationDestinations(request.Context())
	if err != nil {
		writeError(writer, http.StatusConflict, err.Error())
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"probe": probe, "destinations": current, "source_unchanged": true})
}

func verifyTrashProbe(probe *platform.TrashProbeRecord) error {
	if probe == nil || probe.TrashPath == "" {
		return fmt.Errorf("测试文件没有可核验的回收站回执")
	}
	info, err := os.Lstat(probe.TrashPath)
	if err != nil || !info.Mode().IsRegular() || info.Size() > 4096 {
		return fmt.Errorf("测试文件不存在或格式已变化，请重新发送测试文件")
	}
	file, err := os.Open(probe.TrashPath)
	if err != nil {
		return fmt.Errorf("不能读取测试文件: %w", err)
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, io.LimitReader(file, 4097)); err != nil {
		return err
	}
	if hex.EncodeToString(hash.Sum(nil)) != probe.SHA256 {
		return fmt.Errorf("测试文件内容已变化，请重新测试")
	}
	return nil
}

func (s *Server) confirmOperationDestinationsHandler(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		writeError(writer, http.StatusMethodNotAllowed, "只支持 POST")
		return
	}
	s.trashProbeMu.Lock()
	defer s.trashProbeMu.Unlock()
	var input struct {
		DestinationKey string `json:"destination_key"`
		ProbeID        string `json:"probe_id"`
		Found          bool   `json:"found"`
		ArchivePath    string `json:"archive_path"`
	}
	request.Body = http.MaxBytesReader(writer, request.Body, 64<<10)
	if err := json.NewDecoder(request.Body).Decode(&input); err != nil {
		writeError(writer, http.StatusBadRequest, "确认请求不是有效 JSON")
		return
	}
	status, err := s.operationDestinations(request.Context())
	if err != nil || input.DestinationKey != status.Key {
		writeError(writer, http.StatusConflict, "目的地已变化，请重新检测和测试")
		return
	}
	settings, err := s.loadTrashSettings(request.Context())
	if err != nil {
		writeError(writer, http.StatusInternalServerError, err.Error())
		return
	}
	if input.ProbeID != "" {
		if status.Probe == nil || status.Probe.ID != input.ProbeID || status.Probe.State != "moved" {
			writeError(writer, http.StatusConflict, "请先发送测试文件，再核对本次测试标识")
			return
		}
		if !input.Found {
			settings.Verification = nil
		} else {
			if err := verifyTrashProbe(status.Probe); err != nil {
				writeError(writer, http.StatusConflict, err.Error())
				return
			}
			settings.Verification = &platform.TrashVerification{SourceRoot: status.SourceRoot, DestinationKey: status.Key, ProbeID: input.ProbeID, ConfirmedAt: time.Now().UTC()}
		}
	}
	if input.ArchivePath != "" {
		if status.ArchivePath == "" || input.ArchivePath != status.ArchivePath {
			writeError(writer, http.StatusConflict, "归档目录已变化或尚未配置")
			return
		}
		if err := validateLifecycleDestinations(status.SourceRoot, *s.lifecycle, operationPreviewRequest{Archive: true}); err != nil {
			writeError(writer, http.StatusConflict, err.Error())
			return
		}
		settings.ArchiveConfirmation = &platform.ArchiveDestinationConfirmation{SourceRoot: status.SourceRoot, Path: status.ArchivePath, ConfirmedAt: time.Now().UTC()}
	}
	if input.ProbeID == "" && input.ArchivePath == "" {
		writeError(writer, http.StatusBadRequest, "没有需要确认的目的地")
		return
	}
	if err := s.trashSettings.Save(request.Context(), settings); err != nil {
		writeError(writer, http.StatusInternalServerError, err.Error())
		return
	}
	updated, err := s.operationDestinations(request.Context())
	if err != nil {
		writeError(writer, http.StatusConflict, err.Error())
		return
	}
	writeJSON(writer, http.StatusOK, updated)
}

func (s *Server) requireOperationDestinations(ctx context.Context, input operationPreviewRequest) error {
	if s.lifecycle == nil || !s.lifecycle.RequireDestinationConfirmation {
		return nil
	}
	status, err := s.operationDestinations(ctx)
	if err != nil {
		return err
	}
	if input.Trash && !status.TrashVerified {
		return fmt.Errorf("请先完成同盘回收站测试，并确认已在回收站中找到样本")
	}
	if input.Archive && input.ArchiveLocal && !status.ArchiveConfirmed {
		return fmt.Errorf("请先确认归档目录")
	}
	return nil
}
