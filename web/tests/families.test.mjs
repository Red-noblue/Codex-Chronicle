// 中文说明：验证折叠列表、搜索命中子代理、缺失父节点、循环和分页可见性。
import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import { test } from 'node:test';
import ts from 'typescript';
const source = await readFile(new URL('../src/sessions/families.ts', import.meta.url), 'utf8');
const { outputText } = ts.transpileModule(source, { compilerOptions: { target: ts.ScriptTarget.ES2022, module: ts.ModuleKind.ESNext } });
const { projectFamilies, reviewFamilyItems } = await import(`data:text/javascript;base64,${Buffer.from(outputText).toString('base64')}`);
const items = [
  { id: 'grandchild', parent_conversation_id: 'child' },
  { id: 'child', parent_conversation_id: 'root' },
  { id: 'sibling', parent_conversation_id: 'root' },
  { id: 'root' }, { id: 'fork' }
];
const ids = rows => rows.map(row => row.id);

test('默认仅显示根会话，展开后按层级排列，未改变原数组', () => {
  assert.deepEqual(ids(projectFamilies(items, new Set())), ['root', 'fork']);
  const rows = projectFamilies(items, new Set(['root', 'child']));
  assert.deepEqual(ids(rows), ['root', 'child', 'grandchild', 'sibling', 'fork']);
  assert.deepEqual(rows.map(row => row.familyDepth), [0, 1, 2, 1, 0]);
  assert.equal(items[0].id, 'grandchild');
});
test('搜索子代理时自动显示其父链，无结果时返回空列表', () => {
  assert.deepEqual(ids(projectFamilies(items, new Set(), new Set(['grandchild']))), ['root', 'child', 'grandchild']);
  assert.deepEqual(projectFamilies(items, new Set(), new Set()), []);
});
test('分页未载入父会话时仍显示子代理，关系冲突也保持可读', () => {
  assert.deepEqual(ids(projectFamilies([items[0], items[1]], new Set())), ['child']);
  assert.deepEqual(ids(projectFamilies([{ id: 'orphan', parent_conversation_id: 'absent' }], new Set())), ['orphan']);
  assert.deepEqual(ids(projectFamilies([{ id: 'a', parent_conversation_id: 'b', family_conflict: true }, { id: 'b' }], new Set())), ['a', 'b']);
});
test('循环数据不会无限递归或丢失所有会话', () => {
  const rows = projectFamilies([{ id: 'a', parent_conversation_id: 'b' }, { id: 'b', parent_conversation_id: 'a' }], new Set(['a', 'b']));
  assert.deepEqual(ids(rows), ['a', 'b']);
});

test('确认清单先展示原选主会话，不改变服务端执行顺序或丢失自动纳入对象', () => {
  const original = [{ id: 'auto', automatically_included: true, thread_kind: 'subagent' }, { id: 'child', thread_kind: 'subagent' }, { id: 'main' }];
  assert.deepEqual(ids(reviewFamilyItems(original)), ['main', 'child', 'auto']);
  assert.deepEqual(ids(original), ['auto', 'child', 'main']);
});
