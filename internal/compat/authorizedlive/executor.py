# 中文说明：已审阅 schema 的闲置会话执行器；全会话模式须显式开启，逐项保护非目标数据，提供原生废纸篓及确认后的永久删除。
import argparse
import base64
import contextlib
import fcntl
import hashlib
import json
import os
import pathlib
import pickle
import re
import shutil
import socket
import sqlite3
import stat
import struct
import subprocess
import sys
import time
import uuid

ALLOWED_IDS = frozenset((
    '00000000-0000-4000-8000-000000000101',
    '00000000-0000-4000-8000-000000000102',
    '00000000-0000-4000-8000-000000000103',
    '00000000-0000-4000-8000-000000000104',
))
# 来自已逐表、逐触发器审阅的 0.166/0.167 六库；忽略 sqlite_sequence 等内部自动对象。
SCHEMAS = {
    'goals_1.sqlite': 'b5bbeffcdce787cf1c58e2f7f36224636c1e537071d63def3e6126db1aed7b78',
    'logs_2.sqlite': '02d028700f9563ad0e26baa15fc4b7f7eba7cb953da0ac8da6ab322ed105b47e',
    'memories_1.sqlite': '2e7cf952ee319741ae5276b3dee7d135ec37ce8f2b4575447b203167eab8ec92',
    'queue_1.sqlite': 'bcfb7277891e0058458b8d3d65029c7a029ff9846238d3d7398efafd69ee65d8',
    'state_5.sqlite': 'c21296b03b414e9f80764408d01844e463a195f53b5f897553909d1af315eef6',
    'thread_history_1.sqlite': 'dd8535f7ddfcec6371902a2459569aa1323875dd2090125e8d5cd05b715159fe',
}
OWNERS = {
    'thread_goals': 'thread_id', 'thread_goal_continuation_deferrals': 'thread_id',
    'logs': 'thread_id', 'stage1_outputs': 'thread_id', 'jobs': 'job_key',
    'queued_items': 'thread_id', 'queued_thread_revisions': 'thread_id',
    'threads': 'id', 'guardian_review_feedback': 'thread_id',
    'thread_attachments': 'thread_id', 'thread_dynamic_tools': 'thread_id',
    'thread_read_receipts': 'thread_id', 'thread_items': 'thread_id',
    'thread_turns': 'thread_id', 'thread_realtime_items': 'thread_id',
    'thread_history_projection_state': 'thread_id',
    # spawn 边随子线程归属；单删子代理只移除入边，不修改保留父线程的业务记录。
    'thread_spawn_edges': 'child_thread_id',
}
TEXT_FILES = {'session_index.jsonl': 'id', 'history.jsonl': 'session_id'}
KIND = 'chronicle-authorized-idle-experiment-v1'
TRASH_KIND = 'chronicle-conversation-trash-v1'
AUXILIARY_SCHEMAS = {'sqlite/codex-dev.db': 'b9651f52a0db3fd59875e64553ebbdeb625bd83db8fb3066a8c5f9b63ee01e29'}


def require(condition, message):
    if not condition:
        raise RuntimeError(message)


def encoded(value):
    return json.dumps(value, ensure_ascii=True, sort_keys=True, separators=(',', ':')).encode()


def sha(data):
    return hashlib.sha256(data).hexdigest()


def sync_dir(path):
    fd = os.open(path, os.O_RDONLY)
    try:
        os.fsync(fd)
    finally:
        os.close(fd)


