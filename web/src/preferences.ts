// 中文说明：在浏览器缓存与应用数据目录之间同步三项长期界面设置，跨端口恢复时以服务端设置为准。
const keys = ['codex-chronicle.theme.v1', 'codex-chronicle.density.v1', 'codex-chronicle.saved-filters.v1'] as const;
export type PreferenceKey = typeof keys[number];
type Request = (input: string, init?: RequestInit) => Promise<Response>;
let request: Request | undefined;
let values: Record<string, string> = {};
let changed: Record<string, string> = {};
let queue = Promise.resolve();

export function readPreference(key: PreferenceKey): string | null {
  if (key in values) return values[key];
  try { return localStorage.getItem(key); } catch { return null; }
}

function cache(key: string, value: string): boolean {
  values[key] = value;
  try { localStorage.setItem(key, value); return true; } catch { return false; }
}

function reportFailure() {
  window.dispatchEvent(new Event('chronicle-preferences-error'));
}

function persist(patch: Record<string, string>): Promise<void> {
  queue = queue.then(async () => {
    const response = await request!('/api/v1/ui-preferences', {
      method: 'PATCH', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(patch)
    });
    if (!response.ok) throw new Error('保存界面设置失败');
  }).catch(reportFailure);
  return queue;
}

export function savePreference(key: PreferenceKey, value: string): Promise<void> {
  changed[key] = value;
  const cached = cache(key, value);
  if (request) return persist({ [key]: value });
  if (!cached) reportFailure();
  return Promise.resolve();
}

export async function initializePreferences(fetcher: Request): Promise<void> {
  const response = await fetcher('/api/v1/ui-preferences');
  if (!response.ok) throw new Error('读取界面设置失败');
  const body = await response.json() as { available: boolean; preferences: Record<string, string> };
  if (!body.available) return;
  request = fetcher;
  const migrate: Record<string, string> = {};
  for (const key of keys) {
    const value = changed[key] ?? body.preferences[key] ?? readPreference(key);
    if (value === null) continue;
    cache(key, value);
    if (value !== body.preferences[key]) migrate[key] = value;
  }
  changed = {};
  if (Object.keys(migrate).length) await persist(migrate);
}
