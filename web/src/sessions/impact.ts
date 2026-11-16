// 中文说明：将执行预览投影为文件、数据库行和共享文本影响；共享数据库大小不计入待处理文件。
export type ImpactFile = { relative_path: string; size_bytes: number };
export type DiscoveredImpactFile = ImpactFile & { kind: string; owners: number };
export type RestrictedImpact = {
  assets: Array<{ id: string; relative: string; bytes: number }>;
  sqlite_objects_and_rows: Array<{
    database: string;
    table: string;
    row_count: number;
    rows: Array<{ owner: string; primary_key: unknown[] }>;
  }>;
  text_lines: Record<string, { removed: Array<{ id: string; line: number }> }>;
  recovery_path: string;
  permanent_delete?: boolean;
  trash_package?: { format: string; relative: string } | null;
};

export function summarizeImpact(sourceID: string, plannedFiles: ImpactFile[] = [], discoveredFiles: DiscoveredImpactFile[] = [], preview?: RestrictedImpact) {
  const files = preview
    ? preview.assets.filter(asset => asset.id === sourceID).map(asset => ({ relative_path: asset.relative, size_bytes: asset.bytes }))
    : plannedFiles;
  const databases = new Map<string, Array<{ table: string; count: number }>>();
  for (const group of preview?.sqlite_objects_and_rows || []) {
    const count = group.rows.filter(row => row.owner === sourceID).length;
    if (!count) continue;
    const tables = databases.get(group.database) || [];
    tables.push({ table: group.table, count });
    databases.set(group.database, tables);
  }
  const texts = Object.entries(preview?.text_lines || {}).map(([path, value]) => ({
    path, lines: value.removed.filter(line => line.id === sourceID).map(line => line.line)
  })).filter(text => text.lines.length);
  return {
    files,
    fileBytes: files.reduce((total, file) => total + file.size_bytes, 0),
    databases: Array.from(databases, ([path, tables]) => ({ path, tables, rows: tables.reduce((total, table) => total + table.count, 0) })),
    rowCount: Array.from(databases.values()).flat().reduce((total, table) => total + table.count, 0),
    texts,
    textLineCount: texts.reduce((total, text) => total + text.lines.length, 0),
    sharedFiles: discoveredFiles.filter(file => file.kind === 'sqlite' || file.kind === 'session_index_jsonl' || file.owners > 1)
  };
}

export function impactBlockerMessage(error: string): string {
  if (/关联父会话仍被 daemon 加载/.test(error)) return '目标或关联父会话仍在使用中。请结束对应会话后重新检查。';
  if (/spawn.*冲突|spawn.*存在环|子代理.*冲突/.test(error)) return '子代理关系存在冲突，需要先核对父子关系。';
  if (/spawn.*变化/.test(error)) return '子代理关系在预览后变化，请重新检查影响。';
  if (/busy|locked|共享数据库或索引仍被占用/i.test(error)) return '数据库或索引正在被占用。请稍后重新检查。';
  if (/目标仍被 daemon 加载|active_writing|writer|正在写入|仍被.*加载/i.test(error)) return '对话仍在使用中。请结束或退出该对话后重新检查。';
  if (/schema|额外数据库|未适配/i.test(error)) return '当前数据格式尚未支持，需要更新兼容规则。';
  if (/附件|记忆输出|spawned|attached thread|引用目标|未建模/i.test(error)) return '发现尚未支持的附件或会话关联，需要先核对关联数据。';
  if (/已变化|发生变化|指纹|身份不匹配/i.test(error)) return '数据在检查期间发生变化，请重新检查影响。';
  if (/备份|恢复材料/i.test(error)) return '恢复材料未通过检查，暂时不能执行。';
  return error;
}
