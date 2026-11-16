#!/usr/bin/env python3
# 中文说明：校验 DEB/RPM 解包后的完整载荷、普通用户权限和隔离启动，不读取真实 .codex。
import hashlib
import json
import pathlib
import stat
import subprocess
import sys

root = pathlib.Path(sys.argv[1]).resolve()
version, arch = sys.argv[2:4]
manifest_path = root / "codex-chronicle-release.json"
manifest = json.loads(manifest_path.read_text())
assert (manifest["version"], manifest["os"], manifest["arch"]) == (version, "linux", arch)
expected = {"codex-chronicle-release.json"}
for entry in manifest["files"]:
    relative = pathlib.PurePosixPath(entry["path"])
    assert not relative.is_absolute() and ".." not in relative.parts, relative
    path = root.joinpath(*relative.parts)
    info = path.lstat()
    assert stat.S_ISREG(info.st_mode) and info.st_size == entry["size_bytes"], path
    with path.open("rb") as source:
        assert hashlib.file_digest(source, "sha256").hexdigest() == entry["sha256"], path
    expected.add(relative.as_posix())
actual = {path.relative_to(root).as_posix() for path in root.rglob("*") if not path.is_dir()}
assert actual == expected, (actual - expected, expected - actual)
assert {"licenses/LICENSE", "licenses/THIRD_PARTY_NOTICES.txt"} <= expected
for path in [root, *root.rglob("*")]:
    mode = path.lstat().st_mode
    assert not stat.S_ISLNK(mode), path
    required = stat.S_IROTH | (stat.S_IXOTH if path.is_dir() or path.parent == root / "bin" else 0)
    assert mode & required == required, (path, oct(mode))
subprocess.run([str(root / "bin/codex-chronicle-desktop"), "--verify"], check=True, timeout=90)
print(f"Linux 安装载荷校验通过：{version} / {arch} / {len(expected)} 个文件")
