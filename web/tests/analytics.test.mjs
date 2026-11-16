// 中文说明：验证 UTC 日期、连续时间轴、长跨度总量守恒及统计到会话库的精确筛选网址；使用项目现有 TypeScript 编译器兼容 Node 20。
import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import { test } from 'node:test';
import ts from 'typescript';

async function loadTypeScript(path) {
  const source = await readFile(new URL(path, import.meta.url), 'utf8');
  const { outputText } = ts.transpileModule(source, { compilerOptions: { target: ts.ScriptTarget.ES2022, module: ts.ModuleKind.ESNext } });
  return import(`data:text/javascript;base64,${Buffer.from(outputText).toString('base64')}`);
}
const analytics = await loadTypeScript('../src/analytics/view-state.ts');
const sessions = await loadTypeScript('../src/sessions/view-state.ts');

test('日期必须真实存在，最近范围按 UTC 且包含当天', () => {
  assert.equal(analytics.validDate('2024-02-29'), true);
  for (const date of ['2026-02-29', '2026-02-30', '2026-13-01', '2026-1-01', 'invalid']) assert.equal(analytics.validDate(date), false);
  const now = new Date('2026-11-09T23:30:00-08:00');
  for (const [range, from] of [['7d', '2026-11-04'], ['30d', '2026-10-12'], ['90d', '2026-08-13']]) {
    const view = { ...analytics.parseAnalyticsView(''), range };
    assert.deepEqual(analytics.analyticsSelection(view, now), { from, to: '2026-11-10', model: '', project: '' });
  }
});

test('网址恢复筛选与指标，同时拒绝未知选项和无效日期', () => {
  const view = { range: 'custom', from: '2026-10-01', to: '2026-10-31', model: 'gpt-plus', project: '/工作区/带空格 a', metric: 'messages' };
  assert.deepEqual(analytics.parseAnalyticsView(analytics.analyticsViewSearch(view)), view);
  const invalid = analytics.parseAnalyticsView('?range=oops&metric=oops&from=2026-02-30');
  assert.equal(invalid.range, '30d');
  assert.equal(invalid.metric, 'total_tokens');
  assert.equal(invalid.from, '');
  const all = analytics.analyticsSelection({ ...view, range: 'all' });
  assert.equal(all.from, '');
  assert.equal(all.to, '');
});

const point = (date, conversations, total_tokens) => ({ date, conversations, total_tokens, messages: conversations * 3, turns: conversations * 2 });

test('日历补零并保留范围边界，不将无记录日省略', () => {
  const buckets = analytics.buildTimeline([
    point('2026-10-07', 2, 40), point('2026-10-01', 1, 10),
    point('2026-09-30', 99, 9999), point('bad-date', 99, 9999)
  ], { from: '2026-10-01', to: '2026-10-07' });
  assert.equal(buckets.length, 7);
  assert.equal(buckets[0].total_tokens, 10);
  assert.equal(buckets[6].total_tokens, 40);
  assert.equal(buckets[3].conversations, 0);
  assert.equal(buckets.reduce((sum, row) => sum + row.total_tokens, 0), 50);
  assert.equal(analytics.buildTimeline([], { from: '', to: '' }).length, 0);
  assert.equal(analytics.buildTimeline([], { from: '2026-10-07', to: '2026-10-01' }).length, 0);
});

test('长跨度不超过 60 桶，日历连续且所有指标总量守恒', () => {
  const daily = Array.from({ length: 901 }, (_, index) => point(new Date(Date.UTC(2024, 0, 1 + index)).toISOString().slice(0, 10), index % 7, index * 123));
  const buckets = analytics.buildTimeline(daily, { from: '', to: '' });
  assert.ok(buckets.length <= 60);
  assert.equal(buckets[0].from, daily[0].date);
  assert.equal(buckets.at(-1).to, daily.at(-1).date);
  assert.equal(buckets.reduce((sum, row) => sum + row.days, 0), 901);
  for (const field of ['conversations', 'messages', 'turns', 'total_tokens']) {
    assert.equal(buckets.reduce((sum, row) => sum + row[field], 0), daily.reduce((sum, row) => sum + row[field], 0));
  }
  for (let index = 1; index < buckets.length; index++) {
    assert.equal(Date.parse(buckets[index].from) - Date.parse(buckets[index - 1].to), 86_400_000);
  }
});

test('统计筛选与会话网址保留精确模型和项目，包括未知项', () => {
  const selection = { from: '2026-10-01', to: '2026-10-02', model: '未知模型', project: '未知项目' };
  const query = new URLSearchParams(analytics.analyticsAPIQuery(selection));
  assert.equal(query.get('model_exact'), 'true');
  assert.equal(query.get('cwd_exact'), 'true');
  assert.equal(query.get('cwd'), '未知项目');
  const filters = { ...sessions.emptySessionFilters(), from: selection.from, to: selection.to, model: selection.model, cwd: selection.project, modelExact: 'true', cwdExact: 'true' };
  assert.deepEqual(sessions.parseSessionView(sessions.sessionViewSearch(filters)).filters, filters);
  const empty = sessions.parseSessionView('?model_exact=true&cwd_exact=true');
  assert.equal(empty.filters.modelExact, '');
  assert.equal(empty.filters.cwdExact, '');
});
