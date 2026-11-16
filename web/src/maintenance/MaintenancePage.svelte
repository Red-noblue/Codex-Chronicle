<!-- 中文说明：存储维护工作区，分层呈现来源、候选、影响确认和中断恢复；真实权限以服务端快照为准。 -->
<script lang="ts">
  import { onMount } from 'svelte';
  import { ArrowRight, Check, ChevronRight, Copy, Database, File, Folder, HardDrive, Info, RefreshCw, Search, ShieldCheck, TriangleAlert, Undo2, X } from '@lucide/svelte';
  import LibraryDrawer from '../sessions/LibraryDrawer.svelte';
  import { actionLabel, artifactRows, basename, categoryLabels, directoryLabels, filterArtifactRows, filterDirectories, kindLabels, maintenanceViewSearch, parentPath, parseMaintenanceView, retentionDays, retentionFields, retentionPolicy, sourceAPIQuery, stateLabels, validSelection } from './view-state';
  import type { MaintenanceView, RetentionDays } from './view-state';
  import type { CleanupPreview, CleanupRecovery, CleanupResponse, MaintenanceDirectory, MaintenancePreview, MaintenanceReport, MaintenanceRow } from './types';

  export let sourceRoot = '';
  export let sourceName = '';
  export let snapshot = '';
  export let request: (url: string, options?: RequestInit) => Promise<Response>;
  export let confirmDestination: (options: { archive: boolean; trash: boolean }) => Promise<boolean>;
  export let onChanged: () => void | Promise<void>;
  export let onRescan: () => void;

  let view = parseMaintenanceView('');
  let source: MaintenanceReport | null = null;
  let appliedSourceView = { ...view };
  let cleanup: CleanupResponse | null = null;
  let sourceLoading = false;
  let cleanupLoading = false;
  let loadingMore = false;
  let sourceError = '';
  let cleanupError = '';
  let sourceRequest = 0;
  let cleanupRequest = 0;
  let mounted = false;
  let disposed = false;
  let requestedSnapshot = '';
  let selected = new Set<string>();
  let selectedDirectories = new Set<string>();
  let displayLimit = 30;
  let notice = '';
  let noticeError = false;
  let busy = '';
  let drawerOpen = false;
  let drawerMode: 'detail' | 'preview' | 'recover' = 'detail';
  let inspected: MaintenanceRow | null = null;
  let sourcePreview: MaintenancePreview | null = null;
  let cleanupPreview: CleanupPreview | null = null;
  let previewScope: 'source' | 'generated' = 'source';
  let previewRequested: Array<{ path: string; size: number; action: string }> = [];
  let previewFileLimit = 50;
  let previewFilesOpen = false;
  let recovery: CleanupRecovery | null = null;
  let previewError = '';
  let acknowledged = false;
  let copyMessage = '';
  let policyDays: RetentionDays | null = null;
  let policyMessage = '';
  let policyError = false;
  const tabs = [{ id: 'source', label: '目录清理' }, { id: 'generated', label: '本项目中间产物' }, { id: 'recovery', label: '恢复与复核' }, { id: 'retention', label: '保留规则' }] as const;
  const groups = [{ id: 'candidates', label: '待清理' }, { id: 'retained', label: '保留对象' }, { id: 'quarantined', label: '隔离区' }, { id: 'unknown', label: '未登记对象' }, { id: 'crash_residues', label: '中断残留' }] as const;
  const bytes = (value: number) => {
    if (!value) return '0 B';
    const units = ['B', 'KiB', 'MiB', 'GiB', 'TiB'];
    const index = Math.min(units.length - 1, Math.max(0, Math.floor(Math.log(value) / Math.log(1024))));
    return `${(value / 1024 ** index).toFixed(index ? 1 : 0)} ${units[index]}`;
  };
  const number = (value: number) => value.toLocaleString('zh-CN');
  const time = (value?: string) => value && !value.startsWith('0001-') && Number.isFinite(Date.parse(value)) ? new Date(value).toLocaleString('zh-CN', { hour12: false }) : '未知';

  $: snapshotKey = `${sourceRoot}|${snapshot}`;
  $: if (mounted && sourceRoot && !busy && snapshotKey !== requestedSnapshot) {
    requestedSnapshot = snapshotKey;
    void loadSource();
  }
  $: sourceRows = (source?.candidates ?? []).map((item): MaintenanceRow => ({ id: item.relative_path, path: item.relative_path, kind: item.category, size: item.size_bytes, modified: item.mod_time, status: item.status, eligible: item.eligible, reason: item.block_reason || item.reason, risk: item.risk, source: item }));
  $: directoryMode = view.tab === 'source' && view.layout === 'directories';
  $: directories = filterDirectories(source?.directories ?? [], appliedSourceView);
  $: chosenDirectories = (source?.directories ?? []).filter(item => selectedDirectories.has(item.relative_path));
  $: directoryFileCount = chosenDirectories.reduce((sum, item) => sum + item.eligible_count, 0);
  $: directoryRetainedCount = chosenDirectories.reduce((sum, item) => sum + item.retained_count, 0);
  $: allDirectoriesSelected = directories.some(item => item.eligible_count > 0) && directories.filter(item => item.eligible_count > 0).every(item => selectedDirectories.has(item.relative_path));
  $: ungroupedCount = source?.ungrouped_count ?? 0;
  $: generatedRows = filterArtifactRows(artifactRows(cleanup?.cleanup ?? null, view.collection), view);
  $: rows = view.tab === 'source' ? sourceRows : generatedRows.slice(0, displayLimit);
  $: selectedRows = rows.filter(row => selected.has(row.id));
  $: selectedBytes = selectedRows.reduce((sum, row) => sum + row.size, 0) + chosenDirectories.reduce((sum, item) => sum + item.eligible_bytes, 0);
  $: selectionCount = selected.size + selectedDirectories.size;
  $: eligibleRows = rows.filter(row => row.eligible);
  $: allSelected = eligibleRows.length > 0 && eligibleRows.every(row => selected.has(row.id));
  $: loading = sourceLoading || cleanupLoading || loadingMore || !!busy;
  $: listUnavailable = view.tab === 'source' ? !!sourceError : !!cleanupError || !cleanup?.available;
  $: reviewCount = (source?.blocked_count ?? 0) + (cleanup?.cleanup.unknown.length ?? 0) + (cleanup?.cleanup.crash_residues.length ?? 0) + (cleanup?.cleanup.recoveries.length ?? 0);
  $: activePreview = previewScope === 'source' ? sourcePreview : cleanupPreview;
  $: plan = activePreview?.plan;
  $: plannedItems = previewScope === 'source' ? (sourcePreview?.plan?.assets ?? []).map(item => ({ path: item.relative_path, size: item.size_bytes, action: actionLabel(item.action) })) : (cleanupPreview?.plan?.items ?? []).map(item => ({ path: item.manifest.target_path, size: item.before.size_bytes, action: '移入本项目隔离区' }));
  $: previewItems = plannedItems.length ? plannedItems : previewRequested;
  $: previewDirectories = previewScope === 'source' ? sourcePreview?.selection?.directories ?? [] : [];
  $: previewWritable = plannedItems.length > 0 && !!plan?.write_enabled && plan.status === 'prepared' && !activePreview?.blocked && !activePreview?.result && !previewError && (previewScope === 'source' ? !!source?.write_enabled && !sourceError : !!cleanup?.write_enabled && !cleanupError && cleanup.available);

  onMount(() => {
    view = parseMaintenanceView(window.location.search);
    mounted = true;
    void loadCleanup();
    const restore = () => {
      if (window.location.pathname !== '/maintenance' || busy) return;
      view = parseMaintenanceView(window.location.search);
      clearSelection();
      displayLimit = 30;
      void loadSource();
    };
    window.addEventListener('popstate', restore);
    return () => { disposed = true; sourceRequest++; cleanupRequest++; window.removeEventListener('popstate', restore); };
  });

  function syncLocation() { window.history.replaceState({}, '', `/maintenance${maintenanceViewSearch(view)}`); }
  function invalidatePreview() { sourcePreview = null; cleanupPreview = null; acknowledged = false; previewError = ''; previewFileLimit = 50; previewFilesOpen = false; }
  function clearSelection() { selected = new Set(); selectedDirectories = new Set(); invalidatePreview(); }
  function changeLayout(layout: MaintenanceView['layout'], directory = '') { view = { ...view, layout, directory, scope: '' }; applyFilters(); }
  function showUngrouped() { view = { ...parseMaintenanceView(''), layout: 'files', scope: 'ungrouped' }; applyFilters(); }
  function changeTab(tab: MaintenanceView['tab']) {
    if (busy || tab === view.tab) return;
    view = { ...parseMaintenanceView(''), tab };
    clearSelection(); notice = ''; displayLimit = 30; syncLocation();
    if (tab === 'source') void loadSource();
  }
  function applyFilters() {
    clearSelection(); displayLimit = 30; syncLocation();
    if (view.tab === 'source') void loadSource();
  }
  function resetFilters() { view = { ...parseMaintenanceView(''), tab: view.tab, collection: view.collection }; applyFilters(); }
  function selectCategory(category: string) { view = { ...view, category, directory: '', layout: ['sqlite_auxiliary', 'repair_jsonl_tail', 'orphan'].includes(category) ? 'files' : view.layout }; applyFilters(); }
  function selectGroup(collection: MaintenanceView['collection']) { view = { ...view, collection, query: '' }; applyFilters(); }
  function toggle(id: string, checked: boolean) {
    const next = new Set(selected);
    if (checked) next.add(id); else next.delete(id);
    selected = next; invalidatePreview(); notice = '';
  }
  function toggleAll() {
    selected = allSelected ? new Set() : new Set(eligibleRows.map(row => row.id));
    invalidatePreview(); notice = '';
  }
  function toggleDirectory(directory: MaintenanceDirectory, checked: boolean) {
    const next = new Set(selectedDirectories);
    if (checked) next.add(directory.relative_path); else next.delete(directory.relative_path);
    selectedDirectories = next; invalidatePreview(); notice = '';
  }
  function toggleAllDirectories() {
    selectedDirectories = allDirectoriesSelected ? new Set() : new Set(directories.filter(item => item.eligible_count > 0).map(item => item.relative_path));
    invalidatePreview(); notice = '';
  }
  function inspect(row: MaintenanceRow) { inspected = row; drawerMode = 'detail'; copyMessage = ''; drawerOpen = true; }
  async function copy(value: string) {
    try { await navigator.clipboard.writeText(value); copyMessage = '已复制完整路径'; }
    catch { copyMessage = '复制失败，可以选中完整路径手动复制。'; }
  }
  async function loadSource(more = false) {
    if (!sourceRoot || disposed) return;
    requestedSnapshot = snapshotKey;
    const token = ++sourceRequest;
    const root = sourceRoot;
    const previous = source;
    const queriedView = more ? appliedSourceView : { ...view };
    if (more) loadingMore = true;
    else { sourceLoading = true; loadingMore = false; clearSelection(); }
    sourceError = '';
    try {
      const response = await request(`/api/v1/maintenance${sourceAPIQuery(queriedView, more ? source?.candidates.length ?? 0 : 0)}`);
      const body = await response.json() as MaintenanceReport & { error?: string };
      if (!response.ok) throw new Error(body.error || `读取维护候选失败（HTTP ${response.status}）`);
      if (!Array.isArray(body.candidates) || !Array.isArray(body.categories) || !Array.isArray(body.directories) || typeof body.filtered_count !== 'number') throw new Error('维护数据格式不兼容，请更新本机服务。');
      if (disposed || token !== sourceRequest || root !== sourceRoot) return;
      if (more && previous && body.generated_at !== previous.generated_at) {
        notice = '扫描快照已更新，列表已重新载入。'; noticeError = false;
        await loadSource(); return;
      }
      appliedSourceView = queriedView;
      source = more && previous ? { ...body, candidate_offset: 0, candidates: [...previous.candidates, ...body.candidates.filter(item => !previous.candidates.some(existing => existing.relative_path === item.relative_path))] } : body;
      selected = validSelection(selected, source.candidates.map(item => ({ id: item.relative_path, eligible: item.eligible })));
    } catch (cause) {
      if (!disposed && token === sourceRequest && root === sourceRoot) sourceError = cause instanceof Error ? cause.message : '无法读取维护候选';
    } finally {
      if (token === sourceRequest) { sourceLoading = false; loadingMore = false; }
    }
  }
  async function loadCleanup() {
    if (disposed) return;
    const token = ++cleanupRequest;
    const root = sourceRoot;
    cleanupLoading = true; cleanupError = '';
    try {
      const response = await request('/api/v1/cleanup');
      const body = await response.json() as CleanupResponse;
      if (disposed || token !== cleanupRequest || root !== sourceRoot) return;
      if (!response.ok || body.error || !body.available) throw new Error(body.error || `中间产物登记区不可用（HTTP ${response.status}）`);
      if (!body.cleanup?.policy || !Array.isArray(body.cleanup.candidates)) throw new Error('中间产物数据格式不兼容，请更新本机服务。');
      cleanup = body;
      policyDays = retentionDays(body.cleanup.policy);
      if (view.tab === 'generated') selected = validSelection(selected, artifactRows(body.cleanup, view.collection));
    } catch (cause) {
      if (!disposed && token === cleanupRequest) cleanupError = cause instanceof Error ? cause.message : '无法检查本项目中间产物';
    } finally { if (token === cleanupRequest) cleanupLoading = false; }
  }
  async function refresh() {
    if (loading) return;
    clearSelection(); policyMessage = ''; notice = '';
    await Promise.allSettled([loadSource(), loadCleanup()]);
  }
  async function previewSelection(selectedIDs = [...selected], directoryPaths = [...selectedDirectories]) {
    if (!selectedIDs.length && !directoryPaths.length || loading || listUnavailable) return;
    previewScope = view.tab === 'source' ? 'source' : 'generated';
    previewRequested = rows.filter(row => selectedIDs.includes(row.id)).map(row => ({ path: row.path, size: row.size, action: '保留 · 尚未通过检查' }));
    if (directoryPaths.length) previewRequested = chosenDirectories.map(item => ({ path: item.relative_path, size: item.eligible_bytes, action: `检查 ${number(item.eligible_count)} 个候选文件` }));
    invalidatePreview(); drawerMode = 'preview'; drawerOpen = true; busy = '正在检查所选对象…';
    try {
      const response = await request(`/api/v1/${previewScope === 'source' ? 'maintenance' : 'cleanup'}/preview`, {
        method: 'POST', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(previewScope === 'source' ? { paths: selectedIDs, directories: directoryPaths } : { artifact_ids: selectedIDs })
      });
      const body = await response.json() as MaintenancePreview & CleanupPreview;
      if (disposed) return;
      if (previewScope === 'source') sourcePreview = body; else cleanupPreview = body;
      if (!body.plan || !response.ok && !body.blocked) previewError = body.error || `影响检查失败（HTTP ${response.status}）`;
    } catch (cause) { if (!disposed) previewError = cause instanceof Error ? cause.message : '无法查看处理影响'; }
    finally { busy = ''; }
  }
  async function execute() {
    if (!previewWritable || !acknowledged || !plan || busy) return;
    const operationID = plan.operation_id;
    busy = '正在执行…'; notice = '';
    try {
      if (previewScope === 'source' && !await confirmDestination({ archive: false, trash: sourcePreview!.plan!.assets.some(item => item.action === 'isolate') })) return;
      if (disposed || plan?.operation_id !== operationID) return;
      const response = await request(`/api/v1/${previewScope === 'source' ? 'maintenance' : 'cleanup'}/execute`, {
        method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ operation_id: operationID })
      });
      const body = await response.json() as MaintenancePreview & CleanupPreview;
      if (disposed) return;
      if (previewScope === 'source') sourcePreview = body; else cleanupPreview = body;
      if (!response.ok || body.blocked || body.result?.needs_review) throw new Error(body.error || '处理已停止，请在恢复与复核中检查。');
      if (!body.result || body.result.phase !== 'committed') throw new Error('服务未返回完成状态，请刷新并核对处理结果。');
      selected = new Set(); selectedDirectories = new Set(); acknowledged = false; drawerOpen = false;
      busy = '正在更新检查结果…';
      if (previewScope === 'source') await onChanged();
      await Promise.allSettled([loadSource(), loadCleanup()]);
      notice = previewScope === 'source' ? sourceError ? '维护已完成，请重试更新候选列表。' : '维护完成，已更新列表。' : cleanupError ? '中间产物已隔离，请重试更新隔离区列表。' : '已移入本项目隔离区，文件仍占用磁盘空间。';
      noticeError = false;
    } catch (cause) {
      notice = cause instanceof Error ? cause.message : '处理失败，请重新检查'; noticeError = true;
      acknowledged = false; previewError = notice;
      await loadCleanup();
    } finally { busy = ''; }
  }
  function inspectRecovery(item: CleanupRecovery) { recovery = item; drawerMode = 'recover'; previewError = ''; acknowledged = false; drawerOpen = true; }
  async function recover() {
    if (!recovery || !acknowledged || busy || !cleanup?.write_enabled || cleanupError) return;
    busy = '正在恢复中断操作…';
    try {
      const response = await request('/api/v1/cleanup/recover', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ operation_id: recovery.operation_id }) });
      const body = await response.json() as CleanupPreview;
      if (disposed) return;
      if (!response.ok || body.result?.needs_review || body.result?.phase !== 'rolled_back') throw new Error(body.error || '恢复未完成，请检查原路径和隔离目录是否冲突。');
      drawerOpen = false; notice = '中断操作已撤销，隔离文件已恢复到原路径。'; noticeError = false;
      await loadCleanup();
    } catch (cause) { previewError = cause instanceof Error ? cause.message : '无法恢复中断操作'; acknowledged = false; }
    finally { busy = ''; }
  }
  async function savePolicy() {
    if (!cleanup?.management?.policy_write_enabled || !policyDays || busy || cleanupError) return;
    const policy = retentionPolicy(cleanup.cleanup.policy, policyDays);
    if (!policy) { policyMessage = '请输入大于 0、不超过 3650 的有效天数。'; policyError = true; return; }
    busy = '正在保存保留规则…'; policyMessage = '';
    try {
      const response = await request('/api/v1/cleanup/retention', { method: 'PUT', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ policy }) });
      const body = await response.json();
      if (!response.ok || !body.policy) throw new Error(body.error || `保存失败（HTTP ${response.status}）`);
      await loadCleanup();
      policyMessage = '保留规则已保存。已有文件的到期时间保持不变。'; policyError = false;
    } catch (cause) { policyMessage = cause instanceof Error ? cause.message : '无法保存保留规则'; policyError = true; }
    finally { busy = ''; }
  }
  async function setManagement(enabled: boolean) {
    if (!cleanup?.management?.settings_writable || busy || cleanupError) return;
    busy = enabled ? '正在启用中间产物管理…' : '正在关闭中间产物管理…'; policyMessage = '';
    try {
      const response = await request('/api/v1/cleanup/settings', { method: 'PUT', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ enabled }) });
      const body = await response.json();
      if (!response.ok || body.enabled !== enabled) throw new Error(body.error || '中间产物管理设置未保存');
      await loadCleanup();
      policyMessage = enabled ? '已启用中间产物登记和保留规则；到期后由你确认清理。' : '已停止新任务的中间产物登记，现有记录和文件保留。'; policyError = false;
    } catch (cause) { policyMessage = cause instanceof Error ? cause.message : '无法保存中间产物管理设置'; policyError = true; }
    finally { busy = ''; }
  }
