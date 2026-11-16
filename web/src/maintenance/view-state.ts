// 中文说明：维护页的网址状态、候选转换和本地中间产物检索；源目录检索由服务端在分页前完成。
import type { CleanupReport, MaintenanceDirectory, MaintenanceRow, RetentionPolicy } from './types';

export const categoryLabels: Record<string, string> = {
  temporary: '临时文件', cache: '缓存', log: '日志', sqlite_auxiliary: '数据库辅助文件',
  repair_jsonl_tail: '末行待修复', orphan: '未关联会话'
};
export const kindLabels: Record<string, string> = {
  ...categoryLabels, rebuildable_dependency: '可重建依赖', rust_target: '编译产物', frontend_dependency: '前端依赖',
  python_cache: 'Python 缓存', temporary_binary: '临时程序', staging: '中转文件', failed_archive: '失败归档',
  expired_copy: '过期副本', test_database: '测试数据库', runtime_cache: '运行缓存', crash_residue: '中断残留',
  source_code: '源代码', formal_document: '正式文档', sanitized_fixture: '脱敏样本', verification_report: '验证报告', journal: '恢复记录',
  unregistered: '未登记文件', manifest: '登记记录'
};
export const stateLabels: Record<string, string> = {
  stable: '稳定', active_writing: '正在写入', possibly_active: '可能活动', candidate: '可预览', retained: '保留',
  quarantined: '已隔离', purged: '已清理', missing: '文件不存在', unknown: '待确认', blocked: '已阻断',
  prepared: '待执行', applied: '处理中断', committed: '已完成', rolled_back: '已撤销', needs_review: '需要复核'
};
export const actionLabel = (action: string) => action === 'repair_jsonl_tail' ? '修复末条不完整记录' : '移入回收站';
export const basename = (path: string) => path.replaceAll('\\', '/').split('/').filter(Boolean).at(-1) || path;
export const parentPath = (path: string) => path.replaceAll('\\', '/').split('/').slice(0, -1).join('/') || '源目录';

