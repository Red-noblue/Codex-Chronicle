<!-- 中文说明：会话库的完整云备份确认、任务进度、暂停续传和隔离下载入口；上传不会删除本地源。 -->
<script lang="ts">
  import { onMount } from 'svelte';
  import { CloudUpload, Check, Download, Pause, Play, RefreshCw, LoaderCircle } from '@lucide/svelte';
  import LibraryDrawer from './LibraryDrawer.svelte';

  export let open = false;
  export let conversationIDs: string[] = [];
  export let root = '';
  export let request: typeof fetch;

  type Preview = {
    id: string; source_root: string; target: string; ids: string[]; requested_ids: string[]; source_bytes: number;
    assets: { relative: string; id: string; bytes: number }[];
    database_rows: { database: string; table: string; rows: number }[];
    index_lines: Record<string, number>;
  };
  type Task = {
    id: string; archive_id: string; state: string; target: string; source_ids: string[];
    completed_bytes: number; total_bytes: number; source_bytes: number; created_at: string;
    error?: string; cache_retained: boolean; download_state?: string; download_error?: string; recovery_path?: string;
    compression?: { original_bytes: number; compressed_bytes: number };
  };
  const labels: Record<string, string> = { queued: '准备中', exporting: '读取完整快照', compressing: '压缩中', uploading: '上传中', verifying: '回读校验', completed: '已备份', paused: '已暂停', interrupted: '已中断', failed: '未完成' };
  const running = new Set(['queued', 'exporting', 'compressing', 'uploading', 'verifying']);
  let available = false;
  let target = '';
  let targetError = '';
  let tasks: Task[] = [];
  let active = '';
  let preview: Preview | null = null;
  let confirmed = false;
  let busy = '';
  let message = '';
  let loadError = '';
  let reviewedSelection = '';
  let loadVersion = 0;
  let loadedRoot = root;
  $: selectionKey = `${root}:${conversationIDs.join(',')}`;
  $: if (preview && (selectionKey !== reviewedSelection || preview.source_root !== root)) { preview = null; confirmed = false; }
  $: if (root !== loadedRoot) {
    loadedRoot = root; tasks = []; active = ''; target = ''; available = false;
    targetError = ''; loadError = ''; message = '';
    void load();
  }
  $: rowCount = preview?.database_rows.reduce((sum, item) => sum + item.rows, 0) || 0;

  function bytes(value: number) {
    if (!Number.isFinite(value) || value <= 0) return '0 B';
    const unit = Math.min(3, Math.floor(Math.log(value) / Math.log(1024)));
    return `${(value / 1024 ** unit).toFixed(unit ? 1 : 0)} ${['B', 'KiB', 'MiB', 'GiB'][unit]}`;
  }
  async function response(response: Response) {
    const body = await response.json();
    if (!response.ok) throw new Error(body.error || `请求失败（${response.status}）`);
    return body;
  }
  async function post(path: string, body?: unknown) {
    return response(await request(`/api/v1/backups/${path}`, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body || {}) }));
  }
  async function load() {
    const requestedRoot = root;
    const version = ++loadVersion;
    try {
      const body = await response(await request('/api/v1/backups'));
      if (root !== requestedRoot || version !== loadVersion) return;
      available = body.available; target = body.target || ''; targetError = body.target_error || body.error || '';
      tasks = body.items || []; active = body.active || ''; loadError = '';
    } catch (error) {
      if (root === requestedRoot && version === loadVersion) loadError = error instanceof Error ? error.message : '任务读取失败';
    }
  }
  async function inspect() {
    busy = 'preview'; message = ''; preview = null; confirmed = false;
    const key = selectionKey;
    try {
      const result = await post('preview', { conversation_ids: conversationIDs });
      if (key === selectionKey) { reviewedSelection = key; preview = result; }
    } catch (error) { message = error instanceof Error ? error.message : '无法检查备份资产'; }
    finally { busy = ''; }
  }
  async function start() {
    if (!preview || !confirmed) return;
    busy = 'start'; message = '';
    try {
      await post('start', { preview_id: preview.id, confirm_upload: true });
      preview = null; confirmed = false; await load();
    } catch (error) { message = error instanceof Error ? error.message : '无法开始备份'; }
    finally { busy = ''; }
  }
  async function action(task: Task, name: 'pause' | 'resume' | 'download') {
    busy = task.id; message = '';
    try { await post(`${task.id}/${name}`); await load(); }
    catch (error) { message = error instanceof Error ? error.message : '操作失败'; }
    finally { busy = ''; }
  }
  onMount(() => {
    let disposed = false;
    let timer: ReturnType<typeof setTimeout>;
    async function poll() { await load(); if (!disposed) timer = setTimeout(poll, open ? 1500 : 10000); }
    void poll();
    return () => { disposed = true; clearTimeout(timer); };
  });
</script>

