// 中文说明：只在当前浏览器标签页内按源目录保存会话选择；选择是待审阅草稿，不包含密码、资产批准或写入授权。
import type { ExecutionTask } from './execution';

type SelectionStorage = Pick<Storage, 'getItem' | 'setItem' | 'removeItem'>;
const key = (root: string) => `codex-chronicle.selection.v1:${root}`;

export function readConversationSelection(storage: SelectionStorage, root: string): Set<string> {
  if (!root) return new Set();
  const raw = storage.getItem(key(root));
  if (!raw) return new Set();
  const saved = JSON.parse(raw);
  if (saved?.version !== 1 || saved.root !== root || !Array.isArray(saved.ids)) return new Set();
  return new Set(saved.ids.filter((id: unknown): id is string => typeof id === 'string' && id.length > 0 && id.length <= 256));
}

export function writeConversationSelection(storage: SelectionStorage, root: string, ids: Set<string>): void {
  if (!root) return;
  if (!ids.size) { storage.removeItem(key(root)); return; }
  storage.setItem(key(root), JSON.stringify({ version: 1, root, ids: Array.from(ids) }));
}

// 只采用当前源目录中逐项确认完成的移除结果；失败项、仅归档和其他工作区不能清空选择。
export function completedRemovalIDs(root: string, tasks: ExecutionTask[], snapshotAt?: string): Set<string> {
  const snapshotTime = Date.parse(snapshotAt || '');
  return new Set(tasks.filter(task => task.source_root === root && (task.strategy.trash || task.strategy.permanent_delete)
    // 新快照可能包含用户后来恢复的会话，历史删除记录不能把它们永久隐藏。
    && (!Number.isFinite(snapshotTime) || !task.updated_at || Date.parse(task.updated_at) > snapshotTime))
    .flatMap(task => task.preview.items.filter(item => item.completed).map(item => item.conversation_id)));
}
