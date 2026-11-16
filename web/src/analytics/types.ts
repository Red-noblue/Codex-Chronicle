// 中文说明：统计页只读数据、筛选与下钻契约；不包含生命周期操作或源文件写权限。
export type NamedCount = { name: string; count: number; total_tokens?: number; total_bytes?: number };
export type DailyUsage = { date: string; conversations: number; messages: number; turns: number; total_tokens: number };
export type AnalyticsReport = {
  read_only?: boolean;
  generated_at?: string;
  file_count: number;
  total_bytes: number;
  conversation_count: number;
  stable_count: number;
  blocked_count: number;
  token_usage: { input_tokens: number; cached_input_tokens: number; output_tokens: number; reasoning_output_tokens: number; total_tokens: number };
  message_count: number;
  turn_count: number;
  daily: DailyUsage[];
  models?: NamedCount[];
  projects?: NamedCount[];
  statuses?: NamedCount[];
  filtered?: boolean;
  stale?: boolean;
  snapshot_status?: string;
  source_file_count?: number;
  source_total_bytes?: number;
  available_from?: string;
  available_to?: string;
  undated_conversation_count?: number;
};
export type AnalyticsView = {
  range: '7d' | '30d' | '90d' | 'all' | 'custom';
  from: string;
  to: string;
  model: string;
  project: string;
  metric: 'total_tokens' | 'conversations' | 'messages';
};
export type AnalyticsSelection = { from: string; to: string; model: string; project: string; status?: string };
export type TrendBucket = Omit<DailyUsage, 'date'> & { from: string; to: string; days: number };
