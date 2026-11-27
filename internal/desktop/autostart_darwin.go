// 中文说明：通过当前用户的 LaunchAgent 管理登录自启动；不申请管理员权限，登录后只启动后台和菜单栏。
package desktop

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"os"
	"path/filepath"
)

func loginAgentPath() (string, error) {
	home, err := os.UserHomeDir()
	return filepath.Join(home, "Library", "LaunchAgents", "local.codex-chronicle.desktop.plist"), err
}

func loginAgent(executable, dataDir, packageRoot string, enabled bool) []byte {
	var body bytes.Buffer
	body.WriteString(`<?xml version="1.0" encoding="UTF-8"?><!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd"><plist version="1.0"><dict><key>Label</key><string>local.codex-chronicle.desktop</string><key>ProgramArguments</key><array>`)
	for _, argument := range loginArguments(executable, dataDir, packageRoot) {
		body.WriteString("<string>")
		xml.EscapeText(&body, []byte(argument))
		body.WriteString("</string>")
	}
	body.WriteString("</array><key>RunAtLoad</key>")
	if enabled {
		body.WriteString("<true/>")
	} else {
		body.WriteString("<false/>")
	}
	body.WriteString("<key>ProcessType</key><string>Interactive</string></dict></plist>")
	return body.Bytes()
}

func AutostartEnabled(executable, dataDir, packageRoot string) (bool, error) {
	path, err := loginAgentPath()
	if err != nil {
		return false, err
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return matchesLoginEntry(string(data), dataDir, func(entry, root string) (string, error) {
		return string(loginAgent(entry, dataDir, root, true)), nil
	}, executable, packageRoot)
}

func SetAutostart(executable, dataDir, packageRoot string, enabled bool) error {
	path, err := loginAgentPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if info, err := os.Lstat(path); err == nil && !info.Mode().IsRegular() {
		return fmt.Errorf("登录启动项必须是普通文件")
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".chronicle-login-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	_, writeErr := file.Write(loginAgent(executable, dataDir, packageRoot, enabled))
	closeErr := file.Close()
	if writeErr != nil {
		return writeErr
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(file.Name(), path)
}
