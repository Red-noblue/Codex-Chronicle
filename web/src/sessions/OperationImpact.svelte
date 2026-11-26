<!-- 中文说明：用简短结论和按需展开的清单展示操作影响，不把共享数据库整文件误算成可释放空间。 -->
<script lang="ts">
  import { summarizeImpact, type ImpactFile, type DiscoveredImpactFile, type RestrictedImpact } from './impact';

  export let sourceID: string;
  export let files: ImpactFile[] = [];
  export let discoveredFiles: DiscoveredImpactFile[] = [];
  export let preview: RestrictedImpact | undefined = undefined;
  export let action: string;
  export let prepared = false;
  export let completed = false;
  export let blocked = false;
  export let formatBytes: (value: number) => string;

  const tableLabels: Record<string, string> = {
    threads: '会话资料', thread_items: '历史消息与事件', thread_turns: '对话轮次',
    thread_history_projection_state: '历史读取状态', thread_read_receipts: '阅读记录',
    thread_dynamic_tools: '对话工具', thread_goals: '对话目标', thread_goal_events: '目标事件',
    jobs: '任务记录', logs: '日志'
  };
  $: impact = summarizeImpact(sourceID, files, discoveredFiles, preview);
</script>

<section class="operation-impact" aria-label="文件与记录影响">
  {#if impact.files.length}
    <p class="file-conclusion">{completed ? '计划文件' : prepared ? action : '待核对文件'}：<strong>{impact.files.length} 个文件</strong><span>{formatBytes(impact.fileBytes)}</span></p>
  {:else}
    <p class="file-conclusion">{blocked ? '检查已停止，未生成执行清单' : '尚未生成可执行的文件清单'}</p>
  {/if}
  {#if preview}
    <p class="record-conclusion">{completed ? '计划清理' : '将清理'} <strong>{impact.rowCount} 条数据库记录</strong>{#if impact.textLineCount}<span>、{impact.textLineCount} 行索引 / 历史记录</span>{/if}</p>
  {:else if impact.sharedFiles.length}
    <p class="record-conclusion">发现共享文件，具体记录处理范围尚未确认。</p>
  {/if}
  {#if impact.files.length || preview || discoveredFiles.length}
  <details class="impact-detail-list">
    <summary>查看文件与记录清单</summary>
    {#if impact.files.length}
      <div class="impact-group">
        <h4>对话文件</h4>
        {#each impact.files as file}
          <div class="impact-file"><code>{file.relative_path}</code><span>{formatBytes(file.size_bytes)}</span></div>
        {/each}
      </div>
    {/if}
    {#if preview}
      <div class="impact-group">
        <h4>共享数据库：保留文件，只清理目标记录</h4>
        {#if impact.databases.length}
          {#each impact.databases as database}
            <div class="impact-database"><div><code>{database.path}</code><strong>{database.rows} 条</strong></div>
              <ul>{#each database.tables as table}<li><span>{tableLabels[table.table] || table.table}</span><span>{table.count} 条</span></li>{/each}</ul>
            </div>
          {/each}
        {:else}<p>本次没有匹配到需清理的数据库记录。</p>{/if}
      </div>
      {#if impact.texts.length}
        <div class="impact-group"><h4>共享索引与输入历史：仅移除目标行</h4>{#each impact.texts as text}<div class="impact-file"><code>{text.path}</code><span>{text.lines.length} 行</span></div>{/each}</div>
      {/if}
    {:else if discoveredFiles.length}
      <div class="impact-group"><h4>扫描到的关联文件（不代表获准处理）</h4>{#each discoveredFiles as file}<div class="impact-file"><code>{file.relative_path}</code><span>{file.kind === 'sqlite' ? '共享数据库' : file.owners > 1 ? '多个会话共用' : formatBytes(file.size_bytes)}</span></div>{/each}</div>
    {/if}
  </details>
  {/if}
</section>

<style>
  .operation-impact { display: grid; gap: 5px; min-width: 0; color: var(--text-secondary); font-size: var(--font-caption); line-height: 1.6; }
  p { margin: 0; }
  .file-conclusion { display: flex; flex-wrap: wrap; align-items: baseline; gap: 3px 8px; }
  strong { color: var(--text-primary); font-weight: 600; font-variant-numeric: tabular-nums; }
  .record-conclusion { color: var(--text-muted); }
  details { min-width: 0; }
  summary { padding-block: 5px; cursor: pointer; }
  .impact-group { display: grid; gap: 6px; min-width: 0; margin-block: 8px 12px; }
  h4 { margin: 0; color: var(--text-secondary); font-size: inherit; font-weight: 600; }
  .impact-file, .impact-database > div, li { display: flex; align-items: baseline; justify-content: space-between; gap: 12px; min-width: 0; }
  .impact-file > span, li > span:last-child { flex-shrink: 0; font-variant-numeric: tabular-nums; }
  code { min-width: 0; color: var(--text-secondary); font-size: inherit; overflow-wrap: anywhere; }
  .impact-file { padding-block: 4px; border-bottom: 1px solid var(--border); }
  .impact-database { min-width: 0; padding-block: 4px; }
  ul { display: grid; gap: 3px; margin: 5px 0 0; padding: 0 0 0 12px; list-style: none; color: var(--text-muted); }
  summary:focus-visible { outline: 2px solid var(--accent); outline-offset: 2px; }
  @media (max-width: 479px) { .impact-file { flex-wrap: wrap; gap: 2px 8px; } }
</style>
