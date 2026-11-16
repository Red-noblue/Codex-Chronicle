// 中文说明：把已构建的 daemon 和 Web 静态目录打成带 manifest 的离线更新包；不读取或修改 Codex 数据。
package main

import (
	"archive/zip"
	"context"
	"encoding/xml"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	updatepkg "codex-chronicle.local/internal/update"
)

type releaseOptions struct {
	Version               string
	BuildID               string
	Daemon                string
	WebDist               string
	Output                string
	TargetOS              string
	TargetArch            string
	SigningPrivateKeyFile string
	SigningKeyID          string
	Launcher              string
	TrashBridge           string
	Format                string
	MacInstaller          string
	LicenseFile           string
	ThirdPartyNoticesFile string
}

func main() {
	options := releaseOptions{}
	flag.StringVar(&options.Version, "version", "", "发布版本，例如 1.0.0")
	flag.StringVar(&options.BuildID, "build-id", "", "构建标识（可选）")
	flag.StringVar(&options.Daemon, "daemon", "", "已构建 daemon 可执行文件路径")
	flag.StringVar(&options.WebDist, "web-dist", "", "已构建 Web dist 目录路径")
	flag.StringVar(&options.Output, "output", "", "输出 ZIP 路径；已存在时拒绝覆盖")
	flag.StringVar(&options.TargetOS, "os", runtime.GOOS, "manifest 目标操作系统")
	flag.StringVar(&options.TargetArch, "arch", runtime.GOARCH, "manifest 目标架构")
	flag.StringVar(&options.SigningPrivateKeyFile, "signing-private-key-file", "", "Ed25519 发布签名私钥文件；支持 32 字节 seed、64 字节私钥、Base64 或十六进制")
	flag.StringVar(&options.SigningKeyID, "signing-key-id", "", "发布签名 key ID；仅在提供私钥时使用")
	flag.StringVar(&options.Launcher, "launcher", "", "已构建桌面启动器；安装包必须提供")
	flag.StringVar(&options.TrashBridge, "trash-bridge", "", "已构建原生回收站桥接程序")
	flag.StringVar(&options.Format, "format", "zip", "输出格式：zip、directory 或 macos-app")
	flag.StringVar(&options.MacInstaller, "macos-installer", "", "macOS 完整应用安装更新脚本，可选")
	flag.StringVar(&options.LicenseFile, "license-file", "", "随发布载荷分发的项目许可证")
	flag.StringVar(&options.ThirdPartyNoticesFile, "third-party-notices-file", "", "随发布载荷分发的第三方许可说明")
	flag.Parse()
	if err := buildReleaseArchive(context.Background(), options); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("已生成发布产物：%s\n", filepath.Clean(options.Output))
}

func buildReleaseArchive(ctx context.Context, options releaseOptions) error {
	if err := validateReleaseOptions(options); err != nil {
		return err
	}
	options.Version = strings.TrimSpace(options.Version)
	options.BuildID = strings.TrimSpace(options.BuildID)
	options.Daemon = strings.TrimSpace(options.Daemon)
	options.WebDist = strings.TrimSpace(options.WebDist)
	options.Output = strings.TrimSpace(options.Output)
	options.TargetOS = strings.TrimSpace(options.TargetOS)
	options.TargetArch = strings.TrimSpace(options.TargetArch)
	options.SigningPrivateKeyFile = strings.TrimSpace(options.SigningPrivateKeyFile)
	options.SigningKeyID = strings.TrimSpace(options.SigningKeyID)
	output, err := filepath.Abs(filepath.Clean(options.Output))
	if err != nil {
		return fmt.Errorf("解析输出路径失败：%w", err)
	}
	if _, err := os.Lstat(output); err == nil {
		return fmt.Errorf("输出文件已存在，拒绝覆盖：%s", output)
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("检查输出文件失败：%w", err)
	}
	if err := os.MkdirAll(filepath.Dir(output), 0o755); err != nil {
		return fmt.Errorf("创建输出目录失败：%w", err)
	}
	stage, err := os.MkdirTemp(filepath.Dir(output), ".codex-chronicle-release-")
	if err != nil {
		return fmt.Errorf("创建发布 staging 目录失败：%w", err)
	}
	defer os.RemoveAll(stage)
	if err := os.Chmod(stage, 0o755); err != nil {
		return err
	}

	binaryName := "codex-chronicle"
	if options.TargetOS == "windows" {
		binaryName += ".exe"
	}
	if err := copyRegularFile(ctx, options.Daemon, filepath.Join(stage, "bin", binaryName), 0o755); err != nil {
		return fmt.Errorf("复制 daemon 失败：%w", err)
	}
	suffix := ""
	if options.TargetOS == "windows" {
		suffix = ".exe"
	}
	for name, source := range map[string]string{"codex-chronicle-desktop": options.Launcher, "chronicle-native-trash": options.TrashBridge} {
		if strings.TrimSpace(source) == "" {
			continue
		}
		if err := copyRegularFile(ctx, source, filepath.Join(stage, "bin", name+suffix), 0o755); err != nil {
			return fmt.Errorf("复制 %s 失败：%w", name, err)
		}
	}
	if err := copyTree(ctx, options.WebDist, filepath.Join(stage, "web")); err != nil {
		return fmt.Errorf("复制 Web 静态文件失败：%w", err)
	}
	for name, source := range map[string]string{"LICENSE": options.LicenseFile, "THIRD_PARTY_NOTICES.txt": options.ThirdPartyNoticesFile} {
		if source != "" {
			if err := copyRegularFile(ctx, source, filepath.Join(stage, "licenses", name), 0o644); err != nil {
				return fmt.Errorf("复制许可说明失败：%w", err)
			}
		}
	}
	manifest, err := updatepkg.BuildManifestForPlatform(ctx, stage, options.Version, options.BuildID, options.TargetOS, options.TargetArch)
	if err != nil {
		return fmt.Errorf("生成发布 manifest 失败：%w", err)
	}
	if options.SigningPrivateKeyFile != "" {
		privateKey, keyErr := updatepkg.LoadSigningPrivateKey(options.SigningPrivateKeyFile)
		if keyErr != nil {
			return fmt.Errorf("读取发布签名私钥失败：%w", keyErr)
		}
		manifest, err = updatepkg.SignManifest(manifest, privateKey, options.SigningKeyID)
		if err != nil {
			return fmt.Errorf("签名发布 manifest 失败：%w", err)
		}
	}
	if err := updatepkg.WriteManifest(ctx, stage, manifest); err != nil {
		return fmt.Errorf("写入发布 manifest 失败：%w", err)
	}
	// 公开安装载荷不包含用户数据；清单须能被系统安装目录下的普通用户读取。
	if err := os.Chmod(filepath.Join(stage, updatepkg.ManifestName), 0o644); err != nil {
		return err
	}
	if options.Format == "directory" {
		return os.Rename(stage, output)
	}
	if options.Format == "macos-app" {
		return writeMacApp(ctx, stage, output, options.Version, options.MacInstaller)
	}
	if err := writeZip(ctx, stage, output); err != nil {
		return fmt.Errorf("写入离线更新包失败：%w", err)
	}
	return nil
}

