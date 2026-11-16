// 中文说明：定义完整归档预览和归档副本列表使用的最小页面数据类型。
export type ArchivePreview = {
  id?: string;
  digest: string;
  ids: string[];
  requested_ids: string[];
  assets: Array<{ relative: string; id: string; bytes: number; sha256: string }>;
  source_bytes: number;
  database_rows: Array<{ database: string; table: string; rows: number }>;
  index_lines: Record<string, number>;
  target?: string;
  expires_at?: string;
};

export type ArchiveTask = {
  id: string;
  archive_id: string;
  state: string;
  source_root: string;
  source_ids: string[];
  titles?: string[];
  target?: string;
  preview_digest: string;
  created_at: string;
  updated_at: string;
  completed_bytes?: number;
  total_bytes?: number;
  source_bytes: number;
  compression?: { format: string; conversation_ids: string[]; original_bytes: number; compressed_bytes: number; snapshot_sha256: string };
  error?: string;
  cache_retained: boolean;
  local_copy?: boolean;
  remote_copy?: boolean;
  encrypted?: boolean;
  local_path?: string;
  remote_deleted?: boolean;
  management_state?: string;
  management_action?: string;
  management_error?: string;
  restore_status?: string;
  restore_source?: 'local' | 'remote' | string;
  restore_target_digest?: string;
  restored_at?: string;
  restore_checks?: Partial<Record<'local' | 'remote', { status: string; checked_at: string }>>;
};

export type ArchiveCatalogItem = { task: ArchiveTask; local_available: boolean; remote_available: boolean };
export type ArchiveManagementPreview = {
  preview_id: string;
  task_id: string;
  action: string;
  source: 'local' | 'remote';
  code?: string;
  expires_at: string;
  update_if_newer?: boolean;
  update_available?: boolean;
  recover_update?: boolean;
  recover_available?: boolean;
  update_reason?: string;
  update_from_at?: string;
  update_to_at?: string;
  restore?: {
    status: string;
    archive_version_sha256?: string;
    current_version_sha256?: string;
    update_reason?: string;
    ids: string[];
    files: Array<{ relative: string; bytes: number }>;
    database_rows: Array<{ database: string; table: string; rows: number }>;
    index_lines: Record<string, number>;
    conflicts: string[];
  };
};
