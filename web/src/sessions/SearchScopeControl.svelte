<!-- 中文说明：共享会话搜索方式按钮，供结果列表和会话筛选使用。 -->
<script lang="ts">
  import HelpBubble from '../HelpBubble.svelte';
  import type { SearchScope } from './view-state';

  export let value: SearchScope;
  export let helpID: string;
  export let onChange: (value: SearchScope) => void;
  export let helpDetail = '';
  export let onHelpOpen: (() => void) | undefined = undefined;

  const options: Array<{ key: SearchScope; label: string }> = [
    { key: '', label: '全部搜索' },
    { key: 'indexed', label: '仅查索引' },
    { key: 'source', label: '直接读原文' }
  ];
  const help = '全部搜索会利用索引，并扫描原文补齐。\n仅查索引只看已加速的范围。\n直接读原文会逐个读取会话文件。';
</script>

<div class="search-scope-control">
  <div class="search-scope-options" role="group" aria-label="搜索方式">
    {#each options as option (option.key)}
      <button type="button" aria-pressed={value === option.key} on:click={() => onChange(option.key)}>{option.label}</button>
    {/each}
  </div>
  <HelpBubble id={helpID} label="搜索方式与索引覆盖" text={helpDetail ? help + '\n\n' + helpDetail : help} onOpen={onHelpOpen} />
</div>

<style>
  .search-scope-control { display: flex; align-items: center; flex-wrap: wrap; gap: var(--space-1); min-width: 0; }
  .search-scope-options { display: inline-flex; flex-wrap: wrap; max-width: 100%; gap: 2px; padding: 2px; border: 1px solid var(--border); border-radius: var(--radius-control); background: var(--bg-subtle); }
  .search-scope-options button {
    min-height: var(--control-sm);
    padding: 0 var(--space-2);
    border: 1px solid transparent;
    border-radius: calc(var(--radius-control) - 2px);
    background: transparent;
    color: var(--text-secondary);
    cursor: pointer;
    font: inherit;
    font-size: var(--font-caption);
    white-space: nowrap;
  }
  .search-scope-options button:hover { background: var(--bg-hover); }
  .search-scope-options button[aria-pressed="true"] {
    border-color: var(--border-strong);
    background: var(--bg-surface);
    color: var(--accent);
    box-shadow: var(--shadow);
    font-weight: 600;
  }
  @media (pointer: coarse) { .search-scope-options button { min-height: var(--control-touch); } }
</style>
