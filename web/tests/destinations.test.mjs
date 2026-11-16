// 中文说明：验证首次执行的目的地门禁独立于对话授权，两个目的地必须分别完成检查。
import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import { test } from 'node:test';
import ts from 'typescript';
const source = await readFile(new URL('../src/sessions/destinations.ts', import.meta.url), 'utf8');
const { outputText } = ts.transpileModule(source, { compilerOptions: { target: ts.ScriptTarget.ES2022, module: ts.ModuleKind.ESNext } });
const { archiveLocationMissing, destinationsReady } = await import(`data:text/javascript;base64,${Buffer.from(outputText).toString('base64')}`);
test('首次检查要求选中动作对应的目的地分别确认', () => {
  const status = { source_root: '/work/source', trash_verified: false, archive_confirmed: false };
  assert.equal(destinationsReady(status, { trash: true, archive: false }, status.source_root), false);
  assert.equal(destinationsReady({ ...status, trash_verified: true }, { trash: true, archive: true, archive_local: true }, status.source_root), false);
  assert.equal(destinationsReady({ ...status, trash_verified: true, archive_confirmed: true }, { trash: true, archive: true, archive_local: true }, status.source_root), true);
});
test('归档必须选择位置，云端不借用本地确认，双副本仍需本地确认', () => {
  const status = { source_root: '/work/source', trash_verified: true, archive_confirmed: false };
  assert.equal(archiveLocationMissing({ archive: true, archive_local: false, archive_remote: false }), true);
  assert.equal(destinationsReady({ ...status, archive_confirmed: true }, { trash: false, archive: true }, status.source_root), false);
  assert.equal(destinationsReady(status, { trash: false, archive: true, archive_remote: true }, status.source_root), true);
  assert.equal(destinationsReady(status, { trash: false, archive: true, archive_local: true, archive_remote: true }, status.source_root), false);
});
test('不能沿用其他工作区的确认，也不能把缺失状态视为已确认', () => {
  const status = { source_root: '/work/a', trash_verified: true, archive_confirmed: true };
  assert.equal(destinationsReady(status, { trash: true, archive: false }, '/work/b'), false);
  assert.equal(destinationsReady(null, { trash: true, archive: false }, '/work/a'), false);
});
