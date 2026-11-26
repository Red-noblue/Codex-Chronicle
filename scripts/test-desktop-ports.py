#!/usr/bin/env python3
# 中文说明：用隔离安装包验证端口占用自动避让、严格指定、实例复用和跨端口设置恢复，不读取真实 Codex 数据。
import argparse
import http.server
import json
import os
from pathlib import Path
import socket
import subprocess
import tempfile
import threading
import urllib.request


def main():
    parser = argparse.ArgumentParser(description="验证安装版端口与界面设置持久化")
    parser.add_argument("--package-root", type=Path, required=True)
    args = parser.parse_args()
    package = args.package_root.resolve()
    suffix = ".exe" if os.name == "nt" else ""
    launcher = package / "bin" / ("codex-chronicle-desktop" + suffix)

    with tempfile.TemporaryDirectory(prefix="chronicle-ports-") as scratch:
        root = Path(scratch)
        source = root / "empty-source"
        source.mkdir()
        data = root / "profile"
        (data / "runtime").mkdir(parents=True)

        class Sentinel(http.server.BaseHTTPRequestHandler):
            def do_GET(self):
                self.send_response(200)
                self.end_headers()
                self.wfile.write(b"unrelated-service")

            def log_message(self, *args):
                pass

        sentinel = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Sentinel)
        thread = threading.Thread(target=sentinel.serve_forever, daemon=True)
        thread.start()
        occupied = "127.0.0.1:" + str(sentinel.server_port)
        (data / "runtime" / "last-address.json").write_text(
            json.dumps({"address": occupied}), encoding="utf-8"
        )

        def command(*extra, profile=data, check=True):
            result = subprocess.run(
                [str(launcher), "--package-root", str(package), "--data-dir", str(profile),
                 "--no-browser", *extra], text=True, capture_output=True, timeout=40
            )
            if check and result.returncode:
                raise AssertionError(result.stderr)
            return result

        def start(*extra):
            return json.loads(command("--scan-root", str(source), *extra).stdout)

        def get_json(url):
            with urllib.request.urlopen(url, timeout=10) as response:
                return json.load(response)

        try:
            first = start()
            assert first["url"] != "http://" + occupied, first
            assert first["url"].startswith("http://127.0.0.1:"), first
            same = start()
            assert (same["id"], same["pid"], same["url"]) == (first["id"], first["pid"], first["url"])
            status = json.loads(command("--status").stdout)
            assert status["url"] == first["url"]
            print("通过：占用端口自动避让，重复启动及 --status 指向同一实例", flush=True)

            strict = command("--address", occupied, profile=root / "strict-profile", check=False)
            assert strict.returncode != 0, strict
            assert occupied in strict.stderr, strict.stderr
            strict_log = (root / "strict-profile" / "logs" / "daemon.log").read_text(encoding="utf-8")
            assert "显式指定的端口不会自动切换" in strict_log, strict_log
            mismatch = command("--address", occupied, check=False)
            assert mismatch.returncode != 0 and "已有后台运行于" in mismatch.stderr, mismatch.stderr
            with urllib.request.urlopen("http://" + occupied, timeout=10) as response:
                assert response.read() == b"unrelated-service"
            print("通过：显式端口冲突明确失败，占用程序未被停止或误认", flush=True)

            preferences = {
                "codex-chronicle.theme.v1": "dark",
                "codex-chronicle.density.v1": "reading",
                "codex-chronicle.saved-filters.v1": '{"workspace":[]}',
            }
            request = urllib.request.Request(
                first["url"] + "/api/v1/ui-preferences", method="PATCH",
                data=json.dumps(preferences).encode(), headers={"Content-Type": "application/json"}
            )
            with urllib.request.urlopen(request, timeout=10) as response:
                assert json.load(response)["preferences"] == preferences
            command("--quit")
            restarted = start()
            assert restarted["url"] == first["url"] and restarted["id"] != first["id"]
            print("通过：安全退出后复用上次成功地址", flush=True)

            command("--quit")
            port = int(first["url"].rsplit(":", 1)[1])
            with socket.socket() as blocker:
                if os.name != "nt":
                    blocker.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
                blocker.bind(("127.0.0.1", port))
                blocker.listen()
                moved = start()
                assert moved["url"] != first["url"], moved
                assert get_json(moved["url"] + "/api/v1/ui-preferences")["preferences"] == preferences
                assert get_json(moved["url"] + "/healthz")["status"] == "ok"
                print("通过：旧地址再次被占用时更换端口，设置完整恢复", flush=True)
                command("--quit")

            with socket.socket() as free:
                free.bind(("127.0.0.1", 0))
                explicit = "127.0.0.1:" + str(free.getsockname()[1])
            fixed = start("--address", explicit)
            assert fixed["url"] == "http://" + explicit
            assert get_json(fixed["url"] + "/api/v1/ui-preferences")["preferences"] == preferences
            print("通过：显式空闲端口精确生效，沿用原数据与设置", flush=True)

            # 模拟原生端口设置写入；下次启动必须尊重关闭/开启回退的选择。
            saved = {"port": sentinel.server_port, "auto_fallback": False}
            (data / "startup.json").write_text(json.dumps(saved), encoding="utf-8")
            unchanged = json.loads(command("--status").stdout)
            assert unchanged["id"] == fixed["id"], unchanged
            command("--quit")
            refused = command("--scan-root", str(source), check=False)
            assert refused.returncode != 0 and occupied in refused.stderr, refused
            saved["auto_fallback"] = True
            (data / "startup.json").write_text(json.dumps(saved), encoding="utf-8")
            recovered = start()
            assert recovered["url"] != "http://" + occupied, recovered
            assert get_json(recovered["url"] + "/healthz")["status"] == "ok"
            print("通过：保存端口不打断当前后台，关闭回退严格报错，重新开启后自动恢复", flush=True)

            command("--quit")
            with socket.socket() as free:
                free.bind(("127.0.0.1", 0))
                override = "127.0.0.1:" + str(free.getsockname()[1])
            overridden = start("--address", override)
            assert overridden["url"] == "http://" + override, overridden
            print("通过：命令行显式地址优先于保存的端口与回退配置", flush=True)
        finally:
            command("--quit", check=False)
            command("--quit", profile=root / "strict-profile", check=False)
            sentinel.shutdown()
            sentinel.server_close()
            thread.join(timeout=5)


if __name__ == "__main__":
    main()
