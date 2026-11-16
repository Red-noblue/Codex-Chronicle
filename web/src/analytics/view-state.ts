// 中文说明：解析可恢复的统计网址，并按 UTC 补齐时间轴；长跨度自动聚合，避免生成无限量图形节点。
import type { AnalyticsView, AnalyticsSelection, DailyUsage, TrendBucket } from './types';
const day = 86_400_000;
const stamp = (date: string) => Date.parse(`${date}T00:00:00Z`);
const iso = (value: number) => new Date(value).toISOString().slice(0, 10);

export function validDate(value: string): boolean {
  return /^\d{4}-\d{2}-\d{2}$/.test(value) && Number.isFinite(stamp(value)) && iso(stamp(value)) === value;
}

export function parseAnalyticsView(search: string): AnalyticsView {
  const params = new URLSearchParams(search);
  const range = params.get('range') || '30d';
  const metric = params.get('metric') || 'total_tokens';
  return {
    range: ['7d', '30d', '90d', 'all', 'custom'].includes(range) ? range as AnalyticsView['range'] : '30d',
    metric: ['total_tokens', 'conversations', 'messages'].includes(metric) ? metric as AnalyticsView['metric'] : 'total_tokens',
    from: validDate(params.get('from') || '') ? params.get('from')! : '',
    to: validDate(params.get('to') || '') ? params.get('to')! : '',
    model: params.get('model')?.trim() || '',
    project: params.get('project')?.trim() || ''
  };
}

export function analyticsViewSearch(view: AnalyticsView): string {
  const params = new URLSearchParams();
  if (view.range !== '30d') params.set('range', view.range);
  if (view.metric !== 'total_tokens') params.set('metric', view.metric);
  if (view.range === 'custom') {
    if (view.from) params.set('from', view.from);
    if (view.to) params.set('to', view.to);
  }
  if (view.model) params.set('model', view.model);
  if (view.project) params.set('project', view.project);
  return params.size ? `?${params}` : '';
}

export function analyticsSelection(view: AnalyticsView, now = new Date()): AnalyticsSelection {
  let from = '', to = '';
  if (view.range === 'custom') {
    from = view.from;
    to = view.to;
  } else if (view.range !== 'all') {
    const today = stamp(now.toISOString().slice(0, 10));
    to = iso(today);
    from = iso(today - (Number.parseInt(view.range) - 1) * day);
  }
  return { from, to, model: view.model, project: view.project };
}

export function analyticsAPIQuery(selection: AnalyticsSelection): string {
  const params = new URLSearchParams();
  if (selection.from) params.set('from', selection.from);
  if (selection.to) params.set('to', selection.to);
  if (selection.model) { params.set('model', selection.model); params.set('model_exact', 'true'); }
  if (selection.project) { params.set('cwd', selection.project); params.set('cwd_exact', 'true'); }
  return params.size ? `?${params}` : '';
}

export function buildTimeline(daily: DailyUsage[], selection: Pick<AnalyticsSelection, 'from' | 'to'>): TrendBucket[] {
  const known = daily.filter(point => validDate(point.date)).slice().sort((a, b) => a.date.localeCompare(b.date));
  const first = selection.from || known[0]?.date;
  const last = selection.to || known[known.length - 1]?.date;
  if (!first || !last || !validDate(first) || !validDate(last) || first > last) return [];
  const start = stamp(first), end = stamp(last);
  const days = Math.round((end - start) / day) + 1;
  const step = Math.max(1, Math.ceil(days / 60));
  const buckets = Array.from({ length: Math.ceil(days / step) }, (_, index): TrendBucket => ({
    from: iso(start + index * step * day),
    to: iso(Math.min(end, start + ((index + 1) * step - 1) * day)),
    days: Math.min(step, days - index * step),
    total_tokens: 0, conversations: 0, messages: 0, turns: 0
  }));
  for (const point of known) {
    if (point.date < first || point.date > last) continue;
    const bucket = buckets[Math.floor((stamp(point.date) - start) / day / step)];
    bucket.total_tokens += point.total_tokens;
    bucket.conversations += point.conversations;
    bucket.messages += point.messages;
    bucket.turns += point.turns;
  }
  return buckets;
}
