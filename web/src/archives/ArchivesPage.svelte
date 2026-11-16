<!-- 中文说明：集中管理完整会话归档的本地与云端副本、恢复、复制、迁移和删除。 -->
<script lang="ts">
  import { onMount, onDestroy } from 'svelte';
  import { Archive, ArrowDownToLine, Check, ChevronRight, CircleAlert, Cloud, HardDrive, LoaderCircle, RefreshCw, Search, ShieldCheck, Trash2, X } from '@lucide/svelte';
  import LibraryDrawer from '../sessions/LibraryDrawer.svelte';
  import type { ArchiveCatalogItem, ArchiveManagementPreview, ArchiveTask } from './types';

  export let request: (input: RequestInfo | URL, init?: RequestInit) => Promise<Response>;
  export let onLibrary: () => void;
  export let onSettings: () => void;
  export let onChanged: () => void;

  type ArchiveFilter = 'all' | 'paired' | 'local' | 'remote' | 'restored' | 'attention';
  type ArchiveSort = 'newest' | 'oldest' | 'largest';
  const archiveSources: Array<{ id: 'local' | 'remote'; label: string }> = [
    { id: 'local', label: '本地' }, { id: 'remote', label: '云端' }
  ];
  const filters: Array<{ id: ArchiveFilter; label: string }> = [
    { id: 'all', label: '全部' }, { id: 'paired', label: '双副本' }, { id: 'local', label: '本地' },
    { id: 'remote', label: '云端' }, { id: 'restored', label: '已恢复' }, { id: 'attention', label: '需复核' }
  ];
  let items: ArchiveCatalogItem[] = [];
  let loading = true;
  let busy = false;
  let message = '';
  let catalogError = '';
  let remoteTarget = '';
  let remoteError = '';
  let localRoot = '';
  let activeTaskID = '';
  let searchQuery = '';
  let sortOrder: ArchiveSort = 'newest';
  let selectedView: ArchiveFilter = 'all';
  let selectedTaskID = '';
  let detailsOpen = false;
  let returnToDetails = false;
  let sourceChoice: 'local' | 'remote' = 'local';
  let updateIfNewer = false;
  let recoverUpdate = false;
  let password = '';
  let confirmation = '';
  let preview: ArchiveManagementPreview | null = null;
  let activeTask: ArchiveTask | null = null;
  let selectedAction = '';
  let requestGeneration = 0;
  let pollTimer: ReturnType<typeof setInterval>;
  let dialog: HTMLDialogElement;
  let filterCounts: Record<ArchiveFilter, number> = { all: 0, paired: 0, local: 0, remote: 0, restored: 0, attention: 0 };

  $: visibleItems = items.filter(item => {
    const filterMatches = selectedView === 'all' || selectedView === 'paired' && item.local_available && item.remote_available ||
      selectedView === 'local' && item.local_available || selectedView === 'remote' && item.remote_available ||
      selectedView === 'restored' && isRestored(item.task) || selectedView === 'attention' && needsAttention(item);
    const needle = searchQuery.trim().toLocaleLowerCase();
    const textMatches = !needle || [archiveTitle(item.task), ...item.task.source_ids, item.task.id, item.task.target]
      .some(value => value?.toLocaleLowerCase().includes(needle));
    return filterMatches && textMatches;
  }).sort((left, right) => sortOrder === 'oldest'
    ? new Date(left.task.created_at).getTime() - new Date(right.task.created_at).getTime()
    : sortOrder === 'largest'
      ? archiveBytes(right.task) - archiveBytes(left.task)
      : new Date(right.task.created_at).getTime() - new Date(left.task.created_at).getTime());
  $: filterCounts = {
    all: items.length,
    paired: items.filter(item => item.local_available && item.remote_available).length,
    local: items.filter(item => item.local_available).length,
    remote: items.filter(item => item.remote_available).length,
    restored: items.filter(item => isRestored(item.task)).length,
    attention: items.filter(needsAttention).length
  };
  $: selectedItem = items.find(item => item.task.id === selectedTaskID) || null;
  $: selectedTitle = selectedItem ? archiveTitle(selectedItem.task) : '归档详情';
  $: activeCatalogItem = items.find(item => item.task.id === activeTaskID) || null;
  $: activeArchiveTask = activeCatalogItem?.task;
  $: activeItem = items.find(item => item.task.id === activeTask?.id) || null;
  $: remoteActionUnavailable = Boolean(remoteError && activeTask && (sourceChoice === 'remote' || ['copy_remote', 'move_remote', 'delete_remote'].includes(selectedAction)));
  $: requiresCode = Boolean(preview?.code);
  $: canConfirm = Boolean(preview && !busy && (!requiresCode || confirmation.trim() === preview.code) && !['conflict', 'partial'].includes(preview.restore?.status || '') && (preview.restore?.status !== 'recovery_required' || recoverUpdate));

  onMount(() => { void load(); pollTimer = setInterval(() => { if (!busy) void load(); }, 4000); });
  onDestroy(() => clearInterval(pollTimer));
  $: if (dialog) {
    if (activeTask && !dialog.open) dialog.showModal();
    else if (!activeTask && dialog.open) dialog.close();
  }

  async function load() {
    const generation = ++requestGeneration;
    loading = items.length === 0;
    catalogError = '';
    try {
      const response = await request('/api/v1/archive-center');
      const body = await response.json() as { items?: ArchiveCatalogItem[]; local_root?: string; remote_target?: string; remote_error?: string; active?: string; error?: string };
      if (generation !== requestGeneration) return;
      if (!response.ok) throw new Error(body.error || '读取归档失败');
      items = body.items || [];
      localRoot = body.local_root || '';
      remoteTarget = body.remote_target || '';
      remoteError = body.remote_error || '';
      activeTaskID = body.active || '';
    } catch (error) {
      if (generation === requestGeneration) catalogError = error instanceof Error ? error.message : '读取归档失败';
    } finally { if (generation === requestGeneration) loading = false; }
  }

  function archiveTitle(task: ArchiveTask) {
    return task.titles?.filter(Boolean).join('、') || `${task.source_ids.length} 个对话`;
  }

  function archiveIDs(task: ArchiveTask) {
    return task.compression?.conversation_ids || task.source_ids;
  }

  function archiveBytes(task: ArchiveTask) {
    return task.compression?.compressed_bytes ?? task.source_bytes;
  }

  function isRestored(task: ArchiveTask) {
    return task.restore_status === 'restored' || Object.values(task.restore_checks || {}).some(check => check?.status === 'restored');
  }

  function hasRestoreRecord(task: ArchiveTask) {
    return Boolean(task.restore_status || task.restore_checks && Object.keys(task.restore_checks).length);
  }

  function restoreCheckStatus(task: ArchiveTask, source: 'local' | 'remote') {
    return task.restore_checks?.[source]?.status || (task.restore_source === source ? task.restore_status : '');
  }

  function restoreCheckLabel(task: ArchiveTask, source: 'local' | 'remote') {
    const status = restoreCheckStatus(task, source);
    if (!status) return '';
    const checkedAt = task.restore_checks?.[source]?.checked_at || task.restored_at;
    const result = ({
      restored: '上次从此副本恢复',
      already_present: '上次核验时内容一致',
      ready: '上次检查可恢复',
      conflict: '上次检查发现冲突',
      partial: '上次检查发现部分内容已存在'
    } as Record<string, string>)[status] || '上次检查未完成';
    return checkedAt ? `${result} · ${formatTime(checkedAt)}` : result;
  }

  function needsAttention(item: ArchiveCatalogItem) {
    return Boolean(item.task.error || item.task.management_error || item.task.management_state === 'needs_review' ||
      ['failed', 'interrupted'].includes(item.task.state) ||
      ['conflict', 'partial', 'needs_review'].includes(item.task.restore_status || '') ||
      Object.values(item.task.restore_checks || {}).some(check => ['conflict', 'partial', 'needs_review'].includes(check?.status || '')) ||
      !item.local_available && !item.remote_available);
  }

  function taskStatus(item: ArchiveCatalogItem) {
    const task = item.task;
    if (task.management_state === 'running') return `${actionLabel(task.management_action || '归档操作')}中`;
    if (needsAttention(item)) return !item.local_available && !item.remote_available ? '无可用副本' : '需复核';
    if (task.restore_status === 'already_present') return '最近核验内容一致';
    if (isRestored(task)) return '有恢复记录';
    if (task.management_state === 'completed') return '操作已完成';
    return ({ queued: '等待归档', exporting: '整理内容', compressing: '压缩中', uploading: '上传中', verifying: '校验中', interrupted: '已中断', failed: '归档失败', completed: '已归档' } as Record<string, string>)[task.state] || task.state;
  }

  function taskStatusClass(item: ArchiveCatalogItem) {
    if (needsAttention(item)) return 'needs_review';
    if (item.task.management_state === 'running' || item.task.state !== 'completed') return 'active_writing';
    return 'stable';
  }

  function openDetails(item: ArchiveCatalogItem) {
    selectedTaskID = item.task.id;
    detailsOpen = true;
  }

  function openAction(item: ArchiveCatalogItem, action: string, source: 'local' | 'remote' = 'local') {
    activeTask = item.task;
    selectedAction = action;
    if (action === 'restore') {
      sourceChoice = item.task.restore_source === 'local' && item.remote_available ? 'remote'
        : item.task.restore_source === 'remote' && item.local_available ? 'local'
          : item.local_available ? 'local' : 'remote';
    } else if (action === 'copy_local' || action === 'move_local') sourceChoice = 'remote';
    else if (action === 'copy_remote' || action === 'move_remote') sourceChoice = 'local';
    else sourceChoice = source;
    preview = null;
    updateIfNewer = false;
    recoverUpdate = false;
    confirmation = '';
    password = '';
    message = '';
    returnToDetails = true;
    detailsOpen = false;
  }

  function closeAction() {
    if (busy) return;
    activeTask = null;
    preview = null;
    updateIfNewer = false;
    recoverUpdate = false;
    password = '';
    confirmation = '';
    message = '';
    if (returnToDetails) detailsOpen = true;
    returnToDetails = false;
  }

  async function inspect(update = false, recover = false) {
    if (!activeTask || busy) return;
    busy = true;
    message = '';
    try {
      const response = await request(`/api/v1/archive-center/${activeTask.id}/preview`, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ action: selectedAction, source: sourceChoice, archive_password: password, update_if_newer: update, recover_update: recover }) });
      const body = await response.json() as ArchiveManagementPreview & { error?: string };
      if (!response.ok) throw new Error(body.error || '无法生成归档操作预览');
      preview = body;
      updateIfNewer = update;
      recoverUpdate = recover;
      confirmation = '';
    } catch (error) { message = error instanceof Error ? error.message : '归档预览失败'; }
    finally { busy = false; }
  }

  async function execute() {
    if (!activeTask || !preview || !canConfirm) return;
    busy = true;
    message = '';
    try {
      const response = await request(`/api/v1/archive-center/${activeTask.id}/execute`, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ preview_id: preview.preview_id, confirmation_code: confirmation.trim(), confirm: true, archive_password: password }) });
      const body = await response.json() as ArchiveTask & { error?: string };
      if (!response.ok) throw new Error(body.error || '归档操作未能启动');
      message = selectedAction === 'restore' ? (recoverUpdate ? '已恢复到更新前版本，请重新查看影响后决定是否更新。' : updateIfNewer ? '更新任务已启动，完成后会同步显示结果。' : '恢复任务已启动，结果会同步显示在归档详情中。') : '管理任务已启动，结果会同步显示在归档详情中。';
      preview = null;
      await load();
      onChanged();
    } catch (error) { message = error instanceof Error ? error.message : '归档操作失败'; }
    finally { busy = false; }
  }

  function actionLabel(action: string) {
    return ({ restore: '恢复到 .codex', copy_local: '复制到本地', copy_remote: '复制到云端', move_local: '迁移到本地', move_remote: '迁移到云端', delete_local: '删除本地副本', delete_remote: '删除云端副本' } as Record<string, string>)[action] || action;
  }

  function actionDirection(action: string, source: 'local' | 'remote') {
    if (action === 'restore') return `${source === 'local' ? '本地' : '云端'}副本 → 当前 .codex`;
    if (action === 'copy_local' || action === 'move_local') return `云端副本 → ${localRoot || '本地归档目录'}`;
    if (action === 'copy_remote' || action === 'move_remote') return `本地副本 → ${remoteTarget || '云端仓库'}`;
    return action === 'delete_local' ? '直接删除本地归档文件夹' : '从云端仓库移除归档';
  }

  function actionEffect(action: string) {
    const lastLocalCopy = activeItem?.local_available && !activeItem.remote_available;
    const lastRemoteCopy = activeItem?.remote_available && !activeItem.local_available;
    return ({
      restore: updateIfNewer ? '用所选归档替换当前版本；仅在目标与已核验旧版本一致时执行。' : '目标内容一致时跳过；发现差异时阻止覆盖。',
      copy_local: '复制并校验成功后保留云端副本。',
      copy_remote: '上传并校验成功后保留本地副本。',
      move_local: '本地副本校验成功后才删除云端副本。',
      move_remote: '云端副本校验成功后才删除本地副本。',
      delete_local: lastLocalCopy ? '这是唯一归档副本，删除后无法恢复。' : '永久删除本地归档文件夹；云端副本与 .codex 源数据不受影响。',
      delete_remote: lastRemoteCopy ? '这是唯一云端副本；从仓库移除后，Git 历史中仍可能保留内容。' : '本地副本与 .codex 源数据不受影响。'
    } as Record<string, string>)[action] || '';
  }

  function restoreStatusLabel(status: string) {
    return ({ ready: '可恢复', already_present: '内容已存在', restored: '已恢复', updated: '已更新', update_ready: '可更新', recovery_required: '更新中断', update_rolled_back: '已回滚更新', conflict: '发现冲突', partial: '部分内容存在' } as Record<string, string>)[status] || status;
  }

  function formatTime(value?: string) {
    if (!value) return '时间未知';
    const date = new Date(value);
    return Number.isNaN(date.getTime()) ? '时间未知' : date.toLocaleString('zh-CN');
  }

  function formatBytes(value: number) {
    if (value < 1024) return `${value} B`;
    const units = ['KB', 'MB', 'GB', 'TB'];
    let size = value / 1024, index = 0;
    while (size >= 1024 && index < units.length - 1) { size /= 1024; index++; }
    return `${size.toFixed(size >= 100 ? 0 : 1)} ${units[index]}`;
  }
