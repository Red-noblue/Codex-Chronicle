// 中文说明：统一一次性确认与执行任务的数据约定，确认码和密码不写入浏览器持久存储。
export type ExecutionConfirmation = {
  confirmation_id: string;
  preview_id: string;
  code: string;
  additional_code?: string;
  expires_at: string;
  batch_size: number;
  total: number;
};

export type ExecutionTask = {
  id: string;
  preview_id: string;
  source_root: string;
  updated_at?: string;
  state: 'running' | 'completed' | 'stopped' | 'needs_review';
  batch_size: number;
  current_batch: number;
  current_item: number;
  completed: number;
  stop_requested: boolean;
  error?: string;
  recovery_path?: string;
  recovery_command?: string;
  restricted: boolean;
  sequential?: boolean;
  recovery_purged?: boolean;
  packaged_trash?: boolean;
  trash_package_path?: string;
  strategy: { trash: boolean; permanent_delete: boolean; archive: boolean };
  preview: { items: Array<{ conversation_id: string; source_id?: string; title?: string; completed?: boolean; result?: { journal_path?: string; phase?: string; needs_review?: boolean } }> };
};

export const executionStateLabels: Record<ExecutionTask['state'], string> = {
  running: '正在处理', completed: '已完成', stopped: '已停止后续项', needs_review: '需要人工复核'
};

export function executionResultLabel(task: ExecutionTask): string {
  if (task.packaged_trash && task.strategy.trash && !task.trash_package_path && task.completed) {
    return `已清理 ${task.completed} 个对话的源数据，回收文件夹待确认`;
  }
  const action = task.strategy.permanent_delete ? '已删除源版本' : task.strategy.trash ? '已移入回收站' : task.strategy.archive ? '已归档' : '已处理';
  return `${action} ${task.completed} 个对话${task.state === 'completed' ? '' : '，其余项未完成'}`;
}
