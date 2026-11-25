<!-- 中文说明：管理当前工作区的索引内容、空间预算和增量构建。 -->
<script lang="ts">
  import { onMount, onDestroy } from 'svelte';
  import { Database, LoaderCircle, RefreshCw, Square, Trash2 } from '@lucide/svelte';
  import HelpBubble from '../HelpBubble.svelte';
  import IndexContentTree, { indexContentLeaves, type IndexContentNode } from './IndexContentTree.svelte';

  type IndexAction = 'save' | 'build' | 'stop' | 'clear' | 'compact';
  type IndexSettings = { budget_bytes: number; automatic: boolean; content?: string[]; recent_context?: boolean };
  type IndexStatus = {
    settings: IndexSettings; path: string; database_bytes: number; used_bytes: number;
    reclaimable_bytes: number; documents: number; indexed_files: number; catalog_files: number;
    covered_bytes: number; source_bytes: number; over_budget: boolean; legacy_bytes: number; rebuild_required?: boolean;
    partial_files?: number; partial_bytes?: number;
  };
  type IndexTask = { root: string; status: string; phase: string; error?: string;
    progress: { files_done: number; files_total: number; documents_added: number; budget_reached: boolean } };
  type IndexResponse = {
    available: boolean; root?: string; index?: IndexStatus; task?: IndexTask; busy?: boolean; error?: string;
    content_tree?: IndexContentNode[]; compact_content?: string[];
  };

  export let request: typeof fetch;
  let state: IndexResponse | null = null;
  let budgetMiB = 512;
  let automatic = false;
  let recentContext = false;
  let content: string[] | undefined;
  let saved = '';
  let loading = true;
  let refreshing = false;
  let pendingAction: IndexAction | null = null;
  let message = '';
  let failed = false;
  let confirmingClear = false;
  let timer: ReturnType<typeof setTimeout> | undefined;
  let disposed = false;
  let requestVersion = 0;
  $: settings = { budget_bytes: budgetMiB * 1024 * 1024, automatic, content, recent_context: recentContext };
  $: dirty = JSON.stringify(settings) !== saved;
  $: acting = pendingAction !== null;
  $: busy = acting || refreshing || !!state?.busy;
  $: taskHere = !!state?.task?.root && state.task.root === state.root;
  $: valid = Number.isInteger(budgetMiB) && budgetMiB >= 1 && budgetMiB <= 65536 && content?.length !== 0;
  $: index = state?.index;
  $: unbuilt = !!index && !index.documents && !index.indexed_files && !index.partial_files && !index.rebuild_required;
  $: compactBlocked = busy ? '请等待当前索引操作完成' : dirty ? '请先保存索引设置' : index?.reclaimable_bytes === 0 ? '当前没有可释放的索引空间' : '';
  $: nodes = state?.content_tree ?? [];
  $: compact = state?.compact_content ?? [];
  $: allContent = indexContentLeaves(nodes).sort();
  $: selectedContent = new Set(content ?? []);
  $: rangeApplicable = content === undefined || indexContentLeaves(
    nodes.find(node => node.id === 'dialogue')?.children ?? []
  ).some(id => id !== 'summary' && selectedContent.has(id));
  $: changedContent = JSON.stringify(content) !== JSON.stringify(index?.settings.content);
  $: changedScope = changedContent || recentContext !== !!index?.settings.recent_context;
  $: usedPercent = index ? Math.min(100, index.database_bytes / index.settings.budget_bytes * 100) : 0;
  $: taskLabel = state?.busy && !taskHere ? '其他工作区正在构建索引'
    : state?.task?.status === 'running' ? (state.task.phase === 'scan' ? '正在扫描会话' : '正在增量构建')
    : state?.task?.status === 'budget_reached' ? '已达到索引预算'
    : state?.task?.status === 'paused' ? '已停止，可继续增量构建'
    : state?.task?.status === 'completed' && !index?.rebuild_required ? '本次构建完成'
    : state?.task?.status === 'failed' ? '构建未完成' : '';

  function bytes(value: number) {
    if (value >= 1024 ** 3) return `${(value / 1024 ** 3).toFixed(2)} GiB`;
    if (value >= 1024 ** 2) return `${(value / 1024 ** 2).toFixed(1)} MiB`;
    return `${Math.ceil(value / 1024)} KiB`;
  }

  function accept(body: IndexResponse, resetDraft = false) {
    const rootChanged = state?.root !== body.root;
    state = body;
    if (body.index && (resetDraft || rootChanged || !saved || !dirty)) {
      budgetMiB = body.index.settings.budget_bytes / 1024 ** 2;
      automatic = body.index.settings.automatic;
      recentContext = !!body.index.settings.recent_context;
      content = body.index.settings.content;
      saved = JSON.stringify({ budget_bytes: body.index.settings.budget_bytes, automatic, content, recent_context: recentContext });
    }
    clearTimeout(timer);
    if (body.busy && !disposed) timer = setTimeout(() => void load('poll'), 1200);
  }

  async function read(response: Response): Promise<IndexResponse> {
    const body = await response.json();
    if (!response.ok) throw new Error(body.error || `请求失败（HTTP ${response.status}）`);
    return body;
  }

  async function load(mode: 'initial' | 'refresh' | 'poll' = 'refresh') {
    if (acting || mode === 'refresh' && (loading || refreshing)) return;
    const version = ++requestVersion;
    clearTimeout(timer);
    if (mode === 'initial') loading = true;
    if (mode === 'refresh') { refreshing = true; failed = false; message = ''; }
    try {
      const body = await read(await request('/api/v1/search-index'));
      if (!disposed && version === requestVersion) {
        if (failed) { failed = false; message = ''; }
        accept(body, mode === 'initial');
        if (mode === 'refresh') message = '索引状态已刷新。刷新只读取状态，不会构建索引或整理空间。';
      }
    } catch (error) {
      if (!disposed && version === requestVersion) {
        failed = true; message = error instanceof Error ? error.message : '无法读取索引状态';
        if (state?.busy) timer = setTimeout(() => void load('poll'), 1200);
      }
    } finally { if (!disposed && version === requestVersion) { loading = false; refreshing = false; } }
  }

  async function act(action: IndexAction) {
    if (!state?.available || acting || refreshing || action === 'save' && (busy || !dirty || !valid)) return;
    const scopeChanged = changedScope;
    ++requestVersion;
    clearTimeout(timer);
    pendingAction = action; failed = false; message = '';
    try {
      const body = await read(await request(`/api/v1/search-index${action === 'save' ? '' : `/${action}`}`, {
        method: action === 'save' ? 'PUT' : 'POST', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ root: state.root, ...(action === 'save' ? { settings } : {}), ...(action === 'clear' ? { confirm: true } : {}) })
      }));
      if (!disposed) {
        accept(body, action === 'save' || action === 'clear');
        if (action === 'save') message = scopeChanged
          ? `索引范围已保存，点击“${body.index?.rebuild_required ? '重新构建' : '开始构建'}”后应用。保存设置不会立即改变已用空间。`
          : '索引设置已保存。';
        if (action === 'clear') { confirmingClear = false; message = '已清理搜索索引并暂停自动构建。'; }
        if (action === 'compact') message = '空间整理完成。';
      }
    } catch (error) {
      if (!disposed) { failed = true; message = error instanceof Error ? error.message : '操作失败'; }
    } finally { if (!disposed) pendingAction = null; }
  }

  onMount(() => { void load('initial'); });
  onDestroy(() => { disposed = true; clearTimeout(timer); });
