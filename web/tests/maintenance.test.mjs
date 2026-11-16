// 中文说明：验证维护页网址恢复、全量检索参数、生成物排序、选择失效和六类保留规则，不访问真实文件。
import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import { test } from 'node:test';
import ts from 'typescript';

const source = await readFile(new URL('../src/maintenance/view-state.ts', import.meta.url), 'utf8');
const { outputText } = ts.transpileModule(source, { compilerOptions: { target: ts.ScriptTarget.ES2022, module: ts.ModuleKind.ESNext } });
const state = await import(`data:text/javascript;base64,${Buffer.from(outputText).toString('base64')}`);

test('网址恢复维护分区、中文路径筛选和隔离区分类，未知参数回退', () => {
  const view = { tab: 'generated', query: '/项目 a/临时', category: '', eligibility: '', sort: 'oldest', collection: 'quarantined', layout: 'directories', directory: '', scope: '' };
  assert.deepEqual(state.parseMaintenanceView(state.maintenanceViewSearch(view)), view);
  assert.deepEqual(state.parseMaintenanceView('?tab=删除&sort=drop&group=all&state=active&category=other'), state.parseMaintenanceView(''));
  assert.equal(state.maintenanceViewSearch(state.parseMaintenanceView('')), '');
});

test('目录以外的文件使用独立范围，服务端分页筛选且刷新后保持定位', () => {
  const view = { ...state.parseMaintenanceView(''), layout: 'files', scope: 'ungrouped' };
  assert.deepEqual(state.parseMaintenanceView(state.maintenanceViewSearch(view)), view);
  assert.equal(new URLSearchParams(state.sourceAPIQuery(view)).get('scope'), 'ungrouped');
  assert.equal(new URLSearchParams(state.sourceAPIQuery({ ...view, layout: 'directories' })).has('scope'), false);
  assert.equal(state.parseMaintenanceView('?scope=all-files').scope, '');
});

test('默认按目录浏览，文件明细路由可恢复且精确限制目录，不接受越界路径', () => {
  const defaults = state.parseMaintenanceView('');
  assert.equal(defaults.layout, 'directories');
  const view = { ...defaults, layout: 'files', directory: '.tmp' };
  assert.deepEqual(state.parseMaintenanceView(state.maintenanceViewSearch(view)), view);
  assert.equal(new URLSearchParams(state.sourceAPIQuery(view)).get('directory'), '.tmp');
  assert.equal(new URLSearchParams(state.sourceAPIQuery({ ...view, layout: 'directories' })).get('directory'), null);
  assert.equal(state.parseMaintenanceView('?directory=../tmp').directory, '');
  assert.equal(state.parseMaintenanceView('?directory=plugins%2Fcache&layout=files').directory, 'plugins/cache');
});

test('目录检索独立于文件分页，支持中文用途并保留完整数量', () => {
  const directories = [{ relative_path: '.tmp', category: 'temporary', file_count: 5502, eligible_count: 5500, retained_count: 2, size_bytes: 20000 }, { relative_path: 'logs', category: 'log', file_count: 8, eligible_count: 8, retained_count: 0, size_bytes: 500 }];
  const view = state.parseMaintenanceView('');
  assert.equal(state.filterDirectories(directories, { ...view, query: '临时' })[0].eligible_count, 5500);
  assert.deepEqual(state.filterDirectories(directories, { ...view, eligibility: 'blocked' }).map(item => item.relative_path), ['.tmp']);
  assert.deepEqual(state.filterDirectories(directories, { ...view, category: 'log' }).map(item => item.relative_path), ['logs']);
});

