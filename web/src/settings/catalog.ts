// 中文说明：设置页的分类与检索目录；只描述界面，不保存业务状态或调用后端。
export type SettingsCategory = 'appearance' | 'workspaces' | 'search-index' | 'archives' | 'remote' | 'system';
export type SettingsItem = {
  category: SettingsCategory;
  title: string;
  description: string;
  scope: string;
  keywords: string;
};

export const settingsCategories: { id: SettingsCategory; label: string; description: string }[] = [
  { id: 'appearance', label: '外观与阅读', description: '调整并保存阅读体验。' },
  { id: 'workspaces', label: '工作区', description: '管理源目录和独立归档数据库。' },
  { id: 'search-index', label: '搜索索引', description: '管理当前工作区的正文检索加速。' },
  { id: 'archives', label: '归档与恢复', description: '检查回收站、制作副本和恢复已有归档。' },
  { id: 'remote', label: '远端归档', description: '管理远端连接、目标仓库和归档批次。' },
  { id: 'system', label: '系统与更新', description: '查看兼容能力、更新预检和版本切换。' }
];

export const settingsItems = {
  density: { category: 'appearance', title: '界面密度', description: '调整会话列表与模块留白，正文字号保持不变。', scope: '服务设备 · 自动保存', keywords: '外观 紧凑 舒适 阅读 字体 间距 布局 density' },
  workspaces: { category: 'workspaces', title: '工作区目录', description: '识别 Codex 数据目录，或手动添加其他读取源。', scope: '服务设备 · 自动保存', keywords: '目录 路径 注册 识别 自动 CODEX_HOME 源根 数据库 archive.db workspace' },
  searchIndex: { category: 'search-index', title: '索引管理', description: '选择索引内容和空间预算。', scope: '当前工作区', keywords: '搜索 索引 空间 预算 容量 清理 增量 存储 性能 search index' },
  adapter: { category: 'system', title: '数据格式兼容性', description: '检查会话能否读取，设置原始记录保护。', scope: '当前工作区 · 自动保存', keywords: 'schema codex 版本 适配 adapter 兼容 只读 保护 策略 自动识别' },
  trash: { category: 'archives', title: '系统回收站', description: '检查系统候选路径，必要时手动纠正。', scope: '本机服务 · 显式保存', keywords: '回收站 垃圾箱 Trash 删除 路径' },
  copy: { category: 'archives', title: '稳定副本', description: '先生成只读计划，再确认制作外部副本。', scope: '操作 · 单独确认', keywords: '复制 备份 排除 制作 计划 copy' },
  recovery: { category: 'archives', title: '中断操作恢复', description: '查看 journal 和需要人工复核的恢复任务。', scope: '操作 · 单独确认', keywords: '崩溃 journal 回滚 待恢复 断电' },
  restore: { category: 'archives', title: '本地归档恢复', description: '选择已有归档，检查目标冲突后再确认恢复。', scope: '操作 · 单独确认', keywords: 'ArchiveRoot 归档 密码 解密 续接 restore' },
  connection: { category: 'remote', title: '连接与账户', description: '配置 GitHub、检查网络并绑定账户。', scope: '服务设备 · 保存后生效', keywords: 'GitHub 设备 绑定 账户 OAuth Client ID 认证 连接 代理 网络 超时 分片' },
  repositories: { category: 'remote', title: '归档仓库', description: '选择或创建 GitHub 私有仓库作为归档目标。', scope: '远端服务 · 显式操作', keywords: 'GitHub 仓库 私有 创建 目标 repository' },
  remoteArchives: { category: 'remote', title: '远端归档批次', description: '查看、验证或下载已有远端归档。', scope: '操作 · 单独确认', keywords: '远端 provider 下载 分片 校验 删除 恢复' },
  platform: { category: 'system', title: '平台能力', description: '查看当前系统的只读探测结果与能力限制。', scope: '只读状态', keywords: '平台 OS Linux Windows macOS 锁 探针 权限' },
  updates: { category: 'system', title: '应用更新', description: '检查版本、预检更新包，确认后再切换。', scope: '操作 · 单独确认', keywords: '更新 离线 在线 feed 版本 签名 回滚 ZIP TAR' }
} satisfies Record<string, SettingsItem>;

export type SettingsItemId = keyof typeof settingsItems;

export function findSettings(query: string, category: SettingsCategory): SettingsItemId[] {
  const terms = query.trim().toLocaleLowerCase().split(/\s+/).filter(Boolean);
  return (Object.keys(settingsItems) as SettingsItemId[]).filter(id => {
    const item = settingsItems[id];
    if (!terms.length) return item.category === category;
    const text = `${item.title} ${item.description} ${item.keywords}`.toLocaleLowerCase();
    return terms.every(term => text.includes(term));
  });
}
