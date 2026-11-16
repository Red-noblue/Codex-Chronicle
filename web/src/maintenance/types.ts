// 中文说明：存储维护页面的只读快照、候选和影响预览类型，执行权限单独由服务端返回。
export type MaintenanceCandidate = {
  relative_path: string; category: string; action: string; reason: string;
  size_bytes: number; mod_time: string; status: string; risk: string;
  recoverable: boolean; eligible: boolean; block_reason?: string;
};
export type MaintenanceReport = {
  root: string; generated_at: string; write_enabled: boolean; stale: boolean;
  source_file_count: number; source_total_bytes: number;
  candidate_count: number; candidate_bytes: number; eligible_count: number; blocked_count: number;
  filtered_count: number; filtered_bytes: number;
  categories: Array<{ category: string; count: number; size_bytes: number }>;
  directories: MaintenanceDirectory[];
  ungrouped_count: number; ungrouped_blocked_count: number;
  candidate_offset: number; candidate_limit: number; has_more: boolean; candidates: MaintenanceCandidate[];
};
export type MaintenanceDirectory = {
  relative_path: string; category: string; file_count: number; size_bytes: number;
  eligible_count: number; eligible_bytes: number; retained_count: number; retained_bytes: number;
};
export type CleanupCandidate = {
  artifact_id: string; kind: string; target_path: string; manifest_path: string;
  created_at: string; owner: string; operation_id?: string; last_operation_id?: string; lease_until?: string;
  source: string; in_use: boolean; in_use_evidence?: string; process_probe_available: boolean;
  rebuildable: boolean; expires_at: string; risk: string; retention_reason: string;
  size_bytes: number; file_count: number; status: string; eligible: boolean; block_reason?: string;
  active_workspace: boolean; recovery_journal: boolean; remote_archive_verified: boolean;
  quarantine_path?: string; purge_after?: string;
};
export type UnknownArtifact = { path: string; size_bytes: number; kind: string; reason: string; risk: string };
export type CleanupRecovery = { operation_id: string; journal_path: string; phase: string; created_at: string; updated_at: string; needs_review: boolean; reason?: string };
export type RetentionPolicy = {
  version: number; default_ttl_seconds: number; staging_ttl_seconds: number;
  failed_ttl_seconds: number; expired_copy_ttl_seconds: number; runtime_cache_ttl_seconds: number;
  purge_ttl_seconds: number; unknown_report_only: boolean;
};
export type CleanupReport = {
  read_only: boolean; registry_root: string; generated_at: string; policy: RetentionPolicy;
  candidate_count: number; candidate_bytes: number; candidates: CleanupCandidate[]; retained: CleanupCandidate[]; quarantined: CleanupCandidate[];
  unknown: UnknownArtifact[]; crash_residues: UnknownArtifact[]; recoveries: CleanupRecovery[]; errors?: string[];
};
export type CleanupManagement = {
  enabled: boolean; settings_writable: boolean; policy_write_enabled: boolean;
  registration_enabled: boolean; automatic_cleanup_enabled: boolean; block_reason?: string;
};
export type CleanupResponse = { available: boolean; write_enabled: boolean; scanned_at: string; cleanup: CleanupReport; management: CleanupManagement; error?: string };
export type MaintenancePreview = {
  blocked: boolean; error?: string;
  selection?: { directories: MaintenanceDirectory[]; file_count: number; size_bytes: number; retained_count: number; retained: Array<{ relative_path: string; reason: string }> };
  plan?: { operation_id: string; root: string; status: string; write_enabled: boolean; block_reason?: string; assets: Array<{ relative_path: string; action: string; size_bytes: number; sha256: string }> };
  result?: OperationResult;
};
export type CleanupPreview = {
  blocked: boolean; error?: string;
  plan?: { operation_id: string; registry_root: string; status: string; write_enabled: boolean; block_reason?: string; items: Array<{ artifact_id: string; manifest: CleanupCandidate; before: { size_bytes: number; file_count: number } }> };
  result?: OperationResult;
};
export type OperationResult = { operation_id?: string; phase: string; needs_review: boolean; journal_path?: string };
export type MaintenanceRow = {
  id: string; path: string; kind: string; size: number; modified: string; status: string;
  eligible: boolean; reason: string; risk: string;
  source?: MaintenanceCandidate; artifact?: CleanupCandidate; unknown?: UnknownArtifact;
};
