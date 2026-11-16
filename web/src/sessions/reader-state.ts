// 中文说明：维护有界消息窗口，只保存消息锚点和阅读偏好，最多保留 100 个会话，不缓存正文或执行确认。
export type ReaderEvent = {
  id: string; role?: string; kind: string; text: string; truncated: boolean;
  relative_path: string; start_offset: number; end_offset: number; line: number;
  anchor: string; source_version: string; turn_id?: string; turn_inferred?: boolean;
  timestamp?: string; tool_name?: string; call_id?: string;
  item_id?: string; status?: string; exit_code?: number;
  tool?: { kind: string; title: string; target?: string; cwd?: string; output?: string; output_truncated?: boolean; result?: ReaderReference };
  changes?: ReaderChange[];
};
export type ReaderReference = { path: string; start: number; end: number; line: number };
export type ReaderChange = { path: string; move_path?: string; kind: string; status: string; added: number; removed: number; diff?: string; truncated?: boolean };
export type ReaderFile = ReaderChange & { source: ReaderReference; turn_id?: string; call_id?: string };
export type ReaderTurn = { id: string; start: ReaderReference; message?: ReaderReference; caption?: string; status: string; inferred?: boolean };
export type ReaderOverview = { turns?: ReaderTurn[]; files?: ReaderFile[]; total: number; has_more: boolean; source_stamp?: string };
export type ReaderPage = { items: ReaderEvent[]; next_cursor: string; previous_cursor: string; has_more: boolean;
  complete: boolean; diagnostics: string[]; source_changed: boolean; pending: boolean; bytes_read: number;
  source?: string; source_stamp?: string; mainline?: string; versions?: { path: string; mainline: boolean }[];
  indexing?: boolean; indexed_bytes?: number; total_bytes?: number };
export type ReaderLocation = { path: string; start: number; anchor?: string; query?: string; source?: string };
export type ReaderState = { location?: ReaderLocation; inset: number; expanded: string[]; query: string; role: string; navigation: boolean; updated: number };
const key = 'chronicle.reader.v1';
export const maxVisibleMessages = 400;
export type ReaderPageDirection = 'before' | 'after';
export function mergeReaderItems(current: ReaderEvent[], incoming: ReaderEvent[], direction: ReaderPageDirection) {
  const overlap = direction === 'before' ? incoming.findIndex(item => item.id === current[0]?.id) : -1;
  const page = overlap < 0 ? incoming : incoming.slice(0, overlap);
  const retained = new Map(current.map(item => [item.id, item]));
  const ordered = direction === 'before' ? [...page, ...current] : [...current, ...page];
  const merged = [...new Map(ordered.map(item => [item.id, retained.get(item.id) ?? item])).values()];
  return { items: direction === 'before' ? merged.slice(0, maxVisibleMessages) : merged.slice(-maxVisibleMessages), trimmed: merged.length > maxVisibleMessages };
}
export function isReaderMessage(event: ReaderEvent): boolean {
  return !event.tool && (event.role === 'assistant' || event.role === 'user' && event.kind !== 'injected_context');
}
export function readReaderState(storage: Pick<Storage, 'getItem'>, workspace: string, conversation: string): ReaderState | null {
  try {
    const entry = JSON.parse(storage.getItem(key) || '{}')[JSON.stringify([workspace, conversation])];
    if (!entry || !Array.isArray(entry.expanded) || typeof entry.query !== 'string') return null;
    const location = entry.location;
    if (location && (typeof location.path !== 'string' || !Number.isSafeInteger(location.start) || location.start < 0)) return null;
    return { ...entry, expanded: entry.expanded.filter((value: unknown) => typeof value === 'string').slice(-400),
      query: entry.query.slice(0, 4096), role: ['user', 'assistant'].includes(entry.role) ? entry.role : '',
      inset: Number.isFinite(entry.inset) ? entry.inset : 0, navigation: entry.navigation === true };
  } catch { return null; }
}
export function writeReaderState(storage: Pick<Storage, 'getItem' | 'setItem'>, workspace: string, conversation: string, state: ReaderState) {
  try {
    const entries = JSON.parse(storage.getItem(key) || '{}');
    entries[JSON.stringify([workspace, conversation])] = { ...state, expanded: state.expanded.slice(-400), updated: Date.now() };
    storage.setItem(key, JSON.stringify(Object.fromEntries(Object.entries(entries)
      .sort((a, b) => Number((b[1] as ReaderState).updated) - Number((a[1] as ReaderState).updated)).slice(0, 100))));
  } catch { /* 隐私模式或空间不足不影响只读阅读。 */ }
}
export function readerLocationFromURL(search: string): ReaderLocation | null {
  const params = new URLSearchParams(search); const path = params.get('read_path'); const start = params.get('read_start');
  if (!path || start === null || !/^\d+$/.test(start) || !Number.isSafeInteger(Number(start))) return null;
  return { path, start: Number(start), anchor: params.get('read_anchor') || undefined, query: params.get('read_q') || undefined, source: params.get('read_source') || undefined };
}
export function locationFor(event: ReaderEvent, query = '', source = ''): ReaderLocation {
  return { path: event.relative_path, start: event.start_offset, anchor: event.anchor, query, ...(source ? { source } : {}) };
}
