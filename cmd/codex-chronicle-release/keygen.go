// 中文说明：为维护者生成独立的 Ed25519 更新密钥，私钥仅写入新建的受限文件，命令输出只包含公钥、指纹和路径。
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	updatepkg "codex-chronicle.local/internal/update"
)

type signingKeyInfo struct {
	KeyID          string `json:"key_id"`
	PublicKey      string `json:"public_key"`
	PrivateKeyFile string `json:"private_key_file"`
}

func signingKeyCommand(command string, arguments []string) (signingKeyInfo, error) {
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	path := flags.String("output", "", "密钥文件路径；keygen 创建新文件，key-info 只读现有文件")
	if err := flags.Parse(arguments); err != nil {
		return signingKeyInfo{}, errors.New("用法：codex-chronicle-release keygen|key-info --output /仓库外的路径/update-signing.key")
	}
	if strings.TrimSpace(*path) == "" || flags.NArg() != 0 {
		return signingKeyInfo{}, errors.New("必须通过 --output 指定仓库外的更新密钥路径")
	}
	abs, err := filepath.Abs(*path)
	if err != nil {
		return signingKeyInfo{}, err
	}
	if command == "keygen" {
		if err := os.MkdirAll(filepath.Dir(abs), 0o700); err != nil {
			return signingKeyInfo{}, err
		}
		_, private, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			return signingKeyInfo{}, err
		}
		file, err := os.OpenFile(abs, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return signingKeyInfo{}, fmt.Errorf("不能创建更新密钥；已有文件不会覆盖：%w", err)
		}
		complete := false
		defer func() {
			_ = file.Close()
			if !complete {
				_ = os.Remove(abs)
			}
		}()
		if _, err := file.WriteString(base64.StdEncoding.EncodeToString(private.Seed()) + "\n"); err != nil {
			return signingKeyInfo{}, err
		}
		if err := file.Sync(); err != nil {
			return signingKeyInfo{}, err
		}
		if err := file.Close(); err != nil {
			return signingKeyInfo{}, err
		}
		complete = true
	}
	private, err := updatepkg.LoadSigningPrivateKey(abs)
	if err != nil {
		return signingKeyInfo{}, err
	}
	public := private.Public().(ed25519.PublicKey)
	digest := sha256.Sum256(public)
	return signingKeyInfo{KeyID: hex.EncodeToString(digest[:8]), PublicKey: base64.StdEncoding.EncodeToString(public), PrivateKeyFile: abs}, nil
}
