#!/usr/bin/env python3
# 中文说明：用临时六平台产物验证发布渠道、完整资产集、feed 平台唯一性及损坏包拒绝，不访问 GitHub。
import hashlib
import importlib.util
import json
from pathlib import Path
import tempfile
import unittest
import zipfile


spec = importlib.util.spec_from_file_location("release_feed", Path(__file__).with_name("release-feed.py"))
release_feed = importlib.util.module_from_spec(spec)
spec.loader.exec_module(release_feed)


class ReleaseFeedTests(unittest.TestCase):
    def setUp(self):
        self.scratch = tempfile.TemporaryDirectory(prefix="chronicle-feed-")
        self.addCleanup(self.scratch.cleanup)
        self.root = Path(self.scratch.name)
        self.artifacts = self.root / "artifacts"
        self.output = self.root / "publish"
        self.version = "1.2.3-preview.12"
        self.repository = "Red-noblue/Codex-Chronicle"

    def fixtures(self):
        for (operating_system, arch), names in release_feed.expected_packages(self.version).items():
            directory = self.artifacts / (operating_system + "-" + arch)
            directory.mkdir(parents=True)
            for name in names:
                path = directory / name
                if name.endswith(".zip"):
                    with zipfile.ZipFile(path, "w") as archive:
                        archive.writestr("codex-chronicle-release.json", json.dumps({
                            "version": self.version, "os": operating_system, "arch": arch,
                        }))
                else:
                    path.write_bytes(name.encode())
            self.write_sums(directory)

    def write_sums(self, directory):
        rows = []
        for package in sorted(directory.iterdir()):
            if package.name != "SHA256SUMS":
                rows.append(hashlib.sha256(package.read_bytes()).hexdigest() + "  " + package.name)
        (directory / "SHA256SUMS").write_text("\n".join(rows) + "\n", encoding="utf-8")

    def assemble(self):
        return release_feed.assemble_release(self.artifacts, self.output, self.version, self.repository)

    def test_stable_and_preview_use_separate_fixed_sources(self):
        stable = release_feed.release_config("1.2.3", self.repository)
        preview = release_feed.release_config(self.version, self.repository)
        self.assertEqual(stable["channel"], "stable")
        self.assertEqual(preview["channel"], "preview")
        self.assertEqual(stable["feed_url"], "https://github.com/Red-noblue/Codex-Chronicle/releases/latest/download/codex-chronicle-feed.json")
        self.assertEqual(preview["feed_url"], "https://api.github.com/repos/Red-noblue/Codex-Chronicle/releases?per_page=100")

    def test_invalid_versions_and_repository_are_rejected(self):
        for version in ["v1.2.3", "1.2", "01.2.3", "1.2.3-preview.01", "1.2.3;echo bad", "1.2.3-rc.1"]:
            with self.subTest(version=version), self.assertRaises(ValueError):
                release_feed.release_config(version, self.repository)
        with self.assertRaises(ValueError):
            release_feed.release_config("1.2.3", "owner/repo/escape")

    def test_complete_assets_make_one_zip_per_platform(self):
        self.fixtures()
        feed = self.assemble()
        self.assertEqual(len(feed["releases"]), 6)
        self.assertEqual(len({(item["os"], item["arch"]) for item in feed["releases"]}), 6)
        self.assertEqual(feed["channel"], "preview")
        for item in feed["releases"]:
            self.assertTrue(item["url"].endswith(".zip"))
            self.assertIn("/releases/download/v" + self.version + "/", item["url"])
            package = self.output / item["url"].rsplit("/", 1)[1]
            self.assertEqual(item["size_bytes"], package.stat().st_size)
            self.assertEqual(item["sha256"], hashlib.sha256(package.read_bytes()).hexdigest())
        self.assertEqual(len(list(self.output.iterdir())), 18)
        sums = (self.output / "SHA256SUMS").read_text()
        self.assertEqual(len(sums.splitlines()), 17)
        self.assertIn("  codex-chronicle-feed.json\n", sums)

    def test_stable_assets_are_supported(self):
        self.version = "1.2.3"
        self.fixtures()
        self.assertEqual(self.assemble()["channel"], "stable")

    def test_upload_must_be_complete_before_publishing(self):
        self.fixtures()
        self.assemble()
        assets = [{"name": path.name, "state": "uploaded", "size": path.stat().st_size,
                   "digest": "sha256:" + hashlib.sha256(path.read_bytes()).hexdigest()}
                  for path in self.output.iterdir()]
        release = {"tag_name": "v" + self.version, "draft": True, "assets": assets}
        release_feed.verify_uploaded_assets(self.output, release, self.version)
        for field, value in [("draft", False), ("tag_name", "v9.9.9")]:
            with self.subTest(field=field), self.assertRaises(ValueError):
                release_feed.verify_uploaded_assets(self.output, dict(release, **{field: value}), self.version)
        for scenario in ["missing", "state", "size", "digest"]:
            with self.subTest(scenario=scenario):
                damaged = [dict(asset) for asset in assets]
                if scenario == "missing":
                    damaged.pop()
                else:
                    damaged[0][scenario] = {"state": "new", "size": 0, "digest": "sha256:wrong"}[scenario]
                with self.assertRaises(ValueError):
                    release_feed.verify_uploaded_assets(self.output, dict(release, assets=damaged), self.version)

    def test_corrupt_or_missing_asset_never_creates_output(self):
        for scenario in ["digest", "missing", "duplicate", "unsafe-name", "wrong-platform", "wrong-version"]:
            with self.subTest(scenario=scenario), tempfile.TemporaryDirectory(dir=self.root) as scratch:
                self.artifacts = Path(scratch) / "artifacts"
                self.output = Path(scratch) / "publish"
                self.fixtures()
                directory = self.artifacts / "linux-amd64"
                package = next(directory.glob("*.zip"))
                sums = directory / "SHA256SUMS"
                if scenario == "digest":
                    package.write_bytes(b"changed")
                elif scenario == "missing":
                    sums.write_text("".join(line for line in sums.read_text().splitlines(True) if not line.endswith(package.name + "\n")))
                elif scenario == "duplicate":
                    sums.write_text(sums.read_text() + sums.read_text().splitlines(True)[0])
                elif scenario == "unsafe-name":
                    sums.write_text("0" * 64 + "  ../escape.zip\n")
                else:
                    with zipfile.ZipFile(package, "w") as archive:
                        archive.writestr("codex-chronicle-release.json", json.dumps({
                            "version": "9.9.9" if scenario == "wrong-version" else self.version,
                            "os": "windows" if scenario == "wrong-platform" else "linux", "arch": "amd64",
                        }))
                    self.write_sums(directory)
                with self.assertRaises(ValueError):
                    self.assemble()
                self.assertFalse(self.output.exists())


if __name__ == "__main__":
    unittest.main()
