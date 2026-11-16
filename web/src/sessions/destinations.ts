// 中文说明：目的地校验状态与纯判断；通过检查只代表位置确认，不代表已批准对话操作。
export type OperationDestinations = {
  source_root: string;
  key: string;
  mode: 'native' | 'custom' | 'isolation';
  trash_path: string;
  trash_verified: boolean;
  archive_path: string;
  archive_confirmed: boolean;
  reason?: string;
  probe?: { id: string; file_name: string; trash_path?: string; state: string; created_at: string };
};

export function archiveLocationMissing(options: { archive: boolean; archive_local?: boolean; archive_remote?: boolean }): boolean {
  return options.archive && !options.archive_local && !options.archive_remote;
}

export function destinationsReady(status: OperationDestinations | null, options: { archive: boolean; archive_local?: boolean; archive_remote?: boolean; trash: boolean }, root: string): boolean {
  if (archiveLocationMissing(options)) return false;
  const local = options.archive && options.archive_local;
  return Boolean(status && status.source_root === root && (!options.trash || status.trash_verified) && (!local || status.archive_confirmed));
}
