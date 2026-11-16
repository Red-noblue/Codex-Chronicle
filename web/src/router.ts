// 中文说明：集中解析前端页面路径并生成规范链接，不负责页面状态或数据请求。
import type { SettingsCategory } from './settings/catalog';

export type RoutePage = 'home' | 'sessions' | 'archives' | 'pending' | 'analytics' | 'maintenance' | 'settings';
export type AppRoute =
  | { page: 'home' }
  | { page: 'sessions' }
  | { page: 'archives' }
  | { page: 'session'; conversationID: string }
  | { page: 'pending' }
  | { page: 'analytics' }
  | { page: 'maintenance' }
  | { page: 'settings'; category: SettingsCategory }
  | { page: 'not-found'; pathname: string };

const settingsCategories = new Set<SettingsCategory>(['appearance', 'workspaces', 'archives', 'remote', 'system']);

export function parseRoute(pathname: string): AppRoute {
  const normalizedPath = pathname.replace(/\/+$/, '') || '/';
  if (normalizedPath === '/') return { page: 'home' };

  let segments: string[];
  try {
    segments = normalizedPath.slice(1).split('/').map((segment) => decodeURIComponent(segment));
  } catch {
    return { page: 'not-found', pathname: normalizedPath };
  }
  if (segments.some((segment) => !segment || segment.includes('/') || segment.includes('\\'))) {
    return { page: 'not-found', pathname: normalizedPath };
  }

  if (segments.length === 1) {
    switch (segments[0]) {
      case 'sessions': return { page: 'sessions' };
      case 'archives': return { page: 'archives' };
      case 'pending': return { page: 'pending' };
      case 'analytics': return { page: 'analytics' };
      case 'maintenance': return { page: 'maintenance' };
      case 'settings': return { page: 'settings', category: 'appearance' };
    }
  }
  if (segments.length === 2 && segments[0] === 'sessions') {
    return { page: 'session', conversationID: segments[1] };
  }
  if (segments.length === 2 && segments[0] === 'settings' && settingsCategories.has(segments[1] as SettingsCategory)) {
    return { page: 'settings', category: segments[1] as SettingsCategory };
  }
  return { page: 'not-found', pathname: normalizedPath };
}

export function routePath(route: AppRoute): string {
  switch (route.page) {
    case 'home': return '/';
    case 'sessions': return '/sessions';
    case 'archives': return '/archives';
    case 'session': return `/sessions/${encodeURIComponent(route.conversationID)}`;
    case 'pending': return '/pending';
    case 'analytics': return '/analytics';
    case 'maintenance': return '/maintenance';
    case 'settings': return `/settings/${route.category}`;
    case 'not-found': return route.pathname;
  }
}
