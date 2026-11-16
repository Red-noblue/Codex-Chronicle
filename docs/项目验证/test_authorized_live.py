# 中文说明：用独立合成副本验证授权门禁、非目标事务隔离、锁争用和中断恢复；测试不读取本机 .codex。
import base64
import contextlib
import fcntl
import hashlib
import json
import os
import pathlib
import shutil
import socket
import sqlite3
import tempfile
import unittest
import uuid
from unittest.mock import Mock, patch

import authorized_live as live


class DaemonCompatibilityTests(unittest.TestCase):
    def handshake(self, agent, wrong_home=False):
        # 中文说明：隔离 socket 验证真实构造函数中的版本与 HOME 门禁，不连接用户 daemon。
        with tempfile.TemporaryDirectory(prefix='chronicle-daemon-') as temporary:
            root = pathlib.Path(temporary)
            address = str(root / 'daemon.sock')
            with socket.socket(socket.AF_UNIX) as listener:
                listener.bind(address)
                random = b'x' * 16
                key = base64.b64encode(random).decode()
                accept = base64.b64encode(hashlib.sha1((key + '258EAFA5-E914-47DA-95CA-C5AB0DC85B11').encode()).digest())
                header = b'HTTP/1.1 101 Switching Protocols\r\nSec-WebSocket-Accept: ' + accept + b'\r\n\r\n'
                info = {'codexHome': str(root / 'other' if wrong_home else root), 'userAgent': agent}
                with patch.object(live.socket, 'socket', return_value=Mock()), \
                        patch.object(live.os, 'urandom', return_value=random), \
                        patch.object(live.Daemon, 'read', side_effect=[bytes([b]) for b in header]), \
                        patch.object(live.Daemon, 'rpc', return_value=info), \
                        patch.object(live.Daemon, 'send') as send:
                    daemon = live.Daemon(address, root)
                    send.assert_called_once_with({'method': 'initialized', 'params': {}})
                    daemon.close()

    def test_reviewed_exact_versions_accept_full_user_agent(self):
        for version in ('0.166.0', '0.167.0', '0.167.2', '0.169.0'):
            with self.subTest(version=version):
                self.handshake('codex-tui/' + version + ' (Mac OS 26.5.1; arm64) vscode/1.99.3 (chronicle_authorized_audit; 1)')

    def test_unaudited_patch_minor_and_prerelease_remain_blocked(self):
        for version in ('0.167.1', '0.167.3', '0.168.0', '0.169.1', '0.169.0-preview', '0.170.0', ''):
            with self.subTest(version=version), self.assertRaisesRegex(RuntimeError, '版本未经'):
                self.handshake('codex-cli/' + version)

    def test_reviewed_version_does_not_allow_a_different_home(self):
        with self.assertRaisesRegex(RuntimeError, '不是目标 HOME'):
            self.handshake('codex-tui/0.167.2', wrong_home=True)


class AuthorizedLiveTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix='chronicle-authorized-test-')
        self.addCleanup(self.temp.cleanup)
        self.base = pathlib.Path(self.temp.name).resolve()
        home = patch.object(live.pathlib.Path, 'home', return_value=self.base / 'home')
        home.start()
        self.addCleanup(home.stop)
        self.root = self.base / 'source'
        self.root.mkdir()
        self.target = sorted(live.ALLOWED_IDS)[1]
        self.other = 'protected-other-thread'
        self.ids = [self.target]
        live.atomic(self.root / 'authorized-live-copy.json', live.encoded({
            'kind': live.KIND, 'source_root': str(pathlib.Path.home() / '.codex'), 'ids': self.ids}))
        schemas = {
            'goals_1.sqlite': 'CREATE TABLE thread_goals(thread_id TEXT PRIMARY KEY,objective TEXT);',
            'logs_2.sqlite': 'CREATE TABLE logs(id INTEGER PRIMARY KEY,thread_id TEXT,body TEXT);',
            'memories_1.sqlite': 'CREATE TABLE stage1_outputs(thread_id TEXT PRIMARY KEY); CREATE TABLE jobs(kind TEXT,job_key TEXT PRIMARY KEY,status TEXT);',
            'queue_1.sqlite': 'CREATE TABLE queued_items(id TEXT PRIMARY KEY,thread_id TEXT); CREATE TABLE queued_thread_revisions(revision INTEGER PRIMARY KEY,thread_id TEXT);',
            'state_5.sqlite': '''CREATE TABLE threads(id TEXT PRIMARY KEY,rollout_path TEXT,source TEXT,
                attached_thread_parent_id TEXT,title TEXT);
                CREATE TABLE thread_spawn_edges(parent_thread_id TEXT REFERENCES threads(id) ON DELETE CASCADE,
                    child_thread_id TEXT PRIMARY KEY REFERENCES threads(id) ON DELETE CASCADE);
                CREATE TABLE thread_attachments(id TEXT PRIMARY KEY,thread_id TEXT REFERENCES threads(id) ON DELETE CASCADE);''',
            'thread_history_1.sqlite': 'CREATE TABLE thread_items(thread_id TEXT,turn_id TEXT,item_id TEXT,item_json TEXT,PRIMARY KEY(thread_id,turn_id,item_id));',
        }
        fingerprints = {}
        for name, schema in schemas.items():
            with contextlib.closing(sqlite3.connect(self.root / name)) as conn, conn:
                conn.execute('PRAGMA journal_mode=WAL')
                conn.executescript(schema)
                fingerprints[name] = live.schema_hash(conn)
        self.schema_patch = patch.object(live, 'SCHEMAS', fingerprints)
        self.schema_patch.start()
        self.addCleanup(self.schema_patch.stop)
        (self.root / 'sessions').mkdir()
        (self.root / 'tui-thread-reference-capabilities').mkdir()
        for tid in (self.target, self.other):
            rel = 'sessions/rollout-' + tid + '.jsonl'
            (self.root / rel).write_bytes(live.encoded({'type': 'session_meta', 'payload': {'id': tid}}) + b'\n')
            with contextlib.closing(sqlite3.connect(self.root / 'state_5.sqlite')) as conn, conn:
                conn.execute('INSERT INTO threads VALUES (?,?,?,NULL,?)', (tid, str(pathlib.Path.home() / '.codex' / rel), 'cli', tid))
            with contextlib.closing(sqlite3.connect(self.root / 'logs_2.sqlite')) as conn, conn:
                conn.execute('INSERT INTO logs(thread_id,body) VALUES (?,?)', (tid, 'log ' + tid))
            with contextlib.closing(sqlite3.connect(self.root / 'thread_history_1.sqlite')) as conn, conn:
                conn.execute('INSERT INTO thread_items VALUES (?,?,?,?)', (tid, 'turn', 'item', 'message ' + tid))
        (self.root / 'tui-thread-reference-capabilities' / self.target).touch()
        for name, key in live.TEXT_FILES.items():
            (self.root / name).write_bytes(b''.join(live.encoded({key: tid, 'text': tid}) + b'\n' for tid in (self.target, self.other)))
        self.trash = self.base / 'trash'
        self.trash.mkdir()
        self.bridge_binary = self.base / 'bridge-marker'
        self.bridge_binary.touch()
        self.bridge_patch = patch.object(live, 'bridge', self.fake_bridge)
        self.bridge_patch.start()
        self.addCleanup(self.bridge_patch.stop)
        self.work = self.base / 'operation'
        live.prepare(self.root, self.ids, self.work, None)

    def fake_bridge(self, binary, action, plan, asset=None, receipt=None):
        if action == 'locate':
            return {'trash_path': str(self.trash)}
        if action == 'move':
            destination = self.trash / pathlib.Path(asset['relative']).name
            os.rename(pathlib.Path(plan['root']) / asset['relative'], destination)
            return {'operation_id': plan['operation_id'], 'relative_path': asset['relative'], 'trash_path': str(destination)}
        os.rename(receipt['trash_path'], pathlib.Path(plan['root']) / receipt['relative_path'])
        return {'restored': True}

    def baseline(self):
        result = {}
        for name in live.SCHEMAS:
            with contextlib.closing(live.connect(self.root / name)) as conn:
                result[name] = live.digest_others(conn, self.ids)
        return result

    def crash(self, point):
        pid = os.fork()
        if pid == 0:
            try:
                live.execute(self.work, self.bridge_binary, injected=point)
            except BaseException:
                os._exit(92)
            os._exit(0)
        _, status = os.waitpid(pid, 0)
        self.assertEqual(os.waitstatus_to_exitcode(status), 91)

    def test_delete_restore_and_preserve_later_input(self):
        baseline = self.baseline()
        original = {n: (self.root / n).read_bytes() for n in live.TEXT_FILES}
        live.execute(self.work, dry_run=True)
        result = live.execute(self.work, self.bridge_binary)
        self.assertEqual(result['phase'], 'committed')
        self.assertTrue(result['verification']['non_target_unchanged_since_prepare'])
        appended = live.encoded({'session_id': self.other, 'text': 'new input'}) + b'\n'
        with (self.root / 'history.jsonl').open('ab') as f:
            f.write(appended)
        live.execute(self.work, self.bridge_binary, recover=True)
        self.assertEqual(self.baseline(), baseline)
        self.assertEqual((self.root / 'history.jsonl').read_bytes(), original['history.jsonl'] + appended)
        self.assertEqual((self.root / 'session_index.jsonl').read_bytes(), original['session_index.jsonl'])

    def test_crash_after_database_commit_is_recoverable(self):
        before = self.baseline()
        live.execute(self.work, dry_run=True)
        self.crash('logs_2.sqlite')
        live.execute(self.work, self.bridge_binary, recover=True)
        work, plan, rows = live.load(self.work)
        for asset in plan['assets']:
            self.assertEqual(live.fingerprint(self.root / asset['relative'])['sha256'], asset['sha256'])
        for name in live.SCHEMAS:
            with contextlib.closing(live.connect(self.root / name)) as conn:
                self.assertEqual(live.rows_snapshot(conn, self.ids), rows[name])
        self.assertEqual(self.baseline(), before)

    def test_crash_during_history_rewrite_is_recoverable(self):
        before = (self.root / 'history.jsonl').read_bytes()
        live.execute(self.work, dry_run=True)
        self.crash('history.jsonl-before-truncate')
        live.execute(self.work, self.bridge_binary, recover=True)
        self.assertEqual((self.root / 'history.jsonl').read_bytes(), before)

    def test_missing_receipt_can_restore_from_durable_backup(self):
        live.execute(self.work, dry_run=True)
        self.crash('trash-0-before-receipt')
        with patch.object(pathlib.Path, 'iterdir', side_effect=PermissionError('模拟 macOS 目录隐私控制')):
            live.execute(self.work, self.bridge_binary, recover=True)
        journal = json.loads((self.work / 'journal.json').read_bytes())
        self.assertTrue(any(s['type'] == 'file_restored_from_backup' for s in journal['steps']))
        self.assertEqual(journal['phase'], 'rolled_back')

    def test_abandoned_atomic_temporary_does_not_block_recovery(self):
        live.execute(self.work, dry_run=True)
        self.crash('trash-0')
        stale = self.work / '.chronicle-authorized-journal.json-abandoned.tmp'
        stale.write_bytes(b'{"incomplete":')
        live.execute(self.work, self.bridge_binary, recover=True)
        self.assertEqual(json.loads((self.work / 'journal.json').read_bytes())['phase'], 'rolled_back')
        self.assertTrue(stale.exists())  # 恢复不盲删用途未确认的旧临时文件。

    def test_unauthorized_id_and_asset_are_rejected(self):
        with self.assertRaisesRegex(RuntimeError, '未授权'):
            live.validate_scope(self.root, [self.other])
        plan = json.loads((self.work / 'plan.json').read_bytes())
        plan['assets'][0]['relative'] = 'auth.json'
        live.atomic(self.work / 'plan.json', live.encoded(plan))
        live.atomic(self.work / 'plan.sha256', live.sha((self.work / 'plan.json').read_bytes()).encode())
        with self.assertRaisesRegex(RuntimeError, '越权'):
            live.load(self.work)

    def test_active_writer_lock_blocks_before_any_move(self):
        (self.root / 'thread-writer-locks').mkdir(exist_ok=True)
        p = self.root / 'thread-writer-locks' / (self.target + '.lock')
        with p.open('a+b') as holder:
            fcntl.flock(holder, fcntl.LOCK_EX | fcntl.LOCK_NB)
            with self.assertRaises(BlockingIOError):
                live.execute(self.work, dry_run=True)
        self.assertTrue((self.root / 'sessions' / ('rollout-' + self.target + '.jsonl')).exists())

    def test_loaded_idle_target_is_blocked(self):
        plan = json.loads((self.work / 'plan.json').read_bytes())
        plan['socket'] = 'mock-daemon'
        live.atomic(self.work / 'plan.json', live.encoded(plan))
        live.atomic(self.work / 'plan.sha256', live.sha((self.work / 'plan.json').read_bytes()).encode())
        target = self.target

        class LoadedDaemon:
            def __init__(self, path, root):
                pass

            def loaded(self):
                return {target}

            def close(self):
                pass

        with patch.object(live, 'Daemon', LoadedDaemon):
            with self.assertRaisesRegex(RuntimeError, '仍被 daemon 加载'):
                live.execute(self.work, dry_run=True)

    def test_changed_rollout_blocks_stale_plan(self):
        p = self.root / 'sessions' / ('rollout-' + self.target + '.jsonl')
        with p.open('ab') as f:
            f.write(b'{"type":"event_msg","payload":{}}\n')
        with self.assertRaisesRegex(RuntimeError, '资产已变化'):
            live.execute(self.work, dry_run=True)

    def test_busy_database_rolls_back_without_changes(self):
        _, plan, rows = live.load(self.work)
        p = self.root / 'logs_2.sqlite'
        with contextlib.closing(live.connect(p, True)) as holder:
            holder.execute('BEGIN IMMEDIATE')
            with self.assertRaises(sqlite3.OperationalError):
                live.mutate_db(p, p.name, self.ids, rows[p.name])
            holder.rollback()
        live.preflight(self.work, plan, rows)

    def test_idle_target_delete_coexists_with_active_shared_readers_and_writer(self):
        live.execute(self.work, dry_run=True)
        lock_dir = self.root / 'thread-writer-locks'
        lock_dir.mkdir(exist_ok=True)
        other_lock_path = lock_dir / (self.other + '.lock')
        other_lock = other_lock_path.open('a+b')
        fcntl.flock(other_lock, fcntl.LOCK_EX | fcntl.LOCK_NB)

        readers, snapshots = {}, {}
        for name in live.SCHEMAS:
            reader = live.connect(self.root / name)
            reader.execute('BEGIN')
            snapshots[name] = {
                'target': live.rows_snapshot(reader, self.ids),
                'other': live.rows_snapshot(reader, [self.other]),
            }
            readers[name] = reader

        original_fault = live.fault

        def active_session_write(plan, point, requested):
            if point == 'logs_2.sqlite':
                with contextlib.closing(live.connect(self.root / 'logs_2.sqlite', True)) as writer:
                    writer.execute('BEGIN IMMEDIATE')
                    writer.execute('UPDATE logs SET body=? WHERE thread_id=?', ('updated by active session', self.other))
                    writer.commit()
            original_fault(plan, point, requested)

        with patch.object(live, 'fault', side_effect=active_session_write):
            result = live.execute(self.work, self.bridge_binary)

        self.assertEqual(result['phase'], 'committed')
        for name, reader in readers.items():
            self.assertEqual(live.rows_snapshot(reader, self.ids), snapshots[name]['target'])
            self.assertEqual(live.rows_snapshot(reader, [self.other]), snapshots[name]['other'])
        with other_lock_path.open('a+b') as contender:
            with self.assertRaises(BlockingIOError):
                fcntl.flock(contender, fcntl.LOCK_EX | fcntl.LOCK_NB)
        for reader in readers.values():
            reader.close()
        other_lock.close()

        for name in live.SCHEMAS:
            with contextlib.closing(live.connect(self.root / name)) as conn:
                self.assertEqual(live.rows_snapshot(conn, self.ids), [])
                if name == 'logs_2.sqlite':
                    self.assertEqual(conn.execute('SELECT body FROM logs WHERE thread_id=?', (self.other,)).fetchone(), ('updated by active session',))
                else:
                    self.assertEqual(live.rows_snapshot(conn, [self.other]), snapshots[name]['other'])

    def test_shared_database_writer_busy_restores_idle_target_and_preserves_writer_commit(self):
        live.execute(self.work, dry_run=True)
        history_before = {name: (self.root / name).read_bytes() for name in live.TEXT_FILES}
        database = self.root / 'logs_2.sqlite'
        holder = live.connect(database, True)
        holder.execute('BEGIN IMMEDIATE')
        holder.execute('UPDATE logs SET body=? WHERE thread_id=?', ('active writer commit', self.other))
        with self.assertRaises(sqlite3.OperationalError):
            live.execute(self.work, self.bridge_binary)
        self.assertEqual(json.loads((self.work / 'journal.json').read_bytes())['phase'], 'needs_review')
        holder.commit()
        holder.close()

        result = live.execute(self.work, self.bridge_binary, recover=True)
        self.assertEqual(result['phase'], 'rolled_back')
        self.assertTrue((self.root / 'sessions' / ('rollout-' + self.target + '.jsonl')).exists())
        for name, content in history_before.items():
            self.assertEqual((self.root / name).read_bytes(), content)
        with contextlib.closing(live.connect(database)) as conn:
            self.assertEqual(conn.execute('SELECT body FROM logs WHERE thread_id=?', (self.target,)).fetchone(), ('log ' + self.target,))
            self.assertEqual(conn.execute('SELECT body FROM logs WHERE thread_id=?', (self.other,)).fetchone(), ('active writer commit',))

    def test_unknown_schema_and_shared_child_block(self):
        _, plan, rows = live.load(self.work)
        p = self.root / 'state_5.sqlite'
        with contextlib.closing(sqlite3.connect(p)) as conn, conn:
            conn.execute('CREATE TABLE unknown(x)')
        with self.assertRaisesRegex(RuntimeError, 'schema'):
            live.preflight(self.work, plan, rows)
        with contextlib.closing(sqlite3.connect(p)) as conn, conn:
            conn.execute('DROP TABLE unknown')
            conn.execute('INSERT INTO thread_spawn_edges VALUES (?,?)', (self.target, self.other))
        with self.assertRaisesRegex(RuntimeError, 'spawn'):
            live.preflight(self.work, plan, rows)

    def test_trigger_cannot_modify_non_target_rows(self):
        _, _, rows = live.load(self.work)
        p = self.root / 'state_5.sqlite'
        with contextlib.closing(sqlite3.connect(p)) as conn, conn:
            conn.execute("CREATE TRIGGER bad AFTER DELETE ON threads BEGIN UPDATE threads SET title='changed'; END")
            # 即使某个 schema 被错误登记，独立的非目标摘要门禁仍会阻止副作用提交。
            live.SCHEMAS[p.name] = live.schema_hash(conn)
        with self.assertRaisesRegex(RuntimeError, '非目标'):
            live.mutate_db(p, p.name, self.ids, rows[p.name])
        with contextlib.closing(sqlite3.connect(p)) as conn, conn:
            self.assertEqual(conn.execute('SELECT title FROM threads WHERE id=?', (self.other,)).fetchone()[0], self.other)
            self.assertEqual(conn.execute('SELECT COUNT(*) FROM threads').fetchone()[0], 2)

    def test_corrupt_backup_blocks_without_moving_files(self):
        with (self.work / 'backup' / 'logs_2.sqlite').open('ab') as f:
            f.write(b'corrupt')
        with self.assertRaisesRegex(RuntimeError, '恢复材料损坏'):
            live.execute(self.work, dry_run=True)
        self.assertTrue((self.root / 'sessions' / ('rollout-' + self.target + '.jsonl')).exists())

    def general_plan(self, count=1, operation='trash'):
        ids = [str(uuid.uuid4()) for _ in range(count)]
        for tid in ids:
            rel = 'sessions/rollout-' + tid + '.jsonl'
            (self.root / rel).write_bytes(live.encoded({'type': 'session_meta', 'payload': {'id': tid}}) + b'\n')
            with contextlib.closing(sqlite3.connect(self.root / 'state_5.sqlite')) as conn, conn:
                conn.execute('INSERT INTO threads VALUES (?,?,?,NULL,?)', (tid, str(pathlib.Path.home() / '.codex' / rel), 'cli', tid))
            with contextlib.closing(sqlite3.connect(self.root / 'logs_2.sqlite')) as conn, conn:
                conn.execute('INSERT INTO logs(thread_id,body) VALUES (?,?)', (tid, 'private-body-' + tid))
            for name, key in live.TEXT_FILES.items():
                with (self.root / name).open('ab') as f:
                    f.write(live.encoded({key: tid, 'text': 'private-body-' + tid}) + b'\n')
        live.atomic(self.root / 'authorized-live-copy.json', live.encoded({
            'kind': live.KIND, 'source_root': str(pathlib.Path.home() / '.codex'), 'ids': self.ids + ids}))
        work = self.base / ('general-' + uuid.uuid4().hex)
        live.prepare(self.root, ids, work, None, all_conversations=True, operation=operation)
        return work, ids

    def test_general_scope_requires_explicit_mode_and_accepts_new_ids(self):
        work, ids = self.general_plan()
        self.assertFalse(set(ids) & live.ALLOWED_IDS)
        with self.assertRaisesRegex(RuntimeError, '全会话'):
            live.execute(work, dry_run=True)
        for invalid in ('../auth.json', 'not-a-conversation-id'):
            with self.assertRaisesRegex(RuntimeError, '无效'):
                live.validate_scope(self.root, [invalid], all_conversations=True)
        live.execute(work, dry_run=True, all_conversations=True)
        result = live.execute(work, self.bridge_binary, all_conversations=True)
        self.assertEqual(result['completed_ids'], ids)
        live.execute(work, self.bridge_binary, recover=True, all_conversations=True)
        self.assertTrue((self.root / 'sessions' / ('rollout-' + ids[0] + '.jsonl')).exists())

    def test_permanent_delete_purges_body_backups_and_preserves_non_targets(self):
        work, ids = self.general_plan(operation='permanent_delete')
        before = {name: live.fingerprint(self.root / name)['sha256'] for name in ('sessions/rollout-' + self.other + '.jsonl',)}
        live.execute(work, dry_run=True, all_conversations=True)
        with patch.object(live, 'bridge', side_effect=AssertionError('permanent delete must not use trash')):
            result = live.execute(work, all_conversations=True)
        self.assertEqual(result['phase'], 'committed')
        self.assertFalse((work / 'backup').exists())
        self.assertFalse((work / 'rows.json').exists())
        self.assertTrue(json.loads((work / 'journal.json').read_bytes())['recovery_purged'])
        self.assertEqual(live.verify(work, all_conversations=True)['databases']['state_5.sqlite']['integrity'], 'ok')
        for name, digest in before.items():
            self.assertEqual(live.fingerprint(self.root / name)['sha256'], digest)
        with self.assertRaisesRegex(RuntimeError, '不能恢复'):
            live.execute(work, recover=True, all_conversations=True)

    def general_crash(self, work, point):
        live.execute(work, dry_run=True, all_conversations=True)
        pid = os.fork()
        if pid == 0:
            try:
                live.execute(work, self.bridge_binary, injected=point, all_conversations=True)
            except BaseException:
                os._exit(92)
            os._exit(0)
        _, status = os.waitpid(pid, 0)
        self.assertEqual(os.waitstatus_to_exitcode(status), 91)

    def test_permanent_delete_crash_before_receipt_restores_from_backup(self):
        work, ids = self.general_plan(operation='permanent_delete')
        self.general_crash(work, 'delete-0-before-receipt')
        self.assertFalse((self.root / 'sessions' / ('rollout-' + ids[0] + '.jsonl')).exists())
        live.execute(work, recover=True, all_conversations=True)
        _, plan, rows = live.load(work, True)
        for asset in plan['assets']:
            self.assertEqual(live.fingerprint(self.root / asset['relative'])['sha256'], asset['sha256'])
        for name in live.SCHEMAS:
            with contextlib.closing(live.connect(self.root / name)) as conn:
                self.assertEqual(live.rows_snapshot(conn, ids), rows[name])

    def test_permanent_delete_crash_before_purge_keeps_recovery(self):
        work, ids = self.general_plan(operation='permanent_delete')
        self.general_crash(work, 'before-purge')
        self.assertTrue((work / 'backup').is_dir())
        live.execute(work, recover=True, all_conversations=True)
        self.assertTrue((self.root / 'sessions' / ('rollout-' + ids[0] + '.jsonl')).exists())

    def test_partial_batch_crash_restores_text_and_keeps_later_input(self):
        work, ids = self.general_plan(count=3)
        original = {name: (self.root / name).read_bytes() for name in live.TEXT_FILES}
        self.general_crash(work, 'item-0')
        appended = live.encoded({'session_id': self.other, 'text': 'new protected input'}) + b'\n'
        with (self.root / 'history.jsonl').open('ab') as f:
            f.write(appended)
        live.execute(work, self.bridge_binary, recover=True, all_conversations=True)
        self.assertEqual((self.root / 'history.jsonl').read_bytes(), original['history.jsonl'] + appended)
        self.assertEqual((self.root / 'session_index.jsonl').read_bytes(), original['session_index.jsonl'])
        _, _, rows = live.load(work, True)
        for name in live.SCHEMAS:
            with contextlib.closing(live.connect(self.root / name)) as conn:
                self.assertEqual(live.rows_snapshot(conn, ids), rows[name])

    def test_general_stop_preserves_pending_targets_and_recovery(self):
        work, ids = self.general_plan(count=3, operation='permanent_delete')
        live.execute(work, dry_run=True, all_conversations=True)
        def request_stop(plan, point, requested):
            if point == 'item-0':
                (work / 'stop-requested').write_text('stop')
        with patch.object(live, 'fault', request_stop):
            result = live.execute(work, all_conversations=True)
        self.assertEqual(result['phase'], 'stopped')
        self.assertEqual(result['completed_ids'], ids[:1])
        self.assertTrue((work / 'backup').is_dir())
        self.assertTrue((self.root / 'sessions' / ('rollout-' + ids[1] + '.jsonl')).exists())
        live.execute(work, recover=True, all_conversations=True)

    def test_general_twenty_one_targets_use_three_batches_and_one_trash_folder(self):
        work, ids = self.general_plan(count=21)
        live.execute(work, dry_run=True, all_conversations=True)
        result = live.execute(work, self.bridge_binary, all_conversations=True)
        journal = json.loads((work / 'journal.json').read_bytes())
        self.assertEqual(result['completed_ids'], ids)
        self.assertEqual(journal['current_batch'], 3)
        self.assertEqual(journal['current_item'], 21)
        self.assertEqual(len([step for step in journal['steps'] if step['type'] == 'item_completed']), 21)
        self.assertFalse((work / 'backup').exists())
        self.assertEqual(len(list(self.trash.iterdir())), 1)
        live.execute(work, self.bridge_binary, recover=True, all_conversations=True)

    def test_trash_folder_is_self_contained_without_shared_databases_or_other_bodies(self):
        work, ids = self.general_plan(count=2)
        before = self.baseline()
        live.execute(work, dry_run=True, all_conversations=True)
        live.execute(work, self.bridge_binary, all_conversations=True)
        journal = json.loads((work / 'journal.json').read_bytes())
        package = pathlib.Path(journal['trash_package']['receipt']['trash_path'])
        self.assertEqual(list(self.trash.iterdir()), [package])
        self.assertFalse((work / 'backup').exists())
        self.assertFalse((work / 'rows.json').exists())
        self.assertFalse(list(package.rglob('*.sqlite*')))
        for p in package.rglob('*'):
            if p.is_file():
                self.assertNotIn(self.other.encode(), p.read_bytes())
        _, plan = live.load_plan(work, True)
        rows, texts = live.validate_trash_package(package, plan, journal)
        for name, groups in rows.items():
            for group in groups:
                owner = group['columns'].index(group['owner'])
                self.assertTrue(all(row[owner] in ids for row in group['values']))
        for path in work.rglob('*'):
            if path.is_file():
                for tid in ids:
                    self.assertNotIn(('private-body-' + tid).encode(), path.read_bytes())
        for name in live.TEXT_FILES:
            self.assertTrue(all(json.loads(base64.b64decode(r['data']))[texts[name]['key']] in ids
                                for r in texts[name]['records']))
        live.execute(work, self.bridge_binary, recover=True, all_conversations=True)
        self.assertEqual(self.baseline(), before)

    def test_trash_package_publication_crashes_can_recover_without_runtime_backups(self):
        for point in ('package-before-publish', 'package-before-receipt', 'package-published', 'before-purge', 'after-purge'):
            with self.subTest(point=point):
                work, ids = self.general_plan()
                original = {name: (self.root / name).read_bytes() for name in live.TEXT_FILES}
                self.general_crash(work, point)
                live.execute(work, self.bridge_binary, recover=True, all_conversations=True)
                self.assertTrue((self.root / 'sessions' / ('rollout-' + ids[0] + '.jsonl')).exists())
                for name, content in original.items():
                    self.assertEqual((self.root / name).read_bytes(), content)

    def test_corrupt_trash_package_does_not_purge_temporary_backups(self):
        work, ids = self.general_plan()
        live.execute(work, dry_run=True, all_conversations=True)
        def corrupt_bridge(binary, action, plan, asset=None, receipt=None):
            result = self.fake_bridge(binary, action, plan, asset, receipt)
            if action == 'move':
                (pathlib.Path(result['trash_path']) / 'database-rows.json').write_bytes(b'{}')
            return result
        with patch.object(live, 'bridge', corrupt_bridge):
            with self.assertRaisesRegex(RuntimeError, '校验失败'):
                live.execute(work, self.bridge_binary, all_conversations=True)
        self.assertTrue((work / 'backup').is_dir())
        self.assertTrue((work / 'rows.json').is_file())
        self.assertEqual(json.loads((work / 'journal.json').read_bytes())['phase'], 'needs_review')

    def test_trash_restore_history_interruption_preserves_non_target_input(self):
        work, ids = self.general_plan(count=2)
        original = {name: (self.root / name).read_bytes() for name in live.TEXT_FILES}
        live.execute(work, dry_run=True, all_conversations=True)
        live.execute(work, self.bridge_binary, all_conversations=True)
        appended = live.encoded({'session_id': self.other, 'text': 'input after trash'}) + b'\n'
        with (self.root / 'history.jsonl').open('ab') as f:
            f.write(appended)
        pid = os.fork()
        if pid == 0:
            try:
                live.execute(work, self.bridge_binary, recover=True, all_conversations=True,
                             injected='restore-history.jsonl-before-truncate')
            except BaseException:
                os._exit(92)
            os._exit(0)
        _, status = os.waitpid(pid, 0)
        self.assertEqual(os.waitstatus_to_exitcode(status), 91)
        self.assertTrue((work / 'restore-texts').is_dir())
        live.execute(work, self.bridge_binary, recover=True, all_conversations=True)
        self.assertEqual((self.root / 'history.jsonl').read_bytes(), original['history.jsonl'] + appended)
        self.assertEqual((self.root / 'session_index.jsonl').read_bytes(), original['session_index.jsonl'])
        self.assertFalse((work / 'restore-texts').exists())
        self.assertEqual(list(self.trash.iterdir()), [])

    def spawn_plan(self, requested=(0,), edges=True, count=4):
        _, ids = self.general_plan(count=count)
        for index, child in enumerate(ids[1:], 1):
            parent = ids[1] if index == 3 else ids[0]
            source = {'subagent': {'thread_spawn': {'parent_thread_id': parent, 'agent_path': '/root/agent-' + str(index)}}}
            with contextlib.closing(sqlite3.connect(self.root / 'state_5.sqlite')) as conn, conn:
                conn.execute('UPDATE threads SET source=? WHERE id=?', (json.dumps(source), child))
                if edges:
                    conn.execute('INSERT INTO thread_spawn_edges VALUES (?,?)', (parent, child))
            meta = {'id': child, 'session_id': ids[0], 'source': source}
            (self.root / 'sessions' / ('rollout-' + child + '.jsonl')).write_bytes(live.encoded({'type': 'session_meta', 'payload': meta}) + b'\n')
        work = self.base / ('spawn-' + uuid.uuid4().hex)
        live.prepare(self.root, [ids[i] for i in requested], work, None, all_conversations=True)
        return work, ids

    def test_spawn_tree_delete_and_package_restore(self):
        work, ids = self.spawn_plan(requested=(0, 1))
        _, plan, rows = live.load(work, True)
        self.assertEqual(set(plan['ids']), set(ids))
        self.assertEqual(len(plan['ids']), 4)
        self.assertLess(plan['ids'].index(ids[3]), plan['ids'].index(ids[1]))
        self.assertEqual(plan['ids'][-1], ids[0])
        self.ids = plan['ids']
        baseline = self.baseline()
        live.execute(work, dry_run=True, all_conversations=True)
        result = live.execute(work, self.bridge_binary, all_conversations=True)
        self.assertEqual(result['completed_ids'], plan['ids'])
        self.assertEqual(self.baseline(), baseline)
        with contextlib.closing(live.connect(self.root / 'state_5.sqlite')) as conn:
            self.assertEqual(conn.execute('SELECT COUNT(*) FROM thread_spawn_edges').fetchone()[0], 0)
        live.execute(work, self.bridge_binary, recover=True, all_conversations=True)
        for name in live.SCHEMAS:
            with contextlib.closing(live.connect(self.root / name)) as conn:
                self.assertEqual(live.rows_snapshot(conn, plan['ids']), rows[name])
        self.assertEqual(self.baseline(), baseline)

    def test_single_subagent_preserves_parent_and_sibling(self):
        work, ids = self.spawn_plan(requested=(1,))
        _, plan, _ = live.load(work, True)
        self.assertEqual(set(plan['ids']), {ids[1], ids[3]})
        self.ids = plan['ids']
        baseline = self.baseline()
        parent_file = self.root / 'sessions' / ('rollout-' + ids[0] + '.jsonl')
        before = parent_file.read_bytes()
        live.execute(work, dry_run=True, all_conversations=True)
        live.execute(work, self.bridge_binary, all_conversations=True)
        self.assertEqual(self.baseline(), baseline)
        self.assertEqual(parent_file.read_bytes(), before)
        with contextlib.closing(live.connect(self.root / 'state_5.sqlite')) as conn:
            self.assertEqual(conn.execute('SELECT parent_thread_id,child_thread_id FROM thread_spawn_edges').fetchall(), [(ids[0], ids[2])])
        live.execute(work, self.bridge_binary, recover=True, all_conversations=True)
        self.assertEqual(self.baseline(), baseline)

    def test_missing_edge_uses_rollout_and_source(self):
        work, ids = self.spawn_plan(edges=False)
        self.assertEqual(set(live.load(work, True)[1]['ids']), set(ids))
        live.execute(work, dry_run=True, all_conversations=True)
        live.execute(work, self.bridge_binary, all_conversations=True)
        live.execute(work, self.bridge_binary, recover=True, all_conversations=True)

    def test_active_parent_and_descendant_block(self):
        work, ids = self.spawn_plan(requested=(1,))
        (self.root / 'thread-writer-locks').mkdir(exist_ok=True)
        lock = self.root / 'thread-writer-locks' / (ids[0] + '.lock')
        with lock.open('a+b') as holder:
            fcntl.flock(holder, fcntl.LOCK_EX | fcntl.LOCK_NB)
            with self.assertRaises(BlockingIOError):
                live.execute(work, dry_run=True, all_conversations=True)
        lock = self.root / 'thread-writer-locks' / (ids[3] + '.lock')
        with lock.open('a+b') as holder:
            fcntl.flock(holder, fcntl.LOCK_EX | fcntl.LOCK_NB)
            with self.assertRaises(BlockingIOError):
                live.execute(work, dry_run=True, all_conversations=True)

    def test_daemon_loaded_parent_blocks_single_subagent(self):
        work, ids = self.spawn_plan(requested=(1,))
        plan = json.loads((work / 'plan.json').read_bytes())
        plan['socket'] = 'mock-daemon'
        live.atomic(work / 'plan.json', live.encoded(plan))
        live.atomic(work / 'plan.sha256', live.sha((work / 'plan.json').read_bytes()).encode())
        class LoadedDaemon:
            def __init__(self, path, root):
                pass
            def loaded(self):
                return {ids[0]}
            def close(self):
                pass
        with patch.object(live, 'Daemon', LoadedDaemon):
            with self.assertRaisesRegex(RuntimeError, '关联父会话仍被 daemon 加载'):
                live.execute(work, dry_run=True, all_conversations=True)

    def test_delete_parent_preserves_independent_fork_and_memory(self):
        _, ids = self.spawn_plan(count=6)
        for tid, source, meta in (
                (ids[4], 'cli', {'id': ids[4], 'source': 'cli', 'forked_from_id': ids[0]}),
                (ids[5], json.dumps({'subagent': 'memory_consolidation'}), {'id': ids[5], 'source': {'subagent': 'memory_consolidation'}})):
            with contextlib.closing(sqlite3.connect(self.root / 'state_5.sqlite')) as conn, conn:
                conn.execute('DELETE FROM thread_spawn_edges WHERE child_thread_id=?', (tid,))
                conn.execute('UPDATE threads SET source=? WHERE id=?', (source, tid))
            (self.root / 'sessions' / ('rollout-' + tid + '.jsonl')).write_bytes(live.encoded({'type': 'session_meta', 'payload': meta}) + b'\n')
        work = self.base / 'fork-memory-preserved'
        live.prepare(self.root, [ids[0]], work, None, all_conversations=True)
        self.ids = live.load(work, True)[1]['ids']
        self.assertEqual(set(self.ids), set(ids[:4]))
        baseline = self.baseline()
        live.execute(work, dry_run=True, all_conversations=True)
        live.execute(work, self.bridge_binary, all_conversations=True)
        self.assertEqual(self.baseline(), baseline)
        for tid in ids[4:]:
            self.assertTrue((self.root / 'sessions' / ('rollout-' + tid + '.jsonl')).exists())

    def test_edge_only_child_with_root_session_identity(self):
        _, ids = self.spawn_plan()
        for tid in ids[1:]:
            with contextlib.closing(sqlite3.connect(self.root / 'state_5.sqlite')) as conn, conn:
                conn.execute('UPDATE threads SET source=? WHERE id=?', ('cli', tid))
            meta = {'id': tid, 'session_id': ids[0]}
            (self.root / 'sessions' / ('rollout-' + tid + '.jsonl')).write_bytes(live.encoded({'type': 'session_meta', 'payload': meta}) + b'\n')
        work = self.base / 'edge-only'
        live.prepare(self.root, [ids[0]], work, None, all_conversations=True)
        live.execute(work, dry_run=True, all_conversations=True)
        live.execute(work, self.bridge_binary, all_conversations=True)
        live.execute(work, self.bridge_binary, recover=True, all_conversations=True)

    def test_spawn_relation_change_invalidates_preview(self):
        work, ids = self.spawn_plan()
        with contextlib.closing(sqlite3.connect(self.root / 'state_5.sqlite')) as conn, conn:
            conn.execute('INSERT INTO thread_spawn_edges VALUES (?,?)', (ids[0], self.other))
        with self.assertRaisesRegex(RuntimeError, '预览后变化'):
            live.execute(work, dry_run=True, all_conversations=True)
        self.assertTrue((self.root / 'sessions' / ('rollout-' + ids[0] + '.jsonl')).exists())

    def test_family_preview_does_not_create_source_files_and_checks_existing_locks(self):
        work, ids = self.spawn_plan()
        before = {str(p.relative_to(self.root)): p.read_bytes() for p in self.root.rglob('*') if p.is_file()}
        live.prepare(self.root, [ids[0]], self.base / 'readonly-preview', None, all_conversations=True)
        after = {str(p.relative_to(self.root)): p.read_bytes() for p in self.root.rglob('*') if p.is_file()}
        self.assertEqual(before, after)
        (self.root / 'thread-writer-locks').mkdir(exist_ok=True)
        with (self.root / 'thread-writer-locks' / (ids[2] + '.lock')).open('a+b') as holder:
            fcntl.flock(holder, fcntl.LOCK_EX | fcntl.LOCK_NB)
            with self.assertRaises(BlockingIOError):
                live.prepare(self.root, [ids[0]], self.base / 'active-preview', None, all_conversations=True)
        self.assertFalse((self.base / 'active-preview').exists())

    def test_spawn_conflict_cycle_and_missing_child_block(self):
        work, ids = self.spawn_plan()
        with contextlib.closing(sqlite3.connect(self.root / 'state_5.sqlite')) as conn, conn:
            conn.execute('UPDATE thread_spawn_edges SET parent_thread_id=? WHERE child_thread_id=?', (ids[2], ids[1]))
        with contextlib.closing(live.connect(self.root / 'state_5.sqlite')) as conn:
            with self.assertRaisesRegex(RuntimeError, '冲突'):
                live.family_scope(self.root, conn, [ids[0]])
        with contextlib.closing(sqlite3.connect(self.root / 'state_5.sqlite')) as conn, conn:
            conn.execute('UPDATE thread_spawn_edges SET parent_thread_id=? WHERE child_thread_id=?', (ids[0], ids[1]))
            conn.execute('INSERT INTO thread_spawn_edges VALUES (?,?)', (ids[3], ids[0]))
        with contextlib.closing(live.connect(self.root / 'state_5.sqlite')) as conn:
            with self.assertRaisesRegex(RuntimeError, '环'):
                live.family_scope(self.root, conn, [ids[0]])
        with contextlib.closing(sqlite3.connect(self.root / 'state_5.sqlite')) as conn, conn:
            conn.execute('DELETE FROM thread_spawn_edges WHERE child_thread_id=?', (ids[0],))
            conn.execute('DELETE FROM threads WHERE id=?', (ids[3],))
        with contextlib.closing(live.connect(self.root / 'state_5.sqlite')) as conn:
            with self.assertRaisesRegex(RuntimeError, '缺少 threads'):
                live.family_scope(self.root, conn, [ids[0]])

    def test_orphan_subagent_and_fork_are_not_guessed_into_family(self):
        work, ids = self.spawn_plan(requested=(1,))
        with contextlib.closing(sqlite3.connect(self.root / 'state_5.sqlite')) as conn, conn:
            conn.execute('DELETE FROM threads WHERE id=?', (ids[0],))
            conn.execute('UPDATE threads SET source=? WHERE id=?', (json.dumps({'subagent': 'memory_consolidation'}), ids[2]))
        for tid, meta in ((ids[2], {'id': ids[2], 'source': {'subagent': 'memory_consolidation'}}),
                          (ids[0], {'id': ids[0], 'forked_from_id': ids[1], 'source': 'cli'})):
            (self.root / 'sessions' / ('rollout-' + tid + '.jsonl')).write_bytes(live.encoded({'type': 'session_meta', 'payload': meta}) + b'\n')
        with contextlib.closing(live.connect(self.root / 'state_5.sqlite')) as conn:
            family = live.family_scope(self.root, conn, [ids[1]])
        self.assertEqual(set(family['ids']), {ids[1], ids[3]})
        self.assertIn(ids[0], family['watch_ids'])

    def test_spawn_partial_failure_restores_edges_and_texts(self):
        work, ids = self.spawn_plan()
        _, plan, rows = live.load(work, True)
        original_texts = {name: (self.root / name).read_bytes() for name in live.TEXT_FILES}
        self.general_crash(work, 'item-1')
        live.execute(work, self.bridge_binary, recover=True, all_conversations=True)
        for name in live.SCHEMAS:
            with contextlib.closing(live.connect(self.root / name)) as conn:
                self.assertEqual(live.rows_snapshot(conn, plan['ids']), rows[name])
        for name, data in original_texts.items():
            self.assertEqual((self.root / name).read_bytes(), data)

    def test_spawn_actual_thread_limit_and_legacy_authorization(self):
        work, ids = self.spawn_plan(count=12)
        _, plan, _ = live.load(work, True)
        self.assertEqual(len(plan['ids']), 12)
        with self.assertRaisesRegex(RuntimeError, '未授权'):
            live.prepare(self.root, [ids[0]], self.base / 'legacy-spawn', None)
        with contextlib.closing(live.connect(self.root / 'state_5.sqlite')) as conn:
            family = live.family_scope(self.root, conn, [ids[0], ids[1]])
        self.assertEqual(len(family['ids']), 12)

    def test_family_limit_counts_automatically_included_threads(self):
        _, ids = self.general_plan()
        with contextlib.closing(sqlite3.connect(self.root / 'state_5.sqlite')) as conn, conn:
            for _ in range(100):
                child = str(uuid.uuid4())
                conn.execute('INSERT INTO threads VALUES (?,?,?,NULL,?)', (child, 'sessions/rollout-' + child + '.jsonl', 'cli', child))
                conn.execute('INSERT INTO thread_spawn_edges VALUES (?,?)', (ids[0], child))
        with contextlib.closing(live.connect(self.root / 'state_5.sqlite')) as conn:
            with self.assertRaisesRegex(RuntimeError, '超过 100'):
                live.family_scope(self.root, conn, ids)

    def test_history_prefix_dependency_blocks_preview_and_preflight(self):
        # 中文说明：这是隔离 fixture，不修改真实 Codex 数据。
        path = self.root / 'sessions' / ('rollout-' + self.other + '.jsonl')
        path.write_bytes(live.encoded({'type': 'session_meta', 'ordinal': 0, 'payload': {
            'id': self.other, 'history_mode': 'paginated',
            'history_base': {'thread_id': self.target, 'end_ordinal_exclusive': 0, 'end_byte_offset': 0}}}) + b'\n')
        with self.assertRaisesRegex(RuntimeError, 'history_base'):
            live.check_history_dependencies(self.root, [self.target])
        plan = json.loads((self.work / 'plan.json').read_bytes())
        with self.assertRaisesRegex(RuntimeError, 'history_base'):
            live.preflight(self.work, plan, live.load(self.work)[2])
        with self.assertRaisesRegex(RuntimeError, 'history_base'):
            live.check_history_dependencies(self.root, [self.other])

    def test_paginated_root_without_history_base_is_not_a_shared_dependency(self):
        path = self.root / 'sessions' / ('rollout-' + self.target + '.jsonl')
        path.write_bytes(live.encoded({'type': 'session_meta', 'ordinal': 0, 'payload': {
            'id': self.target, 'history_mode': 'paginated'}}) + b'\n')
        live.check_history_dependencies(self.root, [self.target])

    def test_legacy_trash_recovery_remains_compatible(self):
        plan = json.loads((self.work / 'plan.json').read_bytes())
        plan.pop('trash_package')
        live.atomic(self.work / 'plan.json', live.encoded(plan))
        live.atomic(self.work / 'plan.sha256', live.sha((self.work / 'plan.json').read_bytes()).encode())
        live.execute(self.work, dry_run=True)
        live.execute(self.work, self.bridge_binary)
        self.assertTrue((self.work / 'backup').exists())
        live.execute(self.work, self.bridge_binary, recover=True)
        live.preflight(self.work, plan, live.load(self.work)[2])


    def test_cloud_export_contains_only_target_rows_and_index_lines(self):
        before = self.baseline()
        snapshot = live.backup_snapshot(self.root, self.ids, None)
        preview = live.backup_preview(snapshot)
        destination = self.base / 'cloud-export'
        result = live.backup_export(self.root, self.ids, destination, None, preview['digest'])
        self.assertEqual(result['digest'], preview['digest'])
        self.assertEqual(before, self.baseline())
        for name in live.SCHEMAS:
            self.assertFalse((destination / name).exists(), '不允许整库导出')
        saved = json.loads((destination / 'snapshot.json').read_bytes())
        for db in saved['databases'].values():
            for group in db['rows']:
                owner = group['columns'].index(group['owner'])
                self.assertTrue(all(row[owner] in self.ids for row in group['values']))
        for value in saved['indexes'].values():
            self.assertNotIn(self.other.encode(), base64.b64decode(value['content']))
        for asset in saved['assets']:
            self.assertTrue((destination / 'files' / asset['relative']).is_file())
        self.assertFalse(any(p.name.endswith('-wal') for p in destination.rglob('*')))

    def test_cloud_export_refuses_changed_preview_and_active_target(self):
        snapshot = live.backup_snapshot(self.root, self.ids, None)
        with patch.object(live, 'activity', side_effect=RuntimeError('目标活动')):
            with self.assertRaisesRegex(RuntimeError, '目标活动'):
                live.backup_snapshot(self.root, self.ids, None)
        path = self.root / snapshot['assets'][0]['relative']
        with path.open('ab') as handle:
            handle.write(live.encoded({'type': 'event_msg', 'payload': {}}) + b'\n')
        destination = self.base / 'changed-export'
        with self.assertRaisesRegex(RuntimeError, '已变化'):
            live.backup_export(self.root, self.ids, destination, None, live.backup_preview(snapshot)['digest'])
        self.assertFalse(destination.exists())

    def test_cloud_export_refuses_unknown_schema(self):
        with sqlite3.connect(self.root / 'logs_2.sqlite') as conn:
            conn.execute('CREATE TABLE unknown_schema(x TEXT)')
        with self.assertRaisesRegex(RuntimeError, '未知 schema'):
            live.backup_snapshot(self.root, self.ids, None)

    def archive_restore_fixture(self):
        snapshot = live.backup_snapshot(self.root, self.ids, None)
        exported = self.base / 'archive-export'
        live.backup_export(self.root, self.ids, exported, None, live.backup_preview(snapshot)['digest'])
        for name, db in snapshot['databases'].items():
            if db['rows']:
                live.mutate_db(self.root / name, name, self.ids, db['rows'])
        for name, key in live.TEXT_FILES.items():
            (self.root / name).write_bytes(live.filter_text((self.root / name).read_bytes(), key, self.ids)[0])
        for asset in snapshot['assets']:
            (self.root / asset['relative']).unlink()
        return exported, self.base / 'archive-restore-journal'

    def archive_from_root(self, root, name):
        snapshot = live.backup_snapshot(root, self.ids, None)
        exported = self.base / name
        live.backup_export(root, self.ids, exported, None, live.backup_preview(snapshot)['digest'])
        return exported, snapshot

    def restore_archive_fixture(self, exported, snapshot, work):
        for name, database in snapshot['databases'].items():
            if database['rows']:
                live.mutate_db(self.root / name, name, self.ids, database['rows'])
        for name, key in live.TEXT_FILES.items():
            path = self.root / name
            path.write_bytes(live.filter_text(path.read_bytes(), key, self.ids)[0])
        for asset in snapshot['assets']:
            (self.root / asset['relative']).unlink()
        preview = live.archive_restore_preview(self.root, exported, None)
        self.assertEqual(preview['status'], 'ready')
        return live.archive_restore_apply(self.root, exported, work, None, preview['digest'])

    def test_archive_restore_roundtrip_and_second_copy_detects_existing(self):
        before = self.baseline()
        exported, work = self.archive_restore_fixture()
        preview = live.archive_restore_preview(self.root, exported, None)
        self.assertEqual(preview['status'], 'ready')
        restored = live.archive_restore_apply(self.root, exported, work, None, preview['digest'])
        self.assertEqual(restored['status'], 'restored')
        self.assertEqual(self.baseline(), before)
        preview = live.archive_restore_preview(self.root, exported, None)
        self.assertEqual(preview['status'], 'already_present')
        result = live.archive_restore_apply(self.root, exported, self.base / 'second-copy', None, preview['digest'])
        self.assertEqual(result['status'], 'already_present')
        self.assertEqual(self.baseline(), before)

    def test_archive_restore_conflict_and_unowned_partial_are_blocked(self):
        exported, work = self.archive_restore_fixture()
        snapshot = json.loads((exported / 'snapshot.json').read_bytes())
        asset = next(a for a in snapshot['assets'] if a['kind'] == 'capability')
        path = self.root / asset['relative']
        path.write_bytes((exported / 'files' / asset['relative']).read_bytes())
        preview = live.archive_restore_preview(self.root, exported, None)
        self.assertEqual(preview['status'], 'partial')
        with self.assertRaisesRegex(RuntimeError, '恢复被阻断'):
            live.archive_restore_apply(self.root, exported, work, None, preview['digest'])
        path.write_bytes(b'changed')
        preview = live.archive_restore_preview(self.root, exported, None)
        self.assertEqual(preview['status'], 'conflict')
        with self.assertRaisesRegex(RuntimeError, '恢复被阻断'):
            live.archive_restore_apply(self.root, exported, work, None, preview['digest'])

    def test_archive_restore_can_update_a_verified_older_version(self):
        older, older_snapshot = self.archive_from_root(self.root, 'archive-older')
        newer_root = self.base / 'newer-source'
        shutil.copytree(self.root, newer_root)
        rollout = newer_root / ('sessions/rollout-' + self.target + '.jsonl')
        rollout.write_bytes(rollout.read_bytes() + live.encoded({'type': 'event_msg', 'payload': {'text': 'newer'}}) + b'\n')
        capability = newer_root / 'tui-thread-reference-capabilities' / self.target
        capability.write_bytes(b'newer capability state')
        with contextlib.closing(sqlite3.connect(newer_root / 'state_5.sqlite')) as conn, conn:
            conn.execute('UPDATE threads SET title=? WHERE id=?', ('newer title', self.target))
        with contextlib.closing(sqlite3.connect(newer_root / 'logs_2.sqlite')) as conn, conn:
            conn.execute('UPDATE logs SET body=? WHERE thread_id=?', ('newer log', self.target))
        with contextlib.closing(sqlite3.connect(newer_root / 'thread_history_1.sqlite')) as conn, conn:
            conn.execute('UPDATE thread_items SET item_json=? WHERE thread_id=?', ('newer message', self.target))
        for name, key in live.TEXT_FILES.items():
            path = newer_root / name
            before = path.read_bytes()
            replacement = live.encoded({key: self.target, 'text': 'newer'}) + b'\n'
            path.write_bytes(live.merge_text_records(before, key, self.ids, [replacement]))
        newer, newer_snapshot = self.archive_from_root(newer_root, 'archive-newer')

        old_restore = self.restore_archive_fixture(older, older_snapshot, self.base / 'restore-older')
        base_digest = old_restore['installed_version_sha256']
        preview = live.archive_restore_preview(self.root, newer, None, update_if_newer=True,
                                               expected_current_version_digest=base_digest)
        self.assertEqual(preview['status'], 'update_ready')
        updated = live.archive_restore_apply(self.root, newer, self.base / 'update-newer', None,
                                             preview['digest'], update_if_newer=True,
                                             expected_current_version_digest=base_digest)
        self.assertEqual(updated['status'], 'updated')
        self.assertEqual(updated['installed_version_sha256'], preview['archive_version_sha256'])
        self.assertIn(b'newer', (self.root / ('sessions/rollout-' + self.target + '.jsonl')).read_bytes())
        with contextlib.closing(sqlite3.connect(self.root / 'state_5.sqlite')) as conn:
            self.assertEqual(conn.execute('SELECT title FROM threads WHERE id=?', (self.target,)).fetchone(), ('newer title',))
        with contextlib.closing(sqlite3.connect(self.root / 'logs_2.sqlite')) as conn:
            self.assertEqual(conn.execute('SELECT body FROM logs WHERE thread_id=?', (self.target,)).fetchone(), ('newer log',))
        self.assertIn(b'"text":"newer"', (self.root / 'history.jsonl').read_bytes())
        self.assertIn(b'"text":"' + self.other.encode() + b'"', (self.root / 'history.jsonl').read_bytes())

    def test_archive_update_refuses_changes_made_after_the_previous_restore(self):
        older, older_snapshot = self.archive_from_root(self.root, 'archive-change-old')
        newer_root = self.base / 'change-source'
        shutil.copytree(self.root, newer_root)
        rollout = newer_root / ('sessions/rollout-' + self.target + '.jsonl')
        rollout.write_bytes(rollout.read_bytes() + live.encoded({'type': 'event_msg', 'payload': {'text': 'later'}}) + b'\n')
        newer, _ = self.archive_from_root(newer_root, 'archive-change-new')
        old_restore = self.restore_archive_fixture(older, older_snapshot, self.base / 'restore-change-old')
        base_digest = old_restore['installed_version_sha256']
        path = self.root / ('sessions/rollout-' + self.target + '.jsonl')
        path.write_bytes(path.read_bytes() + live.encoded({'type': 'event_msg', 'payload': {'text': 'local edit'}}) + b'\n')
        preview = live.archive_restore_preview(self.root, newer, None, update_if_newer=True,
                                               expected_current_version_digest=base_digest)
        self.assertEqual(preview['status'], 'conflict')
        self.assertIn('变化', preview['update_reason'])
        with self.assertRaisesRegex(RuntimeError, '更新被阻断'):
            live.archive_restore_apply(self.root, newer, self.base / 'blocked-update', None,
                                       preview['digest'], update_if_newer=True,
                                       expected_current_version_digest=base_digest)

    def test_archive_update_requires_explicit_rollback_after_interruption(self):
        older, older_snapshot = self.archive_from_root(self.root, 'archive-update-old')
        newer_root = self.base / 'update-source'
        shutil.copytree(self.root, newer_root)
        rollout = newer_root / ('sessions/rollout-' + self.target + '.jsonl')
        rollout.write_bytes(rollout.read_bytes() + live.encoded({'type': 'event_msg', 'payload': {'text': 'later'}}) + b'\n')
        newer, _ = self.archive_from_root(newer_root, 'archive-update-new')
        old_restore = self.restore_archive_fixture(older, older_snapshot, self.base / 'restore-update-old')
        base_digest = old_restore['installed_version_sha256']
        preview = live.archive_restore_preview(self.root, newer, None, update_if_newer=True,
                                               expected_current_version_digest=base_digest)
        self.assertEqual(preview['status'], 'update_ready')
        work = self.base / 'interrupted-update'
        pid = os.fork()
        if pid == 0:
            live.archive_restore_apply(self.root, newer, work, None, preview['digest'], injected='update-file',
                                       update_if_newer=True,
                                       expected_current_version_digest=base_digest)
            os._exit(0)
        _, status = os.waitpid(pid, 0)
        self.assertEqual(os.waitstatus_to_exitcode(status), 91)
        journal = json.loads((work / 'journal.json').read_bytes())
        self.assertEqual(journal['phase'], 'updating')
        interrupted = live.archive_restore_preview(self.root, newer, None, journal)
        self.assertEqual(interrupted['status'], 'recovery_required')
        with self.assertRaisesRegex(RuntimeError, '先确认“回滚未完成更新”'):
            live.archive_restore_apply(self.root, newer, work, None, preview['digest'],
                                       update_if_newer=True,
                                       expected_current_version_digest=base_digest)

        rolled_back = live.archive_restore_apply(self.root, newer, work, None, interrupted['digest'],
                                                 recover_update=True)
        self.assertEqual(rolled_back['status'], 'update_rolled_back')
        journal = json.loads((work / 'journal.json').read_bytes())
        after_rollback = live.archive_restore_preview(self.root, newer, None, journal)
        self.assertEqual(after_rollback['status'], 'conflict')
        self.assertEqual(after_rollback['current_version_sha256'], base_digest)

        update_preview = live.archive_restore_preview(self.root, newer, None, journal,
                                                      update_if_newer=True,
                                                      expected_current_version_digest=base_digest)
        self.assertEqual(update_preview['status'], 'update_ready')
        result = live.archive_restore_apply(self.root, newer, work, None, update_preview['digest'],
                                            update_if_newer=True,
                                            expected_current_version_digest=base_digest)
        self.assertEqual(result['status'], 'updated')
        self.assertIn(b'later', (self.root / ('sessions/rollout-' + self.target + '.jsonl')).read_bytes())

    def test_archive_restore_interruption_requires_new_preview_then_resumes(self):
        before = self.baseline()
        exported, work = self.archive_restore_fixture()
        preview = live.archive_restore_preview(self.root, exported, None)
        child = os.fork()
        if child == 0:
            live.archive_restore_apply(self.root, exported, work, None, preview['digest'], injected='archive-file')
            os._exit(0)
        _, status = os.waitpid(child, 0)
        self.assertEqual(os.waitstatus_to_exitcode(status), 91)
        journal = json.loads((work / 'journal.json').read_bytes())
        self.assertEqual(journal['phase'], 'restoring')
        preview = live.archive_restore_preview(self.root, exported, None, journal)
        self.assertEqual(preview['status'], 'ready')
        live.archive_restore_apply(self.root, exported, work, None, preview['digest'])
        self.assertEqual(self.baseline(), before)

    def test_archive_restore_refuses_active_target_busy_database_and_changed_review(self):
        exported, work = self.archive_restore_fixture()
        with patch.object(live, 'activity', side_effect=RuntimeError('目标活动')):
            with self.assertRaisesRegex(RuntimeError, '目标活动'):
                live.archive_restore_preview(self.root, exported, None)
        preview = live.archive_restore_preview(self.root, exported, None)
        with sqlite3.connect(self.root / 'state_5.sqlite') as connection:
            connection.execute('BEGIN IMMEDIATE')
            with self.assertRaises(sqlite3.OperationalError):
                live.archive_restore_apply(self.root, exported, work, None, preview['digest'])
            connection.rollback()
        snapshot = json.loads((exported / 'snapshot.json').read_bytes())
        asset = next(a for a in snapshot['assets'] if a['kind'] == 'capability')
        (self.root / asset['relative']).write_bytes(b'new content')
        with self.assertRaisesRegex(RuntimeError, '预览后目标变化'):
            live.archive_restore_apply(self.root, exported, work, None, preview['digest'])


if __name__ == '__main__':
    unittest.main()