func validateReleaseOptions(options releaseOptions) error {
	if strings.TrimSpace(options.Version) == "" {
		return errors.New("--version 不能为空")
	}
	if options.Format != "" && options.Format != "zip" && options.Format != "directory" && options.Format != "macos-app" {
		return errors.New("--format 只支持 zip、directory、macos-app")
	}
	if options.Format == "macos-app" && (options.TargetOS != "darwin" || strings.TrimSpace(options.Launcher) == "" || !strings.HasSuffix(options.Output, ".app")) {
		return errors.New("macos-app 必须指定 darwin 平台、--launcher 和 .app 输出目录")
	}
	for label, value := range map[string]string{"--os": options.TargetOS, "--arch": options.TargetArch} {
		value = strings.TrimSpace(value)
		if value == "" || strings.ContainsAny(value, "/\\\x00") {
			return fmt.Errorf("%s 不是安全的平台标识", label)
		}
	}
	for label, value := range map[string]string{"--daemon": options.Daemon, "--web-dist": options.WebDist, "--output": options.Output} {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("%s 不能为空", label)
		}
	}
	if strings.TrimSpace(options.SigningPrivateKeyFile) == "" && strings.TrimSpace(options.SigningKeyID) != "" {
		return errors.New("--signing-key-id 必须与 --signing-private-key-file 同时使用")
	}
	return nil
}

