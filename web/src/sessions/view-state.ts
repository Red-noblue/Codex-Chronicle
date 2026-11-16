// 中文说明：会话库可分享的筛选与阅读状态；只处理网址，不保存密码、选择集或操作确认。
export type FilterSnapshot = {
  search: string;
  status: string;
  query: string;
  cwd: string;
  model: string;
  modelExact: string;
  cwdExact: string;
  from: string;
  to: string;
  minTokens: string;
  maxTokens: string;
  minBytes: string;
  maxBytes: string;
  diagnostics: string;
};
export type SessionDetailView = 'reading' | 'technical';

const queryFields: Record<keyof FilterSnapshot, string> = {
  search: 'search', status: 'status', query: 'q', cwd: 'cwd', model: 'model',
  modelExact: 'model_exact', cwdExact: 'cwd_exact', from: 'from', to: 'to', minTokens: 'min_tokens', maxTokens: 'max_tokens',
  minBytes: 'min_bytes', maxBytes: 'max_bytes', diagnostics: 'diagnostics'
};

export function emptySessionFilters(): FilterSnapshot {
  return { search: '', status: '全部状态', query: '', cwd: '', model: '', modelExact: '', cwdExact: '', from: '', to: '', minTokens: '', maxTokens: '', minBytes: '', maxBytes: '', diagnostics: '' };
}

export function parseSessionView(search: string): { filters: FilterSnapshot; detail: SessionDetailView } {
  const params = new URLSearchParams(search);
  const filters = emptySessionFilters();
  for (const field of Object.keys(queryFields) as (keyof FilterSnapshot)[]) {
    const value = params.get(queryFields[field]);
    if (value !== null && value.trim()) filters[field] = value.trim();
  }
  for (const field of ['minTokens', 'maxTokens', 'minBytes', 'maxBytes'] as const) {
    if (!/^\d*$/.test(filters[field])) filters[field] = '';
  }
  for (const field of ['from', 'to'] as const) {
    if (!/^\d{4}-\d{2}-\d{2}$/.test(filters[field])) filters[field] = '';
  }
  if (!['true', 'false'].includes(filters.diagnostics)) filters.diagnostics = '';
  if (filters.modelExact !== 'true' || !filters.model) filters.modelExact = '';
  if (filters.cwdExact !== 'true' || !filters.cwd) filters.cwdExact = '';
  return { filters, detail: params.get('view') === 'technical' ? 'technical' : 'reading' };
}

export function sessionViewSearch(filters: FilterSnapshot, detail: SessionDetailView = 'reading'): string {
  const params = new URLSearchParams();
  for (const field of Object.keys(queryFields) as (keyof FilterSnapshot)[]) {
    const value = filters[field].trim();
    if (field === 'modelExact' && !filters.model || field === 'cwdExact' && !filters.cwd) continue;
    if (value && !(field === 'status' && value === '全部状态')) params.set(queryFields[field], value);
  }
  if (detail === 'technical') params.set('view', detail);
  const query = params.toString();
  return query ? `?${query}` : '';
}
