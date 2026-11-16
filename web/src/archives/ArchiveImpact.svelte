<!-- 中文说明：紧凑展示完整归档对话、物理资产、目标数据库行和索引记录数量。 -->
<script lang="ts">
  import { Archive, Cloud, Database, HardDrive, ShieldCheck } from '@lucide/svelte';
  import type { ArchivePreview } from './types';

  export let preview: ArchivePreview;
  export let local = false;
  export let remote = false;
  export let encrypted = false;
  export let formatBytes: (value: number) => string;
  $: rowCount = preview.database_rows.reduce((total, item) => total + item.rows, 0);
  $: indexCount = Object.values(preview.index_lines).reduce((total, count) => total + count, 0);
</script>

<section class="archive-impact" aria-label="归档影响">
  <div class="archive-impact-head"><Archive aria-hidden="true" /><strong>{preview.ids.length} 个完整对话</strong><span>{formatBytes(preview.source_bytes)}</span></div>
  <div class="archive-impact-items">
    <span><HardDrive aria-hidden="true" />{preview.assets.length} 个会话文件</span>
    <span><Database aria-hidden="true" />{rowCount} 条专属数据库记录</span>
    <span><Database aria-hidden="true" />{indexCount} 条索引记录</span>
    {#if encrypted}<span><ShieldCheck aria-hidden="true" />客户端加密</span>{/if}
    {#if local}<span><HardDrive aria-hidden="true" />本地副本</span>{/if}
    {#if remote}<span><Cloud aria-hidden="true" />云端副本{#if preview.target} · {preview.target}{/if}</span>{/if}
  </div>
  <details><summary>查看归档清单</summary>
    <strong>会话文件</strong><div>{#each preview.assets as asset}<div class="archive-asset"><code>{asset.relative}</code><span>{formatBytes(asset.bytes)}</span></div>{/each}</div>
    <strong>数据库记录</strong><div>{#each preview.database_rows as row}<div class="archive-asset"><code>{row.database} · {row.table}</code><span>{row.rows} 行</span></div>{/each}</div>
    <strong>索引记录</strong><div>{#each Object.entries(preview.index_lines) as [name, lines]}<div class="archive-asset"><code>{name}</code><span>{lines} 行</span></div>{/each}</div>
  </details>
</section>

<style>
  .archive-impact { display: grid; gap: var(--space-2); min-width: 0; padding: var(--space-3); border: 1px solid var(--border); border-radius: var(--radius-control); background: var(--bg-subtle); }
  .archive-impact-head, .archive-impact-items, .archive-impact-items span, .archive-asset { display: flex; align-items: center; gap: var(--space-2); min-width: 0; }
  .archive-impact-head { font-size: var(--font-ui); }
  .archive-impact-head > span { margin-inline-start: auto; color: var(--text-secondary); font-variant-numeric: tabular-nums; }
  .archive-impact :global(svg) { flex: 0 0 15px; width: 15px; height: 15px; color: var(--text-muted); }
  .archive-impact-items { flex-wrap: wrap; color: var(--text-secondary); font-size: var(--font-caption); }
  .archive-impact-items span { max-width: 100%; }
  details { border-top: 1px solid var(--border); padding-top: var(--space-2); color: var(--text-muted); font-size: var(--font-caption); }
  summary { cursor: pointer; }
  .archive-asset { justify-content: space-between; gap: var(--space-3); padding-top: var(--space-2); }
  code { min-width: 0; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; color: var(--text-secondary); }
  .archive-asset span { flex-shrink: 0; font-variant-numeric: tabular-nums; }
</style>