test('源目录搜索发送完整快照筛选，不依赖当前已载入的候选', () => {
  const view = { ...state.parseMaintenanceView(''), query: '日志 & 路径', category: 'temporary', eligibility: 'blocked', sort: 'newest' };
  const params = new URLSearchParams(state.sourceAPIQuery(view, 60));
  assert.equal(params.get('offset'), '60');
  assert.equal(params.get('limit'), '30');
  assert.equal(params.get('q'), '日志 & 路径');
  assert.equal(params.get('category'), 'temporary');
  assert.equal(params.get('eligibility'), 'blocked');
  assert.equal(params.get('sort'), 'newest');
  assert.equal(new URLSearchParams(state.sourceAPIQuery({ ...view, tab: 'generated' })).get('q'), null);
});

const artifact = (id, size, created, eligible = true) => ({ artifact_id: id, target_path: `/生成物/${id}`, kind: 'staging', size_bytes: size, created_at: created, owner: '测试', status: eligible ? 'candidate' : 'retained', eligible, block_reason: '检查结论' });
const report = { candidates: [artifact('small', 2, '2026-11-01'), artifact('large', 20, '2026-11-03'), artifact('missing-date', 5, '')], retained: [artifact('retained', 7, '', false)], quarantined: [artifact('quarantine', 30, '', false)], unknown: [{ path: '/未知/文件', size_bytes: 1, kind: 'unknown', reason: '缺少归属', risk: 'high' }], crash_residues: [] };

test('生成物检索支持类型和生成者，并在显示截断前排序，缺失日期排末尾', () => {
  const rows = state.artifactRows(report, 'candidates');
  const view = state.parseMaintenanceView('');
  assert.deepEqual(state.filterArtifactRows(rows, view).map(row => row.id), ['large', 'missing-date', 'small']);
  assert.deepEqual(state.filterArtifactRows(rows, { ...view, sort: 'oldest' }).map(row => row.id), ['small', 'large', 'missing-date']);
  assert.deepEqual(state.filterArtifactRows(rows, { ...view, sort: 'newest' }).map(row => row.id), ['large', 'small', 'missing-date']);
  assert.equal(state.filterArtifactRows(rows, { ...view, query: '中转' }).length, 3);
  assert.equal(state.filterArtifactRows(rows, { ...view, query: '测试' }).length, 3);
  assert.equal(state.filterArtifactRows(rows, { ...view, query: '不存在' }).length, 0);
  assert.equal(rows[0].id, 'small', '排序不能改变缓存原数组');
});

test('刷新移除失效、阻断和消失的选择，隔离物与未知对象不可进入执行选择', () => {
  const selected = new Set(['small', 'retained', 'quarantine', '/未知/文件', 'deleted']);
  const rows = ['candidates', 'retained', 'quarantined', 'unknown'].flatMap(group => state.artifactRows(report, group));
  assert.deepEqual([...state.validSelection(selected, rows)], ['small']);
  assert.equal(state.artifactRows(report, 'unknown')[0].reason, '缺少归属');
});

test('保留规则涵盖六类时间，保留非整天原值并拒绝空值、负数和非有限数', () => {
  const policy = { version: 1, default_ttl_seconds: 604800, staging_ttl_seconds: 43200, failed_ttl_seconds: 604800, expired_copy_ttl_seconds: 2592000, runtime_cache_ttl_seconds: 1209600, purge_ttl_seconds: 604800, unknown_report_only: true };
  const days = state.retentionDays(policy);
  assert.equal(Object.keys(days).length, 6);
  assert.equal(days.staging_ttl_seconds, .5);
  assert.deepEqual(state.retentionPolicy(policy, days), policy);
  for (const value of [0, -1, NaN, undefined, Infinity, 3651]) assert.equal(state.retentionPolicy(policy, { ...days, staging_ttl_seconds: value }), null);
  assert.equal(state.retentionPolicy({ ...policy, unknown_report_only: false }, days).unknown_report_only, true);
});

test('路径展示兼容 Windows 和中文目录，主名称不携带整条路径', () => {
  assert.equal(state.basename('C:\\项目\\临时\\文件.tmp'), '文件.tmp');
  assert.equal(state.parentPath('C:\\项目\\临时\\文件.tmp'), 'C:/项目/临时');
  assert.equal(state.parentPath('state.sqlite-wal'), '源目录');
});
