// 中文说明：在隔离用户目录核验登录启动项的 XML 转义、后台参数和启停，不改动实际登录项。
package desktop

import (
	"encoding/xml"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAutostartSurvivesManagedVersionChange(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	data := t.TempDir()
	oldRoot := filepath.Join(data, "updates", "versions", "0.1.0-preview.19")
	newRoot := filepath.Join(data, "updates", "versions", "0.1.0-preview.20")
	if err := SetAutostart(filepath.Join(oldRoot, "bin", ExecutableName("codex-chronicle-desktop")), data, oldRoot, true); err != nil {
		t.Fatal(err)
	}
	enabled, err := AutostartEnabled(filepath.Join(newRoot, "bin", ExecutableName("codex-chronicle-desktop")), data, newRoot)
	if err != nil || !enabled {
		t.Fatalf("升级后丢失了用户已开启的登录启动设置：%v %v", enabled, err)
	}
	if enabled, err := AutostartEnabled("other", t.TempDir(), newRoot); err != nil || enabled {
		t.Fatalf("把其他用户目录的登录项当成了本次设置：%v %v", enabled, err)
	}
}

func TestLoginAgentRoundTrip(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	executable, data, payload := "/Applications/Chronicle & Test.app/launcher", "/Users/test/data", "/Applications/payload"
	if enabled, err := AutostartEnabled(executable, data, payload); err != nil || enabled {
		t.Fatalf("默认不应登录启动：%v %v", enabled, err)
	}
	for _, enabled := range []bool{true, false, true} {
		if err := SetAutostart(executable, data, payload, enabled); err != nil {
			t.Fatal(err)
		}
		actual, err := AutostartEnabled(executable, data, payload)
		if err != nil || actual != enabled {
			t.Fatalf("登录启动状态错误：%v %v", actual, err)
		}
	}
	path, _ := loginAgentPath()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	decoder := xml.NewDecoder(strings.NewReader(string(body)))
	var texts []string
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("生成的 plist 无效：%v", err)
		}
		if text, ok := token.(xml.CharData); ok {
			texts = append(texts, string(text))
		}
	}
	joined := strings.Join(texts, "\n")
	if !strings.Contains(joined, executable) || !strings.Contains(joined, "--background") || strings.Contains(joined, "--no-browser") {
		t.Fatalf("登录启动参数错误：%s", joined)
	}
}