export type MaintenanceView = {
  tab: 'source' | 'generated' | 'recovery' | 'retention'; query: string; category: string;
  eligibility: '' | 'eligible' | 'blocked'; sort: 'size' | 'path' | 'newest' | 'oldest';
  collection: 'candidates' | 'retained' | 'quarantined' | 'unknown' | 'crash_residues';
  layout: 'directories' | 'files'; directory: string;
  scope: '' | 'ungrouped';
};
export function parseMaintenanceView(search: string): MaintenanceView {
  const query = new URLSearchParams(search);
  const pick = <T extends string>(name: string, values: readonly T[], fallback: T): T => values.includes(query.get(name) as T) ? query.get(name) as T : fallback;
  return {
    tab: pick('tab', ['source', 'generated', 'recovery', 'retention'], 'source'), query: (query.get('q') || '').slice(0, 512),
    category: Object.hasOwn(categoryLabels, query.get('category') || '') ? query.get('category')! : '',
    eligibility: pick('state', ['', 'eligible', 'blocked'], ''), sort: pick('sort', ['size', 'path', 'newest', 'oldest'], 'size'),
    collection: pick('group', ['candidates', 'retained', 'quarantined', 'unknown', 'crash_residues'], 'candidates'),
    layout: pick('layout', ['directories', 'files'], 'directories'),
    scope: pick('scope', ['', 'ungrouped'], ''),
    directory: /^(?:\.tmp|tmp|temp|\.temp|\.cache|cache|caches|log|logs|plugins\/cache)$/i.test(query.get('directory') || '') ? query.get('directory')! : ''
  };
}
export function maintenanceViewSearch(view: MaintenanceView) {
  const query = new URLSearchParams();
  if (view.tab !== 'source') query.set('tab', view.tab);
  if (view.query) query.set('q', view.query);
  if (view.category) query.set('category', view.category);
  if (view.eligibility) query.set('state', view.eligibility);
  if (view.sort !== 'size') query.set('sort', view.sort);
  if (view.collection !== 'candidates') query.set('group', view.collection);
  if (view.layout === 'files') query.set('layout', 'files');
  if (view.directory) query.set('directory', view.directory);
  if (view.scope) query.set('scope', view.scope);
  return query.size ? `?${query}` : '';
}
export function sourceAPIQuery(view: MaintenanceView, offset = 0, limit = 30) {
  const query = new URLSearchParams({ offset: String(offset), limit: String(limit) });
  if (view.tab === 'source') {
    if (view.query.trim()) query.set('q', view.query.trim());
    if (view.category) query.set('category', view.category);
    if (view.eligibility) query.set('eligibility', view.eligibility);
    if (view.layout === 'files' && view.directory) query.set('directory', view.directory);
    if (view.layout === 'files' && view.scope) query.set('scope', view.scope);
    query.set('sort', view.sort);
  }
  return `?${query}`;
}
export const directoryLabels: Record<string, string> = { temporary: '临时工作文件', cache: '缓存目录', log: '运行日志' };
export function filterDirectories(directories: MaintenanceDirectory[], view: MaintenanceView) {
  const query = view.query.trim().toLocaleLowerCase();
  return directories.filter(item => (!view.category || item.category === view.category)
    && (!query || `${item.relative_path}\n${directoryLabels[item.category] || ''}`.toLocaleLowerCase().includes(query))
    && (view.eligibility === 'eligible' ? item.eligible_count > 0 : view.eligibility === 'blocked' ? item.retained_count > 0 : true)
  ).sort((a, b) => view.sort === 'path' ? a.relative_path.localeCompare(b.relative_path) : b.size_bytes - a.size_bytes || a.relative_path.localeCompare(b.relative_path));
}
export function artifactRows(report: CleanupReport | null, group: MaintenanceView['collection']): MaintenanceRow[] {
  if (!report) return [];
  if (group === 'unknown' || group === 'crash_residues') return (report[group] ?? []).map(item => ({
    id: item.path, path: item.path, kind: item.kind, size: item.size_bytes, modified: '', status: 'unknown', eligible: false, reason: item.reason, risk: item.risk, unknown: item
  }));
  return (report[group] ?? []).map(item => ({
    id: item.artifact_id, path: item.target_path, kind: item.kind, size: item.size_bytes, modified: item.created_at,
    status: item.status, eligible: item.eligible, reason: item.block_reason || item.retention_reason, risk: item.risk, artifact: item
  }));
}
export function filterArtifactRows(rows: MaintenanceRow[], view: MaintenanceView) {
  const query = view.query.trim().toLocaleLowerCase();
  return rows.filter(row => !query || `${row.path}\n${row.artifact?.owner || ''}\n${kindLabels[row.kind] || row.kind}`.toLocaleLowerCase().includes(query)).sort((a, b) => {
    const tie = a.path.localeCompare(b.path) || a.id.localeCompare(b.id);
    if (view.sort === 'path') return tie;
    if (view.sort === 'newest' || view.sort === 'oldest') {
      const at = Date.parse(a.modified), bt = Date.parse(b.modified);
      if (!Number.isFinite(at)) return Number.isFinite(bt) ? 1 : tie;
      if (!Number.isFinite(bt)) return -1;
      return (view.sort === 'newest' ? bt - at : at - bt) || tie;
    }
    return b.size - a.size || tie;
  });
}
export function validSelection(selected: Set<string>, rows: Array<Pick<MaintenanceRow, 'id' | 'eligible'>>) {
  return new Set(rows.filter(row => row.eligible && selected.has(row.id)).map(row => row.id));
}
export const retentionFields = [
  { key: 'default_ttl_seconds', label: '其他可重建产物', hint: '未单独指定的中间产物' },
  { key: 'staging_ttl_seconds', label: '中转与临时文件', hint: '中转目录、临时程序和中断残留' },
  { key: 'failed_ttl_seconds', label: '失败归档', hint: '未完成的归档产物' },
  { key: 'expired_copy_ttl_seconds', label: '工作区副本', hint: '已登记的过期副本' },
  { key: 'runtime_cache_ttl_seconds', label: '运行缓存', hint: '运行时和 Python 缓存' },
  { key: 'purge_ttl_seconds', label: '隔离区保留期', hint: '到期只提示，不自动永久删除' }
] as const;
export type RetentionDays = Record<typeof retentionFields[number]['key'], number>;
export function retentionDays(policy: RetentionPolicy): RetentionDays {
  return Object.fromEntries(retentionFields.map(field => [field.key, policy[field.key] / 86400])) as RetentionDays;
}
export function retentionPolicy(policy: RetentionPolicy, days: RetentionDays): RetentionPolicy | null {
  if (retentionFields.some(field => !Number.isFinite(days[field.key]) || days[field.key] <= 0 || days[field.key] > 3650)) return null;
  return { ...policy, ...Object.fromEntries(retentionFields.map(field => [field.key, Math.round(days[field.key] * 86400)])), unknown_report_only: true };
}
