<!-- 中文说明：独立的只读统计工作区，按筛选范围呈现概览、时间分布和排行，并提供带相同口径的会话下钻。 -->
<script lang="ts">
  import { onMount } from 'svelte';
  import { ArrowRight, ChevronRight, Copy, Database, Info, RefreshCw, Search, TriangleAlert, X } from '@lucide/svelte';
  import LibraryDrawer from '../sessions/LibraryDrawer.svelte';
  import { analyticsAPIQuery, analyticsSelection, analyticsViewSearch, buildTimeline, parseAnalyticsView, validDate } from './view-state';
  import type { AnalyticsReport, AnalyticsSelection, AnalyticsView, NamedCount, TrendBucket } from './types';

  export let overview: AnalyticsReport | null = null;
  export let sourceRoot = '';
  export let sourceName = '';
  export let snapshot = '';
  export let request: (url: string) => Promise<Response>;
  export let onInspect: (selection: AnalyticsSelection) => void;
  export let onRescan: () => void;
  export let statusLabels: Record<string, string>;

  let view = parseAnalyticsView('');
  let appliedView = { ...view };
  let data: AnalyticsReport | null = null;
  let dataScope = analyticsSelection(view);
  let dataRoot = '';
  let loading = false;
  let error = '';
  let mounted = false;
  let sourceKey = '';
  let requestedSourceKey = '';
  let requestToken = 0;
  let focusedIndex = 0;
  let hoverIndex = -1;
  let chartElement: HTMLDivElement;
  let modelSort: 'total_tokens' | 'count' = 'total_tokens';
  let projectSort: 'total_tokens' | 'count' | 'total_bytes' = 'total_tokens';
  let drawerOpen = false;
  let drawerKind: 'models' | 'projects' = 'models';
  let rankQuery = '';
  let rankLimit = 50;
  let copyMessage = '';
  const metricLabels = { total_tokens: 'tokens', conversations: '会话', messages: '消息' };
  const numberFormat = new Intl.NumberFormat('zh-CN');
  const compactFormat = new Intl.NumberFormat('zh-CN', { notation: 'compact', maximumFractionDigits: 1 });
  const number = (value: number) => numberFormat.format(value);
  const compact = (value: number) => compactFormat.format(value);
  const bytes = (value: number) => {
    if (!value) return '0 B';
    const units = ['B', 'KiB', 'MiB', 'GiB', 'TiB'];
    const index = Math.min(units.length - 1, Math.floor(Math.log(value) / Math.log(1024)));
    return `${(value / 1024 ** index).toFixed(index ? 1 : 0)} ${units[index]}`;
  };
  const projectName = (value: string) => value.replaceAll('\\', '/').split('/').filter(Boolean).slice(-2).join('/') || value;
  const formatTime = (value?: string) => value ? new Date(value).toLocaleString('zh-CN', { hour12: false }) : '尚无扫描记录';

  $: sourceKey = `${sourceRoot}|${snapshot}`;
  $: if (mounted && sourceRoot && sourceKey !== requestedSourceKey) {
    requestedSourceKey = sourceKey;
    void load();
  }
  $: current = dataRoot === sourceRoot ? data : null;
  $: buckets = buildTimeline(current?.daily ?? [], dataScope);
  $: peak = Math.max(1, ...buckets.map(point => point[view.metric]));
  $: inspectedBucket = buckets[hoverIndex >= 0 ? hoverIndex : focusedIndex] ?? buckets[buckets.length - 1];
  $: models = rank(current?.models ?? [], modelSort);
  $: projects = rank(current?.projects ?? [], projectSort);
  $: modelOptions = overview?.models ?? current?.models ?? [];
  $: projectOptions = overview?.projects ?? current?.projects ?? [];
  $: drawerSort = drawerKind === 'models' ? modelSort : projectSort;
  $: matchingRanks = (drawerKind === 'models' ? models : projects).filter(row => row.name.toLocaleLowerCase().includes(rankQuery.trim().toLocaleLowerCase()));
  $: cacheRatio = current && current.token_usage.input_tokens > 0 && current.token_usage.cached_input_tokens <= current.token_usage.input_tokens ? `${(current.token_usage.cached_input_tokens / current.token_usage.input_tokens * 100).toFixed(1)}%` : '—';

  onMount(() => {
    view = parseAnalyticsView(window.location.search);
    mounted = true;
    const restore = () => {
      if (window.location.pathname !== '/analytics') return;
      view = parseAnalyticsView(window.location.search);
      void load();
    };
    window.addEventListener('popstate', restore);
    return () => { requestToken += 1; window.removeEventListener('popstate', restore); };
  });

  async function load() {
    const token = ++requestToken;
    if (view.range === 'custom' && (!validDate(view.from) || !validDate(view.to) || view.from > view.to)) {
      error = '请选择有效的起止日期，开始日期不能晚于结束日期。';
      loading = false;
      return;
    }
    const root = sourceRoot;
    const selectedView = { ...view };
    const selection = analyticsSelection(selectedView);
    loading = true;
    error = '';
    hoverIndex = -1;
    try {
      const response = await request(`/api/v1/analytics${analyticsAPIQuery(selection)}`);
      const body = await response.json() as AnalyticsReport & { error?: string };
      if (!response.ok) throw new Error(body.error || `统计读取失败（HTTP ${response.status}）`);
      if (!Array.isArray(body.daily) || typeof body.conversation_count !== 'number') throw new Error('统计数据格式不兼容，请检查本机服务版本。');
      if (token !== requestToken || root !== sourceRoot) return;
      data = body;
      dataRoot = root;
      dataScope = selection;
      appliedView = selectedView;
      const points = buildTimeline(body.daily, selection);
      focusedIndex = Math.max(0, points.map(point => point.conversations).findLastIndex(value => value > 0));
    } catch (cause) {
      if (token === requestToken && root === sourceRoot) error = cause instanceof Error ? cause.message : '无法读取统计数据';
    } finally {
      if (token === requestToken) loading = false;
    }
  }

  function syncLocation() {
    window.history.replaceState({}, '', `/analytics${analyticsViewSearch(view)}`);
  }
  function apply() { syncLocation(); void load(); }
  function changeMetric(metric: AnalyticsView['metric']) { view = { ...view, metric }; syncLocation(); }
  function reset() { view = parseAnalyticsView(''); apply(); }
  function inspect(extra: Partial<AnalyticsSelection> = {}) {
    if (current && !loading) onInspect({ ...dataScope, ...extra });
  }
  function inspectBucket(point: TrendBucket) {
    if (point.conversations) inspect({ from: point.from, to: point.to });
  }
  function moveChart(event: KeyboardEvent, index: number) {
    let target = index;
    if (event.key === 'ArrowRight') target = Math.min(buckets.length - 1, index + 1);
    else if (event.key === 'ArrowLeft') target = Math.max(0, index - 1);
    else if (event.key === 'Home') target = 0;
    else if (event.key === 'End') target = buckets.length - 1;
    else return;
    event.preventDefault();
    focusedIndex = target;
    const button = chartElement.querySelectorAll<HTMLButtonElement>('button')[target];
    button?.focus({ preventScroll: true });
    button?.scrollIntoView({ block: 'nearest', inline: 'nearest' });
  }
  function rank(rows: NamedCount[], sort: typeof projectSort) {
    return rows.slice().sort((a, b) => (b[sort] ?? 0) - (a[sort] ?? 0) || a.name.localeCompare(b.name));
  }
  function rankValue(row: NamedCount, sort: typeof projectSort) {
    return sort === 'total_bytes' ? bytes(row.total_bytes ?? 0) : compact(row[sort] ?? 0);
  }
  function openRanking(kind: typeof drawerKind) {
    drawerKind = kind;
    rankQuery = '';
    rankLimit = 50;
    copyMessage = '';
    drawerOpen = true;
  }
  async function copyName(value: string) {
    try { await navigator.clipboard.writeText(value); copyMessage = '已复制完整值'; }
    catch { copyMessage = '复制失败，可以选中下方完整值手动复制。'; }
  }