</script>

<div class="search-index-settings" aria-busy={loading || refreshing || acting}>
  {#if loading}
    <p class="index-feedback" role="status"><LoaderCircle class="index-spinner" size={15} />正在读取索引状态…</p>
  {:else if !state?.available || !index}
    <p class="index-feedback" role="status">{state?.error || message || '当前工作区尚未配置搜索存储。'}</p>
    <button class="secondary-button" on:click={() => void load('initial')}>重新读取</button>
  {:else}
    <div class="index-capacity">
      <Database size={18} aria-hidden="true" />
      <div><strong>{bytes(index.database_bytes)} <span>/ {bytes(index.settings.budget_bytes)}</span></strong><p>索引已用空间 / 预算上限</p></div>
      <button class="secondary-button" aria-label="刷新索引状态" title="仅刷新状态，不会构建索引或整理空间" disabled={acting || refreshing} aria-busy={refreshing} on:click={() => void load()}>
        {#if refreshing}<LoaderCircle class="index-spinner" size={14} aria-hidden="true" />{:else}<RefreshCw size={14} aria-hidden="true" />{/if}
      </button>
    </div>
    <div class="index-meter" role="meter" aria-label="索引预算使用率" aria-valuemin={0} aria-valuemax={100} aria-valuenow={usedPercent}><span class:over={index.over_budget} style={`width:${usedPercent}%`}></span></div>
    <div class="index-summary">
      {#if unbuilt}
        <strong>{state.busy ? '等待首批搜索记录' : '尚未构建索引'}</strong>
      {/if}
      {#if index.catalog_files > 0}<span>已处理 <strong>{index.indexed_files} / {index.catalog_files}</strong> 个会话文件</span>{/if}
      {#if index.source_bytes > 0}<span>源文件 <strong>{bytes(index.covered_bytes)} / {bytes(index.source_bytes)}</strong></span>{/if}
      {#if index.partial_files && index.partial_bytes}
        <span>处理未完成 <strong>{index.partial_files} 个文件 · 已读 {bytes(index.partial_bytes)}</strong></span>
      {/if}
      {#if index.documents > 0}<span>索引记录 <strong>{index.documents.toLocaleString()}</strong></span>
      {:else if !index.rebuild_required}<span>可直接使用「全部搜索」</span>{/if}
      {#if index.reclaimable_bytes > 0}<span>空闲页 <strong>{bytes(index.reclaimable_bytes)}</strong></span>{/if}
    </div>
    {#if unbuilt}<p class="index-note">当前尚无搜索索引记录，已用空间包含索引文件的基础占用。{#if !state.busy}点击“开始构建”生成搜索索引。{/if}</p>{/if}
    {#if refreshing}<p class="index-feedback" role="status">正在刷新索引状态…</p>{/if}
    <form class="index-form" on:submit|preventDefault={() => void act('save')}>
      <div class="index-budget-form">
        <div class="index-field-title">
          <label for="search-index-budget">索引空间预算</label>
          <HelpBubble id="index-budget-help" label="索引空间预算说明" text="预算限制搜索加速文件的大小。会话目录、阅读缓存和构建临时空间另计。预算用完后，其他内容仍可通过全部搜索查找。" />
        </div>
        <div class="index-budget-input"><input id="search-index-budget" type="number" min="1" max="65536" step="1" bind:value={budgetMiB} disabled={busy} /><span>MiB</span></div>
        <div class="index-presets" aria-label="常用索引预算">{#each [256, 512, 1024, 2048] as value}<button type="button" aria-pressed={budgetMiB === value} disabled={busy} on:click={() => budgetMiB = value}>{value < 1024 ? `${value} MiB` : `${value / 1024} GiB`}</button>{/each}</div>
      </div>
      {#if nodes.length}
        <div class="index-content" role="group" aria-labelledby="index-content-title">
          <div class="index-content-header">
            <div class="index-field-title">
              <strong id="index-content-title">索引内容</strong>
              <HelpBubble id="index-content-help" label="索引内容说明" text="点名称选内容，点箭头展开分支。未自选类别时沿用默认范围。正文时间范围只限制用户提问和助手消息；更早的正文仍可用「全部搜索」查找。" />
            </div>
            <span>{content ? `已选 ${selectedContent.size} / ${allContent.length} 项` : '沿用默认范围'}</span>
          </div>
          <div class="index-presets" aria-label="索引内容预设">
            {#if compact.length}<button type="button" disabled={busy} aria-pressed={selectedContent.size === compact.length && compact.every(id => selectedContent.has(id))} on:click={() => content = [...compact].sort()}>省空间</button>{/if}
            <button type="button" disabled={busy} aria-pressed={selectedContent.size === allContent.length} on:click={() => content = [...allContent]}>全选</button>
            {#if selectedContent.size}<button type="button" disabled={busy} on:click={() => content = []}>清空选择</button>{/if}
            {#if changedContent}<button type="button" disabled={busy} on:click={() => content = index?.settings.content}>撤销内容修改</button>{/if}
          </div>
          <p class="index-note">“省空间”保留提问、最终回复、未标记阶段的回复和压缩摘要。内容预设不调整预算上限，保存后需构建索引才能应用。</p>
          <IndexContentTree {nodes} selected={selectedContent} disabled={busy} {recentContext} {rangeApplicable}
            onSelect={ids => content = ids.sort()} onRangeSelect={recent => recentContext = recent} />
        </div>
      {/if}
      <div class="index-save-row">
        <button class="index-automatic" type="button" role="switch" aria-checked={automatic} disabled={busy} on:click={() => automatic = !automatic}>
          <span class="index-switch-track" aria-hidden="true"></span>同步会话目录时自动构建
        </button>
        <button class="secondary-button" type="submit" disabled={busy || !dirty || !valid}>保存设置</button>
      </div>
    </form>
    {#if index.reclaimable_bytes > 0}<p class="index-note">整理空间预计释放 {bytes(index.reclaimable_bytes)}，执行时最多需要约 {bytes(index.database_bytes * 2)} 的额外空闲空间。</p>{/if}
    {#if index.rebuild_required}<p class="index-feedback" role="status">需要重新构建索引。期间可使用全部搜索。</p>{/if}
    {#if dirty}<p class="index-feedback" role="status">{content?.length === 0 ? '请至少选择一项索引内容。' : '设置尚未保存。'}</p>{/if}
    {#if index.over_budget}<p class="index-warning" role="status">现有索引超出预算。可提高预算，或清理索引后重新构建。</p>{/if}
    {#if taskLabel}
      <div class="index-task" role="status">
        {#if state.busy}<LoaderCircle class="index-spinner" size={15} />{/if}
        <span>{taskLabel}{#if taskHere && state.task?.phase === 'index'} · {state.task.progress.files_done}/{state.task.progress.files_total} 个文件 · 新增 {state.task.progress.documents_added.toLocaleString()} 条记录{/if}</span>
      </div>
      {#if state.task?.error}<p class="index-warning" role="alert">{state.task.error}</p>{/if}
    {/if}
    <div class="index-actions">
      {#if state.busy}
        <button type="button" class="secondary-button" disabled={acting || refreshing || !taskHere} on:click={() => void act('stop')}><Square size={14} />停止构建</button>
      {:else}
        <button type="button" class="secondary-button" disabled={acting || refreshing || dirty} on:click={() => void act('build')}>{index.rebuild_required ? '重新构建' : index.indexed_files || index.partial_files ? '增量构建' : '开始构建'}</button>
      {/if}
      <button type="button" class="secondary-button" disabled={!!compactBlocked} title={pendingAction === 'compact' ? undefined : compactBlocked || undefined}
        aria-busy={pendingAction === 'compact'} on:click={() => void act('compact')}>
        {#if pendingAction === 'compact'}<LoaderCircle class="index-spinner" size={14} aria-hidden="true" />整理中…{:else}整理空间{/if}
      </button>
      <button class="secondary-button" disabled={busy} on:click={() => confirmingClear = !confirmingClear}><Trash2 size={14} />清理索引</button>
    </div>
    {#if confirmingClear}
      <div class="index-clear-confirm" role="group" aria-label="确认清理搜索索引">
        <strong>清理搜索加速数据？</strong><p>会暂停自动构建。会话正文、归档和阅读定位保留，后续可重新构建。</p>
        <div class="index-actions"><button class="secondary-button" disabled={busy} on:click={() => confirmingClear = false}>取消</button><button class="danger-button" disabled={busy} on:click={() => void act('clear')}>确认清理</button></div>
      </div>
    {/if}
    {#if message}<p class="index-feedback" class:index-warning={failed} role={failed ? 'alert' : 'status'}>{message}</p>{/if}
    <details class="index-details"><summary>存储详情</summary><code>{index.path}</code>{#if index.legacy_bytes > 0}<p>旧版搜索数据约 {bytes(index.legacy_bytes)}，可通过“清理索引”一并移除。</p>{/if}<p>中断后可再次开始构建，会校验源文件并从已提交位置继续。</p></details>
  {/if}
</div>

<style>
  .search-index-settings { display: grid; gap: var(--space-3); min-width: 0; font-size: var(--font-ui); }
  .index-capacity { display: flex; align-items: center; gap: var(--space-3); }
  .index-capacity > div { flex: 1; min-width: 0; }
  .index-capacity strong { font-size: var(--font-body); font-variant-numeric: tabular-nums; }
  .index-capacity strong span, .index-capacity p, .index-note, .index-details { color: var(--text-muted); }
  .index-capacity p { margin: 2px 0 0; font-size: var(--font-caption); }
  .index-meter { height: 5px; border-radius: 5px; overflow: hidden; background: var(--bg-subtle); }
  .index-meter span { display: block; height: 100%; background: var(--accent); }
  .index-meter span.over { background: var(--warning); }
  .index-summary, .index-actions { display: flex; align-items: center; flex-wrap: wrap; gap: var(--space-2) var(--space-3); }
  .index-summary { font-size: var(--font-caption); color: var(--text-secondary); }
  .index-summary strong { color: var(--text-primary); font-weight: 500; }
  .index-form { display: grid; gap: var(--space-3); min-width: 0; }
  .index-budget-form { display: flex; flex-wrap: wrap; align-items: center; gap: var(--space-2); }
  .index-field-title { display: inline-flex; align-items: center; gap: 2px; }
  .index-budget-input { display: flex; align-items: center; width: 7rem; border: 1px solid var(--border-strong); border-radius: var(--radius-control); overflow: hidden; background: var(--bg-surface); }
  .index-budget-input input { width: 100%; min-width: 0; border: 0; background: transparent; color: var(--text-primary); padding: var(--space-1) var(--space-2); }
  .index-budget-input span { color: var(--text-muted); padding-right: var(--space-2); font-size: var(--font-caption); }
  .index-presets { display: flex; flex-wrap: wrap; gap: var(--space-2); }
  .index-presets button { min-height: var(--control-sm); border: 1px solid var(--border); padding: var(--space-1) var(--space-2); border-radius: var(--radius-control); color: var(--text-secondary); background: var(--bg-surface); cursor: pointer; font-size: var(--font-caption); }
  .index-presets button[aria-pressed="true"] { border-color: var(--accent); color: var(--accent); background: var(--bg-selected); }
  .index-presets button:not(:disabled):hover, .index-automatic:not(:disabled):hover { background: var(--bg-hover); }
  .index-content { display: grid; min-width: 0; gap: var(--space-2); padding: var(--space-3); border: 1px solid var(--border); border-radius: var(--radius-control); background: var(--bg-subtle); }
  .index-content-header, .index-save-row { display: flex; flex-wrap: wrap; justify-content: space-between; align-items: center; gap: var(--space-2); }
  .index-content-header > span { color: var(--text-muted); font-size: var(--font-caption); font-variant-numeric: tabular-nums; }
  .index-automatic { display: inline-flex; align-items: center; gap: var(--space-2); padding: var(--space-1); min-height: var(--control-md); border: 0; border-radius: var(--radius-control); background: transparent; color: var(--text-secondary); font: inherit; cursor: pointer; }
  .index-switch-track { position: relative; flex: 0 0 1.875rem; height: 1rem; border: 1px solid var(--border-strong); border-radius: 1rem; background: var(--bg-subtle); }
  .index-switch-track::after { position: absolute; inset: 2px auto 2px 2px; aspect-ratio: 1; border-radius: 50%; background: var(--text-muted); content: ''; }
  .index-automatic[aria-checked="true"] .index-switch-track { border-color: var(--accent); background: var(--bg-selected); }
  .index-automatic[aria-checked="true"] .index-switch-track::after { inset-inline: auto 2px; background: var(--accent); }
  .index-note, .index-details p, .index-clear-confirm p { margin: 0; font-size: var(--font-caption); line-height: var(--line-ui); }
  .index-feedback, .index-task { display: flex; align-items: center; gap: var(--space-2); margin: 0; color: var(--text-secondary); font-size: var(--font-caption); }
  .index-warning { margin: 0; color: var(--warning); font-size: var(--font-caption); overflow-wrap: anywhere; }
  .index-actions button { display: inline-flex; align-items: center; justify-content: center; gap: var(--space-1); }
  .search-index-settings button:disabled { cursor: not-allowed; }
  .index-clear-confirm { display: grid; gap: var(--space-2); padding: var(--space-3); border: 1px solid var(--border-strong); border-radius: var(--radius-control); background: var(--bg-subtle); }
  .index-clear-confirm strong { font-weight: 600; }
  .index-details { font-size: var(--font-caption); }
  .index-details summary { cursor: pointer; }
  .index-details code { display: block; overflow-wrap: anywhere; margin: var(--space-2) 0; }
  .index-details p + p { margin-top: var(--space-2); }
  :global(.search-index-settings .index-spinner) { flex-shrink: 0; animation: index-spin 1s linear infinite; }
  @keyframes index-spin { to { transform: rotate(360deg); } }
  @media (prefers-reduced-motion: reduce) { :global(.search-index-settings .index-spinner) { animation: none; } }
  @media (max-width: 480px) { .index-actions button { flex: 1 1 auto; } }
</style>