def atomic(path, data):
    # 唯一名称避免上次突然关机残留的临时文件阻断恢复；只清理本次创建的临时文件。
    temporary = path.with_name('.chronicle-authorized-' + path.name + '-' + uuid.uuid4().hex + '.tmp')
    fd = os.open(temporary, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    try:
        with os.fdopen(fd, 'wb') as f:
            f.write(data)
            f.flush()
            os.fsync(f.fileno())
        os.replace(temporary, path)
        sync_dir(path.parent)
    finally:
        if temporary.exists():
            temporary.unlink()


def safe(root, relative):
    root = pathlib.Path(root).absolute()
    rel = pathlib.Path(relative)
    require(not rel.is_absolute() and '..' not in rel.parts, '资产路径越界')
    p = root / rel
    for ancestor in (p, *p.parents):
        if ancestor.exists() or ancestor.is_symlink():
            require(not ancestor.is_symlink(), '路径包含符号链接: ' + str(ancestor))
    return p


def regular(path):
    require(stat.S_ISREG(path.lstat().st_mode), '不是普通文件: ' + str(path))
    return path


def fingerprint(path):
    regular(path)
    before = path.stat()
    h = hashlib.sha256()
    with path.open('rb') as f:
        for block in iter(lambda: f.read(1024 * 1024), b''):
            h.update(block)
    after = path.stat()
    require((before.st_dev, before.st_ino, before.st_size, before.st_mtime_ns) ==
            (after.st_dev, after.st_ino, after.st_size, after.st_mtime_ns), '读取期间文件变化: ' + str(path))
    return {'bytes': after.st_size, 'sha256': h.hexdigest(), 'device': after.st_dev,
            'inode': after.st_ino, 'mtime_ns': after.st_mtime_ns}


def durable_copy(source, destination):
    destination.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
    with source.open('rb') as src, destination.open('xb') as dst:
        os.chmod(destination, 0o600)
        shutil.copyfileobj(src, dst)
        dst.flush()
        os.fsync(dst.fileno())
    sync_dir(destination.parent)


def validate_scope(root, ids, all_conversations=False):
    require(ids and len(ids) <= 100 and len(ids) == len(set(ids)) and
            (all(re.fullmatch(r'[0-9a-f]{8}(?:-[0-9a-f]{4}){3}-[0-9a-f]{12}', tid) for tid in ids)
             if all_conversations else set(ids) <= ALLOWED_IDS), '目标 ID 未授权、无效或重复')
    root = pathlib.Path(root).absolute()
    safe(root, '.')
    real = root == pathlib.Path.home() / '.codex'
    if real:
        require(sys.platform == 'darwin', '本轮真实执行只验证 macOS')
    else:
        require('.codex' not in root.parts, '禁止把其他 .codex 伪装成实验副本')
        marker = json.loads(regular(root / 'authorized-live-copy.json').read_bytes())
        require(marker.get('kind') == KIND and marker.get('source_root') == str(pathlib.Path.home() / '.codex')
                and set(ids) <= set(marker.get('ids', [])), '副本身份不匹配')
    return root, real


class Daemon:
    """只连接已经存在的 Unix WebSocket，仅读取 initialize 和 loaded/list。"""
    def __init__(self, path, root):
        self.sock = socket.socket(socket.AF_UNIX)
        self.sock.settimeout(8)
        require(stat.S_ISSOCK(os.stat(path).st_mode) and os.stat(path).st_uid == os.getuid(), 'daemon socket 身份错误')
        self.sock.connect(path)
        key = base64.b64encode(os.urandom(16)).decode()
        self.sock.sendall(('GET / HTTP/1.1\r\nHost: localhost\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n'
                           'Sec-WebSocket-Key: ' + key + '\r\nSec-WebSocket-Version: 13\r\n\r\n').encode())
        header = b''
        while not header.endswith(b'\r\n\r\n') and len(header) < 16384:
            header += self.read(1)
        accept = base64.b64encode(hashlib.sha1((key + '258EAFA5-E914-47DA-95CA-C5AB0DC85B11').encode()).digest())
        require(header.startswith(b'HTTP/1.1 101 ') and accept in header, 'WebSocket 握手失败')
        self.sequence = 0
        info = self.rpc('initialize', {'clientInfo': {'name': 'chronicle_authorized_audit', 'version': '1'},
                                       'capabilities': {'experimentalApi': True}})
        require(info.get('codexHome') == str(root), '连接的 daemon 不是目标 HOME')
        agent = info.get('userAgent', '')
        version = agent.split(' ', 1)[0].rsplit('/', 1)[-1]
        # 0.169.0 改为 opportunistic stale-lock 清扫，但仍使用同一协调锁和目标锁；当前六库指纹保持兼容。
        require(version in ('0.166.0', '0.167.0', '0.167.2', '0.169.0'), 'daemon 版本未经本轮锁协议审阅: ' + agent)
        self.send({'method': 'initialized', 'params': {}})

    def read(self, n):
        result = b''
        while len(result) < n:
            chunk = self.sock.recv(n - len(result))
            require(chunk, 'daemon 提前断开')
            result += chunk
        return result

    def send(self, value, opcode=1):
        data = encoded(value) if opcode == 1 else value
        n = len(data)
        header = bytes([128 | opcode, 128 | (n if n < 126 else 126 if n < 65536 else 127)])
        if n >= 126:
            header += struct.pack('!H' if n < 65536 else '!Q', n)
        mask = os.urandom(4)
        self.sock.sendall(header + mask + bytes(v ^ mask[i % 4] for i, v in enumerate(data)))

    def receive(self):
        parts = []
        while True:
            first, second = self.read(2)
            size = second & 127
            if size >= 126:
                size = struct.unpack('!H' if size == 126 else '!Q', self.read(2 if size == 126 else 8))[0]
            require(size <= 16777216 and not second & 128, '不支持的 daemon 帧')
            data = self.read(size)
            opcode = first & 15
            if opcode == 9:
                self.send(data, 10)
                continue
            require(opcode in (0, 1), 'daemon 连接关闭或帧类型异常')
            parts.append(data)
            require(sum(map(len, parts)) <= 16777216, 'daemon 消息超限')
            if first & 128:
                return json.loads(b''.join(parts))

    def rpc(self, method, params):
        require(method in ('initialize', 'thread/loaded/list'), '不允许写入 RPC')
        self.sequence += 1
        self.send({'id': self.sequence, 'method': method, 'params': params})
        for _ in range(1000):
            value = self.receive()
            if value.get('id') == self.sequence:
                require('result' in value, 'daemon RPC 失败: ' + str(value.get('error')))
                return value['result']
        raise RuntimeError('daemon 通知过多')

    def loaded(self):
        result, cursor, seen = set(), None, set()
        for _ in range(100):
            page = self.rpc('thread/loaded/list', {'cursor': cursor, 'limit': 100})
            require(isinstance(page.get('data'), list), '未知 loaded/list 响应')
            for value in page['data']:
                tid = value if isinstance(value, str) else value.get('id')
                require(isinstance(tid, str), '无法识别已加载会话')
                result.add(tid)
            cursor = page.get('nextCursor')
            if cursor is None:
                return result
            require(cursor not in seen, 'loaded/list 游标重复')
            seen.add(cursor)
        raise RuntimeError('loaded/list 分页超限')

    def close(self):
        self.sock.close()


def connect(path, writable=False):
    regular(path)
    conn = sqlite3.connect(path.as_uri() + ('?mode=rw' if writable else '?mode=ro'), uri=True, timeout=0)
    conn.execute('PRAGMA foreign_keys=ON')
    conn.execute('PRAGMA busy_timeout=0')
    if not writable:
        conn.execute('PRAGMA query_only=ON')
    return conn


def tables(conn):
    return [r[0] for r in conn.execute("SELECT name FROM sqlite_schema WHERE type='table' AND name NOT LIKE 'sqlite_%' ORDER BY name")]


def quote(name):
    return '"' + name.replace('"', '""') + '"'


def schema_hash(conn):
    rows = conn.execute("SELECT type,name,tbl_name,sql FROM sqlite_schema WHERE name NOT LIKE 'sqlite_%' ORDER BY type,name").fetchall()
    # 和预登记摘要保持相同的数组顺序，不使用本项目 Go 指纹的命名或算法。
    return sha(json.dumps(rows, ensure_ascii=True, separators=(',', ':')).encode())


def cell(value):
    return {'blob': base64.b64encode(value).decode()} if isinstance(value, bytes) else value


def uncell(value):
    return base64.b64decode(value['blob'], validate=True) if isinstance(value, dict) else value


def rows_snapshot(conn, ids):
    result = []
    marks = ','.join('?' for _ in ids)
    for table in tables(conn):
        owner = OWNERS.get(table)
        if not owner:
            continue
        columns = conn.execute('PRAGMA table_info(' + quote(table) + ')').fetchall()
        names = [r[1] for r in columns]
        pk = [r[1] for r in sorted(columns, key=lambda r: r[5]) if r[5]]
        require(pk and owner in names, '归属表缺少稳定主键或归属列: ' + table)
        query = 'SELECT * FROM ' + quote(table) + ' WHERE ' + quote(owner) + ' IN (' + marks + ') ORDER BY ' + ','.join(map(quote, pk))
        values = conn.execute(query, ids).fetchall()
        if values:
            result.append({'table': table, 'columns': names, 'pk': pk, 'owner': owner,
                           'values': [[cell(v) for v in row] for row in values]})
    return result


def select_rows(groups, ids):
    result = []
    for group in groups:
        index = group['columns'].index(group['owner'])
        values = [row for row in group['values'] if uncell(row[index]) in ids]
        if values:
            result.append({**group, 'values': values})
    return result


def digest_others(conn, ids):
    h = hashlib.sha256()
    marks = ','.join('?' for _ in ids)
    count = 0
    for table in tables(conn):
        h.update(encoded(table))
        cols = conn.execute('PRAGMA table_info(' + quote(table) + ')').fetchall()
        pk = [r[1] for r in sorted(cols, key=lambda r: r[5]) if r[5]]
        order = ','.join(map(quote, pk)) if pk else 'rowid'
        query = 'SELECT * FROM ' + quote(table)
        args = []
        if table in OWNERS:
            col = quote(OWNERS[table])
            query += ' WHERE ' + col + ' NOT IN (' + marks + ') OR ' + col + ' IS NULL'
            args = ids
        for row in conn.execute(query + ' ORDER BY ' + order, args):
            data = pickle.dumps(row, protocol=5)  # 只序列化用于摘要，绝不反序列化 pickle。
            h.update(struct.pack('!Q', len(data)))
            h.update(data)
            count += 1
    return {'sha256': h.hexdigest(), 'rows': count}


def spawn_origin(meta):
    source = meta.get('source', {})
    if isinstance(source, str):
        try:
            source = json.loads(source)
        except (ValueError, TypeError):
            source = {}
    source = source if isinstance(source, dict) else {}
    sub = source.get('subagent')
    spawn = sub.get('thread_spawn', {}) if isinstance(sub, dict) else {}
    parents = {p for p in (meta.get('parent_thread_id'), spawn.get('parent_thread_id')) if isinstance(p, str) and p}
    kind = 'automation' if isinstance(sub, str) else 'subagent' if parents or isinstance(sub, dict) or meta.get('thread_source') == 'subagent' else ''
    return {'parents': parents, 'kind': kind,
            'agent_path': meta.get('agent_path') or spawn.get('agent_path'),
            'agent_nickname': meta.get('agent_nickname') or spawn.get('agent_nickname')}


def spawn_index(root, conn):
    """仅读首行和明确来源/边；session_id、forked_from_id 都不推断为 spawn。"""
    parents, kinds, paths = {}, {}, {}
    thread_ids = {row[0] for row in conn.execute('SELECT id FROM threads')}

    def add(tid, meta):
        origin = spawn_origin(meta)
        parents.setdefault(tid, set()).update(origin['parents'])
        if origin['kind']:
            kinds.setdefault(tid, set()).add(origin['kind'])

    for tid, source in conn.execute('SELECT id,source FROM threads'):
        add(tid, {'source': source})
    for parent, child in conn.execute('SELECT parent_thread_id,child_thread_id FROM thread_spawn_edges'):
        require(isinstance(parent, str) and isinstance(child, str) and parent and child, 'spawn 边身份不完整')
        parents.setdefault(child, set()).add(parent)
        kinds.setdefault(child, set()).add('subagent')
    for directory in ('sessions', 'archived_sessions'):
        for path in sorted(safe(root, directory).rglob('*.jsonl')):
            with regular(safe(root, path.relative_to(root))).open('rb') as handle:
                line = handle.readline()
            if not line:
                continue
            record = json.loads(line)
            if record.get('type') != 'session_meta':
                continue
            meta = record.get('payload', {})
            tid = meta.get('id') or value.get('id')
            if isinstance(tid, str) and tid:
                add(tid, meta)
                paths.setdefault(tid, []).append(path.relative_to(root).as_posix())
    return parents, kinds, paths, thread_ids


def family_scope(root, conn, requested):
    parents, kinds, paths, thread_ids = spawn_index(root, conn)
    children = {}
    for child, values in parents.items():
        for parent in values:
            children.setdefault(parent, set()).add(child)
    ordered, visiting, visited = [], set(), set()

    def visit(tid):
        require(tid not in visiting, 'spawn 关系存在环，停止')
        if tid in visited:
            return
        require(len(visiting) + len(visited) < 100, '包含子代理后超过 100 个实际线程，请缩小选择范围')
        require(tid in thread_ids, 'spawned 后代缺少 threads 记录，资产关联不完整: ' + tid)
        require(len(parents.get(tid, ())) <= 1, 'spawn 父关系冲突: ' + tid)
        require(not ('automation' in kinds.get(tid, ()) and parents.get(tid)), '自动任务与 spawn 来源冲突: ' + tid)
        require(kinds.get(tid) != {'subagent'} or parents.get(tid), '子代理缺少父关系，停止: ' + tid)
        visiting.add(tid)
        for child in sorted(children.get(tid, ())):
            visit(child)
        visiting.remove(tid)
        visited.add(tid)
        ordered.append(tid)
        require(len(ordered) <= 100, '包含子代理后超过 100 个实际线程，请缩小选择范围')

    for tid in requested:
        visit(tid)
    watch = set(ordered)
    for tid in ordered:
        chain = {tid}
        while parents.get(tid):
            require(len(parents[tid]) == 1, 'spawn 父关系冲突: ' + tid)
            parent = next(iter(parents[tid]))
            require(parent not in chain, 'spawn 关系存在环，停止')
            chain.add(parent)
            watch.add(parent)
            tid = parent
    edges = sorted([parent, child] for child, values in parents.items() for parent in values
                   if child in watch or parent in visited)
    watch_assets = [{'relative': relative, 'id': tid} for tid in sorted(watch - visited) for relative in paths.get(tid, ())]
    return {'requested_ids': requested, 'ids': ordered, 'edges': edges,
            'watch_ids': sorted(watch), 'watch_threads': sorted(watch & thread_ids), 'watch_assets': watch_assets}


def check_family(root, plan, completed=()):
    family = plan.get('family')
    if not family:
        return
    targets, done = set(plan['ids']), set(completed)
    watched = set(family['watch_ids'])
    with contextlib.closing(connect(safe(root, 'state_5.sqlite'))) as conn:
        require(schema_hash(conn) == SCHEMAS['state_5.sqlite'], '关系检查时 schema 变化')
        parents, kinds, _, thread_ids = spawn_index(root, conn)
    edges = sorted([parent, child] for child, values in parents.items() for parent in values
                   if child in watched or parent in targets)
    require(edges == [edge for edge in family['edges'] if edge[1] not in done], 'spawn 关系在预览后变化，须重新预览')
    require(targets - done <= thread_ids and not done & thread_ids, '线程集合在预览后变化')
    require(watched & thread_ids == set(family['watch_threads']) - done, '关联父会话集合在预览后变化')
    for tid in targets - done:
        require(len(parents.get(tid, ())) <= 1 and not ('automation' in kinds.get(tid, ()) and parents.get(tid)), 'spawn 来源冲突')


def watch_ids(plan):
    return plan.get('family', {}).get('watch_ids', plan['ids'])


def watch_assets(plan):
    return plan['assets'] + plan.get('family', {}).get('watch_assets', [])


def check_relations(conn, name, ids):
    marks = ','.join('?' for _ in ids)
    if name == 'state_5.sqlite':
        require(not conn.execute('SELECT 1 FROM thread_spawn_edges WHERE parent_thread_id IN (' + marks + ') AND child_thread_id NOT IN (' + marks + ') LIMIT 1', ids + ids).fetchone(), '存在未纳入的 spawned 后代，停止')
        require(not conn.execute('SELECT 1 FROM threads WHERE attached_thread_parent_id IN (' + marks + ') OR (id IN (' + marks + ') AND attached_thread_parent_id IS NOT NULL) LIMIT 1', ids + ids).fetchone(), '存在 attached thread 关系')
        require(not conn.execute('SELECT 1 FROM thread_attachments WHERE thread_id IN (' + marks + ') LIMIT 1', ids).fetchone(), '附件归属尚未实现，停止')
        for tid, source in conn.execute('SELECT id,source FROM threads WHERE id NOT IN (' + marks + ')', ids):
            require(not any(target in (source or '') for target in ids), '其他线程 source 引用目标: ' + tid)
    if name == 'queue_1.sqlite':
        for table in ('queued_items', 'queued_thread_revisions'):
            require(not conn.execute('SELECT 1 FROM ' + table + ' WHERE thread_id IN (' + marks + ') LIMIT 1', ids).fetchone(), '目标仍有队列或队列修订')
    if name == 'memories_1.sqlite':
        require(not conn.execute('SELECT 1 FROM stage1_outputs WHERE thread_id IN (' + marks + ') LIMIT 1', ids).fetchone(), '目标已有记忆输出，需另行分析共享记忆')
        for kind, status in conn.execute('SELECT kind,status FROM jobs WHERE job_key IN (' + marks + ')', ids):
            require(kind == 'memory_stage1' and status in ('error', 'done', 'complete', 'completed'), '目标记忆任务可能仍在执行')


def check_auxiliary(root, ids):
    result = {}
    paths = set(root.glob('*.db')) | set(root.glob('*.sqlite'))
    paths |= set((root / 'sqlite').rglob('*.db')) | set((root / 'sqlite').rglob('*.sqlite'))
    for path in sorted(paths):
        relative = path.relative_to(root).as_posix()
        if relative in SCHEMAS:
            continue
        require(relative in AUXILIARY_SCHEMAS, '发现未审阅的额外数据库: ' + relative)
        with contextlib.closing(connect(regular(safe(root, relative)))) as conn:
            conn.execute('BEGIN')
            require(schema_hash(conn) == AUXILIARY_SCHEMAS[relative], '额外数据库 schema 变化: ' + relative)
            for table in tables(conn):
                for col in conn.execute('PRAGMA table_info(' + quote(table) + ')').fetchall():
                    for tid in ids:
                        hit = conn.execute('SELECT 1 FROM ' + quote(table) + ' WHERE instr(CAST(' + quote(col[1]) + ' AS TEXT),?) > 0 LIMIT 1', (tid,)).fetchone()
                        require(not hit, '额外数据库引用目标，需要独立适配: ' + relative + '.' + table)
            result[relative] = {'schema_sha256': schema_hash(conn), 'target_references': 0, 'action': 'preserve'}
    return result


def json_refs(value, ids):
    if isinstance(value, dict):
        return any(k in ids or json_refs(v, ids) for k, v in value.items())
    if isinstance(value, list):
        return any(json_refs(v, ids) for v in value)
    return isinstance(value, str) and value in ids


def filter_text(data, key, ids):
    kept, removed, offset = [], [], 0
    for line_number, line in enumerate(data.splitlines(keepends=True), 1):
        require(line.endswith(b'\n'), '共享 JSONL 尾行不完整')
        record = json.loads(line)
        if record.get(key) in ids:
            removed.append({'line': line_number, 'start': offset, 'end': offset + len(line), 'id': record[key]})
        else:
            kept.append(line)
        offset += len(line)
    return b''.join(kept), removed


def check_history_dependencies(root, ids):
    """中文说明：历史前缀不属于 spawn；在支持行级迁移前，对相关引用保守阻断。"""
    selected = set(ids)
    entries = []
    physical = {}
    for directory in ('sessions', 'archived_sessions'):
        for path in sorted(safe(root, directory).rglob('*.jsonl')):
            regular(safe(root, path.relative_to(root)))
            with path.open('rb') as handle:
                line = handle.readline(128 * 1024 + 1)
            require(len(line) <= 128 * 1024, '历史身份记录过大，无法完整分析引用，停止')
            require(bool(line), '历史身份为空，无法完整分析引用，停止')
            value = json.loads(line)
            require(value.get('type') == 'session_meta', '历史身份格式未知，无法完整分析引用，停止')
            meta = value.get('payload', {})
            tid = meta.get('id')
            require(bool(tid), '历史身份缺失，无法完整分析引用，停止')
            suffix = path.stem.rsplit('_', 1)[-1]
            pid = suffix if len(suffix) == 36 else tid
            physical.setdefault(pid, set()).add(tid)
            entries.append((tid, meta, value.get('ordinal')))
    for tid, meta, ordinal in entries:
        base = meta.get('history_base')
        if base is None and (meta.get('history_mode') != 'paginated' or ordinal == 0):
            continue
        referenced = base.get('thread_id') if isinstance(base, dict) else None
        owners = physical.get(referenced, set())
        # 未知引用的完整关联无法证明；不让删除绕过潜在共享历史。
        require(tid not in selected and not selected.intersection(owners) and
                referenced not in selected and len(owners) == 1,
                '检测到目标相关或未解析的 history_base 前缀依赖，须完成专门兼容验证后再执行')


def scan_files(root, source_root, ids, thread_rows, family=None):
    check_history_dependencies(root, ids)
    assets, metadata = [], []
    parent_map = {child: parent for parent, child in (family or {}).get('edges', [])}
    for row in thread_rows:
        d = dict(zip(row['columns'], map(uncell, row['values'])))
        tid = d['id']
        path = pathlib.Path(d['rollout_path'])
        rel = path.relative_to(source_root) if path.is_absolute() else path
        require(rel.parts[0] in ('sessions', 'archived_sessions') and path.name.endswith(tid + '.jsonl'), 'rollout 路径与目标不一致')
        p = regular(safe(root, rel))
        require(p.stat().st_nlink == 1, 'rollout 具有共享硬链接，停止')
        first, line_count = None, 0
        with p.open('rb') as f:
            for line in f:
                require(line.endswith(b'\n'), 'rollout 尾行不完整')
                value = json.loads(line)
                if first is None:
                    first = value
                line_count += 1
                # 本轮不支持带本地附件 URL 的复杂资产，宁可在预览阶段阻断。
                require(b'/attachments/' not in line and b'"local_image"' not in line, 'rollout 含附件路径，需要补充资产处理')
        meta = first.get('payload', {}) if first and first.get('type') == 'session_meta' else {}
        origin = spawn_origin(meta)
        ancestors, parent = set(), parent_map.get(tid)
        while parent and parent not in ancestors:
            ancestors.add(parent)
            parent = parent_map.get(parent)
        require(meta.get('id') == tid and (meta.get('session_id', tid) == tid or meta.get('session_id') in ancestors), 'rollout 元数据身份不匹配')
        metadata.append({'id': tid, 'forked_from_id': meta.get('forked_from_id'),
                         'parent_thread_id': parent_map.get(tid), 'thread_kind': 'subagent' if parent_map.get(tid) else origin['kind'],
                         'agent_path': origin['agent_path'], 'agent_nickname': origin['agent_nickname'],
                         'project_id': d.get('project_id'), 'thread_section_id': d.get('thread_section_id'),
                         'cli_version': d.get('cli_version'), 'lines': line_count})
        assets.append({'relative': rel.as_posix(), 'id': tid, 'kind': 'rollout', **fingerprint(p)})
        capability = pathlib.Path('tui-thread-reference-capabilities') / tid
        if safe(root, capability).exists():
            assets.append({'relative': capability.as_posix(), 'id': tid, 'kind': 'capability', **fingerprint(safe(root, capability))})
    require({a['id'] for a in assets} == set(ids), '部分目标没有找到 rollout')
    known_paths = {a['relative'] for a in assets}
    for base, dirs, names in os.walk(root):
        dirs[:] = [d for d in dirs if d not in ('packages', 'backups', 'skills', 'plugins', 'vendor_imports', '.git')]
        for name in names:
            relative = (pathlib.Path(base) / name).relative_to(root).as_posix()
            if any(t in relative for t in ids) and not relative.startswith('thread-writer-locks/'):
                require(relative in known_paths, '出现未建模的目标资产: ' + relative)
    for directory in ('sessions', 'archived_sessions'):
        for p in safe(root, directory).rglob('*.jsonl'):
            regular(safe(root, p.relative_to(root)))
            if any(p.name.endswith(t + '.jsonl') for t in ids):
                require(str(p.relative_to(root)) in {a['relative'] for a in assets}, '同一目标有多个 rollout')
                continue
            with p.open('rb') as f:
                line = f.readline()
            if line:
                value = json.loads(line)
                meta = dict(value.get('payload', {}))
                # fork 的历史已复制，来源标识只是溯源证据；保留 fork，不级联或改写其来源。
                meta.pop('forked_from_id', None)
                require(not json_refs(meta, set(ids)), '其他 rollout 元数据引用目标: ' + p.name)
    global_state = root / '.codex-global-state.json'
    if global_state.exists():
        require(not json_refs(json.loads(regular(safe(root, global_state.name)).read_bytes()), set(ids)), '共享全局状态引用目标，需要专门适配')
    for p in safe(root, 'memories').rglob('*'):
        if p.is_file() and '.git' not in p.parts:
            data = regular(safe(root, p.relative_to(root))).read_bytes()
            require(not any(t.encode() in data for t in ids), '共享记忆文件引用目标: ' + str(p.relative_to(root)))
    return assets, metadata


def not_open(root, assets):
    paths = [str(root / a['relative']) for a in assets if (root / a['relative']).exists()]
    if not paths:
        return
    result = subprocess.run(['lsof', '-n', '-P', '-Fpn', '--', *paths], capture_output=True, text=True)
    require(result.returncode in (0, 1), '无法检查目标文件句柄: ' + result.stderr)
    pid = None
    for line in result.stdout.splitlines():
        if line.startswith('p'):
            pid = int(line[1:])
        elif line.startswith('n'):
            require(pid == os.getpid(), '目标文件仍被进程占用: ' + str(pid))


def probe_writer_locks(root, ids):
    """预览只检查已存在的锁，不在源目录创建锁文件；执行阶段另行持有完整排他锁。"""
    with contextlib.ExitStack() as stack:
        paths = ['thread-writer-locks/.coordination.lock'] + ['thread-writer-locks/' + tid + '.lock' for tid in sorted(ids)]
        for relative in paths:
            path = safe(root, relative)
            if path.exists():
                handle = stack.enter_context(regular(path).open('rb'))
                fcntl.flock(handle, fcntl.LOCK_EX | fcntl.LOCK_NB)
                require(os.fstat(handle.fileno()).st_ino == path.stat().st_ino, '活动检查期间锁身份发生变化')


@contextlib.contextmanager
def locks(root, ids):
    """遵循 0.169 的 coordination → 每目标 .lock 协议；不清扫其他会话的锁。"""
    held = []
    directory = safe(root, 'thread-writer-locks')
    directory.mkdir(mode=0o700, exist_ok=True)
    coord = safe(root, 'thread-writer-locks/.coordination.lock').open('a+b')
    try:
        fcntl.flock(coord, fcntl.LOCK_EX | fcntl.LOCK_NB)
        try:
            for tid in sorted(ids):
                f = safe(root, 'thread-writer-locks/' + tid + '.lock').open('a+b')
                try:
                    fcntl.flock(f, fcntl.LOCK_EX | fcntl.LOCK_NB)
                except BaseException:
                    f.close()
                    raise
                held.append(f)
        finally:
            fcntl.flock(coord, fcntl.LOCK_UN)
        yield
    finally:
        # 只释放句柄，保留锁文件 inode；Codex 的原生清扫机制会回收无主目标锁。
        for f in reversed(held):
            f.close()
        coord.close()


@contextlib.contextmanager
def text_lock(root, name):
    require(name in TEXT_FILES, '未知共享文本')
    lock_path = safe(root, name + '.lock' if name == 'session_index.jsonl' else name)
    with lock_path.open('a+b' if name == 'session_index.jsonl' else 'r+b') as f:
        fcntl.flock(f, fcntl.LOCK_EX | fcntl.LOCK_NB)
        require(os.fstat(f.fileno()).st_ino == lock_path.stat().st_ino, '共享文本锁 inode 发生变化')
        yield f


def activity(root, real, socket_path, ids, assets):
    loaded = []
    if real or socket_path:
        require(socket_path, '真实执行必须连接现有 daemon 核实 loaded/list')
        daemon = Daemon(socket_path, root)
        try:
            loaded = sorted(daemon.loaded())
        finally:
            daemon.close()
        require(not set(ids) & set(loaded), '目标或关联父会话仍被 daemon 加载: ' + ','.join(sorted(set(ids) & set(loaded))))
    not_open(root, assets)
    return {'loaded_ids': loaded, 'checked_at': time.time()}


def prepare(root, ids, work, socket_path, all_conversations=False, operation='trash'):
    require(operation == 'trash' or (operation == 'permanent_delete' and all_conversations), '当前模式不允许永久删除')
    root, real = validate_scope(root, ids, all_conversations)
    # 缺失目标应在复制六库之前拒绝，避免无效预览产生大恢复包。
    with contextlib.closing(connect(safe(root, 'state_5.sqlite'))) as conn:
        require(schema_hash(conn) == SCHEMAS['state_5.sqlite'], '未知 schema: state_5.sqlite')
        marks = ','.join('?' for _ in ids)
        found = {r[0] for r in conn.execute('SELECT id FROM threads WHERE id IN (' + marks + ')', ids)}
        require(found == set(ids), '目标线程已不存在，请刷新会话库；没有创建恢复包')
        family = family_scope(root, conn, ids)
    ids = family['ids']
    validate_scope(root, ids, all_conversations)
    probe_writer_locks(root, family['watch_ids'])
    work = pathlib.Path(work).absolute()
    require(root != work and root not in work.parents, '恢复材料必须放在源目录之外')
    require(not work.exists(), '预览目录已存在，拒绝覆盖')
    work.mkdir(parents=True, mode=0o700)
    sync_dir(work.parent)
    backup = work / 'backup'
    backup.mkdir(mode=0o700)
    sync_dir(work)
    auxiliary = check_auxiliary(root, ids)
    database_rows, baselines, db_backups = {}, {}, {}
    for name, expected in SCHEMAS.items():
        with contextlib.closing(connect(safe(root, name))) as conn:
            require(schema_hash(conn) == expected, '未知 schema: ' + name)
            check_relations(conn, name, ids)
            with contextlib.closing(sqlite3.connect(backup / name)) as dest:
                conn.backup(dest)
        os.chmod(backup / name, 0o600)
        with (backup / name).open('rb') as f:
            os.fsync(f.fileno())
        sync_dir(backup)
        with contextlib.closing(connect(backup / name)) as conn:
            require(schema_hash(conn) == expected, '备份 schema 漂移: ' + name)
            check_relations(conn, name, ids)
            require(conn.execute('PRAGMA integrity_check').fetchall() == [('ok',)], '备份完整性失败: ' + name)
            require(not conn.execute('PRAGMA foreign_key_check').fetchall(), '备份外键检查失败: ' + name)
            database_rows[name] = rows_snapshot(conn, ids)
            baselines[name] = digest_others(conn, ids)
        db_backups[name] = fingerprint(backup / name)
    thread_group = next((r for r in database_rows['state_5.sqlite'] if r['table'] == 'threads'), None)
    require(thread_group and len(thread_group['values']) == len(ids), '数据库缺少目标线程')
    thread_rows = [{'columns': thread_group['columns'], 'values': r} for r in thread_group['values']]
    source_root = root if real else pathlib.Path(json.loads((root / 'authorized-live-copy.json').read_bytes())['source_root'])
    assets, metadata = scan_files(root, source_root, ids, thread_rows, family)
    for asset in assets:
        destination = backup / asset['relative']
        durable_copy(safe(root, asset['relative']), destination)
        require(fingerprint(destination)['sha256'] == asset['sha256'], '资产备份不匹配')
    sqlite_preview = []
    for name, groups in database_rows.items():
        for group in groups:
            owner_index = group['columns'].index(group['owner'])
            key_indexes = [group['columns'].index(k) for k in group['pk']]
            sqlite_preview.append({'database': name, 'table': group['table'], 'row_count': len(group['values']),
                                   'primary_columns': group['pk'],
                                   'rows': [{'owner': r[owner_index], 'primary_key': [r[i] for i in key_indexes]}
                                            for r in group['values']]})
    texts = {}
    for name, key in TEXT_FILES.items():
        data = regular(safe(root, name)).read_bytes()
        after, removed = filter_text(data, key, ids)
        atomic(backup / name, data)
        texts[name] = {'key': key, 'before_sha256': sha(data), 'after_sha256': sha(after),
                       'before_bytes': len(data), 'after_bytes': len(after), 'removed': removed}
    probe_writer_locks(root, family['watch_ids'])
    probe = activity(root, real, socket_path, family['watch_ids'], assets + family['watch_assets'])
    for directory, _, _ in os.walk(backup, topdown=False):
        sync_dir(directory)
    sync_dir(work)
    plan = {'kind': KIND, 'operation_id': 'authorized-' + uuid.uuid4().hex, 'root': str(root),
            'scope': 'all_conversations' if all_conversations else 'legacy_ids', 'operation': operation,
            'source_root': str(source_root), 'real': real, 'ids': ids, 'socket': socket_path,
            'family': family,
            'assets': assets, 'metadata': metadata, 'texts': texts, 'schemas': SCHEMAS,
            'sqlite_preview': sqlite_preview,
            'auxiliary_sqlite': auxiliary,
            'database_backups': db_backups, 'non_target_baselines': baselines, 'activity': probe,
            'protected': ['credentials', 'non-target rows/files', 'shared attachments/memories',
                          'fork parents', 'WAL/SHM (no manual editing)', 'existing backups', 'project files']}
    if operation == 'trash':
        plan['trash_package'] = {'format': TRASH_KIND, 'relative': 'Codex-Chronicle-' + plan['operation_id']}
    check_family(root, plan)
    atomic(work / 'rows.json', encoded(database_rows))
    plan['rows_sha256'] = sha((work / 'rows.json').read_bytes())
    atomic(work / 'plan.json', encoded(plan))
    atomic(work / 'plan.sha256', sha((work / 'plan.json').read_bytes()).encode())
    atomic(work / 'journal.json', encoded({'phase': 'prepared', 'receipts': {}, 'steps': []}))
    return summary(plan)


def summary(plan):
    return {'ids': plan['ids'], 'real': plan['real'], 'assets': plan['assets'],
            'metadata': plan['metadata'], 'texts': plan['texts'], 'activity': plan['activity'],
            'operation_id': plan['operation_id']}


def load_plan(work, all_conversations=False):
    work = pathlib.Path(work).absolute()
    safe(work, '.')
    data = regular(work / 'plan.json').read_bytes()
    require(sha(data) == (work / 'plan.sha256').read_text(), '影响预览损坏')
    plan = json.loads(data)
    require(plan['kind'] == KIND and plan['schemas'] == SCHEMAS, '预览类型或 schema 不匹配')
    general = plan.get('scope', 'legacy_ids') == 'all_conversations'
    require(plan.get('scope', 'legacy_ids') in ('legacy_ids', 'all_conversations') and
            (not general or all_conversations), '此预览需要全会话执行模式，不能通过旧入口扩大权限')
    require(plan.get('operation', 'trash') == 'trash' or
            (general and plan.get('operation') == 'permanent_delete'), '预览操作未获授权')
    root, real = validate_scope(plan['root'], plan['ids'], general)
    require(real == plan['real'] and root != work and root not in work.parents and
            plan['source_root'] == str(pathlib.Path.home() / '.codex'), '恢复根身份错误')
    require(set(plan['texts']) == set(TEXT_FILES), '共享文本资产集合不完整')
    return work, plan


def load(work, all_conversations=False):
    work, plan = load_plan(work, all_conversations)
    require((work / 'rows.json').is_file(), '永久删除的恢复正文已清理，不能恢复' if plan.get('operation') == 'permanent_delete' else '行级恢复材料缺失')
    data = regular(work / 'rows.json').read_bytes()
    require(sha(data) == plan['rows_sha256'], '行级恢复材料损坏')
    rows = json.loads(data)
    validate_rows(plan, rows)
    return work, plan, rows


def validate_rows(plan, rows):
    require(set(rows) == set(SCHEMAS), '恢复材料数据库集合错误')
    for name, groups in rows.items():
        for group in groups:
            require(group['owner'] == OWNERS.get(group['table']), '非法归属规则')
            index = group['columns'].index(group['owner'])
            for row in group['values']:
                require(uncell(row[index]) in plan['ids'], '行快照越过 ID 授权范围')
    threads = next(g for g in rows['state_5.sqlite'] if g['table'] == 'threads')
    expected_rollouts = {}
    for values in threads['values']:
        thread = dict(zip(threads['columns'], map(uncell, values)))
        path = pathlib.Path(thread['rollout_path'])
        relative = path.relative_to(plan['source_root']) if path.is_absolute() else path
        require(relative.parts[0] in ('sessions', 'archived_sessions') and
                relative.name.endswith(thread['id'] + '.jsonl'), '行快照的 rollout 身份不匹配')
        expected_rollouts[thread['id']] = relative.as_posix()
    require(set(expected_rollouts) == set(plan['ids']), '行快照缺少授权线程')
    require(len({a['relative'] for a in plan['assets']}) == len(plan['assets']), '资产清单重复')
    for asset in plan['assets']:
        require(asset['id'] in plan['ids'] and (
            asset['kind'] == 'rollout' and asset['relative'] == expected_rollouts[asset['id']] or
            asset['kind'] == 'capability' and asset['relative'] == 'tui-thread-reference-capabilities/' + asset['id']), '文件资产越权')
    require({a['id'] for a in plan['assets'] if a['kind'] == 'rollout'} == set(plan['ids']), '文件清单缺少 rollout')


def package_relative(plan):
    package = plan.get('trash_package', {})
    expected = 'Codex-Chronicle-' + plan['operation_id']
    require(package == {'format': TRASH_KIND, 'relative': expected}, '回收文件夹配置不匹配')
    return expected


def stage_trash_package(work, plan, journal):
    # 暂存包仅包含目标数据；共享整库和其他会话文本只在执行中的临时备份内存在。
    relative = package_relative(plan)
    require(work.stat().st_dev == pathlib.Path(plan['root']).stat().st_dev,
            '回收暂存目录与源不在同一卷，无法保证进入已确认的系统回收站')
    folder = safe(work, relative)
    folder.mkdir(mode=0o700)
    sync_dir(work)
    journal['trash_package'] = {'format': TRASH_KIND, 'state': 'staging', 'relative': relative}
    save_journal(work, journal)
    for asset in plan['assets']:
        durable_copy(safe(work / 'backup', asset['relative']), safe(folder / 'files', asset['relative']))
    durable_copy(work / 'rows.json', folder / 'database-rows.json')
    texts = {}
    for name, text in plan['texts'].items():
        before = regular(safe(work / 'backup', name)).read_bytes()
        removed_bytes, records = 0, []
        for line in text['removed']:
            data = before[line['start']:line['end']]
            require(json.loads(data).get(text['key']) == line['id'], '回收文本记录归属错误')
            records.append({'offset': line['start'] - removed_bytes, 'data': base64.b64encode(data).decode()})
            removed_bytes += len(data)
        texts[name] = {'key': text['key'], 'before_sha256': text['before_sha256'],
                       'before_bytes': text['before_bytes'], 'after_sha256': text['after_sha256'],
                       'after_bytes': text['after_bytes'], 'records': records}
    text_data = encoded(texts)
    atomic(folder / 'text-records.json', text_data)
    manifest = {'format': TRASH_KIND, 'operation_id': plan['operation_id'], 'source_root': plan['root'],
                'original_codex_root': plan['source_root'], 'conversation_ids': plan['ids'],
                'created_at': time.time(), 'schemas': plan['schemas'], 'assets': plan['assets'],
                'database_rows_sha256': plan['rows_sha256'], 'text_records_sha256': sha(text_data)}
    atomic(folder / 'manifest.json', encoded(manifest))
    for directory, _, _ in os.walk(folder, topdown=False):
        sync_dir(directory)
    journal['trash_package'].update({'manifest_sha256': sha(encoded(manifest)),
                                    'device': folder.stat().st_dev, 'inode': folder.stat().st_ino})
    save_journal(work, journal)
    validate_trash_package(folder, plan, journal)
    return folder


def validate_trash_package(folder, plan, journal, restoring=False):
    folder = safe(folder, '.')
    require(folder.is_dir(), '回收文件夹不存在')
    data = regular(safe(folder, 'manifest.json')).read_bytes()
    require(sha(data) == journal['trash_package']['manifest_sha256'], '回收清单校验失败')
    manifest = json.loads(data)
    require(manifest['format'] == TRASH_KIND and manifest['operation_id'] == plan['operation_id']
            and manifest['source_root'] == plan['root'] and manifest['original_codex_root'] == plan['source_root']
            and manifest['conversation_ids'] == plan['ids'] and manifest['schemas'] == plan['schemas']
            and manifest['assets'] == plan['assets'], '回收清单身份不匹配')
    row_data = regular(safe(folder, 'database-rows.json')).read_bytes()
    require(sha(row_data) == manifest['database_rows_sha256'] == plan['rows_sha256'], '回收数据库行校验失败')
    rows = json.loads(row_data)
    validate_rows(plan, rows)
    text_data = regular(safe(folder, 'text-records.json')).read_bytes()
    require(sha(text_data) == manifest['text_records_sha256'], '回收索引记录校验失败')
    texts = json.loads(text_data)
    require(set(texts) == set(TEXT_FILES), '回收索引集合错误')
    for name, text in texts.items():
        expected = plan['texts'][name]
        require({k: text[k] for k in ('key', 'before_sha256', 'before_bytes', 'after_sha256', 'after_bytes')} ==
                {k: expected[k] for k in ('key', 'before_sha256', 'before_bytes', 'after_sha256', 'after_bytes')}, '回收索引元数据不匹配')
        removed_bytes, preview = 0, []
        for record in text['records']:
            raw = base64.b64decode(record['data'], validate=True)
            require(raw.endswith(b'\n') and len(raw.splitlines()) == 1 and
                    json.loads(raw).get(text['key']) in plan['ids'], '回收索引记录越权')
            start = record['offset'] + removed_bytes
            preview.append((start, start + len(raw), json.loads(raw)[text['key']]))
            removed_bytes += len(raw)
        require(preview == [(line['start'], line['end'], line['id']) for line in expected['removed']], '回收索引记录与影响预览不一致')
    expected_files = {'manifest.json', 'database-rows.json', 'text-records.json'}
    for asset in plan['assets']:
        relative = 'files/' + asset['relative']
        expected_files.add(relative)
        source = safe(folder, relative)
        if restoring and not source.exists():
            source = safe(pathlib.Path(plan['root']), asset['relative'])
        require(fingerprint(source)['sha256'] == asset['sha256'], '回收对话文件校验失败: ' + asset['relative'])
    for path in folder.rglob('*'):
        require(not path.is_symlink() and (path.is_dir() or path.is_file()), '回收包包含不支持的文件')
        if path.is_file():
            require(path.relative_to(folder).as_posix() in expected_files, '回收包含清单外文件')
    return rows, texts


def published_trash_package(work, plan, journal, binary):
    package = journal.get('trash_package')
    if not package or package.get('state') == 'staging':
        return None
    relative = package_relative(plan)
    receipt = package.get('receipt')
    if receipt:
        require(receipt['operation_id'] == plan['operation_id'] and receipt['relative_path'] == relative,
                '回收文件夹回执身份错误')
        return safe(pathlib.Path(receipt['trash_path']), '.')
    if safe(work, relative).exists():
        return None
    # 原生目录移动成功但回执未落盘：只按目录 inode 和已落盘的清单摘要定位，不能猜名称。
    location = bridge(binary, 'locate', {**plan, 'root': str(work)})
    trash = safe(pathlib.Path(location['trash_path']), '.')
    candidates = [trash / relative]
    if not candidates[0].exists():
        candidates = list(trash.iterdir())
    found = [p for p in candidates if not p.is_symlink() and p.is_dir()
             and (p.stat().st_dev, p.stat().st_ino) == (package['device'], package['inode'])]
    require(len(found) == 1, '回收文件夹回执缺失，无法唯一定位；已保留临时恢复材料')
    package['receipt'] = {'operation_id': plan['operation_id'], 'relative_path': relative, 'trash_path': str(found[0])}
    package['state'] = 'published'
    save_journal(work, journal)
    return found[0]


def publish_trash_package(work, plan, journal, binary, injected):
    folder = safe(work, package_relative(plan))
    validate_trash_package(folder, plan, journal)
    journal['trash_package']['state'] = 'publish_intent'
    save_journal(work, journal)
    fault(plan, 'package-before-publish', injected)
    receipt = bridge(binary, 'move', {**plan, 'root': str(work)}, {'relative': folder.name})
    fault(plan, 'package-before-receipt', injected)
    journal['trash_package'].update({'receipt': receipt, 'state': 'published'})
    save_journal(work, journal)
    published = published_trash_package(work, plan, journal, binary)
    validate_trash_package(published, plan, journal)
    sync_dir(published)
    if sys.platform == 'darwin':
        # macOS 可能禁止枚举/打开废纸篓根；通过已知包中的清单请求完整磁盘同步，无需扩大目录权限。
        with (published / 'manifest.json').open('rb') as handle:
            fcntl.fcntl(handle.fileno(), fcntl.F_FULLFSYNC)
    else:
        sync_dir(published.parent)
    sync_dir(work)
    fault(plan, 'package-published', injected)


def purge_temporary_recovery(work, journal):
    for name in ('backup', 'restore-texts'):
        if (work / name).exists():
            shutil.rmtree(safe(work, name))
    if (work / 'rows.json').exists():
        (work / 'rows.json').unlink()
    sync_dir(work)
    journal['recovery_purged'] = True
    save_journal(work, journal, step={'type': 'recovery_purged'})


def restored_text(current, text):
    # 只在原有非目标前缀完整或已经恢复时合并，保留执行之后的合法追加；重排/改写时拒绝猜测。
    if sha(current[:text['before_bytes']]) == text['before_sha256']:
        return current
    size = text['after_bytes']
    require(sha(current[:size]) == text['after_sha256'], '共享文本已改写，不能自动合并回收记录')
    parts, previous = [], 0
    for record in text['records']:
        offset = record['offset']
        require(previous <= offset <= size, '回收文本偏移无效')
        parts.extend((current[previous:offset], base64.b64decode(record['data'], validate=True)))
        previous = offset
    parts.append(current[previous:size])
    before = b''.join(parts)
    require(sha(before) == text['before_sha256'], '回收文本重建校验失败')
    return before + current[size:]


def restore_package_text(work, plan, journal, name, text, dry_run=False, injected=None):
    root = pathlib.Path(plan['root'])
    with text_lock(root, name) as text_handle:
        current = regular(safe(root, name)).read_bytes()
        checkpoint = journal.get('restore_text_checkpoints', {}).get(name)
        if checkpoint:
            versions = []
            for version in ('before', 'after'):
                saved = checkpoint[version]
                data = regular(safe(work / 'restore-texts', saved['file'])).read_bytes()
                require(sha(data) == saved['sha256'], '恢复过程的临时文本校验失败')
                versions.append(data)
            before, after = versions
            if current.startswith(after):
                result = current
            elif current.startswith(before):
                result = after + current[len(before):]
            else:
                prefix = next((i for i, (a, b) in enumerate(zip(current, after)) if a != b), min(len(current), len(after)))
                partial = (len(current) == len(before) and current[prefix:] == before[prefix:]) or (
                    len(before) < len(current) <= len(after) and current == after[:len(current)])
                require(name == 'history.jsonl' and partial, '共享文本已被其他操作改写，停止恢复')
                result = after
        else:
            result = restored_text(current, text)
        if dry_run or result == current:
            return
        # 恢复也可能断电；只在恢复期间暂存当前文本与预期结果，成功后清理，不把非目标正文放入回收包。
        directory = safe(work, 'restore-texts')
        directory.mkdir(mode=0o700, exist_ok=True)
        token = uuid.uuid4().hex
        saved_versions = {}
        for version, data in (('before', current), ('after', result)):
            relative = token + '-' + version
            atomic(directory / relative, data)
            saved_versions[version] = {'file': relative, 'sha256': sha(data)}
        journal.setdefault('restore_text_checkpoints', {})[name] = saved_versions
        save_journal(work, journal)
        write_text(root / name, result, text_handle, name == 'history.jsonl', plan, 'restore-' + name, injected)
        save_journal(work, journal, step={'type': 'text_restored', 'name': name})


def restore_trash_package(work, plan, journal, binary, folder, injected=None):
    rows, texts = validate_trash_package(folder, plan, journal, restoring=True)
    root = pathlib.Path(plan['root'])
    activity(root, plan['real'], plan['socket'], watch_ids(plan), watch_assets(plan))
    check_auxiliary(root, plan['ids'])
    # 在第一次实际恢复前检查全部共享文本、目标文件冲突和行级恢复事务。
    for name, text in texts.items():
        restore_package_text(work, plan, journal, name, text, dry_run=True)
    for asset in plan['assets']:
        target = safe(root, asset['relative'])
        require(not target.exists() or fingerprint(target)['sha256'] == asset['sha256'], '恢复文件冲突')
    for name, groups in rows.items():
        if groups:
            mutate_db(safe(root, name), name, plan['ids'], groups, restore=True, dry_run=True)
    save_journal(work, journal, 'restoring')
    for name, groups in rows.items():
        if groups:
            check = mutate_db(safe(root, name), name, plan['ids'], groups, restore=True)
            save_journal(work, journal, step={'type': 'db_restored', **check})
    for name, text in texts.items():
        restore_package_text(work, plan, journal, name, text, injected=injected)
    for asset in plan['assets']:
        target = safe(root, asset['relative'])
        if not target.exists():
            receipt = {'operation_id': plan['operation_id'], 'relative_path': asset['relative'],
                       'trash_path': str(safe(folder / 'files', asset['relative']))}
            bridge(binary, 'restore', plan, receipt=receipt)
        require(fingerprint(target)['sha256'] == asset['sha256'], '恢复文件校验失败')
        sync_dir(target.parent)
    for name in SCHEMAS:
        with contextlib.closing(connect(safe(root, name))) as conn:
            require(rows_snapshot(conn, plan['ids']) == rows[name], '回收记录恢复不完整')
            require(conn.execute('PRAGMA integrity_check').fetchall() == [('ok',)], '恢复后完整性失败')
            require(not conn.execute('PRAGMA foreign_key_check').fetchall(), '恢复后外键失败')
    journal.pop('restore_text_checkpoints', None)
    save_journal(work, journal, 'rolled_back')
    purge_temporary_recovery(work, journal)
    validate_trash_package(folder, plan, journal, restoring=True)
    # 用原生接口将已恢复包的剩余元数据取回暂存区，再清理本次生成物；不打开废纸篓根目录。
    bridge(binary, 'restore', {**plan, 'root': str(work)}, receipt=journal['trash_package']['receipt'])
    shutil.rmtree(safe(work, package_relative(plan)))
    sync_dir(work)
    journal['trash_package']['state'] = 'restored'
    save_journal(work, journal)
    return {'phase': 'rolled_back', 'ids': plan['ids']}


def check_backups(work, plan):
    for name, expected in plan['database_backups'].items():
        require(fingerprint(regular(safe(work / 'backup', name)))['sha256'] == expected['sha256'], '数据库恢复材料损坏: ' + name)
    for a in plan['assets']:
        require(a['id'] in plan['ids'] and a['kind'] in ('rollout', 'capability'), '文件越权')
        require(fingerprint(regular(safe(work / 'backup', a['relative'])))['sha256'] == a['sha256'], '文件恢复材料损坏')
    for name, text in plan['texts'].items():
        require(name in TEXT_FILES and text['key'] == TEXT_FILES[name], '文本资产越权')
        before = regular(safe(work / 'backup', name)).read_bytes()
        require(sha(before) == text['before_sha256'], '共享文本恢复材料损坏')
        after, removed = filter_text(before, text['key'], plan['ids'])
        require(sha(after) == text['after_sha256'] and removed == text['removed'], '文本影响预览不匹配')


def preflight(work, plan, rows):
    root = pathlib.Path(plan['root'])
    check_history_dependencies(root, plan['ids'])
    check_backups(work, plan)
    check_auxiliary(root, plan['ids'])
    check_family(root, plan)
    probe = activity(root, plan['real'], plan['socket'], watch_ids(plan), watch_assets(plan))
    for a in plan['assets']:
        require(fingerprint(safe(root, a['relative'])) == {k: a[k] for k in ('bytes', 'sha256', 'device', 'inode', 'mtime_ns')}, '资产已变化: ' + a['relative'])
    for name, text in plan['texts'].items():
        with text_lock(root, name):
            require(sha(regular(safe(root, name)).read_bytes()) == text['before_sha256'], '共享索引已变化，须重新预览: ' + name)
    for name in SCHEMAS:
        with contextlib.closing(connect(safe(root, name))) as conn:
            conn.execute('BEGIN')
            require(schema_hash(conn) == SCHEMAS[name], 'schema 已变化: ' + name)
            check_relations(conn, name, plan['ids'])
            require(rows_snapshot(conn, plan['ids']) == rows[name], '目标数据库行已变化: ' + name)
    return probe


def mutate_db(path, name, ids, expected_rows, restore=False, dry_run=False):
    start = time.monotonic()
    with contextlib.closing(connect(path, True)) as conn:
        conn.execute('BEGIN IMMEDIATE')
        try:
            require(schema_hash(conn) == SCHEMAS[name], '事务内 schema 不匹配: ' + name)
            check_relations(conn, name, ids)
            before = digest_others(conn, ids)
            current = rows_snapshot(conn, ids)
            if not restore:
                require(current == expected_rows, '事务内目标行已变化: ' + name)
                # 先删子表，父表最后；history 的 projection 删除触发器须在 realtime 之后执行。
                priority = {'threads': 100, 'thread_goals': 100, 'thread_history_projection_state': 90}
                for group in sorted(expected_rows, key=lambda g: priority.get(g['table'], 0)):
                    marks = ','.join('?' for _ in ids)
                    conn.execute('DELETE FROM ' + quote(group['table']) + ' WHERE ' + quote(group['owner']) + ' IN (' + marks + ')', ids)
                require(not rows_snapshot(conn, ids), '目标行没有完全清除: ' + name)
            else:
                # 不覆盖已存在的行，也不拿整库备份覆盖共享数据库。
                current_map = {g['table']: g for g in current}
                for group in sorted(expected_rows, key=lambda g: 0 if g['table'] in ('threads', 'thread_goals') else 1):
                    names, pk = group['columns'], group['pk']
                    for row in group['values']:
                        key_values = [uncell(row[names.index(k)]) for k in pk]
                        found = conn.execute('SELECT * FROM ' + quote(group['table']) + ' WHERE ' + ' AND '.join(quote(k) + ' IS ?' for k in pk), key_values).fetchall()
                        if found:
                            require(len(found) == 1 and [cell(v) for v in found[0]] == row, '恢复行冲突: ' + name + '.' + group['table'])
                        else:
                            conn.execute('INSERT INTO ' + quote(group['table']) + ' (' + ','.join(map(quote, names)) + ') VALUES (' + ','.join('?' for _ in names) + ')', list(map(uncell, row)))
                    current_map.pop(group['table'], None)
                require(not current_map and rows_snapshot(conn, ids) == expected_rows, '出现未备份的新目标行，停止恢复')
            require(digest_others(conn, ids) == before, '触发器或外键改变了非目标数据，事务已回滚: ' + name)
            require(not conn.execute('PRAGMA foreign_key_check').fetchall(), '事务内外键检查失败: ' + name)
            conn.execute('ROLLBACK' if dry_run else 'COMMIT')
            return {'database': name, 'rows': sum(len(g['values']) for g in expected_rows),
                    'non_target': before, 'seconds': round(time.monotonic() - start, 4), 'dry_run': dry_run}
        except BaseException:
            conn.rollback()
            raise


def bridge(binary, action, plan, asset=None, receipt=None):
    request = {'action': action, 'root': plan['root'], 'operation_id': plan['operation_id']}
    if asset:
        request['relative'] = asset['relative']
    if receipt:
        request['receipt'] = receipt
    result = subprocess.run([str(binary)], input=encoded(request), capture_output=True)
    require(result.returncode == 0, '原生废纸篓操作失败: ' + result.stderr.decode(errors='replace'))
    return json.loads(result.stdout)


def save_journal(work, journal, phase=None, step=None):
    if phase:
        journal['phase'] = phase
    if step:
        journal['steps'].append(step)
    atomic(work / 'journal.json', encoded(journal))


def fault(plan, point, requested):
    if point == requested:
        require(not plan['real'], '禁止对真实目录注入中断')
        os._exit(91)


def write_text(path, data, history, is_history, plan, point, requested):
    if is_history:
        history.seek(0)
        history.write(data)
        history.flush()
        fault(plan, point + '-before-truncate', requested)
        history.truncate()
        os.fsync(history.fileno())
    else:
        atomic(path, data)


def execute(work, binary=None, dry_run=False, recover=False, injected=None, all_conversations=False):
    work, plan = load_plan(work, all_conversations)
    root = pathlib.Path(plan['root'])
    journal = json.loads((work / 'journal.json').read_bytes())
    require(not injected or not plan['real'], '真实操作不允许故障注入')
    if not dry_run:
        require(plan.get('operation', 'trash') == 'permanent_delete' or
                (binary and pathlib.Path(binary).is_file()), '缺少项目原生回收站 bridge')
        require(journal['phase'] in (('prepared',) if not recover else ('applying', 'needs_review', 'committed', 'restoring', 'stopped')), 'journal 状态不允许此操作')
    with locks(root, watch_ids(plan)):
        if recover and journal.get('trash_package'):
            try:
                folder = published_trash_package(work, plan, journal, binary)
                if folder:
                    return restore_trash_package(work, plan, journal, binary, folder, injected)
            except BaseException as error:
                journal['error'] = str(error)
                save_journal(work, journal, 'needs_review')
                raise
        _, _, rows = load(work, all_conversations)
        if recover:
            check_backups(work, plan)
            activity(root, plan['real'], plan['socket'], watch_ids(plan), watch_assets(plan))
            save_journal(work, journal, 'restoring')
            try:
                restore(work, plan, rows, journal, binary)
                if journal.get('trash_package'):
                    folder = safe(work, package_relative(plan))
                    if folder.exists():
                        shutil.rmtree(folder)
                        sync_dir(work)
                save_journal(work, journal, 'rolled_back')
                return {'phase': 'rolled_back', 'ids': plan['ids']}
            except BaseException as error:
                journal['error'] = str(error)
                save_journal(work, journal, 'needs_review')
                raise
        probe = preflight(work, plan, rows)
        checks = []
        if dry_run:
            for name, groups in rows.items():
                if groups:
                    checks.append(mutate_db(safe(root, name), name, plan['ids'], groups, dry_run=True))
            save_journal(work, journal, step={'type': 'dry_run', 'checks': checks, 'activity': probe, 'at': time.time()})
            return {'dry_run': True, 'checks': checks}
        require(any(s['type'] == 'dry_run' for s in journal['steps']), '必须先完成 dry-run')
        save_journal(work, journal, 'applying', {'type': 'begin', 'activity': probe, 'at': time.time()})
        try:
            packaged = plan.get('operation', 'trash') == 'trash' and 'trash_package' in plan
            if packaged:
                stage_trash_package(work, plan, journal)
            # 全会话模式逐项提交，每十项计为一批；只保存一份共享库快照，停止/中断后不自动续删。
            groups = [[tid] for tid in plan['ids']] if plan.get('scope') == 'all_conversations' else [plan['ids']]
            completed = []
            for index, selected in enumerate(groups):
                if (work / 'stop-requested').exists():
                    save_journal(work, journal, 'stopped')
                    return {'phase': 'stopped', 'completed_ids': completed}
                journal['current_batch'], journal['current_item'] = index // 10 + 1, index + 1
                save_journal(work, journal)
                check_family(root, plan, completed)
                activity(root, plan['real'], plan['socket'], watch_ids(plan), watch_assets(plan))
                for i, asset in enumerate(plan['assets']):
                    if asset['id'] not in selected:
                        continue
                    source = safe(root, asset['relative'])
                    require(fingerprint(source) == {k: asset[k] for k in ('bytes', 'sha256', 'device', 'inode', 'mtime_ns')}, '操作前文件发生变化')
                    permanent = plan.get('operation') == 'permanent_delete'
                    save_journal(work, journal, step={'type': 'delete_intent' if permanent else 'trash_intent', 'relative': asset['relative'], 'inode': asset['inode']})
                    if permanent or packaged:
                        # 新回收格式已将目标数据耐久写入暂存包；成功发布后才清理临时恢复备份。
                        source.unlink()
                        fault(plan, ('delete-' if permanent else 'trash-') + str(i) + '-before-receipt', injected)
                    else:
                        receipt = bridge(binary, 'move', plan, asset)
                        fault(plan, 'trash-' + str(i) + '-before-receipt', injected)
                        journal['receipts'][asset['relative']] = receipt
                        require(fingerprint(pathlib.Path(receipt['trash_path']))['sha256'] == asset['sha256'], '废纸篓文件校验失败')
                    sync_dir(source.parent)
                    save_journal(work, journal, step={'type': 'deleted' if permanent else 'trashed', 'relative': asset['relative']})
                    fault(plan, ('delete-' if permanent else 'trash-') + str(i), injected)
                for name, text in plan['texts'].items():
                    original = (work / 'backup' / name).read_bytes()
                    before, _ = filter_text(original, text['key'], completed)
                    after, _ = filter_text(original, text['key'], completed + selected)
                    save_journal(work, journal, step={'type': 'text_intent', 'name': name})
                    with text_lock(root, name) as text_handle:
                        require(regular(safe(root, name)).read_bytes() == before, '文本在执行中变化，请保留恢复材料并复核')
                        write_text(root / name, after, text_handle, name == 'history.jsonl', plan, name, injected)
                    save_journal(work, journal, step={'type': 'text_done', 'name': name})
                    fault(plan, name, injected)
                for name, all_rows in rows.items():
                    selected_rows = select_rows(all_rows, selected)
                    if not selected_rows:
                        continue
                    save_journal(work, journal, step={'type': 'db_intent', 'name': name})
                    check = mutate_db(safe(root, name), name, selected, selected_rows)
                    checks.append(check)
                    save_journal(work, journal, step={'type': 'db_done', **check})
                    fault(plan, name, injected)
                completed += selected
                result = verify(work, all_conversations, completed)
                journal['completed_ids'] = completed[:]
                save_journal(work, journal, step={'type': 'item_completed', 'ids': selected})
                fault(plan, 'item-' + str(index), injected)
            if packaged:
                publish_trash_package(work, plan, journal, binary, injected)
            save_journal(work, journal, 'committed', {'type': 'verified', **result})
            if plan.get('operation') == 'permanent_delete' or packaged:
                # 成功回收的恢复数据只存在废纸篓包内；永久删除则不留下恢复正文。
                fault(plan, 'before-purge', injected)
                purge_temporary_recovery(work, journal)
                fault(plan, 'after-purge', injected)
            return {'phase': 'committed', 'ids': plan['ids'], 'completed_ids': completed, 'checks': checks,
                    'receipts': journal['receipts'], 'trash_package': journal.get('trash_package'), 'verification': result}
        except BaseException as error:
            journal['error'] = str(error)
            save_journal(work, journal, 'needs_review')
            raise


def restore(work, plan, rows, journal, binary):
    root = pathlib.Path(plan['root'])
    for name, groups in rows.items():
        if groups:
            check = mutate_db(safe(root, name), name, plan['ids'], groups, restore=True)
            save_journal(work, journal, step={'type': 'db_restored', **check})
    for name, text in plan['texts'].items():
        before = (work / 'backup' / name).read_bytes()
        versions = [filter_text(before, text['key'], plan['ids'][:n])[0] for n in range(len(plan['ids']) + 1)]
        with text_lock(root, name) as text_handle:
            current = regular(safe(root, name)).read_bytes()
            if current.startswith(before):
                continue
            matches = [(index, data) for index, data in enumerate(versions[1:], 1) if current.startswith(data)]
            if matches:
                index, after = max(matches, key=lambda item: len(item[1]))
                restored = before + current[len(after):]
                # 历史文件写入后、truncate 前中断的确切过渡版本；不能把旧尾部当作新输入追加。
                if name == 'history.jsonl' and any(current == after + previous[len(after):] for previous in versions[:index]):
                    restored = before
            else:
                partial = False
                for previous, after in zip(versions, versions[1:]):
                    prefix = next((i for i, (a, b) in enumerate(zip(current, after)) if a != b), min(len(current), len(after)))
                    partial |= len(current) == len(previous) and current[prefix:] == previous[prefix:]
                require(name == 'history.jsonl' and partial and
                        any(s.get('type') == 'text_intent' and s.get('name') == name for s in journal['steps']),
                        '共享文本已被其他操作改写，停止自动恢复: ' + name)
                restored = before
            write_text(root / name, restored, text_handle, name == 'history.jsonl', plan, name, None)
        save_journal(work, journal, step={'type': 'text_restored', 'name': name})
    for asset in plan['assets']:
        target = safe(root, asset['relative'])
        if target.exists():
            require(fingerprint(target)['sha256'] == asset['sha256'], '恢复文件冲突: ' + asset['relative'])
            continue
        receipt = journal['receipts'].get(asset['relative'])
        if not receipt and plan.get('operation', 'trash') == 'trash':
            # 原生移动成功、回执写盘前进程消失：仅凭同卷 inode 找回本次目标，绝不匹配名称猜测。
            candidate = pathlib.Path.home() / '.Trash' / pathlib.Path(asset['relative']).name
            def identical(p):
                return not p.is_symlink() and p.is_file() and (p.stat().st_dev, p.stat().st_ino) == (asset['device'], asset['inode'])
            matches = [candidate] if identical(candidate) else []
            if not matches:
                try:
                    matches = [p for p in (pathlib.Path.home() / '.Trash').iterdir() if identical(p)]
                except PermissionError:
                    matches = []
            require(len(matches) <= 1, '回收站 inode 证据有歧义')
            if matches:
                receipt = {'operation_id': plan['operation_id'], 'relative_path': asset['relative'], 'trash_path': str(matches[0])}
        if receipt and pathlib.Path(receipt['trash_path']).exists():
            require(fingerprint(pathlib.Path(receipt['trash_path']))['sha256'] == asset['sha256'], '废纸篓恢复源不匹配')
            bridge(binary, 'restore', plan, receipt=receipt)
        else:
            durable_copy(regular(safe(work / 'backup', asset['relative'])), target)
            save_journal(work, journal, step={'type': 'file_restored_from_backup', 'relative': asset['relative'],
                                             'note': '源资产已恢复；回执丢失时废纸篓中可能另有副本，不能猜测清理'})
        require(fingerprint(target)['sha256'] == asset['sha256'], '恢复文件校验失败')
        sync_dir(target.parent)
        save_journal(work, journal, step={'type': 'file_restored', 'relative': asset['relative']})
    for name in SCHEMAS:
        with contextlib.closing(connect(safe(root, name))) as conn:
            require(rows_snapshot(conn, plan['ids']) == rows[name], '恢复结果不完整: ' + name)
            require(conn.execute('PRAGMA integrity_check').fetchall() == [('ok',)], '恢复后完整性失败: ' + name)
            require(not conn.execute('PRAGMA foreign_key_check').fetchall(), '恢复后外键失败: ' + name)


def verify(work, all_conversations=False, completed_ids=None):
    work, plan = load_plan(work, all_conversations)
    ids = completed_ids if completed_ids is not None else plan['ids']
    require(ids and set(ids) <= set(plan['ids']), '验证目标超出计划')
    root = pathlib.Path(plan['root'])
    result = {'databases': {}, 'texts': {}, 'codex_bytes_removed': sum(a['bytes'] for a in plan['assets']),
              'disk_bytes_freed': 0, 'non_target_unchanged_since_prepare': True}
    for asset in plan['assets']:
        if asset['id'] in ids:
            require(not safe(root, asset['relative']).exists(), '目标文件仍在源目录')
    for name, text in plan['texts'].items():
        current = regular(safe(root, name)).read_bytes()
        _, removed = filter_text(current, text['key'], ids)
        require(not removed, '共享索引仍包含目标')
        backup = work / 'backup' / name
        expected, removed_lines = filter_text(backup.read_bytes(), text['key'], ids) if backup.exists() else (None, text['removed'])
        if expected is not None:
            require(current.startswith(expected), '非目标共享文本的既有前缀发生变化: ' + name)
        else:
            require(ids == plan['ids'] and 'after_bytes' in text and sha(current[:text['after_bytes']]) == text['after_sha256'], '永久删除后的索引前缀无法核验')
        result['texts'][name] = {'non_target_prefix_kept': True, 'target_lines_removed': len(removed_lines),
                                'source_bytes_removed': sum(line['end'] - line['start'] for line in removed_lines)}
    for name in SCHEMAS:
        with contextlib.closing(connect(safe(root, name))) as conn:
            conn.execute('BEGIN')
            require(schema_hash(conn) == SCHEMAS[name], '验证时 schema 发生变化')
            require(not rows_snapshot(conn, ids), '数据库仍有目标行: ' + name)
            require(conn.execute('PRAGMA integrity_check').fetchall() == [('ok',)], '完整性失败: ' + name)
            require(not conn.execute('PRAGMA foreign_key_check').fetchall(), '外键失败: ' + name)
            other = digest_others(conn, plan['ids'])
            unchanged = other == plan['non_target_baselines'][name]
            result['databases'][name] = {'integrity': 'ok', 'foreign_keys': 'ok', 'non_target': other,
                                         'unchanged_since_prepare': unchanged}
            result['non_target_unchanged_since_prepare'] &= unchanged
    return result


def make_copy(work, destination, all_conversations=False):
    work, plan, _ = load(work, all_conversations)
    check_backups(work, plan)
    destination = pathlib.Path(destination).absolute()
    require(not destination.exists() and '.codex' not in destination.parts, '副本目录不安全或已存在')
    shutil.copytree(work / 'backup', destination)
    atomic(destination / 'authorized-live-copy.json', encoded({'kind': KIND, 'source_root': plan['source_root'], 'ids': plan['ids']}))
    return {'copy': str(destination)}



# 中文说明：云备份复用已审阅的归属与活动门禁，仅读取目标行；绝不复制共享整库或执行源写入。
def backup_snapshot(root, requested, socket_path):
    root, real = validate_scope(root, requested, True)
    with contextlib.closing(connect(safe(root, 'state_5.sqlite'))) as conn:
        require(schema_hash(conn) == SCHEMAS['state_5.sqlite'], '未知 schema: state_5.sqlite')
        family = family_scope(root, conn, requested)
    ids = family['ids']
    probe_writer_locks(root, family['watch_ids'])
    databases = {}
    for name, expected in SCHEMAS.items():
        with contextlib.closing(connect(safe(root, name))) as conn:
            conn.execute('BEGIN')
            require(schema_hash(conn) == expected, '未知 schema: ' + name)
            check_relations(conn, name, ids)
            databases[name] = {'schema_sha256': expected, 'rows': rows_snapshot(conn, ids),
                               'schema': conn.execute("SELECT type,name,tbl_name,sql FROM sqlite_master WHERE sql IS NOT NULL ORDER BY type,name").fetchall()}
    group = next((g for g in databases['state_5.sqlite']['rows'] if g['table'] == 'threads'), None)
    require(group and len(group['values']) == len(ids), '目标线程缺失')
    source_root = root if real else pathlib.Path(json.loads((root / 'authorized-live-copy.json').read_bytes())['source_root'])
    assets, metadata = scan_files(root, source_root, ids, [{'columns': group['columns'], 'values': r} for r in group['values']], family)
    indexes = {}
    for name, key in TEXT_FILES.items():
        # 流式跳过非目标行，避免把整份全局 history/index 载入内存。
        selected, count = bytearray(), 0
        with regular(safe(root, name)).open('rb') as handle:
            for line in handle:
                require(line.endswith(b'\n'), '共享 JSONL 尾行不完整')
                record = json.loads(line)
                if record.get(key) in ids:
                    selected.extend(line)
                    count += 1
                    require(len(selected) <= 64 * 1024 * 1024, '目标索引快照超过本轮预算')
        indexes[name] = {'owner': key, 'content': base64.b64encode(selected).decode(), 'sha256': sha(selected), 'lines': count}
    auxiliary = check_auxiliary(root, ids)
    activity(root, real, socket_path, family['watch_ids'], assets + family['watch_assets'])
    probe_writer_locks(root, family['watch_ids'])
    return {'kind': 'chronicle-cloud-snapshot-v1', 'ids': ids, 'requested_ids': requested,
            'family': family, 'assets': assets, 'metadata': metadata, 'databases': databases,
            'indexes': indexes, 'auxiliary': auxiliary}


def backup_preview(snapshot):
    return {'digest': sha(encoded(snapshot)), 'ids': snapshot['ids'], 'requested_ids': snapshot['requested_ids'],
            'assets': snapshot['assets'], 'family': snapshot['family'],
            'source_bytes': sum(a['bytes'] for a in snapshot['assets']),
            'database_rows': [{'database': name, 'table': g['table'], 'rows': len(g['values'])}
                              for name, db in snapshot['databases'].items() for g in db['rows']],
            'index_lines': {name: value['lines'] for name, value in snapshot['indexes'].items()},
            'source_read_only': True}


def restore_version_digest(ids, family, assets, databases, indexes, auxiliary):
    # 中文说明：版本身份忽略文件 inode 和修改时间，只比较目标会话内容及其专属数据库/索引记录。
    value = {
        'ids': ids,
        'family': family,
        'assets': sorted(({key: asset[key] for key in ('relative', 'id', 'kind', 'bytes', 'sha256')}
                          for asset in assets), key=lambda item: item['relative']),
        'databases': {name: {'schema_sha256': db['schema_sha256'], 'rows': db['rows']}
                      for name, db in sorted(databases.items())},
        'indexes': {name: {'owner': text['owner'], 'sha256': text['sha256'], 'lines': text['lines']}
                    for name, text in sorted(indexes.items())},
        'auxiliary': auxiliary,
    }
    return sha(encoded(value))


def merge_text_records(data, key, ids, replacement):
    selected = set(ids)
    output, inserted = [], False
    for line in data.splitlines(keepends=True):
        require(line.endswith(b'\n'), '共享 JSONL 尾行不完整')
        record = json.loads(line)
        if record.get(key) in selected:
            if not inserted:
                output.extend(replacement)
                inserted = True
        else:
            output.append(line)
    if not inserted:
        output.extend(replacement)
    return b''.join(output)


def replace_db_rows(path, name, ids, expected_rows, replacement_rows):
    with contextlib.closing(connect(path, True)) as conn:
        conn.execute('BEGIN IMMEDIATE')
        try:
            require(schema_hash(conn) == SCHEMAS[name], '更新事务内 schema 不匹配: ' + name)
            check_relations(conn, name, ids)
            before = digest_others(conn, ids)
            current = rows_snapshot(conn, ids)
            require(current == expected_rows or current == replacement_rows,
                    '更新事务内目标行与预览或归档不一致: ' + name)
            if current != replacement_rows:
                delete_priority = {'threads': 100, 'thread_goals': 100,
                                   'thread_history_projection_state': 90}
                for group in sorted(current, key=lambda item: delete_priority.get(item['table'], 0)):
                    marks = ','.join('?' for _ in ids)
                    conn.execute('DELETE FROM ' + quote(group['table']) + ' WHERE ' + quote(group['owner']) +
                                 ' IN (' + marks + ')', ids)
                insert_priority = {'threads': 100, 'thread_goals': 100,
                                   'thread_history_projection_state': 90}
                for group in sorted(replacement_rows,
                                    key=lambda item: -insert_priority.get(item['table'], 0)):
                    columns = group['columns']
                    for row in group['values']:
                        conn.execute('INSERT INTO ' + quote(group['table']) + ' (' +
                                     ','.join(map(quote, columns)) + ') VALUES (' +
                                     ','.join('?' for _ in columns) + ')',
                                     [uncell(value) for value in row])
            require(rows_snapshot(conn, ids) == replacement_rows, '更新后数据库记录与归档不一致: ' + name)
            require(digest_others(conn, ids) == before, '更新触发器或外键影响了非目标记录: ' + name)
            require(not conn.execute('PRAGMA foreign_key_check').fetchall(), '更新数据库外键检查失败: ' + name)
            conn.execute('COMMIT')
        except BaseException:
            conn.rollback()
            raise


def replace_target_text(root, work, name, key, ids, replacement, expected, journal, injected=None):
    path = safe(root, name)
    with text_lock(root, name) as handle:
        current = regular(path).read_bytes()
        _, records = filter_text(current, key, ids)
        selected = b''.join(current[item['start']:item['end']] for item in records)
        require(selected == expected or selected == replacement, '更新索引与预览或归档不一致: ' + name)
        if selected != replacement:
            result = merge_text_records(current, key, ids, replacement.splitlines(keepends=True))
            save_journal(work, journal, step={'type': 'update_text_intent', 'name': name})
            write_text(path, result, handle, name == 'history.jsonl',
                       {'real': False}, 'update-' + name, injected)
            save_journal(work, journal, step={'type': 'update_text_done', 'name': name})


def update_restore_backup(root, exported, work, snapshot, current_state, journal):
    # 中文说明：先持久保存旧版本的目标文件、专属行和索引记录，任何替换前均可回滚。
    backup = safe(work, 'update-before')
    require(not backup.exists(), '发现未登记的更新临时备份，停止并人工复核')
    journal['phase'] = 'update_backup_in_progress'
    save_journal(work, journal)
    backup.mkdir(mode=0o700, parents=True, exist_ok=True)
    assets = []
    required_bytes = sum(asset['bytes'] for asset in snapshot['assets'])
    require(shutil.disk_usage(backup).free >= required_bytes + 16 * 1024 * 1024,
            '可用空间不足，无法为更新保留可恢复材料')
    for asset in snapshot['assets']:
        source = safe(root, asset['relative'])
        saved = safe(backup / 'files', asset['relative'])
        before = fingerprint(source)
        durable_copy(source, saved)
        require(fingerprint(saved)['sha256'] == before['sha256'],
                '更新前文件在备份期间变化: ' + asset['relative'])
        assets.append({'relative': asset['relative'], 'id': asset['id'], 'kind': asset['kind'],
                       'mode': source.stat().st_mode & 0o777,
                       'sha256': before['sha256'], 'bytes': before['bytes']})
    databases = {}
    for name in SCHEMAS:
        with contextlib.closing(connect(safe(root, name))) as conn:
            require(schema_hash(conn) == SCHEMAS[name], '更新前 schema 变化: ' + name)
            databases[name] = {'schema_sha256': SCHEMAS[name], 'rows': rows_snapshot(conn, snapshot['ids'])}
    indexes = {}
    for name, key in TEXT_FILES.items():
        data = regular(safe(root, name)).read_bytes()
        _, records = filter_text(data, key, snapshot['ids'])
        selected = b''.join(data[item['start']:item['end']] for item in records)
        indexes[name] = {'owner': key, 'content': base64.b64encode(selected).decode(),
                         'sha256': sha(selected), 'lines': len(records)}
    captured = {'ids': snapshot['ids'], 'family': snapshot.get('family', {}), 'assets': assets,
                'databases': databases, 'indexes': indexes, 'auxiliary': current_state['auxiliary']}
    captured['version_sha256'] = restore_version_digest(
        captured['ids'], captured['family'], captured['assets'], captured['databases'],
        captured['indexes'], captured['auxiliary'])
    require(captured['version_sha256'] == current_state['version_sha256'],
            '更新前目标与预览摘要不一致，请重新查看影响')
    journal['update_before'] = captured
    journal['phase'] = 'update_prepared'
    save_journal(work, journal)
    return captured


def restore_update_before(root, exported, work, journal):
    before = journal.get('update_before')
    require(before and before.get('ids'), '更新 journal 缺少旧版本恢复材料')
    ids = before['ids']
    # 文件替换采用同目录临时文件和 os.replace，恢复可重复执行。
    for asset in before['assets']:
        target = safe(root, asset['relative'])
        saved = safe(work / 'update-before' / 'files', asset['relative'])
        require(fingerprint(saved)['sha256'] == asset['sha256'], '更新回滚文件损坏: ' + asset['relative'])
        if target.exists() and fingerprint(target)['sha256'] == asset['sha256']:
            continue
        temporary = target.with_name('.chronicle-update-restore-' + journal['restore_id'] + '.tmp')
        if temporary.exists():
            temporary.unlink()
        durable_copy(saved, temporary)
        os.chmod(temporary, asset['mode'])
        os.replace(temporary, target)
        sync_dir(target.parent)
    archived = load_archive_snapshot(root, exported)[3]
    for name in sorted(SCHEMAS, key=lambda item: 0 if item == 'state_5.sqlite' else 1):
        expected = archived['databases'][name]['rows']
        replacement = before['databases'][name]['rows']
        replace_db_rows(safe(root, name), name, ids, expected, replacement)
    for name, key in TEXT_FILES.items():
        replacement = base64.b64decode(before['indexes'][name]['content'], validate=True)
        expected = base64.b64decode(archived['indexes'][name]['content'], validate=True)
        path = safe(root, name)
        with text_lock(root, name) as handle:
            current = regular(path).read_bytes()
            _, records = filter_text(current, key, ids)
            selected = b''.join(current[item['start']:item['end']] for item in records)
            require(selected == expected or selected == replacement,
                    '更新回滚时索引已出现第三种状态: ' + name)
            if selected != replacement:
                result = merge_text_records(current, key, ids, replacement.splitlines(keepends=True))
                write_text(path, result, handle, name == 'history.jsonl', {'real': False}, 'rollback-' + name, None)
    journal['phase'] = 'update_rolled_back'
    save_journal(work, journal)


def apply_archive_update(root, exported, work, socket_path, snapshot, preview, injected=None):
    ids = snapshot['ids']
    if injected:
        require(pathlib.Path(root).resolve() != (pathlib.Path.home() / '.codex').resolve(), '真实目录禁止故障注入')
    # 独立回滚材料不放入归档，也不写入 Codex 根目录。
    journal_path = safe(work, 'journal.json')
    if journal_path.exists():
        journal = json.loads(regular(journal_path).read_bytes())
        require(journal.get('kind') == 'chronicle-archive-update-v1' and
                journal.get('snapshot_sha256') == sha(regular(exported / 'snapshot.json').read_bytes()) and
                journal.get('root') == str(pathlib.Path(root)), '更新恢复 journal 身份不匹配')
        if journal.get('phase') not in ('completed', 'update_rolled_back'):
            restore_update_before(root, exported, work, journal)
        if journal.get('phase') == 'completed':
            return {**preview, 'status': 'updated', 'installed_version_sha256': preview['archive_version_sha256']}
    else:
        journal = {'kind': 'chronicle-archive-update-v1', 'root': str(pathlib.Path(root)),
                   'snapshot_sha256': sha(regular(exported / 'snapshot.json').read_bytes()),
                   'restore_id': uuid.uuid4().hex, 'phase': 'prepared', 'steps': []}
    current_state = preview['_current_state']
    target_databases = {name: db['rows'] for name, db in snapshot['databases'].items()}
    if journal.get('phase') == 'update_rolled_back' and journal.get('update_before'):
        before = journal['update_before']
    else:
        before = update_restore_backup(root, exported, work, snapshot, current_state, journal)
    journal['update_after'] = {'databases': snapshot['databases']}
    journal['phase'] = 'updating'
    save_journal(work, journal)
    try:
        for asset in snapshot['assets']:
            target = safe(root, asset['relative'])
            source = safe(exported / 'files', asset['relative'])
            old = next(item for item in before['assets'] if item['relative'] == asset['relative'])
            require(fingerprint(target)['sha256'] in (old['sha256'], asset['sha256']),
                    '更新文件出现第三种状态: ' + asset['relative'])
            staged = target.with_name('.chronicle-update-' + journal['restore_id'] + '.tmp')
            if staged.exists():
                require(fingerprint(staged)['sha256'] == asset['sha256'], '更新暂存文件摘要不符')
            else:
                durable_copy(source, staged)
            require(fingerprint(staged)['sha256'] == asset['sha256'], '更新暂存文件校验失败')
            os.chmod(staged, old['mode'])
            save_journal(work, journal, step={'type': 'update_file_intent', 'relative': asset['relative']})
            os.replace(staged, target)
            sync_dir(target.parent)
            require(fingerprint(target)['sha256'] == asset['sha256'], '更新文件发布校验失败: ' + asset['relative'])
            save_journal(work, journal, step={'type': 'update_file_done', 'relative': asset['relative']})
            fault({'real': False}, 'update-file', injected)
        for name in sorted(SCHEMAS, key=lambda item: 0 if item == 'state_5.sqlite' else 1):
            old = before['databases'][name]['rows']
            new = target_databases[name]
            if old or new:
                replace_db_rows(safe(root, name), name, ids, old, new)
            save_journal(work, journal, step={'type': 'update_db_done', 'name': name})
            fault({'real': False}, 'update-db-' + name, injected)
        for name, key in TEXT_FILES.items():
            text = snapshot['indexes'][name]
            replacement = base64.b64decode(text['content'], validate=True)
            old = base64.b64decode(before['indexes'][name]['content'], validate=True)
            replace_target_text(root, work, name, key, ids, replacement, old, journal, injected)
        result = archive_restore_preview(root, exported, socket_path, locked=True, update_if_newer=False)
        require(result['status'] == 'already_present', '更新后归档一致性校验失败')
        journal['phase'] = 'completed'
        journal.pop('update_before', None)
        journal.pop('update_after', None)
        save_journal(work, journal, step={'type': 'update_verified', 'version_sha256': result['archive_version_sha256']})
        shutil.rmtree(safe(work, 'update-before'))
        sync_dir(work)
        return {**result, 'status': 'updated', 'installed_version_sha256': result['archive_version_sha256']}
    except BaseException as error:
        try:
            restore_update_before(root, exported, work, journal)
        except BaseException as rollback_error:
            journal['phase'] = 'needs_review'
            journal['error'] = str(error)
            journal['rollback_error'] = str(rollback_error)
            save_journal(work, journal)
            raise RuntimeError('更新中断且自动回滚未完成，保留恢复材料并需人工复核: ' + str(rollback_error)) from error
        raise


def backup_export(root, ids, work, socket_path, expected):
    require(expected, '备份必须绑定已确认的资产预览')
    snapshot = backup_snapshot(root, ids, socket_path)
    require(sha(encoded(snapshot)) == expected, '预览后目标内容或关联已变化，请重新检查')
    root, _ = validate_scope(root, ids, True)
    work = pathlib.Path(work).absolute()
    require(root != work and root not in work.parents and work not in root.parents, '备份缓存不能与源重叠')
    safe(work, '.')
    work.mkdir(parents=True, mode=0o700, exist_ok=False)
    try:
        for asset in snapshot['assets']:
            dest = work / 'files' / asset['relative']
            durable_copy(safe(root, asset['relative']), dest)
            require(fingerprint(dest)['sha256'] == asset['sha256'], '备份源在复制时发生变化')
        current = backup_snapshot(root, ids, socket_path)
        require(sha(encoded(current)) == expected, '导出期间目标发生变化，未上传')
        atomic(work / 'snapshot.json', encoded(snapshot))
        return backup_preview(snapshot)
    except BaseException:
        shutil.rmtree(work)
        raise

# 中文说明：完整归档只按专属行恢复；重复副本识别为已存在，变更或来源不明的部分状态拒绝覆盖。
def load_archive_snapshot(root, exported):
    exported = safe(pathlib.Path(exported).absolute(), '.')
    raw = regular(exported / 'snapshot.json').read_bytes()
    require(len(raw) <= 128 * 1024 * 1024, '归档快照超出预算')
    snapshot = json.loads(raw)
    require(snapshot['kind'] == 'chronicle-cloud-snapshot-v1', '不支持的归档快照')
    root, real = validate_scope(root, snapshot['ids'], True)
    require(root != exported and root not in exported.parents and exported not in root.parents, '归档恢复材料与目标重叠')
    rows = {name: db['rows'] for name, db in snapshot['databases'].items()}
    source = str(root) if real else json.loads((root / 'authorized-live-copy.json').read_bytes())['source_root']
    plan = {'ids': snapshot['ids'], 'assets': snapshot['assets'], 'source_root': source}
    validate_rows(plan, rows)
    require(set(snapshot['indexes']) == set(TEXT_FILES), '归档索引不完整')
    for name, text in snapshot['indexes'].items():
        data = base64.b64decode(text['content'], validate=True)
        require(text['owner'] == TEXT_FILES[name] and sha(data) == text['sha256'], '索引归属或摘要错误')
        require(all(line.endswith(b'\n') and json.loads(line).get(TEXT_FILES[name]) in snapshot['ids']
                    for line in data.splitlines(keepends=True)), '归档索引含非目标记录')
    for asset in snapshot['assets']:
        f = fingerprint(safe(exported / 'files', asset['relative']))
        require(f['sha256'] == asset['sha256'] and f['bytes'] == asset['bytes'], '归档文件校验失败')
    return root, real, exported, snapshot, sha(raw)


def archive_restore_preview(root, exported, socket_path, journal=None, locked=False,
                            update_if_newer=False, expected_current_version_digest=None):
    root, real, exported, snapshot, digest = load_archive_snapshot(root, exported)
    ids = snapshot['ids']
    watching = snapshot.get('family', {}).get('watch_ids', ids)
    if not locked: probe_writer_locks(root, watching)
    activity(root, real, socket_path, watching, snapshot['assets'] + snapshot.get('family', {}).get('watch_assets', []))
    if journal and journal.get('kind') == 'chronicle-archive-update-v1' and \
            journal.get('phase') not in ('completed', 'update_rolled_back'):
        check_auxiliary(root, ids)
        check_history_dependencies(root, ids)
        state_digest = sha(encoded(journal))
        return {'status': 'recovery_required', 'ids': ids, 'snapshot_sha256': digest,
                'conflicts': [], 'files': [{'relative': a['relative'], 'bytes': a['bytes']}
                                           for a in snapshot['assets']],
                'database_rows': backup_preview(snapshot)['database_rows'],
                'index_lines': {name: text['lines'] for name, text in snapshot['indexes'].items()},
                'recovery_reason': '上次更新没有完成；先回滚到更新前版本，再重新检查。',
                'digest': sha(encoded({'snapshot': digest, 'journal': state_digest,
                                       'phase': journal.get('phase')}))}
    check_auxiliary(root, ids)
    check_history_dependencies(root, ids)
    statuses, state, conflicts = [], {}, []
    current_assets, current_databases, current_indexes = [], {}, {}
    owned_recovery = bool(journal and journal.get('snapshot_sha256') == digest and journal.get('root') == str(root))
    for asset in snapshot['assets']:
        path = safe(root, asset['relative'])
        status = 'missing'
        if path.exists():
            current = fingerprint(path)
            status = 'present' if current['sha256'] == asset['sha256'] else 'conflict'
            current_assets.append({'relative': asset['relative'], 'id': asset['id'], 'kind': asset['kind'],
                                   'bytes': current['bytes'], 'sha256': current['sha256']})
        else:
            current_assets.append({'relative': asset['relative'], 'id': asset['id'], 'kind': asset['kind'],
                                   'bytes': -1, 'sha256': ''})
        state['file:' + asset['relative']] = status
        statuses.append(status)
        if status == 'conflict': conflicts.append(asset['relative'])
    for name, db in snapshot['databases'].items():
        with contextlib.closing(connect(safe(root, name))) as conn:
            conn.execute('BEGIN')
            require(db['schema_sha256'] == schema_hash(conn) == SCHEMAS[name], '恢复 schema 不匹配: ' + name)
            check_relations(conn, name, ids)
            current = rows_snapshot(conn, ids)
            current_databases[name] = {'schema_sha256': schema_hash(conn), 'rows': current}
            for group in db['rows']:
                columns = conn.execute('PRAGMA table_info(' + quote(group['table']) + ')').fetchall()
                require(group['columns'] == [c[1] for c in columns] and
                        group['pk'] == [c[1] for c in sorted(columns, key=lambda c: c[5]) if c[5]], '恢复表定义不匹配')
            if current == db['rows']:
                status = 'present'
            elif not current:
                status = 'missing'
            else:
                expected = {g['table']: g for g in db['rows']}
                subset = all(g['table'] in expected and g['columns'] == expected[g['table']]['columns'] and
                             all(row in expected[g['table']]['values'] for row in g['values']) for g in current)
                status = 'partial' if subset and owned_recovery else 'conflict'
            state['db:' + name] = sha(encoded(current))
            if db['rows'] or current: statuses.append(status)
            if status == 'conflict': conflicts.append(name)
    for name, text in snapshot['indexes'].items():
        current = regular(safe(root, name)).read_bytes()
        checkpoint = (journal or {}).get('restore_text_checkpoints', {}).get(name)
        _, records = filter_text(current, TEXT_FILES[name], ids)
        if checkpoint and owned_recovery:
            status = 'partial'
            selected = b''
        else:
            selected = b''.join(current[r['start']:r['end']] for r in records)
            expected = base64.b64decode(text['content'], validate=True)
            status = 'present' if selected == expected else 'missing' if not selected else 'conflict'
        state['text:' + name] = sha(selected)
        current_indexes[name] = {'owner': TEXT_FILES[name], 'content': base64.b64encode(selected).decode(),
                                 'sha256': sha(selected), 'lines': len(records)}
        if text['lines'] or selected: statuses.append(status)
        if status == 'conflict': conflicts.append(name)
    auxiliary = check_auxiliary(root, ids)
    family = snapshot.get('family', {})
    current_family = family
    if family:
        actual_threads = set()
        for group in current_databases['state_5.sqlite']['rows']:
            if group['table'] == 'threads':
                owner = group['columns'].index(group['owner'])
                actual_threads.update(uncell(row[owner]) for row in group['values'])
        if not actual_threads.intersection(ids):
            current_family = {'target_threads_missing': sorted(ids)}
        else:
            with contextlib.closing(connect(safe(root, 'state_5.sqlite'))) as conn:
                current_family = family_scope(root, conn, snapshot.get('requested_ids', ids))
        if current_family != family:
            if actual_threads.intersection(ids):
                conflicts.append('spawn 关系')
            state['family'] = sha(encoded(current_family))
        else:
            state['family'] = sha(encoded(current_family))
    current_version = restore_version_digest(ids, current_family, current_assets, current_databases,
                                            current_indexes, auxiliary)
    archive_version = restore_version_digest(ids, family, snapshot['assets'], snapshot['databases'],
                                            snapshot['indexes'], snapshot.get('auxiliary', {}))
    status = ('conflict' if conflicts else 'already_present' if all(s == 'present' for s in statuses)
              else 'ready' if all(s == 'missing' for s in statuses) or owned_recovery else 'partial')
    update_reason = ''
    if update_if_newer and status == 'conflict':
        if not expected_current_version_digest:
            update_reason = '没有可验证的上次恢复版本记录'
        elif current_version != expected_current_version_digest:
            update_reason = '当前对话在上次恢复后又有变化，不能覆盖'
        else:
            status = 'update_ready'
    result = {'status': status, 'ids': ids, 'snapshot_sha256': digest, 'conflicts': conflicts,
            'files': [{'relative': a['relative'], 'bytes': a['bytes']} for a in snapshot['assets']],
            'database_rows': backup_preview(snapshot)['database_rows'],
            'index_lines': {name: t['lines'] for name, t in snapshot['indexes'].items()},
            'archive_version_sha256': archive_version, 'current_version_sha256': current_version,
            'update_reason': update_reason,
            'digest': sha(encoded({'snapshot': digest, 'state': state, 'status': status,
                                   'update_if_newer': update_if_newer,
                                   'expected_current_version_digest': expected_current_version_digest}))}
    result['_current_state'] = {'version_sha256': current_version, 'auxiliary': auxiliary}
    return result


def archive_restore_apply(root, exported, work, socket_path, expected, injected=None,
                          update_if_newer=False, expected_current_version_digest=None,
                          recover_update=False):
    root, real, exported, snapshot, digest = load_archive_snapshot(root, exported)
    require(not injected or not real, '真实恢复禁止故障注入')
    work = safe(pathlib.Path(work).absolute(), '.')
    require(root != work and root not in work.parents and work not in root.parents, '恢复日志必须独立于源')
    work.mkdir(mode=0o700, parents=True, exist_ok=True)
    journal_path = work / 'journal.json'
    journal = json.loads(regular(journal_path).read_bytes()) if journal_path.exists() else None
    if journal:
        require(journal.get('snapshot_sha256') == digest and journal.get('root') == str(root), '恢复日志身份不匹配')
        if journal.get('kind') == 'chronicle-archive-update-v1' and journal.get('phase') == 'completed':
            leftover = safe(work, 'update-before')
            if leftover.exists():
                shutil.rmtree(leftover)
    watching = snapshot.get('family', {}).get('watch_ids', snapshot['ids'])
    with locks(root, watching):
        if journal and journal.get('kind') == 'chronicle-archive-update-v1':
            phase = journal.get('phase')
            if recover_update:
                require(phase not in ('completed', 'update_rolled_back'), '没有待恢复的更新 journal')
                preview = archive_restore_preview(root, exported, socket_path, journal, locked=True)
                require(preview['status'] == 'recovery_required' and preview['digest'] == expected,
                        '更新恢复预览已变化，请重新检查')
                if phase == 'update_backup_in_progress' and not journal.get('update_before'):
                    incomplete = safe(work, 'update-before')
                    if incomplete.exists():
                        shutil.rmtree(incomplete)
                    journal['phase'] = 'update_rolled_back'
                    save_journal(work, journal)
                else:
                    restore_update_before(root, exported, work, journal)
                for asset in snapshot['assets']:
                    target = safe(root, asset['relative'])
                    staged = target.with_name('.chronicle-update-' + journal['restore_id'] + '.tmp')
                    if staged.exists():
                        require(fingerprint(staged)['sha256'] == asset['sha256'], '更新暂存文件身份不符，保留待复核')
                        staged.unlink()
                return {'status': 'update_rolled_back', 'ids': snapshot['ids'],
                        'installed_version_sha256': journal.get('update_before', {}).get('version_sha256', '')}
            if phase not in ('completed', 'update_rolled_back'):
                raise RuntimeError('上次更新尚未完成，请先确认“回滚未完成更新”')
        preview = archive_restore_preview(root, exported, socket_path, journal, locked=True,
                                          update_if_newer=update_if_newer,
                                          expected_current_version_digest=expected_current_version_digest)
        require(preview['digest'] == expected, '恢复预览后目标变化，请重新检查')
        if preview['status'] == 'already_present':
            return {**preview, 'installed_version_sha256': preview['archive_version_sha256']}
        if update_if_newer:
            require(preview['status'] == 'update_ready', '更新被阻断: ' +
                    (preview.get('update_reason') or preview['status']))
            return apply_archive_update(root, exported, work, socket_path, snapshot, preview, injected)
        require(preview['status'] == 'ready', '恢复被阻断: ' + preview['status'])
        rows = {name: db['rows'] for name, db in snapshot['databases'].items()}
        checks = [mutate_db(safe(root, name), name, snapshot['ids'], groups, restore=True, dry_run=True)
                  for name, groups in rows.items() if groups]
        if journal is None:
            journal = {'kind': 'chronicle-archive-restore-v1', 'root': str(root), 'snapshot_sha256': digest,
                       'restore_id': uuid.uuid4().hex, 'phase': 'prepared', 'steps': [], 'dry_run': checks}
            save_journal(work, journal)
        plan = {'root': str(root), 'ids': snapshot['ids'], 'real': real}
        save_journal(work, journal, 'restoring')
        try:
            # 文件先耐久写到目标卷的私有临时名，再用硬链接原子发布，避免跨卷和覆盖目标。
            for asset in snapshot['assets']:
                path = safe(root, asset['relative'])
                if not path.exists():
                    token = journal.get('restore_id')
                    require(isinstance(token, str) and re.fullmatch(r'[0-9a-f]{32}', token), '恢复日志缺少临时文件归属标识')
                    path.parent.mkdir(parents=True, mode=0o700, exist_ok=True)
                    stage = safe(path.parent, '.chronicle-restore-' + token + '-' + sha(asset['relative'].encode())[:16] + '.tmp')
                    if stage.exists(): require(fingerprint(stage)['sha256'] == asset['sha256'], '中断文件校验失败')
                    else: durable_copy(safe(exported / 'files', asset['relative']), stage)
                    save_journal(work, journal, step={'type': 'file_intent', 'relative': asset['relative']})
                    # O_EXCL 原子发布完整文件，随后移除本次恢复专属的暂存名；不使用符号链接。
                    os.link(stage, path)
                    sync_dir(path.parent)
                    stage.unlink()
                require(fingerprint(path)['sha256'] == asset['sha256'], '恢复文件冲突')
                save_journal(work, journal, step={'type': 'file_restored', 'relative': asset['relative']})
                fault(plan, 'archive-file', injected)
            for name in sorted(rows, key=lambda n: 0 if n == 'state_5.sqlite' else 1):
                if rows[name]:
                    result = mutate_db(safe(root, name), name, snapshot['ids'], rows[name], restore=True)
                    save_journal(work, journal, step={'type': 'db_restored', **result})
                    fault(plan, 'archive-db-' + name, injected)
            for name, text in snapshot['indexes'].items():
                current = regular(safe(root, name)).read_bytes()
                expected_text = base64.b64decode(text['content'], validate=True)
                _, records = filter_text(current, TEXT_FILES[name], snapshot['ids'])
                selected = b''.join(current[r['start']:r['end']] for r in records)
                require(not selected or selected == expected_text, '恢复索引冲突')
                combined = current if selected == expected_text else current + expected_text
                text_plan = {'before_bytes': len(combined), 'before_sha256': sha(combined),
                             'after_bytes': len(current), 'after_sha256': sha(current),
                             'records': [] if selected == expected_text else [{'offset': len(current), 'data': text['content']}]}
                restore_package_text(work, plan, journal, name, text_plan, injected=injected)
            result = archive_restore_preview(root, exported, socket_path, locked=True)
            require(result['status'] == 'already_present', '恢复后校验未通过')
            for name in SCHEMAS:
                with contextlib.closing(connect(safe(root, name))) as conn:
                    require(conn.execute('PRAGMA integrity_check').fetchall() == [('ok',)], '恢复后完整性失败')
                    require(not conn.execute('PRAGMA foreign_key_check').fetchall(), '恢复后外键失败')
            journal.pop('restore_text_checkpoints', None)
            save_journal(work, journal, 'completed')
            if (work / 'restore-texts').exists(): shutil.rmtree(work / 'restore-texts')
            return {**result, 'status': 'restored',
                    'installed_version_sha256': result['archive_version_sha256']}
        except BaseException as error:
            journal['error'] = str(error)
            save_journal(work, journal, 'needs_review')
            raise


def main():
    parser = argparse.ArgumentParser(description='已审阅 schema 的闲置会话执行器；全会话与永久删除须显式启用')
    parser.add_argument('action', choices=('prepare', 'copy', 'check', 'dry-run', 'apply', 'recover', 'verify', 'backup-preview', 'backup-export', 'restore-preview', 'restore-apply'))
    parser.add_argument('--root', default=str(pathlib.Path.home() / '.codex'))
    parser.add_argument('--id', action='append', dest='ids')
    parser.add_argument('--work', required=True)
    parser.add_argument('--socket')
    parser.add_argument('--bridge')
    parser.add_argument('--destination')
    parser.add_argument('--expected-digest')
    parser.add_argument('--fault', help='仅副本允许的中断注入点')
    parser.add_argument('--update-if-newer', action='store_true', help='在外部版本门禁通过后更新已知旧版本')
    parser.add_argument('--expected-current-version-digest')
    parser.add_argument('--recover-update', action='store_true', help='回滚一项中断的更新并恢复更新前版本')
    parser.add_argument('--all-conversations', action='store_true', help='允许明确选择的任意 UUID 目标；不绕过活动/关联/schema 门禁')
    parser.add_argument('--operation', choices=('trash', 'permanent_delete'), default='trash')
    args = parser.parse_args()
    if args.action == 'restore-preview':
        journal_path = pathlib.Path(args.work) / 'journal.json'
        journal = json.loads(regular(journal_path).read_bytes()) if journal_path.exists() else None
        result = archive_restore_preview(args.root, args.destination, args.socket, journal,
                                         update_if_newer=args.update_if_newer,
                                         expected_current_version_digest=args.expected_current_version_digest)
    elif args.action == 'restore-apply':
        result = archive_restore_apply(args.root, args.destination, args.work, args.socket, args.expected_digest,
                                       args.fault, args.update_if_newer,
                                       args.expected_current_version_digest, args.recover_update)
    elif args.action == 'backup-preview':
        result = backup_preview(backup_snapshot(args.root, args.ids or [], args.socket))
    elif args.action == 'backup-export':
        result = backup_export(args.root, args.ids or [], args.work, args.socket, args.expected_digest)
    elif args.action == 'prepare':
        require(not args.all_conversations or args.ids, '全会话模式必须明确选择 ID，不能默认删除全部')
        result = prepare(args.root, args.ids or sorted(ALLOWED_IDS), args.work, args.socket, args.all_conversations, args.operation)
    elif args.action == 'copy':
        require(args.destination, '必须指定副本目标')
        result = make_copy(args.work, args.destination, args.all_conversations)
    elif args.action in ('apply', 'recover', 'dry-run'):
        result = execute(args.work, args.bridge, args.action == 'dry-run', args.action == 'recover', args.fault, args.all_conversations)
    elif args.action == 'verify':
        result = verify(args.work, args.all_conversations)
    else:
        work, plan, rows = load(args.work, args.all_conversations)
        result = preflight(work, plan, rows)
    result.pop('_current_state', None)
    print(json.dumps(result, ensure_ascii=False, indent=2))


if __name__ == '__main__':
    try:
        main()
    except Exception as error:
        print(json.dumps({'error': str(error)}, ensure_ascii=False), file=sys.stderr)
        sys.exit(1)