<LibraryDrawer bind:open title="云端备份" description="压缩上传完整会话，保留本地原始数据。">
  <section class="cloud-destination">
    <CloudUpload size={20} aria-hidden="true" /><div><strong>{target || '尚未选择仓库'}</strong><span>{target ? 'GitHub 私有仓库' : '先到设置 → 远端归档连接账户并选择私有仓库'}</span></div>
    <button type="button" class="library-icon-button" aria-label="刷新云备份状态" on:click={load}><RefreshCw size={16} /></button>
  </section>
  {#if targetError}<p class="cloud-note" role="status">{targetError}</p>{/if}
  {#if loadError}<p class="cloud-error" role="alert">{loadError}</p>{/if}

  {#if conversationIDs.length}
    <section class="cloud-selection">
      <header><h3>备份所选 {conversationIDs.length} 项</h3><button class="secondary-button" disabled={!!busy || !available || !target || !!active} on:click={inspect}>{#if busy === 'preview'}<LoaderCircle class="cloud-spinner" size={14} />检查中…{:else}检查备份内容{/if}</button></header>
      {#if preview}
        <div class="cloud-facts"><span><strong>{preview.ids.length}</strong> 个实际会话</span><span><strong>{preview.assets.length}</strong> 个文件</span><span><strong>{rowCount}</strong> 条数据库记录</span><span><strong>{bytes(preview.source_bytes)}</strong> 原始文件</span></div>
        {#if preview.ids.length > preview.requested_ids.length}<p class="cloud-note">已包含 {preview.ids.length - preview.requested_ids.length} 个关联子代理。</p>{/if}
        <details class="cloud-assets"><summary>核对完整备份清单</summary>
          <h4>文件</h4>{#each preview.assets as asset}<div class="cloud-asset"><code title={asset.relative}>{asset.relative}</code><span>{bytes(asset.bytes)}</span></div>{/each}
          <h4>数据库专属记录</h4>{#each preview.database_rows as group}<div class="cloud-asset"><code>{group.database} / {group.table}</code><span>{group.rows} 条</span></div>{/each}
          <h4>目标索引行</h4>{#each Object.entries(preview.index_lines) as [name, count]}<div class="cloud-asset"><code>{name}</code><span>{count} 条</span></div>{/each}
          <h4>原始会话 ID</h4>{#each preview.ids as id}<code class="cloud-id">{id}</code>{/each}
        </details>
        <label class="cloud-consent"><input type="checkbox" bind:checked={confirmed} disabled={!!busy} /><span>我确认将清单中的完整内容上传到 <strong>{preview.target}</strong></span></label>
        <button class="primary-button cloud-start" disabled={!confirmed || !!busy || !!active} on:click={start}><CloudUpload size={16} />{busy === 'start' ? '正在创建任务…' : '确认上传并保留本地'}</button>
      {:else}<p class="cloud-note">先检查文件与数据库行。活动会话或关联未解析完整的会话会停止检查。</p>{/if}
    </section>
  {:else}<p class="cloud-note">在会话库选择一个或多个会话，或在对话详情中点击云端备份。</p>{/if}

  {#if message}<p class="cloud-error" role="alert">{message}</p>{/if}
  <section class="cloud-tasks" aria-label="云备份任务">
    <h3>备份任务 <span>{tasks.length}</span></h3>
    {#if !tasks.length}<div class="cloud-empty">还没有备份任务</div>{/if}
    {#each tasks as task (task.id)}
      <article class="cloud-task" class:cloud-failed={task.state === 'failed'}>
        <header><strong>{task.source_ids.length} 项会话</strong><span class="cloud-state" class:cloud-completed={task.state === 'completed'}>{#if running.has(task.state)}<LoaderCircle class="cloud-spinner" size={14} />{:else if task.state === 'completed'}<Check size={14} />{/if}{labels[task.state] || task.state}</span></header>
        <p class="cloud-target" title={task.target}>{task.target}</p>
        {#if task.total_bytes && (running.has(task.state) || task.state === 'paused' || task.state === 'interrupted' || task.state === 'failed')}<progress max={task.total_bytes} value={task.completed_bytes} aria-label="已完成或核对的上传进度"></progress><div class="cloud-transfer">{bytes(task.completed_bytes)} / {bytes(task.total_bytes)}</div>{/if}
        {#if task.compression}<p class="cloud-note">{bytes(task.compression.original_bytes)} → {bytes(task.compression.compressed_bytes)}{#if task.compression.original_bytes > 0} · 减少 {Math.max(0, (1 - task.compression.compressed_bytes / task.compression.original_bytes) * 100).toFixed(0)}%{/if}</p>{/if}
        {#if task.error}<p class="cloud-error">{task.error}</p>{/if}
        <div class="cloud-actions">
          {#if active === task.id}<button class="secondary-button" disabled={!!busy} on:click={() => action(task, 'pause')}><Pause size={14} />暂停</button>
          {:else if ['paused', 'interrupted', 'failed'].includes(task.state)}<button class="secondary-button" disabled={!!busy || !!active} on:click={() => action(task, 'resume')}><Play size={14} />继续上传</button>{/if}
          {#if task.state === 'completed' && task.download_state !== 'completed'}<button class="secondary-button" disabled={!!busy || !!active} on:click={() => action(task, 'download')}><Download size={14} />{task.download_state === 'running' ? '下载校验中…' : '下载并校验'}</button>{/if}
        </div>
        {#if task.download_error}<p class="cloud-error">{task.download_error}</p>{/if}
        {#if task.recovery_path}<div class="cloud-recovery"><span><Check size={14} />已下载并验证正文、目标数据库行与索引</span><code>{task.recovery_path}</code></div>{/if}
        <details class="cloud-technical"><summary>任务详情</summary><code>{task.id}</code><span>{new Date(task.created_at).toLocaleString('zh-CN')}</span><span>{task.cache_retained ? '保留续传缓存' : '上传缓存已清理'} · 本地源未移除</span></details>
      </article>
    {/each}
  </section>
</LibraryDrawer>

<style>
  .cloud-destination, header, .cloud-actions, .cloud-facts, .cloud-state, .cloud-recovery span { display: flex; align-items: center; gap: var(--space-2); }
  .cloud-destination { padding-bottom: var(--space-3); border-bottom: 1px solid var(--border); }
  .cloud-destination > div { flex: 1; min-width: 0; display: grid; gap: var(--space-1); }
  .cloud-destination strong, .cloud-target { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
  .cloud-destination span, .cloud-note, .cloud-transfer, .cloud-technical, .cloud-asset, .cloud-id { font-size: var(--font-caption); color: var(--text-muted); }
  .cloud-selection, .cloud-tasks { margin-top: var(--space-4); min-width: 0; }
  h3 { font-size: var(--font-body); margin: 0; } h4 { font-size: var(--font-caption); margin: var(--space-3) 0 var(--space-1); }
  header { justify-content: space-between; flex-wrap: wrap; } header > strong { font-size: var(--font-body); }
  .cloud-note { line-height: 1.55; margin: var(--space-2) 0; } .cloud-error { font-size: var(--font-caption); color: var(--danger, #b95050); overflow-wrap: anywhere; line-height: 1.5; }
  .cloud-facts { flex-wrap: wrap; margin-top: var(--space-3); gap: var(--space-2) var(--space-4); font-size: var(--font-caption); color: var(--text-secondary); }
  .cloud-assets { margin: var(--space-3) 0; } summary { cursor: pointer; font-size: var(--font-caption); color: var(--text-secondary); }
  .cloud-asset { display: grid; grid-template-columns: minmax(0, 1fr) auto; gap: var(--space-2); padding: var(--space-1) 0; }
  code { overflow-wrap: anywhere; font-size: var(--font-caption); } .cloud-asset code { min-width: 0; } .cloud-id { display: block; padding: var(--space-1) 0; }
  .cloud-consent { display: flex; align-items: flex-start; gap: var(--space-2); font-size: var(--font-caption); line-height: 1.6; margin: var(--space-3) 0; }
  .cloud-consent input { margin-top: 4px; flex-shrink: 0; } .cloud-consent strong { overflow-wrap: anywhere; } .cloud-start { display: inline-flex; gap: var(--space-2); align-items: center; min-height: var(--control-md); padding: var(--space-2) var(--space-3); border: 1px solid var(--accent-solid); border-radius: var(--radius-control); background: var(--accent-solid); color: var(--on-accent); font-size: var(--font-ui); cursor: pointer; } .cloud-start:disabled { opacity: .5; cursor: not-allowed; }
  .cloud-tasks > h3 { margin-bottom: var(--space-2); } .cloud-tasks > h3 span { color: var(--text-muted); font-weight: 400; }
  .cloud-task { padding: var(--space-3) 0; border-top: 1px solid var(--border); min-width: 0; } .cloud-state, .cloud-target { font-size: var(--font-caption); color: var(--text-secondary); margin: 0; } .cloud-target { margin-top: var(--space-1); }
  .cloud-completed { color: var(--success, #358768); } .cloud-failed .cloud-state { color: var(--danger, #b95050); }
  progress { width: 100%; height: 5px; margin-top: var(--space-3); accent-color: var(--accent); } .cloud-transfer { text-align: right; font-variant-numeric: tabular-nums; }
  .cloud-actions { flex-wrap: wrap; margin-top: var(--space-2); } .cloud-actions button { display: inline-flex; gap: var(--space-1); align-items: center; }
  .cloud-technical { margin-top: var(--space-2); } .cloud-technical > code, .cloud-technical > span { display: block; margin-top: var(--space-1); }
  .cloud-recovery { margin-top: var(--space-3); font-size: var(--font-caption); } .cloud-recovery span { color: var(--success, #358768); margin-bottom: var(--space-1); }
  .cloud-empty { padding: var(--space-4) 0; font-size: var(--font-caption); color: var(--text-muted); }
  button:focus-visible, summary:focus-visible { outline: 2px solid var(--accent); outline-offset: 2px; }
  :global(.cloud-spinner) { animation: cloud-spin 1s linear infinite; } @keyframes cloud-spin { to { transform: rotate(360deg); } }
  @media (prefers-reduced-motion: reduce) { :global(.cloud-spinner) { animation: none; } }
</style>