// writeMacApp 从共同发布载荷生成完整 .app，不创建 Applications 符号链接或写入用户目录。
func writeMacApp(ctx context.Context, payload, output, version, installer string) error {
	app, err := os.MkdirTemp(filepath.Dir(output), ".chronicle-app-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(app)
	if err := os.Chmod(app, 0o755); err != nil {
		return err
	}
	if installer != "" {
		if err := copyRegularFile(ctx, installer, filepath.Join(app, "Contents", "Resources", "install-macos.sh"), 0o755); err != nil {
			return err
		}
	}
	if err := copyTree(ctx, payload, filepath.Join(app, "Contents", "Resources", "app")); err != nil {
		return err
	}
	if err := copyRegularFile(ctx, filepath.Join(payload, "bin", "codex-chronicle-desktop"), filepath.Join(app, "Contents", "MacOS", "Codex Chronicle"), 0o755); err != nil {
		return err
	}
	var escapedVersion strings.Builder
	if err := xml.EscapeText(&escapedVersion, []byte(version)); err != nil {
		return err
	}
	shortVersion := strings.SplitN(strings.TrimPrefix(version, "v"), "-", 2)[0]
	shortVersion = strings.SplitN(shortVersion, "+", 2)[0]
	var escapedShort strings.Builder
	if err := xml.EscapeText(&escapedShort, []byte(shortVersion)); err != nil {
		return err
	}
	plist := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!-- 中文说明：Codex Chronicle macOS 安装应用元数据；业务数据存放在安装包之外的当前用户应用数据目录。 -->
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
<key>CFBundleName</key><string>Codex Chronicle</string>
<key>CFBundleDisplayName</key><string>Codex Chronicle</string>
<key>CFBundleIdentifier</key><string>io.github.red-noblue.codex-chronicle</string>
<key>CFBundleExecutable</key><string>Codex Chronicle</string>
<key>CFBundlePackageType</key><string>APPL</string>
<key>CFBundleShortVersionString</key><string>%s</string>
<key>CFBundleVersion</key><string>%s</string>
<key>ChronicleReleaseVersion</key><string>%s</string>
<key>LSMinimumSystemVersion</key><string>14.0</string>
<key>LSUIElement</key><true/>
<key>NSHighResolutionCapable</key><true/>
</dict></plist>
`, escapedShort.String(), escapedShort.String(), escapedVersion.String())
	if err := os.WriteFile(filepath.Join(app, "Contents", "Info.plist"), []byte(plist), 0o644); err != nil {
		return err
	}
	return os.Rename(app, output)
}

func copyTree(ctx context.Context, sourceRoot, destinationRoot string) error {
	sourceRoot, err := safeDirectory(sourceRoot)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(destinationRoot, 0o755); err != nil {
		return err
	}
	return filepath.WalkDir(sourceRoot, func(path string, entry os.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			return walkErr
		}
		if path == sourceRoot {
			return nil
		}
		relative, err := filepath.Rel(sourceRoot, path)
		if err != nil {
			return err
		}
		destination := filepath.Join(destinationRoot, relative)
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("发布目录包含符号链接：%s", filepath.ToSlash(relative))
		}
		if info.IsDir() {
			return os.MkdirAll(destination, info.Mode().Perm())
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("发布目录包含特殊文件：%s", filepath.ToSlash(relative))
		}
		return copyRegularFile(ctx, path, destination, info.Mode().Perm())
	})
}

func copyRegularFile(ctx context.Context, source, destination string, mode os.FileMode) error {
	sourceInfo, err := os.Lstat(source)
	if err != nil {
		return err
	}
	if sourceInfo.Mode()&os.ModeSymlink != 0 || !sourceInfo.Mode().IsRegular() {
		return fmt.Errorf("源文件必须是普通文件：%s", source)
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
		return err
	}
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	output, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		_ = output.Close()
		if !committed {
			_ = os.Remove(destination)
		}
	}()
	if _, err := io.Copy(output, &contextReader{ctx: ctx, reader: input}); err != nil {
		return err
	}
	if err := output.Sync(); err != nil {
		return err
	}
	if err := output.Close(); err != nil {
		return err
	}
	committed = true
	return nil
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r *contextReader) Read(buffer []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(buffer)
}

func writeZip(ctx context.Context, sourceRoot, output string) error {
	paths := make([]string, 0)
	err := filepath.WalkDir(sourceRoot, func(path string, entry os.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			return walkErr
		}
		if path == sourceRoot || entry.IsDir() {
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 || !entry.Type().IsRegular() {
			return fmt.Errorf("ZIP 源包含不支持的文件：%s", path)
		}
		relative, err := filepath.Rel(sourceRoot, path)
		if err != nil {
			return err
		}
		paths = append(paths, filepath.ToSlash(relative))
		return nil
	})
	if err != nil {
		return err
	}
	sort.Strings(paths)
	temporary, err := os.CreateTemp(filepath.Dir(output), ".codex-chronicle-package-")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	committed := false
	defer func() {
		_ = temporary.Close()
		if !committed {
			_ = os.Remove(temporaryPath)
		}
	}()
	archive := zip.NewWriter(temporary)
	for _, relative := range paths {
		if err := ctx.Err(); err != nil {
			_ = archive.Close()
			return err
		}
		path := filepath.Join(sourceRoot, filepath.FromSlash(relative))
		info, err := os.Stat(path)
		if err != nil {
			_ = archive.Close()
			return err
		}
		header, err := zip.FileInfoHeader(info)
		if err != nil {
			_ = archive.Close()
			return err
		}
		header.Name = relative
		header.Method = zip.Deflate
		writer, err := archive.CreateHeader(header)
		if err != nil {
			_ = archive.Close()
			return err
		}
		input, err := os.Open(path)
		if err != nil {
			_ = archive.Close()
			return err
		}
		_, copyErr := io.Copy(writer, &contextReader{ctx: ctx, reader: input})
		closeErr := input.Close()
		if copyErr != nil {
			_ = archive.Close()
			return copyErr
		}
		if closeErr != nil {
			_ = archive.Close()
			return closeErr
		}
	}
	if err := archive.Close(); err != nil {
		return err
	}
	if err := temporary.Sync(); err != nil {
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, output); err != nil {
		return err
	}
	committed = true
	return nil
}

func safeDirectory(path string) (string, error) {
	abs, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		return "", err
	}
	info, err := os.Lstat(abs)
	if err != nil {
		return "", err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return "", fmt.Errorf("路径必须是普通目录：%s", abs)
	}
	return abs, nil
}