</script>

<main class="archives-page">
  <header class="archives-heading">
    <div><div class="archives-title"><Archive aria-hidden="true" /><h1>归档中心</h1></div><p>管理已保存的 Codex 对话副本</p></div>
    <div class="archives-actions"><button type="button" class="secondary-button" on:click={onSettings}>存储设置</button><button type="button" class="archive-icon-button" aria-label="刷新归档" title="刷新归档" on:click={load} disabled={loading || busy}><RefreshCw aria-hidden="true" /></button></div>
  </header>

  <section class="archive-location-strip" aria-label="归档位置">
    <div class="archive-location-entry"><HardDrive aria-hidden="true" /><div><strong>本地</strong><span title={localRoot}>{localRoot || '未配置归档目录'}</span></div></div>
    <div class="archive-location-entry"><Cloud aria-hidden="true" /><div><strong>云端</strong><span title={remoteTarget}>{remoteTarget || (remoteError ? '未连接' : '未选择仓库')}</span></div>{#if remoteError}<span class="archive-location-warning" title={remoteError}>需要设置</span>{/if}</div>
  </section>

  <section class="archive-overview" aria-label="归档统计">
    <div><strong>{filterCounts.all}</strong><span>归档记录</span></div>
    <div><strong>{filterCounts.paired}</strong><span>双副本</span></div>
    <div><strong>{filterCounts.local}</strong><span>含本地副本</span></div>
    <div><strong>{filterCounts.remote}</strong><span>含云端副本</span></div>
    <div><strong>{filterCounts.attention}</strong><span>需复核</span></div>
  </section>

  {#if activeArchiveTask && activeCatalogItem}<div class="archive-task-banner" role="status"><LoaderCircle aria-hidden="true" /><span><strong>{archiveTitle(activeArchiveTask)}</strong> · {taskStatus(activeCatalogItem)}</span><button type="button" class="text-action" on:click={load}>刷新状态</button></div>{/if}

  <div class="archive-toolbar">
    <label class="archive-search"><Search aria-hidden="true" /><input bind:value={searchQuery} placeholder="搜索标题或对话 ID" aria-label="搜索归档标题或对话 ID" />{#if searchQuery}<button type="button" aria-label="清除搜索" title="清除搜索" on:click={() => searchQuery = ''}><X aria-hidden="true" /></button>{/if}</label>
    <label class="archive-sort"><span>排序</span><select bind:value={sortOrder} aria-label="归档排序"><option value="newest">最近归档</option><option value="oldest">最早归档</option><option value="largest">体积最大</option></select></label>
  </div>

  <nav class="archive-filters" aria-label="归档筛选">{#each filters as filter}<button type="button" class:active={selectedView === filter.id} aria-pressed={selectedView === filter.id} on:click={() => selectedView = filter.id}>{filter.label}<span>{filterCounts[filter.id]}</span></button>{/each}</nav>

  {#if catalogError}<div class="archive-error" role="alert"><CircleAlert aria-hidden="true" /><span>{catalogError}</span><button type="button" class="secondary-button" on:click={load}>重试</button></div>
  {:else if loading && !items.length}<div class="archive-empty" role="status"><LoaderCircle class="archive-loading" aria-hidden="true" /><span>正在读取归档…</span></div>
  {:else if !visibleItems.length}<div class="archive-empty"><Archive aria-hidden="true" /><strong>{items.length ? '没有匹配的归档' : '还没有归档记录'}</strong><span>{items.length ? '修改搜索词或筛选条件后重试。' : '从会话库归档对话后，副本会显示在这里。'}</span>{#if items.length}<button type="button" class="secondary-button" on:click={() => { searchQuery = ''; selectedView = 'all'; }}>清除筛选</button>{:else}<button class="secondary-button" type="button" on:click={onLibrary}>打开会话库</button>{/if}</div>
  {:else}
    <section class="archive-table" aria-label="归档列表">
      <div class="archive-table-head" aria-hidden="true"><span>归档内容</span><span>副本位置</span><span>占用空间</span><span>归档时间</span><span>状态</span><span></span></div>
      <div class="archive-list" role="list">
      {#each visibleItems as item (item.task.id)}
        <div class="archive-row-list-item" role="listitem">
        <button type="button" class="archive-row" class:selected={selectedTaskID === item.task.id} aria-pressed={selectedTaskID === item.task.id} on:click={() => openDetails(item)}>
          <span class="archive-row-content"><span class="archive-row-title"><strong title={archiveTitle(item.task)}>{archiveTitle(item.task)}</strong>{#if item.task.encrypted}<span class="archive-encrypted" title="客户端加密"><ShieldCheck aria-hidden="true" /></span>{/if}</span><code class="archive-row-ids" title={archiveIDs(item.task).join(' · ')}>{archiveIDs(item.task).length === 1 ? archiveIDs(item.task)[0] : `${archiveIDs(item.task).length} 个对话 · ${archiveIDs(item.task).slice(0, 2).join(' · ')}${archiveIDs(item.task).length > 2 ? ' …' : ''}`}</code>{#if needsAttention(item)}<small class="archive-row-error">{item.task.management_error || item.task.error || '当前没有可用副本'}</small>{/if}</span>
          <span class="archive-row-copies"><span class:present={item.local_available}><HardDrive aria-hidden="true" />本地</span><span class:present={item.remote_available}><Cloud aria-hidden="true" />云端</span></span>
          <span class="archive-row-size"><strong>{formatBytes(archiveBytes(item.task))}</strong>{#if item.task.compression}<small>原始 {formatBytes(item.task.compression.original_bytes)}</small>{/if}</span>
          <time class="archive-row-time" datetime={item.task.created_at} title={formatTime(item.task.created_at)}>{formatTime(item.task.created_at)}</time>
          <span class={`status-pill ${taskStatusClass(item)}`}>{taskStatus(item)}</span>
          <ChevronRight class="archive-row-chevron" aria-hidden="true" />
        </button>
        </div>
      {/each}
      </div>
    </section>
  {/if}
</main>

<LibraryDrawer bind:open={detailsOpen} title={selectedTitle} description={selectedItem ? `${selectedItem.task.source_ids.length} 个对话 · 归档于 ${formatTime(selectedItem.task.created_at)}` : ''}>
  {#if selectedItem}
    <div class="archive-detail">
      <section class="archive-detail-section">
        <div class="archive-detail-heading"><strong>副本位置</strong><span class={`status-pill ${taskStatusClass(selectedItem)}`}>{taskStatus(selectedItem)}</span></div>
        <div class="archive-copy-location"><HardDrive aria-hidden="true" /><div><strong>本地归档</strong><span>{selectedItem.local_available ? localRoot || '本地副本已发现' : '无本地副本'}</span>{#if restoreCheckLabel(selectedItem.task, 'local')}<small>{restoreCheckLabel(selectedItem.task, 'local')}</small>{/if}</div><Check class={selectedItem.local_available ? 'copy-visible' : ''} aria-label={selectedItem.local_available ? '本地副本存在' : '无本地副本'} /></div>
        <div class="archive-copy-location"><Cloud aria-hidden="true" /><div><strong>云端归档</strong><span>{selectedItem.remote_available ? selectedItem.task.target || remoteTarget || '云端副本已发现' : remoteError || '无云端副本'}</span>{#if restoreCheckLabel(selectedItem.task, 'remote')}<small>{restoreCheckLabel(selectedItem.task, 'remote')}</small>{/if}</div><Check class={selectedItem.remote_available ? 'copy-visible' : ''} aria-label={selectedItem.remote_available ? '云端副本存在' : '无云端副本'} /></div>
      </section>

      <section class="archive-detail-section archive-detail-stats">
        <div><span>当前副本</span><strong>{formatBytes(archiveBytes(selectedItem.task))}</strong></div>
        <div><span>原始大小</span><strong>{formatBytes(selectedItem.task.compression?.original_bytes ?? selectedItem.task.source_bytes)}</strong></div>
        <div><span>归档时间</span><strong>{formatTime(selectedItem.task.created_at)}</strong></div>
      </section>

      <section class="archive-detail-section">
        <div class="archive-detail-heading"><strong>恢复与核验</strong>{#if selectedItem.task.restore_status === 'restored'}<span class="status-pill stable">最近恢复成功</span>{:else if selectedItem.task.restore_status === 'already_present'}<span class="status-pill stable">最近仅核验</span>{/if}</div>
        {#if hasRestoreRecord(selectedItem.task)}
          {#if restoreCheckLabel(selectedItem.task, 'local') || restoreCheckLabel(selectedItem.task, 'remote')}
            <div class="archive-restore-checks">
              {#each archiveSources as source (source.id)}
                {#if restoreCheckLabel(selectedItem.task, source.id)}
                  <div><span>{#if source.id === 'local'}<HardDrive aria-hidden="true" />{:else}<Cloud aria-hidden="true" />{/if}{source.label}</span><strong>{restoreCheckLabel(selectedItem.task, source.id)}</strong></div>
                {/if}
              {/each}
            </div>
          {:else}<p class="archive-restore-record archive-muted">旧版本已记录{selectedItem.task.restore_status === 'restored' ? '恢复成功' : '内容已存在'}{#if selectedItem.task.restored_at} · {formatTime(selectedItem.task.restored_at)}{/if}</p>{/if}
          <small class="archive-restore-note">每条记录只代表当时的结果。恢复前仍会重新比较目标；差异会阻止覆盖。</small>
        {:else}<p class="archive-restore-record archive-muted">尚无恢复记录</p>{/if}
        {#if !selectedItem.task.compression}<small class="legacy-note">旧格式归档暂不支持完整会话恢复。</small>{/if}
      </section>

      <section class="archive-detail-section">
        <details class="archive-identifiers"><summary>{archiveIDs(selectedItem.task).length} 个原始 conversation ID</summary><ul>{#each archiveIDs(selectedItem.task) as id}<li><code>{id}</code></li>{/each}</ul></details>
        {#if selectedItem.task.error || selectedItem.task.management_error}<p class="archive-row-error">{selectedItem.task.management_error || selectedItem.task.error}</p>{/if}
      </section>

      <section class="archive-detail-section archive-management-actions">
        <strong>操作</strong>
        {#if selectedItem.task.compression && (selectedItem.local_available || selectedItem.remote_available)}<button type="button" class="archive-primary-action" on:click={() => openAction(selectedItem, 'restore')} disabled={selectedItem.task.management_state === 'running'}><ArrowDownToLine aria-hidden="true" />恢复或核验到 .codex</button>{/if}
        {#if selectedItem.local_available && selectedItem.remote_available}
          <div class="archive-action-group"><span>保留位置</span><small>保留一份会删除另一位置的副本，并要求输入确认数字。</small><div class="archive-action-pair archive-retain-actions"><button type="button" on:click={() => openAction(selectedItem, 'delete_remote', 'remote')} disabled={Boolean(remoteError) || selectedItem.task.management_state === 'running'}><HardDrive aria-hidden="true" />仅保留本地</button><button type="button" on:click={() => openAction(selectedItem, 'delete_local', 'local')} disabled={selectedItem.task.management_state === 'running'}><Cloud aria-hidden="true" />仅保留云端</button></div></div>
        {:else if selectedItem.local_available}
          <div class="archive-action-group"><span>云端副本尚未保存</span><div class="archive-action-pair"><button type="button" on:click={() => openAction(selectedItem, 'copy_remote')} disabled={Boolean(remoteError) || selectedItem.task.management_state === 'running'}><Cloud aria-hidden="true" />复制到云端</button><button type="button" on:click={() => openAction(selectedItem, 'move_remote')} disabled={Boolean(remoteError) || selectedItem.task.management_state === 'running'}><Cloud aria-hidden="true" />迁移到云端</button></div><button type="button" class="archive-last-copy-action" on:click={() => openAction(selectedItem, 'delete_local', 'local')} disabled={selectedItem.task.management_state === 'running'}><Trash2 aria-hidden="true" />删除唯一副本</button></div>
        {:else if selectedItem.remote_available}
          <div class="archive-action-group"><span>本地副本尚未保存</span><div class="archive-action-pair"><button type="button" on:click={() => openAction(selectedItem, 'copy_local')} disabled={selectedItem.task.management_state === 'running'}><HardDrive aria-hidden="true" />复制到本地</button><button type="button" on:click={() => openAction(selectedItem, 'move_local')} disabled={selectedItem.task.management_state === 'running'}><HardDrive aria-hidden="true" />迁移到本地</button></div><button type="button" class="archive-last-copy-action" on:click={() => openAction(selectedItem, 'delete_remote', 'remote')} disabled={Boolean(remoteError) || selectedItem.task.management_state === 'running'}><Trash2 aria-hidden="true" />删除唯一副本</button></div>
        {/if}
      </section>
    </div>
  {/if}
</LibraryDrawer>

{#if activeTask}
  <div class="archive-dialog-backdrop" role="presentation" on:click={(event) => event.target === event.currentTarget && closeAction()}>
    <dialog bind:this={dialog} class="archive-dialog" aria-labelledby="archive-action-title" aria-modal="true" on:cancel={closeAction} on:close={() => activeTask && !busy && closeAction()}>
      <header><div><span class="archive-action-kind"><Archive aria-hidden="true" />归档操作</span><h2 id="archive-action-title">{actionLabel(selectedAction)}</h2></div><button type="button" aria-label="关闭" on:click={closeAction} disabled={busy}>×</button></header>
      <div class="archive-dialog-body"><strong>{archiveTitle(activeTask)}</strong><code>{activeTask.source_ids.join(' · ')}</code>
        <div class="archive-action-route"><strong>{actionDirection(selectedAction, sourceChoice)}</strong><span>{actionEffect(selectedAction)}</span></div>
        {#if selectedAction === 'restore' || selectedAction === 'copy_local' || selectedAction === 'copy_remote' || selectedAction === 'move_local' || selectedAction === 'move_remote'}
          <div class="archive-source-select"><span>来源副本</span><label><input type="radio" bind:group={sourceChoice} value="local" disabled={Boolean(preview) || !activeItem?.local_available} />本地</label><label><input type="radio" bind:group={sourceChoice} value="remote" disabled={Boolean(preview) || !activeItem?.remote_available} />云端</label></div>
        {/if}
        {#if activeTask.encrypted && !preview}<label class="archive-password">归档密码<input type="password" bind:value={password} autocomplete="current-password" /></label>{/if}
        {#if remoteActionUnavailable}<p class="archive-dialog-message" role="status">{remoteError}</p>{/if}
        {#if preview?.restore}<div class="restore-preview"><div class="restore-preview-heading"><span class={`status-pill ${preview.restore.status === 'conflict' || preview.restore.status === 'partial' || preview.restore.status === 'recovery_required' ? 'needs_review' : preview.restore.status === 'ready' || preview.restore.status === 'restored' || preview.restore.status === 'update_ready' ? 'stable' : 'plan-prepared'}`}>{restoreStatusLabel(preview.restore.status)}</span><span>{preview.restore.ids.length} 个对话 · {preview.restore.files.length} 个文件 · {preview.restore.database_rows.reduce((total,row)=>total+row.rows,0)} 条数据库记录</span></div>{#if preview.restore.conflicts.length}<strong>影响项</strong><ul>{#each preview.restore.conflicts as conflict}<li>{conflict}</li>{/each}</ul>{/if}<small>{preview.restore.status === 'already_present' ? '当前 .codex 与所选副本完全一致，不会写入文件或数据库。确认后只记录本次核验来源。' : preview.restore.status === 'conflict' ? '目标内容有差异，普通恢复不会覆盖。' : preview.restore.status === 'partial' ? '目标内容只有部分存在，无法确认归属，操作已阻止。' : preview.restore.status === 'update_ready' ? '更新会替换所列会话文件、专属 SQLite 记录和索引记录；其他会话记录保留。' : preview.restore.status === 'recovery_required' ? '上次更新没有完成。回滚后会恢复到更新前状态，之后可以重新检查并选择是否更新。' : '仅恢复目标中缺失的文件与专属数据库行；已存在的内容不会覆盖。'}</small></div>{/if}
        {#if preview?.recover_available && !recoverUpdate}<div class="restore-update-choice"><strong>发现中断的更新</strong><span>目标可能处于新旧版本混合状态</span><small>{preview.update_reason || '先将内容恢复到更新前版本；完成后可重新预览更新。'}</small><button type="button" class="secondary-button" on:click={() => inspect(false, true)} disabled={busy}>查看回滚影响</button></div>
        {:else if preview?.recover_update}<div class="restore-update-choice"><strong>回滚未完成的更新</strong><small>将按 journal 恢复更新前的文件、SQLite 行和索引；不会删除归档副本。</small></div>
        {/if}
        {#if preview?.update_available && !updateIfNewer}<div class="restore-update-choice"><strong>发现较新的归档版本</strong><span>{formatTime(preview.update_from_at)} → {formatTime(preview.update_to_at)}</span><small>当前内容与上次恢复版本一致，可以选择更新。更新会替换此归档涉及的会话数据。</small><button type="button" class="secondary-button" on:click={() => inspect(true)} disabled={busy}>查看更新影响</button></div>
        {:else if preview?.update_if_newer && preview.restore?.status === 'update_ready'}<div class="restore-update-choice"><strong>确认更新版本</strong><span>{formatTime(preview.update_from_at)} → {formatTime(preview.update_to_at)}</span><small>已重新检查目标和归档。继续后将用较新归档版本替换当前会话内容。</small></div>
        {:else if preview?.update_reason && preview.restore?.status === 'conflict'}<p class="archive-dialog-message" role="status">不能安全更新：{preview.update_reason}</p>{/if}
        {#if preview && !preview.restore}<div class="archive-preview-ready"><Check aria-hidden="true" /><span><strong>来源副本已校验</strong><small>{actionEffect(selectedAction)}</small></span></div>{/if}
        {#if requiresCode}<label class="archive-code-label">输入确认数字 <code>{preview?.code}</code><input bind:value={confirmation} inputmode="numeric" maxlength="6" autocomplete="off" /></label>{/if}
        {#if message}<p class="archive-dialog-message" role="status">{message}</p>{/if}
      </div>
      <footer><button type="button" class="secondary-button" on:click={closeAction} disabled={busy}>取消</button>{#if !preview}<button type="button" class="primary-action" on:click={() => inspect(false)} disabled={busy || remoteActionUnavailable}>{busy ? '检查中…' : '查看影响'}</button>{:else}<button type="button" class="primary-action" on:click={execute} disabled={!canConfirm}>{busy ? '处理中…' : recoverUpdate ? '确认回滚到更新前版本' : preview.restore?.status === 'already_present' ? '记录核验结果' : ['conflict', 'partial', 'recovery_required'].includes(preview.restore?.status || '') ? '存在冲突，无法执行' : preview.restore?.status === 'update_ready' ? '确认更新到此版本' : selectedAction === 'restore' ? '确认恢复到 .codex' : `确认${actionLabel(selectedAction)}`}</button>{/if}</footer>
    </dialog>
  </div>
{/if}

<style>
  .archives-page { display: grid; gap: var(--section-gap); width: min(100%, 1440px); min-width: 0; margin: 0 auto; padding: var(--content-inset); }
  .archives-heading, .archives-actions, .archives-title { display: flex; align-items: center; min-width: 0; }
  .archives-heading { justify-content: space-between; gap: var(--space-3); }
  .archives-heading > div:first-child { min-width: 0; }
  .archives-title { gap: var(--space-2); }
  .archives-title :global(svg) { width: 20px; height: 20px; color: var(--accent); }
  h1 { margin: 0; font-size: var(--font-title); }
  .archives-heading p { margin: var(--space-1) 0 0; color: var(--text-muted); font-size: var(--font-caption); }
  .archives-actions { gap: var(--space-2); }
  .archive-icon-button { display: grid; place-items: center; width: var(--control-md); min-width: var(--control-md); height: var(--control-md); border: 1px solid var(--border); border-radius: var(--radius-control); background: var(--bg-surface); color: var(--text-secondary); cursor: pointer; }
  .archive-icon-button :global(svg) { width: 16px; height: 16px; }
  .archive-location-strip { display: grid; grid-template-columns: repeat(2, minmax(0, 1fr)); border-block: 1px solid var(--border); }
  .archive-location-entry { display: flex; align-items: center; gap: var(--space-2); min-width: 0; padding: var(--space-3) var(--space-2); }
  .archive-location-entry + .archive-location-entry { border-left: 1px solid var(--border); }
  .archive-location-entry > :global(svg) { width: 18px; height: 18px; flex: 0 0 18px; color: var(--text-muted); }
  .archive-location-entry > div { display: grid; gap: 2px; min-width: 0; }
  .archive-location-entry strong { font-size: var(--font-caption); font-weight: 600; }
  .archive-location-entry span { overflow: hidden; color: var(--text-muted); font-size: var(--font-caption); text-overflow: ellipsis; white-space: nowrap; }
  .archive-location-entry .archive-location-warning { flex: 0 0 auto; color: var(--warning); }
  .archive-overview { display: grid; grid-template-columns: repeat(5, minmax(0, 1fr)); border-bottom: 1px solid var(--border); }
  .archive-overview > div { display: grid; gap: 3px; padding: var(--space-2) var(--space-3); border-left: 1px solid var(--border); }
  .archive-overview > div:first-child { border-left: 0; padding-left: var(--space-2); }
  .archive-overview strong { font-size: var(--font-heading); font-variant-numeric: tabular-nums; line-height: 1.1; }
  .archive-overview span, .archive-task-banner { color: var(--text-muted); font-size: var(--font-caption); }
  .archive-task-banner { display: flex; align-items: center; gap: var(--space-2); min-width: 0; padding: var(--space-2) var(--space-3); border-left: 2px solid var(--accent); background: var(--bg-subtle); }
  .archive-task-banner > :global(svg) { flex: 0 0 16px; width: 16px; height: 16px; animation: archive-spin .9s linear infinite; }
  .archive-task-banner > span { min-width: 0; flex: 1; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
  .archive-task-banner strong { color: var(--text-primary); }
  .text-action { flex: 0 0 auto; border: 0; background: transparent; color: var(--accent); cursor: pointer; font: inherit; font-size: var(--font-caption); }
  .archive-toolbar { display: flex; justify-content: space-between; align-items: center; gap: var(--space-3); min-width: 0; }
  .archive-search { display: flex; align-items: center; gap: var(--space-2); width: min(440px, 100%); min-width: 0; height: var(--control-md); padding: 0 var(--space-2); border: 1px solid var(--border-strong); border-radius: var(--radius-control); background: var(--bg-surface); color: var(--text-muted); }
  .archive-search > :global(svg) { flex: 0 0 16px; width: 16px; height: 16px; }
  .archive-search input { width: 100%; min-width: 0; height: 100%; border: 0; outline: 0; background: transparent; color: var(--text-primary); font-size: var(--font-ui); }
  .archive-search button { display: grid; place-items: center; width: var(--control-sm); height: var(--control-sm); border: 0; background: transparent; color: var(--text-muted); cursor: pointer; }
  .archive-search button :global(svg) { width: 16px; height: 16px; }
  .archive-sort { display: flex; align-items: center; gap: var(--space-2); color: var(--text-muted); font-size: var(--font-caption); }
  .archive-sort select { min-height: var(--control-md); padding: 0 var(--space-2); border: 1px solid var(--border); border-radius: var(--radius-control); background: var(--bg-surface); color: var(--text-primary); font: inherit; }
  .archive-filters { display: flex; gap: var(--space-1); overflow-x: auto; border-bottom: 1px solid var(--border); scrollbar-width: thin; }
  .archive-filters button { display: inline-flex; flex: 0 0 auto; align-items: center; gap: var(--space-1); min-height: var(--control-md); padding: 0 var(--space-2); border: 0; border-bottom: 2px solid transparent; background: transparent; color: var(--text-secondary); cursor: pointer; font: inherit; font-size: var(--font-caption); }
  .archive-filters button:hover, .archive-filters button.active { color: var(--text-primary); }
  .archive-filters button.active { border-bottom-color: var(--accent); }
  .archive-filters span { color: var(--text-muted); font-variant-numeric: tabular-nums; }
  .archive-table { min-width: 0; }
  .archive-table-head, .archive-row { display: grid; grid-template-columns: minmax(240px, 2fr) minmax(112px, .9fr) minmax(86px, .65fr) minmax(128px, .9fr) minmax(116px, .85fr) 16px; align-items: center; gap: var(--space-2); }
  .archive-table-head { padding: var(--space-2); color: var(--text-muted); font-size: var(--font-caption); }
  .archive-row { width: 100%; min-height: 72px; padding: var(--space-2); border: 0; border-bottom: 1px solid var(--border); background: transparent; color: var(--text-primary); cursor: pointer; text-align: left; }
  .archive-row:hover, .archive-row.selected { background: var(--bg-hover); }
  .archive-row-content { display: grid; gap: 4px; min-width: 0; }
  .archive-row-title { display: flex; align-items: center; gap: var(--space-1); min-width: 0; }
  .archive-row-title strong { min-width: 0; overflow: hidden; font-size: var(--font-ui); font-weight: 550; text-overflow: ellipsis; white-space: nowrap; }
  .archive-encrypted { display: inline-flex; flex: 0 0 auto; color: var(--text-muted); }
  .archive-encrypted :global(svg) { width: 14px; height: 14px; }
  .archive-row-ids { overflow: hidden; color: var(--text-muted); font-size: var(--font-caption); text-overflow: ellipsis; white-space: nowrap; }
  .archive-row-copies { display: flex; flex-wrap: wrap; gap: 4px var(--space-2); min-width: 0; color: var(--text-muted); font-size: var(--font-caption); }
  .archive-row-copies span { display: inline-flex; align-items: center; gap: 4px; white-space: nowrap; }
  .archive-row-copies :global(svg) { width: 14px; height: 14px; }
  .archive-row-copies .present { color: var(--text-primary); }
  .archive-row-size { display: grid; gap: 2px; min-width: 0; font-size: var(--font-caption); font-variant-numeric: tabular-nums; }
  .archive-row-size strong { font-weight: 550; }
  .archive-row-size small, .archive-row-time { color: var(--text-muted); font-size: var(--font-caption); }
  .archive-row-time { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
  .archive-row .status-pill { justify-self: start; max-width: 100%; overflow: hidden; text-overflow: ellipsis; }
  .archive-row-chevron { width: 16px; height: 16px; color: var(--text-muted); }
  .archive-row-error { grid-column: 1 / -1; margin: 0; color: var(--warning); font-size: var(--font-caption); overflow-wrap: anywhere; }
  .archive-empty { min-height: 220px; display: grid; align-content: center; justify-items: center; gap: var(--space-2); color: var(--text-muted); text-align: center; }
  .archive-empty > :global(svg) { width: 22px; height: 22px; }
  .archive-empty strong { color: var(--text-primary); font-size: var(--font-ui); }
  .archive-empty span { font-size: var(--font-caption); }
  .archive-error { display: flex; align-items: center; gap: var(--space-2); padding: var(--space-2) var(--space-3); border-left: 2px solid var(--danger); color: var(--danger); font-size: var(--font-caption); }
  .archive-error > span { min-width: 0; flex: 1; overflow-wrap: anywhere; }
  .archive-error :global(svg) { width: 16px; height: 16px; flex: 0 0 16px; }
  .archive-loading { animation: archive-spin .9s linear infinite; }
  @keyframes archive-spin { to { transform: rotate(360deg); } }
  .archive-detail { display: grid; gap: 0; }
  .archive-detail-section { display: grid; gap: var(--space-2); min-width: 0; padding: var(--space-3) 0; border-bottom: 1px solid var(--border); }
  .archive-detail-section:first-child { padding-top: 0; }
  .archive-detail-heading { display: flex; justify-content: space-between; align-items: center; gap: var(--space-2); min-width: 0; }
  .archive-detail-heading > strong, .archive-management-actions > strong { font-size: var(--font-ui); font-weight: 600; }
  .archive-copy-location { display: grid; grid-template-columns: 18px minmax(0, 1fr) 16px; align-items: center; gap: var(--space-2); min-width: 0; }
  .archive-copy-location > :global(svg) { width: 16px; height: 16px; color: var(--text-muted); }
  .archive-copy-location > div { display: grid; gap: 2px; min-width: 0; }
  .archive-copy-location strong { font-size: var(--font-caption); }
  .archive-copy-location span { overflow: hidden; color: var(--text-muted); font-size: var(--font-caption); text-overflow: ellipsis; white-space: nowrap; }
  .archive-copy-location small { color: var(--text-muted); font-size: var(--font-caption); line-height: 1.4; overflow-wrap: anywhere; }
  .archive-copy-location > :global(svg:last-child) { opacity: 0; }
  .archive-copy-location > :global(svg.copy-visible) { color: var(--success); opacity: 1; }
  .archive-detail-stats { grid-template-columns: repeat(2, minmax(0, 1fr)); }
  .archive-detail-stats > div { display: grid; gap: 3px; min-width: 0; }
  .archive-detail-stats > div:last-child { grid-column: 1 / -1; }
  .archive-detail-stats span, .archive-muted, .archive-restore-note { color: var(--text-muted); font-size: var(--font-caption); }
  .archive-detail-stats strong { overflow-wrap: anywhere; font-size: var(--font-caption); font-weight: 550; }
  .archive-restore-record { margin: 0; color: var(--text-primary); font-size: var(--font-caption); line-height: 1.5; overflow-wrap: anywhere; }
  .archive-restore-note { line-height: 1.5; }
  .archive-restore-checks { display: grid; gap: var(--space-2); }
  .archive-restore-checks > div { display: grid; gap: 3px; min-width: 0; }
  .archive-restore-checks > div > span { display: inline-flex; align-items: center; gap: 6px; color: var(--text-secondary); font-size: var(--font-caption); }
  .archive-restore-checks > div > span :global(svg) { width: 14px; height: 14px; }
  .archive-restore-checks strong { color: var(--text-primary); font-size: var(--font-caption); font-weight: 500; overflow-wrap: anywhere; }
  .archive-identifiers { color: var(--text-secondary); font-size: var(--font-caption); }
  .archive-identifiers summary { cursor: pointer; }
  .archive-identifiers ul { display: grid; gap: var(--space-1); max-height: 200px; margin: var(--space-2) 0 0; padding-left: 18px; overflow: auto; }
  .archive-identifiers code { overflow-wrap: anywhere; color: var(--text-muted); }
  .legacy-note { color: var(--warning); font-size: var(--font-caption); }
  .archive-management-actions { gap: var(--space-2); }
  .archive-action-group { display: grid; gap: var(--space-2); min-width: 0; padding-top: var(--space-2); border-top: 1px solid var(--border); }
  .archive-action-group > span { color: var(--text-secondary); font-size: var(--font-caption); font-weight: 600; }
  .archive-action-group > small { color: var(--text-muted); font-size: var(--font-caption); line-height: 1.45; }
  .archive-management-actions button { display: inline-flex; align-items: center; justify-content: center; gap: var(--space-1); min-width: 0; min-height: var(--control-md); padding: 0 var(--space-2); border: 1px solid var(--border); border-radius: var(--radius-control); background: var(--bg-surface); color: var(--text-secondary); cursor: pointer; font: inherit; font-size: var(--font-caption); }
  .archive-management-actions button:hover { border-color: var(--border-strong); background: var(--bg-hover); color: var(--text-primary); }
  .archive-management-actions button :global(svg) { width: 15px; height: 15px; flex: 0 0 15px; }
  .archive-management-actions .archive-primary-action { justify-content: flex-start; width: 100%; border-color: var(--accent-solid); background: var(--accent-solid); color: var(--on-accent); font-weight: 600; }
  .archive-action-pair { display: grid; grid-template-columns: repeat(2, minmax(0, 1fr)); gap: var(--space-2); }
  .archive-management-actions .archive-last-copy-action { justify-self: start; border-color: var(--danger-border); color: var(--danger); }
  .archive-management-actions button:disabled { opacity: .5; cursor: not-allowed; }
  .archive-dialog-backdrop { position: fixed; inset: 0; z-index: 1000; display: grid; place-items: center; padding: 16px; background: var(--overlay-backdrop, #0008); }
  .archive-dialog::backdrop { background: transparent; }
  .archive-dialog { position: fixed; inset: 50% auto auto 50%; transform: translate(-50%, -50%); width: min(560px, calc(100vw - 32px)); max-height: min(90dvh, 760px); margin: 0; padding: 0; display: grid; grid-template-rows: auto minmax(0, 1fr) auto; overflow: hidden; border: 1px solid var(--border-strong); border-radius: var(--radius-dialog); background: var(--bg-surface); color: var(--text-primary); box-shadow: var(--shadow); }
  .archive-dialog header, .archive-dialog footer { display: flex; align-items: center; justify-content: space-between; gap: var(--space-3); padding: var(--space-3) var(--space-4); border-bottom: 1px solid var(--border); }
  .archive-dialog header > div { display: grid; gap: 3px; min-width: 0; }
  .archive-dialog h2 { margin: 0; font-size: var(--font-heading); }
  .archive-dialog header button { display: grid; place-items: center; width: var(--control-md); height: var(--control-md); border: 1px solid var(--border); border-radius: var(--radius-control); background: var(--bg-surface); color: var(--text-secondary); font-size: 22px; cursor: pointer; }
  .archive-action-kind { display: inline-flex; align-items: center; gap: 6px; color: var(--text-muted); font-size: var(--font-caption); }
  .archive-action-kind :global(svg) { width: 15px; height: 15px; }
  .archive-dialog-body { display: grid; align-content: start; gap: var(--space-3); min-height: 0; overflow: auto; padding: var(--space-4); }
  .archive-dialog-body > strong { overflow-wrap: anywhere; font-size: var(--font-ui); }
  .archive-dialog-body > code { color: var(--text-muted); font-size: var(--font-caption); overflow-wrap: anywhere; }
  .archive-action-route { display: grid; gap: 3px; padding: var(--space-2) 0; border-block: 1px solid var(--border); }
  .archive-action-route strong { overflow-wrap: anywhere; font-size: var(--font-caption); font-weight: 600; }
  .archive-action-route span, .archive-preview-ready small { color: var(--text-muted); font-size: var(--font-caption); }
  .archive-source-select { display: flex; align-items: center; flex-wrap: wrap; gap: var(--space-3); color: var(--text-secondary); font-size: var(--font-caption); }
  .archive-source-select label { display: inline-flex; align-items: center; gap: 5px; }
  .archive-password, .archive-code-label { display: grid; gap: var(--space-2); color: var(--text-secondary); font-size: var(--font-caption); }
  .archive-password input, .archive-code-label input { width: 100%; min-width: 0; height: var(--control-lg, 40px); padding: 8px 10px; border: 1px solid var(--border-strong); border-radius: var(--radius-control); background: var(--bg-surface); color: var(--text-primary); font: inherit; }
  .archive-code-label code { justify-self: start; padding: 5px 9px; border-radius: var(--radius-control); background: var(--bg-subtle); font: 600 20px var(--font-mono, monospace); }
  .restore-preview { display: grid; gap: var(--space-2); padding: var(--space-2) 0; border-block: 1px solid var(--border); color: var(--text-secondary); font-size: var(--font-caption); }
  .restore-update-choice { display: grid; gap: var(--space-1); padding: var(--space-2); border: 1px solid var(--border); border-left: 3px solid var(--accent); border-radius: var(--radius-control); background: var(--bg-surface); color: var(--text-secondary); font-size: var(--font-caption); }
  .restore-update-choice strong { color: var(--text-primary); }
  .restore-update-choice span { color: var(--text-primary); font-variant-numeric: tabular-nums; }
  .restore-update-choice small { color: var(--text-muted); line-height: 1.5; }
  .restore-update-choice button { justify-self: start; margin-top: var(--space-1); }
  .restore-preview-heading { display: flex; align-items: center; flex-wrap: wrap; gap: var(--space-2); }
  .restore-preview ul { display: grid; gap: 4px; max-height: 160px; margin: 0; padding-left: 18px; color: var(--danger); }
  .restore-preview li { overflow-wrap: anywhere; }
  .restore-preview small { color: var(--text-muted); line-height: 1.5; }
  .archive-preview-ready { display: flex; align-items: flex-start; gap: var(--space-2); padding: var(--space-2) 0; border-block: 1px solid var(--border); }
  .archive-preview-ready > :global(svg) { width: 16px; height: 16px; flex: 0 0 16px; color: var(--success); }
  .archive-preview-ready span { display: grid; gap: 3px; }
  .archive-preview-ready strong { font-size: var(--font-caption); }
  .archive-dialog-message { margin: 0; color: var(--danger); font-size: var(--font-caption); overflow-wrap: anywhere; }
  .archive-dialog footer { justify-content: flex-end; border-top: 1px solid var(--border); border-bottom: 0; }
  .primary-action { min-height: var(--control-md); padding: 8px 14px; border: 1px solid var(--accent-solid); border-radius: var(--radius-control); background: var(--accent-solid); color: var(--on-accent); font: inherit; font-size: var(--font-ui); font-weight: 600; cursor: pointer; }
  button:disabled { opacity: .5; cursor: not-allowed; }
  button:focus-visible, input:focus-visible, select:focus-visible, summary:focus-visible { outline: 2px solid var(--accent); outline-offset: 2px; }
  @media (max-width: 1200px) {
    .archives-page { padding: var(--space-3); }
    .archive-table-head, .archive-row { grid-template-columns: minmax(200px, 1.7fr) minmax(100px, .8fr) minmax(76px, .55fr) minmax(102px, .7fr) minmax(100px, .75fr) 14px; gap: var(--space-1); }
  }
  @media (max-width: 900px) {
    .archive-location-strip { grid-template-columns: 1fr; }
    .archive-location-entry + .archive-location-entry { border-left: 0; border-top: 1px solid var(--border); }
    .archive-table-head { display: none; }
    .archive-row { grid-template-columns: minmax(0, 1fr) auto 16px; padding-block: var(--space-3); }
    .archive-row-copies { grid-column: 2; grid-row: 1; }
    .archive-row-size, .archive-row-time, .archive-row .status-pill { grid-column: 1; }
    .archive-row-size { display: inline-flex; gap: var(--space-2); }
    .archive-row-size small::before { content: '原始 '; }
    .archive-row-chevron { grid-column: 3; grid-row: 1; }
  }
  @media (max-width: 560px) {
    .archives-heading { align-items: flex-start; }
    .archives-actions .secondary-button { min-height: var(--control-md); padding-inline: var(--space-2); }
    .archive-overview { grid-template-columns: repeat(3, minmax(0, 1fr)); }
    .archive-overview > div:nth-child(4) { border-left: 0; }
    .archive-toolbar { align-items: stretch; flex-direction: column; }
    .archive-search { width: 100%; }
    .archive-sort { justify-content: space-between; }
    .archive-row { grid-template-columns: minmax(0, 1fr) auto 14px; }
    .archive-row-copies { flex-direction: column; align-items: flex-start; }
    .archive-detail-stats { grid-template-columns: 1fr 1fr; }
    .archive-action-pair { grid-template-columns: 1fr; }
  }
  @media (prefers-reduced-motion: reduce) { .archive-loading, .archive-task-banner > :global(svg) { animation: none; } }
</style>
