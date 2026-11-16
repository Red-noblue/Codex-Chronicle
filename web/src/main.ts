// 中文说明：恢复浏览器主题并挂载 Codex Chronicle 的本地工作台。
import { mount } from 'svelte';
import App from './App.svelte';
import { initializeTheme } from './theme';
import './themes.css';
import './ui-tokens.css';
import './app.css';
import './settings/settings.css';
import './sessions/sessions.css';
import './pending.css';
import './analytics/analytics.css';
import './maintenance/maintenance.css';

initializeTheme();
const app = mount(App, { target: document.getElementById('app')! });

export default app;

