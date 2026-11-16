# 中文说明：保留既有实验和恢复命令入口；实现统一位于正式兼容模块，测试 patch 直接作用于同一个模块。
import importlib.util
import json
import pathlib
import sys

_path = pathlib.Path(__file__).resolve().parents[2] / 'internal/compat/authorizedlive/executor.py'
_spec = importlib.util.spec_from_file_location('chronicle_authorized_live', _path)
_module = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(_module)
if __name__ == '__main__':
    try:
        _module.main()
    except Exception as error:
        print(json.dumps({'error': str(error)}, ensure_ascii=False), file=sys.stderr)
        sys.exit(1)
else:
    sys.modules[__name__] = _module
