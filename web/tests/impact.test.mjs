// 中文说明：核对目标专属文件和数据库行的预览计数，避免将共享数据库大小或其他会话的记录计入删除范围。
import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import { test } from 'node:test';
import ts from 'typescript';

const source = await readFile(new URL('../src/sessions/impact.ts', import.meta.url), 'utf8');
const { outputText } = ts.transpileModule(source, { compilerOptions: { target: ts.ScriptTarget.ES2022, module: ts.ModuleKind.ESNext } });
const { summarizeImpact, impactBlockerMessage } = await import(`data:text/javascript;base64,${Buffer.from(outputText).toString('base64')}`);

const rollout = { relative_path: 'sessions/target.jsonl', size_bytes: 12888647, kind: 'rollout_jsonl', owners: 1 };
const shared = [
  { relative_path: 'state_5.sqlite', size_bytes: 9818112, kind: 'sqlite', owners: 178 },
  { relative_path: 'thread_history_1.sqlite', size_bytes: 201641984, kind: 'sqlite', owners: 87 }
];
const group = (database, table, owners) => ({ database, table, row_count: owners.length, rows: owners.map((owner, i) => ({ owner, primary_key: [owner, i] })) });
const preview = {
  assets: [{ id: 'target', relative: rollout.relative_path, bytes: rollout.size_bytes }, { id: 'other', relative: 'sessions/other.jsonl', bytes: 59363 }],
  sqlite_objects_and_rows: [
    group('state_5.sqlite', 'threads', ['target', 'other']),
    group('thread_history_1.sqlite', 'thread_items', Array(965).fill('target')),
    group('thread_history_1.sqlite', 'thread_turns', Array(10).fill('target')),
    group('thread_history_1.sqlite', 'thread_history_projection_state', ['target'])
  ],
  text_lines: { 'session_index.jsonl': { removed: [{ id: 'target', line: 3 }, { id: 'other', line: 5 }] }, 'history.jsonl': { removed: [] } },
  recovery_path: '/outside/recovery'
};

test('共享数据库整文件大小不计入目标文件大小，批量计划按原始会话 ID 分开', () => {
  const impact = summarizeImpact('target', [rollout], [rollout, ...shared], preview);
  assert.equal(impact.fileBytes, 12888647);
  assert.deepEqual(impact.files, [{ relative_path: rollout.relative_path, size_bytes: rollout.size_bytes }]);
  assert.equal(impact.sharedFiles.length, 2);
  assert.equal(impact.rowCount, 977);
  assert.equal(impact.databases.find(db => db.path === 'state_5.sqlite').rows, 1);
  assert.equal(impact.databases.find(db => db.path === 'thread_history_1.sqlite').rows, 976);
  assert.deepEqual(impact.texts, [{ path: 'session_index.jsonl', lines: [3] }]);
  const other = summarizeImpact('other', [], [], preview);
  assert.equal(other.rowCount, 1);
  assert.equal(other.fileBytes, 59363);
  assert.equal(other.textLineCount, 1);
});

test('未获执行计划时，不把扫描到的关联文件当作将删除文件', () => {
  const impact = summarizeImpact('target', [], [rollout, ...shared]);
  assert.equal(impact.files.length, 0);
  assert.equal(impact.fileBytes, 0);
  assert.equal(impact.rowCount, 0);
  assert.equal(impact.sharedFiles.length, 2);
});

test('普通计划只展示实际计划文件，无目标行的真实预览不借用其他会话记录', () => {
  assert.equal(summarizeImpact('target', [rollout], [rollout]).fileBytes, rollout.size_bytes);
  const missing = summarizeImpact('absent', [rollout], [], preview);
  assert.equal(missing.files.length, 0);
  assert.equal(missing.rowCount, 0);
  assert.equal(missing.textLineCount, 0);
});

test('阻断原因使用可行动的文字，未识别原因保留原文', () => {
  assert.match(impactBlockerMessage('目标仍被 daemon 加载'), /对话仍在使用中/);
  assert.match(impactBlockerMessage('SQLite busy/locked'), /稍后重新检查/);
  assert.match(impactBlockerMessage('共享数据库或索引仍被占用，关联记录暂不可修改；对话状态 active_writing 不允许生成写入计划'), /数据库或索引正在被占用/);
  assert.match(impactBlockerMessage('未知 schema: state_5.sqlite'), /数据格式尚未支持/);
  assert.match(impactBlockerMessage('附件归属尚未实现，停止'), /附件或会话关联/);
  assert.equal(impactBlockerMessage('该目标未获授权'), '该目标未获授权');
});