</script>

<main class="workspace-view maintenance-view" aria-busy={loading}>
  <div class="storage-workspace">
    <header class="storage-heading">
      <div><h2>存储维护</h2><p><Folder aria-hidden="true" /><span title={sourceRoot}>{sourceName || basename(sourceRoot) || '当前工作区'}</span><span>清理临时文件、缓存和日志</span></p></div>
      <button type="button" class="secondary-button" on:click={refresh} disabled={loading}><RefreshCw aria-hidden="true" class={sourceLoading || cleanupLoading ? 'storage-spinning' : ''} />{sourceLoading || cleanupLoading ? '检查中…' : '刷新检查'}</button>
    </header>
    <section class="storage-summary" aria-label="存储检查概览">
      <div class="storage-source-size"><HardDrive aria-hidden="true" /><div><span>源目录大小</span><strong>{source ? bytes(source.source_total_bytes) : '—'}</strong><small>{source ? `${number(source.source_file_count)} 个扫描文件` : '等待扫描'}</small></div></div>
      <button type="button" on:click={() => { changeTab('source'); selectCategory(''); }} disabled={!!busy}><span>待检查大小 <Info aria-hidden="true"><title>包括缓存、临时文件和受保护对象，不代表可释放空间。</title></Info></span><strong>{source ? bytes(source.candidate_bytes) : '—'}</strong><small>{number(source?.candidate_count ?? 0)} 个候选</small></button>
      <button type="button" on:click={() => { changeTab('source'); view = { ...view, eligibility: 'eligible' }; applyFilters(); }} disabled={!!busy}><span>可进入预览</span><strong>{source ? number(source.eligible_count) : '—'}<small>项</small></strong><small>执行前重新检查活动状态</small></button>
      <button type="button" class:storage-has-review={reviewCount > 0} on:click={() => changeTab('recovery')} disabled={!!busy}><span>需要复核</span><strong>{number(reviewCount)}<small>项</small></strong><small>受保护、未登记或中断对象</small></button>
    </section>
    <nav class="storage-tabs" aria-label="存储维护分区">{#each tabs as tab}<button type="button" class:active={view.tab === tab.id} aria-current={view.tab === tab.id ? 'page' : undefined} disabled={!!busy} on:click={() => changeTab(tab.id)}>{tab.label}{#if tab.id === 'recovery' && (cleanup?.cleanup.recoveries.length ?? 0) > 0}<span>{cleanup!.cleanup.recoveries.length}</span>{/if}</button>{/each}</nav>
    {#if busy && !drawerOpen}<div class="storage-feedback" role="status"><RefreshCw class="storage-spinning" aria-hidden="true" /><span>{busy}</span></div>{/if}
    {#if notice}<div class="storage-feedback" class:error={noticeError} role={noticeError ? 'alert' : 'status'}>{#if noticeError}<TriangleAlert aria-hidden="true" />{:else}<Check aria-hidden="true" />{/if}<span>{notice}</span><button type="button" aria-label="关闭处理提示" on:click={() => notice = ''}><X aria-hidden="true" /></button></div>{/if}
    {#if sourceError && (view.tab === 'source' || view.tab === 'recovery')}<div class="storage-feedback error" role="alert"><TriangleAlert aria-hidden="true" /><div><strong>源目录检查失败{source ? '，保留上次结果' : ''}</strong><details><summary>查看原因</summary><p>{sourceError}</p></details></div><button type="button" on:click={() => loadSource()} disabled={loading}>重试</button></div>{/if}
    {#if source?.stale && view.tab === 'source'}<div class="storage-feedback warning" role="status"><Info aria-hidden="true" /><span>当前显示上次扫描结果，查看影响时会重新检查所选文件。</span><button type="button" on:click={onRescan} disabled={loading}>更新扫描</button></div>{/if}
    {#if cleanupError && view.tab !== 'source'}<div class="storage-feedback error" role="alert"><TriangleAlert aria-hidden="true" /><div><strong>本项目中间产物检查失败{cleanup ? '，保留上次结果' : ''}</strong><details><summary>查看原因</summary><p>{cleanupError}</p></details></div><button type="button" on:click={loadCleanup} disabled={loading}>重试</button></div>{/if}
    {#if policyMessage && view.tab === 'generated'}<p class="storage-feedback" class:error={policyError} role={policyError ? 'alert' : 'status'}>{policyMessage}</p>{/if}

    {#if view.tab === 'source' || view.tab === 'generated'}
      <section class="storage-browser" aria-label={view.tab === 'source' ? '源目录维护候选' : '本项目登记的中间产物'}>
        <aside class="storage-categories" aria-label={view.tab === 'source' ? '维护分类' : '中间产物状态'}>
          <h3>{view.tab === 'source' ? '维护分类' : '中间产物状态'}</h3>
          {#if view.tab === 'source'}
            <button type="button" class:active={!view.category} aria-pressed={!view.category} on:click={() => selectCategory('')} disabled={loading}><span>全部候选</span><small>{number(source?.candidate_count ?? 0)}</small></button>
            {#each Object.entries(categoryLabels) as [category, label]}{@const summary = source?.categories.find(item => item.category === category)}<button type="button" class:active={view.category === category} aria-pressed={view.category === category} on:click={() => selectCategory(category)} disabled={loading}><span>{label}</span><small>{number(summary?.count ?? 0)}</small></button>{/each}
          {:else}
            {#each groups as group}<button type="button" class:active={view.collection === group.id} aria-pressed={view.collection === group.id} on:click={() => selectGroup(group.id)} disabled={loading}><span>{group.label}</span><small>{number(cleanup?.cleanup[group.id]?.length ?? 0)}</small></button>{/each}
          {/if}
          <div class="storage-scope-note"><ShieldCheck aria-hidden="true" /><span>{view.tab === 'source' ? source?.write_enabled ? '隔离样本 · 可执行' : '当前源目录仅检查与预览' : cleanup?.write_enabled ? '隔离样本 · 可执行' : '中间产物仅检查与预览'}</span></div>
        </aside>
        <div class="storage-list-panel">
          <header class="storage-list-heading"><div><h3>{directoryMode ? '按目录清理' : view.tab === 'source' ? view.scope === 'ungrouped' ? '目录以外的文件' : view.directory ? `${view.directory}/` : view.category ? categoryLabels[view.category] : '文件明细' : groups.find(group => group.id === view.collection)?.label}</h3><span>{directoryMode ? `${number(directories.length)} 个目录` : view.tab === 'source' ? `${number(source?.filtered_count ?? 0)} 项 · ${bytes(source?.filtered_bytes ?? 0)}` : `${number(generatedRows.length)} 项 · ${bytes(generatedRows.reduce((sum, row) => sum + row.size, 0))}`}</span></div><button type="button" class="storage-text-button" on:click={() => { inspected = null; drawerMode = 'detail'; copyMessage = ''; drawerOpen = true; }}><Info aria-hidden="true" />检查说明</button></header>
          {#if view.tab === 'source'}<div class="storage-view-switch" aria-label="清理浏览方式"><button type="button" class:active={directoryMode} aria-pressed={directoryMode} disabled={loading} on:click={() => changeLayout('directories')}>按目录</button><button type="button" class:active={!directoryMode} aria-pressed={!directoryMode} disabled={loading} on:click={() => changeLayout('files', view.directory)}>文件明细</button>{#if view.directory && !directoryMode}<button type="button" class="storage-text-button" disabled={loading} on:click={() => changeLayout('files')}>清除目录范围</button>{/if}</div>{/if}
          {#if view.tab === 'source' && !directoryMode && view.scope === 'ungrouped'}
            <div class="storage-range-note"><span>这里只显示未归入上方清理目录的文件，共 {number(ungroupedCount)} 个。需保留项不代表损坏；原因显示在文件下方。</span><button type="button" class="storage-text-button" disabled={loading} on:click={() => { view = { ...view, eligibility: view.eligibility === 'blocked' ? '' : 'blocked' }; applyFilters(); }}>{view.eligibility === 'blocked' ? '查看全部独立文件' : '只看需保留项'}</button><button type="button" class="storage-text-button" disabled={loading} on:click={() => { view = { ...parseMaintenanceView(''), layout: 'files' }; applyFilters(); }}>显示全部文件</button></div>
          {/if}
          <form class="storage-toolbar" on:submit|preventDefault={applyFilters}>
            <label class="storage-search"><Search aria-hidden="true" /><input bind:value={view.query} placeholder={directoryMode ? '搜索目录名称或用途' : view.tab === 'source' ? '搜索全部候选路径' : '搜索路径、类型或生成者'} aria-label="搜索维护对象" disabled={!!busy} />{#if view.query}<button type="button" aria-label="清除维护搜索" on:click={() => { view = { ...view, query: '' }; applyFilters(); }} disabled={loading}><X aria-hidden="true" /></button>{/if}</label>
            <button type="submit" class="secondary-button" disabled={loading}>搜索</button>
            {#if view.tab === 'source'}<select bind:value={view.eligibility} aria-label="维护候选状态" on:change={applyFilters} disabled={loading}><option value="">全部状态</option><option value="eligible">可预览</option><option value="blocked">已阻断</option></select>{/if}
            <select bind:value={view.sort} aria-label="维护候选排序" on:change={applyFilters} disabled={loading}><option value="size">大小优先</option>{#if !directoryMode}<option value="newest">{view.tab === 'source' ? '最近修改' : '最近创建'}</option><option value="oldest">{view.tab === 'source' ? '最早修改' : '最早创建'}</option>{/if}<option value="path">路径名称</option></select>
          </form>
          {#if directoryMode}
            <div class="storage-directories" class:storage-list-updating={sourceLoading}>
              {#if directories.length}
                <div class="storage-directory-select"><label><input type="checkbox" aria-label="选择全部可预览目录" checked={allDirectoriesSelected} indeterminate={selectedDirectories.size > 0 && !allDirectoriesSelected} disabled={loading || listUnavailable || !directories.some(item => item.eligible_count > 0)} on:change={toggleAllDirectories} />选择全部目录</label><span>包含目录下全部候选，无需逐页加载</span></div>
                {#each directories as directory (directory.relative_path)}
                  <article class="storage-directory" class:storage-directory-selected={selectedDirectories.has(directory.relative_path)}>
                    <label class="storage-directory-check"><input type="checkbox" aria-label={`选择整个 ${directory.relative_path} 目录`} checked={selectedDirectories.has(directory.relative_path)} disabled={!directory.eligible_count || loading || listUnavailable} on:change={(event) => toggleDirectory(directory, event.currentTarget.checked)} /><span class="storage-sr-only">选择 {directory.relative_path}</span></label>
                    <Folder class="storage-directory-icon" aria-hidden="true" />
                    <div class="storage-directory-name"><strong>{directoryLabels[directory.category] || '维护目录'}</strong><code title={`${sourceRoot}/${directory.relative_path}`}>{directory.relative_path}/</code><span>{number(directory.file_count)} 个文件{#if directory.retained_count} · 保留 {number(directory.retained_count)} 个{/if}</span></div>
                    <div class="storage-directory-size"><strong>{bytes(directory.size_bytes)}</strong><small>候选 {bytes(directory.eligible_bytes)}</small></div>
                    <button type="button" class="storage-text-button storage-directory-files" disabled={loading} on:click={() => { view = { ...view, query: '', eligibility: '' }; changeLayout('files', directory.relative_path); }}>查看文件<ChevronRight aria-hidden="true" /></button>
                  </article>
                {/each}
              {:else}<div class="storage-empty" role="status">{#if sourceLoading}<RefreshCw class="storage-spinning" aria-hidden="true" /><strong>正在汇总目录…</strong>{:else}<Folder aria-hidden="true" /><strong>{listUnavailable ? '目录检查尚未就绪' : '没有匹配的维护目录'}</strong><button type="button" class="secondary-button" disabled={loading} on:click={() => changeLayout('files')}>查看文件明细</button>{/if}</div>{/if}
            </div>
            {#if ungroupedCount}<button type="button" class="storage-text-button storage-ungrouped" disabled={loading} on:click={showUngrouped}>查看目录以外的 {number(ungroupedCount)} 个文件（{number(source?.ungrouped_blocked_count ?? 0)} 个需保留）<ChevronRight aria-hidden="true" /></button>{/if}
            <div class="storage-directory-note"><ShieldCheck aria-hidden="true" /><span>受保护和状态异常的文件保留原位，查看影响时重新检查。</span></div>
          {:else}
          <div class="storage-table-wrap" class:storage-list-updating={sourceLoading && view.tab === 'source' || cleanupLoading && view.tab === 'generated'}>
            <table class="storage-table"><caption class="storage-sr-only">{view.tab === 'source' ? '源目录维护候选，筛选覆盖完整快照' : '已登记的本项目中间产物'}</caption><thead><tr><th class="storage-select-cell"><input type="checkbox" aria-label="选择当前已载入的可预览项目" checked={allSelected} disabled={!eligibleRows.length || loading || listUnavailable} on:change={toggleAll} /></th><th>名称与位置</th><th class="storage-kind-cell">类型</th><th class="storage-size-cell">大小</th><th class="storage-state-cell">状态</th><th class="storage-detail-cell"><span class="storage-sr-only">详情</span></th></tr></thead><tbody>
              {#each rows as row (row.id)}<tr class:storage-row-selected={selected.has(row.id)}><td class="storage-select-cell"><input type="checkbox" aria-label={`选择 ${basename(row.path)}`} checked={selected.has(row.id)} disabled={!row.eligible || loading || listUnavailable} on:change={(event) => toggle(row.id, event.currentTarget.checked)} /></td><td class="storage-name-cell"><button type="button" title={row.path} on:click={() => inspect(row)}><File aria-hidden="true" /><span><strong>{basename(row.path)}</strong><small>{parentPath(row.path)}</small></span></button>{#if view.tab === 'source' && view.scope === 'ungrouped'}<p class="storage-row-reason">{row.eligible ? '可检查 · ' : '保留 · '}{row.reason}</p>{/if}</td><td class="storage-kind-cell"><span>{kindLabels[row.kind] || row.kind || '未分类'}</span></td><td class="storage-size-cell">{bytes(row.size)}</td><td class="storage-state-cell"><button type="button" class="storage-status" class:storage-status-ready={row.eligible} class:storage-status-warning={!row.eligible && row.status !== 'quarantined' && row.status !== 'purged'} title={row.reason} on:click={() => inspect(row)}>{row.eligible ? '可预览' : row.source ? '已阻断' : stateLabels[row.status] || '待复核'}</button></td><td class="storage-detail-cell"><button type="button" class="storage-icon-button" aria-label={`查看 ${basename(row.path)} 的详情`} on:click={() => inspect(row)}><ChevronRight aria-hidden="true" /></button></td></tr>{/each}
            </tbody></table>
            {#if !rows.length}<div class="storage-empty" role="status">{#if loading}<RefreshCw class="storage-spinning" aria-hidden="true" /><strong>正在检查文件…</strong>{:else if listUnavailable}<Database aria-hidden="true" /><strong>检查结果尚未就绪</strong><span>请重试检查，或查看上方的错误原因。</span>{:else if view.query || view.category || view.eligibility}<Search aria-hidden="true" /><strong>没有匹配的项目</strong><span>试试其他路径片段，或清除筛选。</span><button type="button" class="secondary-button" on:click={resetFilters}>清除筛选</button>{:else}<ShieldCheck aria-hidden="true" /><strong>{view.tab === 'source' ? '没有发现维护候选' : !cleanup?.management?.enabled ? '尚未启用中间产物管理' : view.collection === 'candidates' ? '暂无到期的可清理中间产物' : '这个分类暂无项目'}</strong><span>{view.tab === 'generated' ? cleanup?.management?.enabled ? '后续副本、更新与维护流程将自动登记中间文件；历史目录不会自动纳入。' : '启用后自动登记本项目后续产生的中间文件，并可设置保留期。' : '可以稍后刷新，检查新的扫描结果。'}</span>{#if view.tab === 'generated' && !cleanup?.management?.enabled && cleanup?.management?.settings_writable}<button type="button" class="primary-button" on:click={() => setManagement(true)} disabled={loading}>启用中间产物管理</button>{/if}{/if}</div>{/if}
          </div>
          <footer class="storage-list-footer"><span>已载入 {number(rows.length)} / {number(view.tab === 'source' ? source?.filtered_count ?? 0 : generatedRows.length)} 项{view.tab === 'source' ? ' · 筛选覆盖全部候选' : ''}</span>{#if view.tab === 'source' && source?.has_more}<button type="button" class="storage-text-button" on:click={() => loadSource(true)} disabled={loading}>{loadingMore ? '载入中…' : '加载更多'}<ArrowRight aria-hidden="true" /></button>{:else if view.tab === 'generated' && generatedRows.length > displayLimit}<button type="button" class="storage-text-button" on:click={() => displayLimit += 30}>加载更多<ArrowRight aria-hidden="true" /></button>{/if}</footer>
          {/if}
          {#if directoryMode || eligibleRows.length || selectionCount}<div class="storage-selection"><span>{selectedDirectories.size ? `已选 ${selectedDirectories.size} 个目录 · ${number(directoryFileCount)} 个候选 · ${bytes(selectedBytes)}${directoryRetainedCount ? ` · 保留 ${number(directoryRetainedCount)} 个文件` : ''}` : selected.size ? `已选 ${selected.size} 项 · ${bytes(selectedBytes)}` : directoryMode ? '勾选目录，批量检查全部候选' : '选择项目后查看处理影响'}</span>{#if selectionCount}<button type="button" class="storage-text-button" on:click={clearSelection} disabled={loading}>清空选择</button>{/if}<button type="button" class="primary-button" on:click={() => previewSelection()} disabled={!selectionCount || loading || listUnavailable}>查看{directoryMode ? '清理' : ''}影响<ArrowRight aria-hidden="true" /></button></div>{/if}
        </div>
      </section>
    {:else if view.tab === 'recovery'}
      <section class="storage-review-panel"><header class="storage-section-heading"><div><h3>中断操作</h3><p>检查原路径和隔离目录后，可撤销尚未完成的中间产物清理。</p></div><span>{cleanup?.cleanup.recoveries.length ?? 0} 项</span></header>
        {#if cleanup?.cleanup.recoveries.length}<div class="storage-recovery-list">{#each cleanup.cleanup.recoveries as item}<article><Undo2 aria-hidden="true" /><div><strong>{stateLabels[item.phase] || '需要复核'}</strong><span>{time(item.updated_at)}</span><small title={item.operation_id}>{item.operation_id}</small></div><button type="button" class="secondary-button" on:click={() => inspectRecovery(item)} disabled={loading}>检查恢复</button></article>{/each}</div>{:else}<div class="storage-empty storage-empty-compact"><ShieldCheck aria-hidden="true" /><strong>{cleanupLoading ? '正在检查中断操作…' : cleanupError ? '中断操作尚未检查' : '没有待恢复的中断操作'}</strong></div>{/if}
      </section>
      <section class="storage-review-panel"><header class="storage-section-heading"><div><h3>需人工确认的对象</h3><p>先了解用途和关联，受保护对象不会自动进入执行计划。</p></div></header><div class="storage-review-links"><button type="button" disabled={!!busy} on:click={() => { changeTab('source'); view = { ...parseMaintenanceView(''), layout: 'files', eligibility: 'blocked' }; applyFilters(); }}><Database aria-hidden="true" /><span><strong>源目录阻断项</strong><small>活动文件、数据库辅助文件或关联不明的会话</small></span><b>{number(source?.blocked_count ?? 0)}</b><ChevronRight aria-hidden="true" /></button>{#each [['unknown', '未登记对象', '缺少归属信息，只报告，不清理'], ['crash_residues', '中断残留', '确认是否仍有恢复用途']] as [group, label, description]}<button type="button" disabled={!!busy} on:click={() => { changeTab('generated'); selectGroup(group as MaintenanceView['collection']); }}><Folder aria-hidden="true" /><span><strong>{label}</strong><small>{description}</small></span><b>{number(cleanup?.cleanup[group as 'unknown' | 'crash_residues'].length ?? 0)}</b><ChevronRight aria-hidden="true" /></button>{/each}</div>
        {#if cleanup?.cleanup.errors?.length}<details class="storage-technical"><summary>登记文件检查异常（{cleanup.cleanup.errors.length}）</summary>{#each cleanup.cleanup.errors as error}<p>{error}</p>{/each}</details>{/if}
      </section>
    {:else}
      <section class="storage-rules">
        <header class="storage-section-heading"><div><h3>中间产物保留规则</h3><p>按保留期列出待清理文件，清理需手动确认。</p></div><span class="storage-status">{cleanup?.management?.enabled ? '已启用' : '未启用'}</span></header>
        {#if cleanupLoading && !cleanup}<div class="storage-empty storage-empty-compact"><RefreshCw class="storage-spinning" aria-hidden="true" /><strong>正在读取管理设置…</strong></div>
        {:else if cleanup?.management?.enabled && policyDays}
          <div class="storage-management-state"><span><Check aria-hidden="true" />后续任务自动登记中间产物</span><button type="button" class="storage-text-button" disabled={loading} on:click={() => setManagement(false)}>关闭管理</button></div>
          <form on:submit|preventDefault={savePolicy}><div class="storage-policy-fields">{#each retentionFields as field}<label><span><strong>{field.label}</strong><small>{field.hint}</small></span><span class="storage-day-input"><input type="number" min="0.001" max="3650" step="any" bind:value={policyDays[field.key]} aria-label={`${field.label}保留天数`} disabled={!!busy || !!cleanupError} required />天</span></label>{/each}</div><footer><p>新规则适用于后续登记，不改写已有产物的到期时间。</p><button type="submit" class="primary-button" disabled={!!busy || !!cleanupError}>保存规则</button></footer></form>
        {:else}
          <div class="storage-empty storage-management-start"><HardDrive aria-hidden="true" /><strong>尚未启用中间产物管理</strong><span>启用后，自动登记后续副本、更新与维护流程产生的中间文件，并开放保留期设置。</span>{#if cleanup?.management?.settings_writable}<button type="button" class="primary-button" disabled={loading} on:click={() => setManagement(true)}>启用中间产物管理</button>{:else}<span>{cleanup?.management?.block_reason || cleanupError || '服务尚未配置独立的中间产物登记目录。'}</span>{/if}</div>
        {/if}
        {#if policyMessage}<p class="storage-feedback" class:error={policyError} role={policyError ? 'alert' : 'status'}>{policyMessage}</p>{/if}
        <p class="storage-automation-note"><Info aria-hidden="true" /><span>自动清理尚未实现。启用管理只登记文件、应用保留期；不会自动移动或删除文件。</span></p>
        <details class="storage-technical"><summary>管理范围与登记位置</summary><p>仅管理本项目已登记的中间产物，不扫描整个项目，不为 .codex 的临时目录设置清理周期。源代码、正式文档、脱敏样本和验证报告受保护。</p><p>关闭后停止新任务登记，现有记录和保留规则继续保存。文件清理执行仍受独立权限限制。</p><code>{cleanup?.cleanup.registry_root || '登记区未配置'}</code></details>
      </section>
    {/if}
  </div>
</main>

<LibraryDrawer bind:open={drawerOpen} title={drawerMode === 'preview' ? '处理影响' : drawerMode === 'recover' ? '检查中断恢复' : inspected ? '文件详情' : '检查说明'}>
  <div class="storage-drawer">
    {#if drawerMode === 'detail'}
      {#if inspected}<header class="storage-object-heading"><File aria-hidden="true" /><h3>{basename(inspected.path)}</h3></header><div class="storage-detail-meta"><span>{kindLabels[inspected.kind] || inspected.kind}</span><strong>{bytes(inspected.size)}</strong><span class="storage-status" class:storage-status-warning={!inspected.eligible}>{inspected.eligible ? '可预览' : inspected.source ? '已阻断' : stateLabels[inspected.status] || '待复核'}</span></div><div class="storage-path"><span>完整路径</span><button type="button" class="storage-icon-button" aria-label="复制完整路径" title="复制完整路径" on:click={() => copy(inspected!.source ? `${sourceRoot}/${inspected!.path}` : inspected!.path)}><Copy aria-hidden="true" /></button><code>{#each (inspected.source ? `${sourceRoot}/${inspected.path}` : inspected.path).replaceAll('\\', '/').split('/') as part, i}{#if i}<wbr />/{/if}{part}{/each}</code></div>{#if copyMessage}<p class="storage-muted" role="status">{copyMessage}</p>{/if}<section class="storage-detail-section"><h4>{inspected.eligible ? '检查结论' : '保留原因'}</h4><p>{inspected.reason || '需要人工确认用途。'}</p>{#if inspected.source}<dl><dt>处理方式</dt><dd>{inspected.eligible ? actionLabel(inspected.source.action) : '保留，需先完成检查'}</dd><dt>风险等级</dt><dd>{({ low: '低', medium: '中', high: '高', block: '受保护' } as Record<string, string>)[inspected.risk] || '待确认'}</dd><dt>最后修改</dt><dd>{time(inspected.modified)}</dd><dt>文件状态</dt><dd>{stateLabels[inspected.status] || inspected.status}</dd></dl>{:else if inspected.artifact}<dl><dt>生成者</dt><dd>{inspected.artifact.owner || '未知'}</dd><dt>文件数量</dt><dd>{inspected.artifact.file_count}</dd><dt>创建时间</dt><dd>{time(inspected.artifact.created_at)}</dd><dt>到期时间</dt><dd>{time(inspected.artifact.expires_at)}</dd><dt>可重建</dt><dd>{inspected.artifact.rebuildable ? '是' : '否'}</dd></dl>{/if}</section>{#if inspected.artifact}<details class="storage-technical"><summary>技术记录</summary><dl><dt>登记 ID</dt><dd><code>{inspected.artifact.artifact_id}</code></dd><dt>登记文件</dt><dd><code>{inspected.artifact.manifest_path}</code></dd><dt>活动证据</dt><dd>{inspected.artifact.in_use_evidence || (inspected.artifact.process_probe_available ? '未发现打开文件' : '未取得进程证据')}</dd>{#if inspected.artifact.quarantine_path}<dt>隔离路径</dt><dd><code>{inspected.artifact.quarantine_path}</code></dd><dt>隔离保留至</dt><dd>{time(inspected.artifact.purge_after)}</dd>{/if}</dl></details>{/if}
      {:else}<section class="storage-detail-section"><h3>按目录选择，批量检查</h3><p>勾选一个目录，会自动选择其中全部可预览候选，不受分页限制。你无需逐个理解或勾选文件。</p><h4>清理范围</h4><p>处理影响先显示目录、文件数和大小，明细按需展开。受保护和状态异常的文件保留原位；目录选择不会自动修复会话。</p><h4>本项目中间产物</h4><p>只处理登记信息完整、已到期且可重建的本项目产物。选择后先查看实际路径、数量和动作。</p><h4>空间大小</h4><p>“待检查大小”包含不能处理的文件。移入回收站或本项目隔离区后，文件仍会占用磁盘；这里不推算已释放空间。</p><h4>执行权限</h4><p>本页的维护、修复和中间产物清理目前仅在显式隔离样本中开放执行。会话库的删除权限与本页独立。</p></section>{/if}
    {:else if drawerMode === 'preview'}
      {#if busy}<div class="storage-feedback" role="status"><RefreshCw class="storage-spinning" aria-hidden="true" /><span>{busy}</span></div>{/if}
      {#if previewError}<div class="storage-feedback error" role="alert"><TriangleAlert aria-hidden="true" /><div><strong>未能继续处理</strong><details><summary>查看原因</summary><p>{previewError}</p></details></div></div>{/if}
      {#if plan || sourcePreview?.selection}
        <div class="storage-preview-total"><span>本次清理范围</span><strong>{number(sourcePreview?.selection?.file_count ?? previewItems.length)} 个文件 <small>{bytes(sourcePreview?.selection?.size_bytes ?? previewItems.reduce((sum, item) => sum + item.size, 0))}</small></strong>{#if previewDirectories.length}<span>{previewDirectories.length} 个目录 · 保留 {number(sourcePreview?.selection?.retained_count ?? 0)} 个文件</span>{/if}</div>
        <div class="storage-feedback" class:warning={!previewWritable}><Info aria-hidden="true" /><span>{activePreview?.result?.needs_review ? '操作需要复核，请检查恢复与复核页面。' : activePreview?.result ? `操作状态：${stateLabels[activePreview.result.phase] || activePreview.result.phase}` : previewWritable ? '确认后会再次校验文件变化和活动状态。' : plan?.status === 'invalid' ? activePreview?.error || '检查未通过，请查看原因。' : !plan?.write_enabled ? '当前源目录仅开放检查与预览，尚未开放目录清理执行。' : '检查未通过，请查看原因。'}</span></div>
        {#if previewDirectories.length}
          <div class="storage-impact-list storage-directory-impact">{#each previewDirectories as directory}<article><Folder aria-hidden="true" /><div><strong>{directory.relative_path}/</strong><span>{number(directory.eligible_count)} 个候选 · 保留 {number(directory.retained_count)} 个文件</span></div><b>{bytes(directory.eligible_bytes)}</b></article>{/each}</div>
        {/if}
        {#if sourcePreview?.selection?.retained_count}
          <details class="storage-technical"><summary>保留文件与原因（{number(sourcePreview.selection.retained_count)}）</summary><p>以下文件保持原位，不包含在清理计划中。目录本身保留。</p>{#each sourcePreview.selection.retained as item}<div class="storage-retained-file"><code>{item.relative_path}</code><span>{item.reason}</span></div>{/each}{#if sourcePreview.selection.retained_count > sourcePreview.selection.retained.length}<p>显示前 {sourcePreview.selection.retained.length} 个，可返回文件明细筛选“已阻断”继续查看。</p>{/if}</details>
        {/if}
        <details class="storage-preview-files" open={!previewDirectories.length} on:toggle={(event) => previewFilesOpen = event.currentTarget.open}>
          <summary>文件处理明细（{number(previewItems.length)}）</summary>
          {#if previewFilesOpen || !previewDirectories.length}
            <div class="storage-impact-list">{#each previewItems.slice(0, previewFileLimit) as item}<article><div><strong title={item.path}>{basename(item.path)}</strong><span>{item.action}</span><small title={item.path}>{parentPath(item.path)}</small></div><b>{bytes(item.size)}</b></article>{/each}</div>
            {#if previewItems.length > previewFileLimit}<button type="button" class="storage-text-button" on:click={() => previewFileLimit += 50}>继续显示 50 项（已显示 {previewFileLimit} / {number(previewItems.length)}）</button>{/if}
          {/if}
        </details>
        <p class="storage-muted">{previewScope === 'source' ? '只处理预览中的文件，目录内其他内容保持原位。临时文件移入回收站；末行修复会保留恢复材料。' : '文件将移入本项目隔离区，不会永久删除。'}移入后仍占用磁盘，清空回收站后才能释放空间。</p>
        <details class="storage-technical"><summary>操作记录</summary><dl><dt>数据根目录</dt><dd><code>{previewScope === 'source' ? sourcePreview?.plan?.root : cleanupPreview?.plan?.registry_root}</code></dd><dt>操作 ID</dt><dd><code>{plan?.operation_id || '未生成可执行计划'}</code></dd>{#if activePreview?.error || plan?.block_reason}<dt>检查原因</dt><dd>{activePreview?.error || plan?.block_reason}</dd>{/if}</dl>{#if activePreview?.result?.journal_path}<code>{activePreview.result.journal_path}</code>{/if}</details>
        {#if previewWritable}<label class="storage-acknowledge"><input type="checkbox" bind:checked={acknowledged} disabled={!!busy} /><span>我已核对清理范围和保留项</span></label>{/if}
      {:else if !busy && !previewError}<div class="storage-empty"><TriangleAlert aria-hidden="true" /><strong>没有取得有效的影响清单</strong><span>请返回列表重新选择并检查。</span></div>{/if}
    {:else if recovery}
      <div class="storage-feedback warning"><Undo2 aria-hidden="true" /><span>将撤销此中断操作，尝试把隔离文件移回原路径。发现路径冲突会停止。</span></div><dl><dt>状态</dt><dd>{stateLabels[recovery.phase] || recovery.phase}</dd><dt>更新时间</dt><dd>{time(recovery.updated_at)}</dd></dl>{#if recovery.reason}<p>{recovery.reason}</p>{/if}<details class="storage-technical"><summary>恢复记录</summary><code>{recovery.operation_id}</code><code class="storage-full-path">{recovery.journal_path}</code></details>{#if previewError}<p class="storage-feedback error" role="alert">{previewError}</p>{/if}{#if cleanup?.write_enabled && !cleanupError}<label class="storage-acknowledge"><input type="checkbox" bind:checked={acknowledged} disabled={!!busy} /><span>我确认撤销这次中断的隔离操作</span></label>{:else}<p class="storage-muted">当前服务未开放中间产物恢复写入。</p>{/if}
    {/if}
  </div>
  <svelte:fragment slot="footer"><div class="storage-drawer-actions"><button type="button" class="secondary-button" on:click={() => drawerOpen = false}>{busy ? '关闭面板' : '返回列表'}</button>{#if drawerMode === 'detail' && inspected?.source?.action === 'repair_jsonl_tail' && !inspected.eligible}<button type="button" class="primary-button" disabled={loading || !!sourceError} on:click={() => previewSelection([inspected!.id])}>检查修复条件</button>{/if}{#if drawerMode === 'preview' && previewWritable}<button type="button" class="primary-button" disabled={!!busy || !acknowledged} on:click={execute}>{busy ? '执行中…' : previewScope === 'source' ? '确认执行维护' : '确认移入隔离区'}</button>{:else if drawerMode === 'recover' && cleanup?.write_enabled}<button type="button" class="primary-button" disabled={!!busy || !acknowledged || !!cleanupError} on:click={recover}>{busy ? '恢复中…' : '确认撤销并恢复'}</button>{/if}</div></svelte:fragment>
</LibraryDrawer>
