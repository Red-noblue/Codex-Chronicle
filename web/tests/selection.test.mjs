// 中文说明：验证刷新可恢复会话选择、不同源目录隔离及清空行为，缓存不携带任何写入授权。
import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import { test } from 'node:test';
import ts from 'typescript';
const source = await readFile(new URL('../src/sessions/selection.ts', import.meta.url), 'utf8');
const { outputText } = ts.transpileModule(source, { compilerOptions: { target: ts.ScriptTarget.ES2022, module: ts.ModuleKind.ESNext } });
const { completedRemovalIDs, readConversationSelection: read, writeConversationSelection: write } = await import(`data:text/javascript;base64,${Buffer.from(outputText).toString('base64')}`);
function storage() {
  const values = new Map();
  return { values, getItem: key => values.get(key) ?? null, setItem: (key, value) => values.set(key, value), removeItem: key => values.delete(key) };
}

test('21 项选择可在刷新后完整恢复，且只保存 ID 与源目录', () => {
  const cache = storage();
  const ids = new Set(Array.from({ length: 21 }, (_, index) => `conversation-${index}`));
  write(cache, '/work/.codex', ids);
  assert.deepEqual(read(cache, '/work/.codex'), ids);
  const saved = JSON.parse([...cache.values.values()][0]);
  assert.deepEqual(Object.keys(saved).sort(), ['ids', 'root', 'version']);
});

test('切换工作区不串用选择，清空某一工作区不改变其他工作区', () => {
  const cache = storage();
  write(cache, '/work/a', new Set(['a']));
  write(cache, '/work/b', new Set(['b']));
  assert.deepEqual(read(cache, '/work/a'), new Set(['a']));
  assert.deepEqual(read(cache, '/work/c'), new Set());
  write(cache, '/work/a', new Set());
  assert.deepEqual(read(cache, '/work/a'), new Set());
  assert.deepEqual(read(cache, '/work/b'), new Set(['b']));
});

test('不接受错误源目录和版本，重复或非法 ID 被排除，伪造授权字段被忽略', () => {
  const cache = storage();
  const key = 'codex-chronicle.selection.v1:/work/a';
  cache.setItem(key, JSON.stringify({ version: 1, root: '/work/b', ids: ['b'] }));
  assert.equal(read(cache, '/work/a').size, 0);
  cache.setItem(key, JSON.stringify({ version: 2, root: '/work/a', ids: ['a'] }));
  assert.equal(read(cache, '/work/a').size, 0);
  cache.setItem(key, JSON.stringify({ version: 1, root: '/work/a', ids: ['a', 'a', '', null, 12, 'x'.repeat(257)], approved: true, write_enabled: true }));
  assert.deepEqual(read(cache, '/work/a'), new Set(['a']));
});

test('无法存储或 JSON 损坏时向界面报告失败，不假装已持久化', () => {
  const cache = storage();
  cache.getItem = () => '{invalid-json';
  assert.throws(() => read(cache, '/work/a'));
  cache.setItem = () => { throw new Error('storage denied'); };
  assert.throws(() => write(cache, '/work/a', new Set(['a'])), /storage denied/);
});

const task = (root, strategy, state, items) => ({ source_root: root, strategy, state, completed: 2, preview: { items } });
test('回收和永久删除完成项退出选择，刷新缓存后不会重新出现', () => {
  const cache = storage();
  const tasks = [task('/work/a', { trash: true }, 'completed', [{ conversation_id: 'a', completed: true }]), task('/work/a', { permanent_delete: true }, 'completed', [{ conversation_id: 'b', completed: true }])];
  const completed = completedRemovalIDs('/work/a', tasks);
  const remaining = new Set(['a', 'b', 'unfinished'].filter(id => !completed.has(id)));
  write(cache, '/work/a', remaining);
  assert.deepEqual(read(cache, '/work/a'), new Set(['unfinished']));
  write(cache, '/work/a', new Set([...remaining].filter(id => id !== 'unfinished')));
  assert.deepEqual(read(cache, '/work/a'), new Set());
});

test('部分失败只清除逐项确认完成的目标，不根据总进度猜测结果', () => {
  const tasks = [task('/work/a', { trash: true }, 'needs_review', [
    { conversation_id: 'done', completed: true }, { conversation_id: 'failed', completed: false }, { conversation_id: 'unknown' }
  ])];
  assert.deepEqual(completedRemovalIDs('/work/a', tasks), new Set(['done']));
  tasks[0].state = 'running';
  assert.deepEqual(completedRemovalIDs('/work/a', tasks), new Set(['done']));
});

test('其他工作区和仅归档操作不清除当前选择', () => {
  const tasks = [task('/work/b', { trash: true }, 'completed', [{ conversation_id: 'same-id', completed: true }]), task('/work/a', { archive: true }, 'completed', [{ conversation_id: 'archived', completed: true }])];
  assert.deepEqual(completedRemovalIDs('/work/a', tasks), new Set());
});