</script>

<main class="workspace-view analytics-view" aria-busy={loading}>
  <div class="analytics-workspace">
    <header class="analytics-heading">
      <div><h2>统计分析</h2><p><span title={sourceRoot}>{sourceName || '当前工作区'}</span><time title={current?.generated_at || snapshot}>更新于 {formatTime(current?.generated_at || snapshot)}</time></p></div>
      <button type="button" class="secondary-button analytics-refresh" on:click={load} disabled={loading}><RefreshCw aria-hidden="true" class={loading ? 'analytics-spinning' : ''} />{loading ? '读取中…' : '刷新统计'}</button>
    </header>
    <form class="analytics-filters" on:submit|preventDefault={apply}>
      <label><span>时间范围 · UTC</span><select bind:value={view.range} aria-label="统计时间范围" on:change={() => { if (view.range !== 'custom') apply(); }}><option value="7d">最近 7 天</option><option value="30d">最近 30 天</option><option value="90d">最近 90 天</option><option value="all">全部日期</option><option value="custom">自定义日期</option></select></label>
      <label><span>模型</span><select bind:value={view.model} aria-label="统计模型" on:change={apply}><option value="">全部模型</option>{#each modelOptions as row}<option value={row.name}>{row.name}</option>{/each}{#if view.model && !modelOptions.some(row => row.name === view.model)}<option value={view.model}>{view.model}</option>{/if}</select></label>
      <label class="analytics-project-filter"><span>项目</span><select bind:value={view.project} aria-label="统计项目" title={view.project || '全部项目'} on:change={apply}><option value="">全部项目</option>{#each projectOptions as row}<option value={row.name}>{projectName(row.name)}</option>{/each}{#if view.project && !projectOptions.some(row => row.name === view.project)}<option value={view.project}>{projectName(view.project)}</option>{/if}</select></label>
      {#if view.range === 'custom'}<label><span>开始日期</span><input type="date" bind:value={view.from} aria-label="统计开始日期" /></label><label><span>结束日期</span><input type="date" bind:value={view.to} aria-label="统计结束日期" /></label><button type="submit" class="secondary-button">应用日期</button>{/if}
      {#if view.range !== '30d' || view.model || view.project}<button type="button" class="analytics-text-button" on:click={reset}>重置筛选</button>{/if}
    </form>
    {#if error}<div class="analytics-feedback error" role="alert"><TriangleAlert aria-hidden="true" /><span>{error}{current ? ' 当前保留上次成功读取的结果。' : ''}</span><button type="button" on:click={load}>重试</button></div>{/if}
    {#if current?.stale}<div class="analytics-feedback warning" role="status"><Info aria-hidden="true" /><span>源数据有变化，当前显示上次扫描结果。</span><button type="button" on:click={onRescan}>更新扫描</button></div>{/if}
    {#if current}
      <div class="analytics-scope"><span>{appliedView.range === 'all' ? '全部日期' : `${dataScope.from} 至 ${dataScope.to}`} · UTC</span>{#if dataScope.model}<span title={dataScope.model}>模型：{dataScope.model}</span>{/if}{#if dataScope.project}<span title={dataScope.project}>项目：{projectName(dataScope.project)}</span>{/if}<button type="button" class="analytics-text-button" disabled={loading || !current.conversation_count} on:click={() => inspect()}>查看这 {number(current.conversation_count)} 个会话 <ArrowRight aria-hidden="true" /></button></div>
      <section class="analytics-metrics" aria-label="当前范围概览">
        <div><span>会话</span><strong>{number(current.conversation_count)}</strong><small>{number(current.message_count)} 条消息 · {number(current.turn_count)} 回合</small></div>
        <div><span>记录 tokens</span><strong title={number(current.token_usage.total_tokens)}>{compact(current.token_usage.total_tokens)}</strong><small>所选会话的累计记录</small></div>
        <div><span>{current.filtered ? '关联文件大小' : '源目录大小'}</span><strong title={number(current.total_bytes) + ' 字节'}>{bytes(current.total_bytes)}</strong><small>{number(current.file_count)} 个{current.filtered ? '关联文件' : '扫描文件'}</small></div>
        <div><span>有会话更新的日期</span><strong>{number(current.daily.length)}<small>天</small></strong><small>{current.undated_conversation_count ? `${current.undated_conversation_count} 个会话日期未知` : '按最后更新时间归组'}</small></div>
      </section>
      <section class="analytics-panel analytics-trend-panel" aria-labelledby="analytics-trend-title">
        <header class="analytics-section-heading"><div><h3 id="analytics-trend-title">{view.metric === 'total_tokens' ? '会话 token 分布' : view.metric === 'conversations' ? '会话更新分布' : '消息数量分布'}</h3><span>按会话最后更新时间 · {buckets[0]?.days > 1 ? `每 ${buckets[0].days} 天合并` : '每日'} · UTC</span></div><div class="analytics-metric-tabs" aria-label="时间图指标">{#each Object.entries(metricLabels) as [metric, label]}<button type="button" class:active={view.metric === metric} aria-pressed={view.metric === metric} on:click={() => changeMetric(metric as AnalyticsView['metric'])}>{label}</button>{/each}</div></header>
        {#if current.conversation_count && buckets.some(point => point.conversations > 0)}
          <div class="analytics-chart-readout"><span>{inspectedBucket?.from}{#if inspectedBucket && inspectedBucket.from !== inspectedBucket.to} 至 {inspectedBucket.to}{/if}</span><strong>{number(inspectedBucket?.[view.metric] ?? 0)} <small>{metricLabels[view.metric]}</small></strong><span>{number(inspectedBucket?.conversations ?? 0)} 个会话</span></div>
          <div class="analytics-chart-scroll"><div class="analytics-chart" bind:this={chartElement} role="group" aria-label="时间分布；使用左右方向键切换日期，回车查看对应会话">
            {#each buckets as point, index (point.from)}<button type="button" class:empty={!point[view.metric]} class:inspected={point === inspectedBucket} tabindex={index === focusedIndex ? 0 : -1} aria-label={`${point.from}${point.to !== point.from ? ` 至 ${point.to}` : ''}，${number(point[view.metric])} ${metricLabels[view.metric]}，${point.conversations} 个会话`} title={`${point.from} 至 ${point.to}：${number(point[view.metric])} ${metricLabels[view.metric]}`} disabled={loading} on:mouseenter={() => hoverIndex = index} on:mouseleave={() => hoverIndex = -1} on:focus={() => focusedIndex = index} on:keydown={(event) => moveChart(event, index)} on:click={() => inspectBucket(point)}><i style={`height: ${point[view.metric] ? Math.max(1.5, point[view.metric] / peak * 100) : 0}%`}></i></button>{/each}
          </div></div>
          <div class="analytics-chart-axis"><span>{buckets[0].from}</span><span>{buckets[Math.floor(buckets.length / 2)].from}</span><span>{buckets[buckets.length - 1].to}</span></div>
          <details class="analytics-data-details"><summary>查看时间明细（{buckets.length} 组）</summary><div class="analytics-table-scroll"><table><caption>按 UTC 会话更新时间归组的累计记录，包含零值日期</caption><thead><tr><th>日期</th><th>会话</th><th>消息</th><th>tokens</th><th><span class="analytics-sr-only">查看会话</span></th></tr></thead><tbody>{#each buckets as point}<tr><td>{point.from}{#if point.to !== point.from}<small>至 {point.to}</small>{/if}</td><td>{number(point.conversations)}</td><td>{number(point.messages)}</td><td>{number(point.total_tokens)}</td><td><button type="button" class="analytics-text-button" disabled={!point.conversations || loading} on:click={() => inspectBucket(point)}>查看</button></td></tr>{/each}</tbody></table></div></details>
        {:else}<div class="analytics-empty"><Database aria-hidden="true" /><strong>{current.conversation_count ? '这些会话没有可用日期' : '当前范围没有会话'}</strong><span>{current.conversation_count ? '累计记录仍计入概览，时间图不推测缺失日期。' : '可以扩大时间范围，或清除模型与项目筛选。'}</span><button type="button" class="secondary-button" on:click={reset}>重置筛选</button></div>{/if}
      </section>
      <div class="analytics-rank-grid">
        {#each ['models', 'projects'] as kind}
          {@const isProject = kind === 'projects'}
          {@const rows = isProject ? projects : models}
          {@const sort = isProject ? projectSort : modelSort}
          <section class="analytics-panel analytics-ranking">
            <header class="analytics-section-heading"><div><h3>{isProject ? '项目排行' : '模型排行'}</h3><span>{rows.length} 个{isProject ? '项目' : '模型'}</span></div><select aria-label={isProject ? '项目排行指标' : '模型排行指标'} value={sort} on:change={(event) => { if (isProject) projectSort = event.currentTarget.value as typeof projectSort; else modelSort = event.currentTarget.value as typeof modelSort; }}><option value="total_tokens">按 tokens</option><option value="count">按会话数</option>{#if isProject}<option value="total_bytes">按关联大小</option>{/if}</select></header>
            {#if rows.length}<div class="analytics-ranking-list">{#each rows.slice(0, 6) as row (row.name)}<button type="button" disabled={loading} on:click={() => inspect(isProject ? { project: row.name } : { model: row.name })} title={`查看${row.name}的 ${row.count} 个会话`}><span class="analytics-rank-name">{isProject ? projectName(row.name) : row.name}</span><strong>{rankValue(row, sort)}</strong><span class="analytics-rank-bar"><i style={`width: ${Math.max(0, (row[sort] ?? 0) / Math.max(1, rows[0]?.[sort] ?? 0) * 100)}%`}></i></span><small>{sort === 'count' ? `${compact(row.total_tokens ?? 0)} tokens` : `${number(row.count)} 个会话`}</small></button>{/each}</div>{:else}<p class="analytics-rank-empty">当前范围没有可归类的{isProject ? '项目' : '模型'}。</p>{/if}
            <button type="button" class="analytics-text-button analytics-ranking-more" disabled={!rows.length} on:click={() => openRanking(isProject ? 'projects' : 'models')}>全部{isProject ? '项目' : '模型'} <ArrowRight aria-hidden="true" /></button>
          </section>
        {/each}
      </div>
      <section class="analytics-secondary" aria-label="Token 构成与状态">
        <details class="analytics-token-details"><summary>Token 记录与会话状态</summary><div class="analytics-token-grid"><div><span>输入</span><strong>{number(current.token_usage.input_tokens)}</strong></div><div><span>缓存输入</span><strong>{number(current.token_usage.cached_input_tokens)}</strong><small>输入内占比 {cacheRatio}</small></div><div><span>输出</span><strong>{number(current.token_usage.output_tokens)}</strong></div><div><span>推理输出</span><strong>{number(current.token_usage.reasoning_output_tokens)}</strong></div></div><div class="analytics-status-list">{#each current.statuses ?? [] as row}<button type="button" class="secondary-button" disabled={loading} on:click={() => inspect({ status: row.name })}>{statusLabels[row.name] || row.name}<strong>{number(row.count)}</strong></button>{/each}</div><p>缓存输入、推理输出可能是对应类别的子项，各字段不直接相加或换算费用。</p></details>
        <details class="analytics-method"><summary><Info aria-hidden="true" />统计口径与数据来源</summary><p>时间图将每段会话的累计记录归入其最后更新时间；缺少更新时间时使用开始时间。它表示会话的时间分布，不是当天实际 token 消耗。日期统一使用 UTC。</p><p>“全部日期”包含日期未知的会话；选定日期范围后，这些会话不参与范围统计。没有会话的日期显示为零，长时间范围按连续日期合并。</p><p>筛选后的大小只统计已解析出的关联文件，并对同一范围、同一项目内的相同路径去重。共享文件可能同时属于多个项目，整文件大小不代表可回收空间；排行之间不能据此直接相加。</p><dl><dt>源目录</dt><dd><code>{sourceRoot}</code></dd><dt>扫描时间</dt><dd>{formatTime(current.generated_at)}</dd><dt>源文件总量</dt><dd>{number(current.source_file_count ?? overview?.file_count ?? current.file_count)} 个 · {bytes(current.source_total_bytes ?? overview?.total_bytes ?? current.total_bytes)}</dd><dt>有记录的日期</dt><dd>{current.available_from || '未知'} 至 {current.available_to || '未知'}</dd></dl></details>
      </section>
    {:else if loading}<div class="analytics-empty" role="status"><RefreshCw class="analytics-spinning" aria-hidden="true" /><strong>正在读取统计快照…</strong></div>
    {:else if !error}<div class="analytics-empty"><Database aria-hidden="true" /><strong>等待工作区数据</strong><span>扫描完成后会显示统计结果。</span></div>{/if}
  </div>
</main>

<LibraryDrawer bind:open={drawerOpen} title={drawerKind === 'models' ? '全部模型' : '全部项目'} description="按当前统计范围显示。点击查看可打开对应会话，完整值可复制。">
  <div class="analytics-rank-drawer">
    <label class="analytics-rank-search"><Search aria-hidden="true" /><input bind:value={rankQuery} on:input={() => rankLimit = 50} aria-label="搜索排行完整名称" placeholder={drawerKind === 'models' ? '搜索模型名称' : '搜索完整项目路径'} />{#if rankQuery}<button type="button" aria-label="清除排行搜索" on:click={() => rankQuery = ''}><X aria-hidden="true" /></button>{/if}</label>
    {#if copyMessage}<p role="status">{copyMessage}</p>{/if}
    <p>{matchingRanks.length} / {drawerKind === 'models' ? models.length : projects.length} 项 · 按{drawerSort === 'count' ? '会话数' : drawerSort === 'total_bytes' ? '关联大小' : 'tokens'}排序</p>
    {#each matchingRanks.slice(0, rankLimit) as row (row.name)}<article><div><code>{row.name}</code><button type="button" class="analytics-copy-button" aria-label={`复制${row.name}`} title="复制完整值" on:click={() => copyName(row.name)}><Copy aria-hidden="true" /></button></div><footer><span>{number(row.count)} 个会话 · {compact(row.total_tokens ?? 0)} tokens{#if drawerKind === 'projects'} · {bytes(row.total_bytes ?? 0)}{/if}</span><button type="button" class="analytics-text-button" disabled={loading} on:click={() => { drawerOpen = false; inspect(drawerKind === 'projects' ? { project: row.name } : { model: row.name }); }}>查看 <ChevronRight aria-hidden="true" /></button></footer></article>{/each}
    {#if !matchingRanks.length}<div class="analytics-empty"><Search aria-hidden="true" /><strong>没有匹配项</strong><span>试试其他名称或路径片段。</span></div>{/if}
    {#if matchingRanks.length > rankLimit}<button type="button" class="secondary-button" on:click={() => rankLimit += 50}>加载更多（已显示 {rankLimit} / {matchingRanks.length}）</button>{/if}
  </div>
</LibraryDrawer>
