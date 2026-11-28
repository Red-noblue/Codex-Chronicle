#!/usr/bin/env python3
# 中文说明：隔离验证候选字节冻结、来源门禁、篡改拒绝、发布幂等和禁止覆盖；模拟 GitHub 响应，不公开任何版本。
import copy
import importlib.util
import json
import os
from pathlib import Path
import tempfile
import subprocess
import unittest
from unittest.mock import patch
import zipfile


spec = importlib.util.spec_from_file_location("release_candidate", Path(__file__).with_name("release-candidate.py"))
candidate = importlib.util.module_from_spec(spec)
spec.loader.exec_module(candidate)


class CandidateTests(unittest.TestCase):
    def setUp(self):
        self.scratch = tempfile.TemporaryDirectory(prefix="chronicle-candidate-test-")
        self.addCleanup(self.scratch.cleanup)
        self.root = Path(self.scratch.name)
        self.output, self.artifacts = self.root / "publish", self.root / "artifacts"
        self.version, self.repository, self.commit = "1.2.3-preview.16", "Red-noblue/Codex-Chronicle", "a" * 40
        for (system, arch), names in candidate.release_feed.expected_packages(self.version).items():
            directory = self.artifacts / (system + "-" + arch)
            directory.mkdir(parents=True)
            rows = []
            for name in names:
                path = directory / name
                if name.endswith(".zip"):
                    with zipfile.ZipFile(path, "w") as archive:
                        archive.writestr("codex-chronicle-release.json", json.dumps({"version": self.version, "os": system, "arch": arch,
                                          "build_id": self.commit, "signature": {"algorithm": "ed25519"}}))
                else:
                    path.write_bytes(name.encode())
                rows.append(candidate.release_feed.file_digest(path) + "  " + name)
            (directory / "SHA256SUMS").write_text("\n".join(rows) + "\n")
        candidate.release_feed.assemble_release(self.artifacts, self.output, self.version, self.repository)
        self.digest = candidate.seal(self.output, self.version, self.repository, self.commit, 123, 1, "已审核的测试发布说明")
        self.selection = {"repository": self.repository, "version": self.version, "source_commit": self.commit,
                          "source_branch": "main", "run_id": 123, "run_attempt": 1, "artifact_id": 456, "artifact_digest": "sha256:" + "b" * 64}
        confirmation = patch.dict(os.environ, {"CHRONICLE_IMMUTABLE_RELEASES_CONFIRMED": "true"})
        confirmation.start()
        self.addCleanup(confirmation.stop)

    def verify(self, digest=None):
        return candidate.verify(self.output, self.version, self.repository, digest or self.digest, self.selection)

    def test_recommendation_requires_observation_and_exact_immutable_release(self):
        manifest = self.verify()
        release = {"id": 10, "tag_name": "v" + self.version, "draft": False, "immutable": True, "prerelease": True,
                   "assets": [{"name": p.name, "state": "uploaded", "size": p.stat().st_size,
                               "digest": "sha256:" + candidate.release_feed.file_digest(p)} for p in self.output.iterdir()]}
        def api(args, **kwargs):
            endpoint = args[1]
            if "/compare/" in endpoint: return {"status": "ahead"}
            if "/contents/" in endpoint: return None
            if "/git/ref/" in endpoint: return {"object": {"type": "commit", "sha": self.commit}}
            if "/releases/tags/" in endpoint: return release
            self.fail("推荐核验不应写入GitHub：" + str(args))
        with patch.dict(os.environ, {"GITHUB_ACTIONS": "true", "GITHUB_REF": "refs/heads/main", "GITHUB_REPOSITORY": self.repository}):
            with patch.object(candidate, "select_from_github", return_value=self.selection), patch.object(candidate, "verify_attestation"), patch.object(candidate, "gh", side_effect=api):
                for confirmed, note in [(False, "已完成实机观察"), (True, " ")]:
                    with self.assertRaises(ValueError): candidate.recommend(self.output, manifest, self.selection, self.digest, confirmed, note)
                self.assertEqual(candidate.recommend(self.output, manifest, self.selection, self.digest, True, "模式与暂停续传实测通过"), release)
                release["immutable"] = False
                with self.assertRaises(ValueError): candidate.recommend(self.output, manifest, self.selection, self.digest, True, "已观察")
                release["immutable"] = True
                release["assets"][0]["digest"] = "sha256:" + "0" * 64
                with self.assertRaises(ValueError): candidate.recommend(self.output, manifest, self.selection, self.digest, True, "已观察")

    def test_publishing_and_recommendation_are_separate_workflow_operations(self):
        workflow = Path(__file__).resolve().parents[1] / ".github/workflows/package-preview.yml"
        text = workflow.read_text()
        channel = text.split("  update-channel:", 1)[1]
        self.assertIn("inputs.operation == 'recommend'", channel)
        self.assertNotIn("inputs.operation == 'publish'", channel)
        self.assertIn("CHRONICLE_OBSERVATION_CONFIRMED", channel)
        self.assertIn("核验公开资产仍与观察候选完全一致", channel)

    def test_frozen_candidate_covers_all_final_bytes_and_refuses_reseal(self):
        before = {p.name: p.read_bytes() for p in self.output.iterdir()}
        manifest = self.verify()
        self.assertEqual(len(manifest["files"]), 18)
        self.assertEqual(manifest["source_commit"], self.commit)
        self.assertEqual({p.name: p.read_bytes() for p in self.output.iterdir()}, before)
        with self.assertRaises(ValueError):
            candidate.seal(self.output, self.version, self.repository, self.commit, 123, 1, "不能覆盖")

    def test_changed_asset_metadata_checksum_and_extra_file_are_rejected(self):
        originals = {p: p.read_bytes() for p in self.output.iterdir()}
        cases = [next(self.output.glob("*.exe")), self.output / candidate.MANIFEST, self.output / "SHA256SUMS", self.output / "unexpected.txt"]
        for path in cases:
            with self.subTest(path=path.name):
                path.write_bytes(b"changed")
                with self.assertRaises((ValueError, json.JSONDecodeError)):
                    self.verify()
                if path in originals:
                    path.write_bytes(originals[path])
                else:
                    path.unlink()
        with self.assertRaises(ValueError):
            self.verify("0" * 64)

    def test_wrong_run_attempt_repository_and_commit_are_rejected(self):
        for field, value in [("run_id", 999), ("run_attempt", 2), ("repository", "someone/else"), ("source_commit", "c" * 40)]:
            with self.subTest(field=field):
                selection = dict(self.selection, **{field: value})
                with self.assertRaises(ValueError):
                    candidate.verify(self.output, self.version, self.repository, self.digest, selection)

    def source_records(self):
        run = {"repository": {"full_name": self.repository}, "head_repository": {"full_name": self.repository},
               "id": 123, "run_attempt": 1, "workflow_id": 88, "path": candidate.WORKFLOW, "event": "workflow_dispatch",
               "status": "completed", "conclusion": "success", "head_sha": self.commit, "head_branch": "main"}
        names = ["回归 / " + x for x in ("windows-latest", "ubuntu-latest", "macos-latest")]
        names += ["安装包 / " + system + "-" + arch for system in ("darwin", "windows", "linux") for arch in ("amd64", "arm64")]
        names.append(candidate.SEAL_JOB)
        jobs = [{"name": name, "conclusion": "success"} for name in names]
        artifacts = [{"name": f"Codex-Chronicle-candidate-{self.version}-123-1", "id": 456, "expired": False, "digest": "sha256:" + "b" * 64}]
        return run, jobs, artifacts

    def test_selection_requires_successful_gates_and_immutable_artifact_identity(self):
        run, jobs, artifacts = self.source_records()
        self.assertEqual(candidate.select_run(run, jobs, artifacts, self.version, self.repository, 88, True), self.selection)
        for scenario in ("fork", "failed", "workflow", "pull-request", "missing-job", "failed-job", "expired", "duplicate", "no-digest"):
            r, j, a = copy.deepcopy((run, jobs, artifacts))
            if scenario == "fork": r["head_repository"]["full_name"] = "someone/else"
            elif scenario == "failed": r["conclusion"] = "failure"
            elif scenario == "workflow": r["workflow_id"] = 99
            elif scenario == "pull-request": r["event"] = "pull_request"
            elif scenario == "missing-job": j.pop()
            elif scenario == "failed-job": j[0]["conclusion"] = "failure"
            elif scenario == "expired": a[0]["expired"] = True
            elif scenario == "duplicate": a.append(a[0])
            else: a[0].pop("digest")
            with self.subTest(scenario=scenario), self.assertRaises(ValueError):
                candidate.select_run(r, j, a, self.version, self.repository, 88, True)

    def test_branch_candidate_can_be_verified_but_cannot_be_published(self):
        run, jobs, artifacts = self.source_records()
        run["head_branch"] = "release/test"
        candidate.select_run(run, jobs, artifacts, self.version, self.repository, 88)
        with self.assertRaises(ValueError):
            candidate.select_run(run, jobs, artifacts, self.version, self.repository, 88, True)
        with patch.dict(os.environ, {"GITHUB_ACTIONS": "true", "GITHUB_REF": "refs/heads/release/test", "GITHUB_REPOSITORY": self.repository}):
            with patch.object(candidate, "gh") as api, self.assertRaises(ValueError):
                candidate.publish(self.output, self.verify(), self.selection, self.digest)
            api.assert_not_called()

    def test_published_identical_candidate_is_idempotent_and_conflict_never_overwrites(self):
        manifest = self.verify()
        release = {"id": 7, "tag_name": "v" + self.version, "draft": False, "immutable": True, "prerelease": True,
                   "assets": [{"name": p.name, "state": "uploaded", "size": p.stat().st_size,
                               "digest": "sha256:" + candidate.release_feed.file_digest(p)} for p in self.output.iterdir()]}
        def api(args, **kwargs):
            endpoint = args[1]
            if "/compare/" in endpoint: return {"status": "ahead"}
            if "/contents/" in endpoint: return None
            if "/git/ref/" in endpoint: return {"object": {"type": "commit", "sha": self.commit}}
            if "/releases/tags/" in endpoint: return release
            self.fail("幂等验证不应写入 GitHub：" + str(args))
        with patch.dict(os.environ, {"GITHUB_ACTIONS": "true", "GITHUB_REF": "refs/heads/main", "GITHUB_REPOSITORY": self.repository}):
            with patch.object(candidate, "select_from_github", return_value=self.selection), patch.object(candidate, "verify_attestation"), patch.object(candidate, "gh", side_effect=api):
                self.assertEqual(candidate.publish(self.output, manifest, self.selection, self.digest), release)
                release["assets"][0]["digest"] = "sha256:" + "0" * 64
                with self.assertRaises(ValueError):
                    candidate.publish(self.output, manifest, self.selection, self.digest)

    def test_draft_resume_uploads_only_missing_bytes_and_locks_after_complete_verification(self):
        for incomplete_digest in (False, True):
            with self.subTest(incomplete_digest=incomplete_digest):
                paths = list(self.output.iterdir())
                def asset(path):
                    return {"name": path.name, "state": "uploaded", "size": path.stat().st_size,
                            "digest": "sha256:" + candidate.release_feed.file_digest(path)}
                release = {"id": 7, "tag_name": "v" + self.version, "draft": True, "immutable": False,
                           "prerelease": True, "body": "已审核的测试发布说明", "assets": [asset(paths[0])]}
                written = []
                def api(args, **kwargs):
                    endpoint = args[1]
                    if "/compare/" in endpoint: return {"status": "ahead"}
                    if "/contents/" in endpoint: return None
                    if "/git/ref/" in endpoint: return {"object": {"type": "commit", "sha": self.commit}}
                    if "/releases/tags/" in endpoint: return copy.deepcopy(release)
                    if endpoint.endswith("/releases/7"):
                        if kwargs.get("body"):
                            self.assertEqual(len(release["assets"]), len(paths))
                            written.append(kwargs["body"])
                            release.update(draft=False, immutable=True)
                        return copy.deepcopy(release)
                    self.fail("未预期的远端写入：" + str(args))
                def upload(args, **kwargs):
                    self.assertEqual(args[:3], ["gh", "release", "upload"])
                    self.assertNotIn("--clobber", args)
                    missing = args[6:]
                    self.assertEqual(len(missing), len(paths)-1)
                    self.assertNotIn(str(paths[0]), missing)
                    release["assets"] += [asset(Path(path)) for path in missing]
                    if incomplete_digest:
                        release["assets"][-1].pop("digest")
                    return subprocess.CompletedProcess(args, 0, "", "")
                with patch.dict(os.environ, {"GITHUB_ACTIONS": "true", "GITHUB_REF": "refs/heads/main", "GITHUB_REPOSITORY": self.repository}):
                    with patch.object(candidate, "select_from_github", return_value=self.selection), patch.object(candidate, "verify_attestation"), patch.object(candidate, "gh", side_effect=api), patch.object(candidate.subprocess, "run", side_effect=upload):
                        if incomplete_digest:
                            with self.assertRaises(ValueError):
                                candidate.publish(self.output, self.verify(), self.selection, self.digest)
                            self.assertEqual(written, [])
                            self.assertTrue(release["draft"])
                        else:
                            result = candidate.publish(self.output, self.verify(), self.selection, self.digest)
                            self.assertTrue(result["immutable"])
                            self.assertEqual(len(written), 1)


    def test_operator_dispatch_checks_administration_locally_and_reserves_exact_tag(self):
        writes = []
        def api(args, **kwargs):
            endpoint = args[1]
            if endpoint.endswith("/immutable-releases"): return {"enabled": True}
            if "/compare/" in endpoint: return {"status": "ahead"}
            if "/contents/" in endpoint: return None
            if "/git/ref/" in endpoint: return None
            if endpoint.endswith("/git/refs"):
                self.assertEqual(kwargs["body"], {"ref": "refs/tags/v" + self.version, "sha": self.commit})
                writes.append("tag")
                return {}
            if endpoint.endswith("/dispatches"):
                self.assertEqual(writes, ["tag"])
                inputs = kwargs["body"]["inputs"]
                self.assertEqual(inputs["candidate_sha256"], self.digest)
                self.assertEqual(inputs["candidate_run_id"], "123")
                self.assertIs(inputs["immutable_releases_confirmed"], True)
                writes.append("dispatch")
                return None
            self.fail("未预期的发布入口请求：" + str(args))
        with patch.dict(os.environ, {"GITHUB_ACTIONS": "false"}), patch.object(candidate, "gh", side_effect=api), patch.object(candidate, "select_from_github", return_value=self.selection):
            candidate.dispatch_publish(123, self.version, self.repository, self.digest)
        self.assertEqual(writes, ["tag", "dispatch"])

    def test_operator_dispatch_never_overrides_disabled_immutability_or_conflicting_tag(self):
        for scenario in ("disabled", "permission", "tag-conflict", "outside-main"):
            def api(args, **kwargs):
                endpoint = args[1]
                self.assertNotIn("body", kwargs, "安全检查失败前不应写入")
                if endpoint.endswith("/immutable-releases"):
                    if scenario == "permission": raise ValueError("HTTP 403")
                    return {"enabled": scenario != "disabled"}
                if "/compare/" in endpoint: return {"status": "diverged" if scenario == "outside-main" else "ahead"}
                if "/contents/" in endpoint: return None
                if "/git/ref/" in endpoint: return {"object": {"type": "commit", "sha": "f" * 40}}
                self.fail("失败前不应触发其他请求")
            with self.subTest(scenario=scenario), patch.dict(os.environ, {"GITHUB_ACTIONS": "false"}), patch.object(candidate, "gh", side_effect=api), patch.object(candidate, "select_from_github", return_value=self.selection), self.assertRaises(ValueError):
                candidate.dispatch_publish(123, self.version, self.repository, self.digest)

    def test_ci_publish_requires_operator_confirmation_without_administration_access(self):
        with patch.dict(os.environ, {"GITHUB_ACTIONS": "true", "GITHUB_REF": "refs/heads/main", "GITHUB_REPOSITORY": self.repository, "CHRONICLE_IMMUTABLE_RELEASES_CONFIRMED": "false"}), patch.object(candidate, "gh") as api, self.assertRaises(ValueError):
            candidate.publish(self.output, self.verify(), self.selection, self.digest)
        api.assert_not_called()

    def test_ci_never_creates_a_missing_tag_or_requires_workflow_write_for_existing_tag(self):
        release = {"id": 7, "tag_name": "v" + self.version, "draft": True, "immutable": False,
                   "prerelease": True, "body": "已审核的测试发布说明", "assets": []}
        for missing_tag in (True, False):
            writes = []
            def api(args, **kwargs):
                endpoint = args[1]
                if "/compare/" in endpoint: return {"status": "ahead"}
                if "/contents/" in endpoint: return None
                if "/git/ref/" in endpoint: return None if missing_tag else {"object": {"type": "commit", "sha": self.commit}}
                if "/releases/tags/" in endpoint: return None
                if endpoint.endswith("/releases"):
                    self.assertNotIn("target_commitish", kwargs["body"])
                    writes.append("draft")
                    return copy.deepcopy(release)
                if endpoint.endswith("/releases/7"):
                    if kwargs.get("body"):
                        release.update(draft=False, immutable=True)
                        writes.append("publish")
                    return copy.deepcopy(release)
                self.fail("CI 不应读取管理设置或写入工作流标签：" + str(args))
            def upload(args, **kwargs):
                release["assets"] = [{"name": p.name, "state": "uploaded", "size": p.stat().st_size,
                                      "digest": "sha256:" + candidate.release_feed.file_digest(p)} for p in self.output.iterdir()]
                return subprocess.CompletedProcess(args, 0, "", "")
            with self.subTest(missing_tag=missing_tag), patch.dict(os.environ, {"GITHUB_ACTIONS": "true", "GITHUB_REF": "refs/heads/main", "GITHUB_REPOSITORY": self.repository}), patch.object(candidate, "select_from_github", return_value=self.selection), patch.object(candidate, "verify_attestation"), patch.object(candidate, "gh", side_effect=api), patch.object(candidate.subprocess, "run", side_effect=upload):
                if missing_tag:
                    with self.assertRaises(ValueError):candidate.publish(self.output, self.verify(), self.selection, self.digest)
                    self.assertEqual(writes, [])
                else:
                    self.assertTrue(candidate.publish(self.output, self.verify(), self.selection, self.digest)["immutable"])
                    self.assertEqual(writes, ["draft", "publish"])


if __name__ == "__main__":
    unittest.main()
