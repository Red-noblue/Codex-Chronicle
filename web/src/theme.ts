// 中文说明：在页面挂载前使用缓存恢复主题，并将后续修改同步到应用数据目录。
import { readPreference, savePreference } from './preferences';
export type Theme = 'light' | 'dark';
const storageKey = 'codex-chronicle.theme.v1';

export function initializeTheme(): void {
  let theme: Theme = 'light';
  try {
    if (readPreference(storageKey) === 'dark') theme = 'dark';
  } catch {
    // 浏览器不允许本地存储时使用默认亮色。
  }
  setTheme(theme, false);
}

export function setTheme(theme: Theme, persist = true): void {
  document.documentElement.dataset.theme = theme;
  document.querySelector<HTMLMetaElement>('meta[name="theme-color"]')?.setAttribute('content', theme === 'dark' ? '#12161e' : '#f4f6f9');
  window.dispatchEvent(new Event('chronicle-theme'));
  if (persist) void savePreference(storageKey, theme);
}
