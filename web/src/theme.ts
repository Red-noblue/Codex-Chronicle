// 中文说明：在页面挂载前恢复明暗主题；外观设置只保存在当前浏览器。
export type Theme = 'light' | 'dark';
const storageKey = 'codex-chronicle.theme.v1';

export function initializeTheme(): void {
  let theme: Theme = 'light';
  try {
    if (localStorage.getItem(storageKey) === 'dark') theme = 'dark';
  } catch {
    // 浏览器不允许本地存储时使用默认亮色。
  }
  document.documentElement.dataset.theme = theme;
  document.querySelector<HTMLMetaElement>('meta[name="theme-color"]')?.setAttribute('content', theme === 'dark' ? '#12161e' : '#f4f6f9');
}

export function setTheme(theme: Theme): void {
  document.documentElement.dataset.theme = theme;
  document.querySelector<HTMLMetaElement>('meta[name="theme-color"]')?.setAttribute('content', theme === 'dark' ? '#12161e' : '#f4f6f9');
  try {
    localStorage.setItem(storageKey, theme);
  } catch {
    // 本地存储不可用时，当前页面仍能切换外观。
  }
}
