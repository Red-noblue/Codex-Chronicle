<!-- 中文说明：有界长对话阅读器，集成定位、页内搜索、阅读恢复和不抢位置的运行态更新。 -->
<script lang="ts">
  import { onMount, tick, createEventDispatcher } from 'svelte';
  import { List, Search } from '@lucide/svelte';
  import ReaderMessage from './ReaderMessage.svelte';
  import ReaderPageIndicator from './ReaderPageIndicator.svelte';
  import ReaderTool from './ReaderTool.svelte';
  import { isReaderMessage, locationFor, maxVisibleMessages, mergeReaderItems, readerLocationFromURL, readReaderState, writeReaderState, type ReaderEvent, type ReaderLocation, type ReaderPage, type ReaderPageDirection, type ReaderOverview, type ReaderTurn, type ReaderFile } from './reader-state';
  import './reader.css';
  export let conversationID: string;
  export let workspace: string;
  export let apiFetch: (input: RequestInfo | URL, init?: RequestInit) => Promise<Response>;
  export let location: ReaderLocation | null = null;
  const dispatch = createEventDispatcher<{ evidence: ReaderEvent }>();
  let host: HTMLDivElement; let scrollRoot: HTMLElement | null = null; let mounted = false; let destroyed = false;
  let toolbarHeight = 0;
  let beforeEdge: HTMLDivElement; let afterEdge: HTMLDivElement;
  let paging: ReaderPageDirection | '' = ''; let failedPage: ReaderPageDirection | '' = ''; let scrollIntent: ReaderPageDirection | '' = '';
  let lastScrollTop = 0; let scrollbarDragging = false; let touchY = 0;
  let items: ReaderEvent[] = []; let expanded = new Set<string>(); let activeID = ''; let next = ''; let previous = ''; let trimmedBefore = false; let hasMore = false;
  let showRecords = false;
  let moreOpen = false; let moreMenu: HTMLDetailsElement; let moreTrigger: HTMLElement;
  let loading = true; let error = ''; let diagnostics: string[] = []; let complete = true; let searchOpen = false; let navigation = false;
  let query = ''; let role = ''; let searching = false; let searchCursor = ''; let searchDone = false; let searchPending = false; let matches: ReaderEvent[] = []; let matchIndex = -1;
  let following = false; let hasNew = false; let stamp = ''; let fullBusy = ''; let status = ''; let currentQuestion = '';
  let request: AbortController | null = null; let searchRequest: AbortController | null = null; let liveRequest: AbortController | null = null; let fullRequest: AbortController | null = null;
  let searchInput: HTMLInputElement; let restoreTimer: ReturnType<typeof setTimeout>; let liveTimer: ReturnType<typeof setInterval>;
  let lastLocation: ReaderLocation | null = null; let pollBusy = false; let savePending = false;
  let source = ''; let mainline = ''; let versions: { path: string; mainline: boolean }[] = [];
  let indexedBytes = 0; let totalBytes = 0; let indexing = false;
  let builds: { token: object; read: number; total: number }[] = [];
  let directory: '' | 'turns' | 'files' = ''; let directoryBusy = false; let directoryError = ''; let directoryTotal = 0; let directoryMore = false;
  let allTurns: ReaderTurn[] = []; let files: ReaderFile[] = []; let directoryRequest: AbortController | null = null;
  const endpoint = `/api/v1/conversations/${encodeURIComponent(conversationID)}/reader`;
  $: if (mounted && location && location !== lastLocation) { lastLocation = location; void jump(location); }
  $: indexing = builds.length > 0;
  $: indexedBytes = builds[0]?.read || 0;
  $: totalBytes = builds[0]?.total || 0;
  $: messageCount = items.filter(isReaderMessage).length;
  $: recordCount = items.length - messageCount;
  $: visibleItems = showRecords ? items : items.filter(isReaderMessage);
  $: pageFailureHint = `加载失败：${error}。点击重试，或继续滚动重试`;
  $: beforePageHint = failedPage === 'before' ? pageFailureHint : paging === 'before' ? '正在读取前面的内容' : '继续向上滚动，加载更早内容';
  $: afterPageHint = failedPage === 'after' ? pageFailureHint : paging === 'after' ? '正在读取后续内容' : '继续向下滚动，加载后续内容';

  async function json<T = ReaderPage>(url: string, signal?: AbortSignal): Promise<T> {
    const target = new URL(url, window.location.origin); if (source && !target.searchParams.has('source')) target.searchParams.set('source', source);
    const token = {};
    try { for (;;) {
      const response = await apiFetch(target.pathname + target.search, { signal }); const body = await response.json();
      if (!response.ok) throw new Error(body.error || `读取失败（${response.status}）`);
      if (response.status !== 202 || !body.indexing) return body as T;
      builds = [...builds.filter(build => build.token !== token), { token, read: body.indexed_bytes || 0, total: body.total_bytes || 0 }];
      await new Promise<void>((resolve, reject) => {
        if (signal?.aborted) { reject(new DOMException('已取消', 'AbortError')); return; }
        const abort = () => { clearTimeout(timer); reject(new DOMException('已取消', 'AbortError')); };
        const timer = setTimeout(() => { signal?.removeEventListener('abort', abort); resolve(); }, 300);
        signal?.addEventListener('abort', abort, { once: true });
      });
    } } finally { builds = builds.filter(build => build.token !== token); }
  }
  function stopWaiting() {
    request?.abort(); searchRequest?.abort(); directoryRequest?.abort(); fullRequest?.abort();
    loading = false; searching = false; directoryBusy = false; builds = []; scrollIntent = ''; status = '已停止等待';
  }
  async function showDirectory(kind: 'turns' | 'files', append = false) {
    if (directory === kind && !append && !directoryError) { directoryRequest?.abort(); directory = ''; navigation = false; return; }
    directoryRequest?.abort(); const controller = new AbortController(); directoryRequest = controller;
    directory = kind; navigation = kind === 'turns'; directoryBusy = true; directoryError = '';
    if (!append) { allTurns = []; files = []; directoryTotal = 0; }
    const offset = kind === 'turns' ? allTurns.length : files.length;
    try {
      const overview = await json<ReaderOverview>(`${endpoint}?${new URLSearchParams({ overview: kind, offset: String(offset) })}`, controller.signal);
      if (controller !== directoryRequest || destroyed) return;
      allTurns = append ? [...allTurns, ...(overview.turns || [])] : overview.turns || [];
      files = append ? [...files, ...(overview.files || [])] : overview.files || [];
      directoryTotal = overview.total; directoryMore = overview.has_more;
    } catch (cause) { if (!controller.signal.aborted) directoryError = cause instanceof Error ? cause.message : '目录读取失败'; }
    finally { if (controller === directoryRequest) directoryBusy = false; }
  }
  async function switchSource(value: string) {
    searchRequest?.abort(); directoryRequest?.abort(); directory = ''; navigation = false; matches = []; searchCursor = ''; searchDone = false;
    source = value; expanded = new Set(); following = false; hasNew = false;
    if (await load()) {
      await tick(); save(); const url = new URL(window.location.href); url.searchParams.set('read_source', source);
      for (const key of ['read_path', 'read_start', 'read_anchor', 'read_q']) url.searchParams.delete(key);
      if (items[0]) { url.searchParams.set('read_path', items[0].relative_path); url.searchParams.set('read_start', String(items[0].start_offset)); url.searchParams.set('read_anchor', items[0].anchor); }
      window.history.replaceState(window.history.state, '', url);
    }
  }
  function jumpReference(ref: { path: string; start: number }) { return jump({ path: ref.path, start: ref.start, source }); }
  function visibleAnchor() {
    if (!host) return null;
    const top = scrollRoot?.getBoundingClientRect().top || 0;
    const element = Array.from(host.querySelectorAll<HTMLElement>('[data-reader-id]')).find(element => element.getBoundingClientRect().bottom > top + toolbarHeight + 8);
    const event = items.find(item => item.id === element?.dataset.readerId);
    return event && element ? { event, inset: element.getBoundingClientRect().top - top } : null;
  }
  function refreshStickyQuestion() {
    const anchor = visibleAnchor();
    if (!anchor || !scrollRoot) { currentQuestion = ''; return; }
    const anchorIndex = items.findIndex(item => item.id === anchor.event.id);
    const question = items.slice(0, anchorIndex + 1).reverse().find(item => item.role === 'user' && item.kind !== 'agent_task' && item.kind !== 'injected_context');
    if (!question) { currentQuestion = ''; return; }
    const element = Array.from(host.querySelectorAll<HTMLElement>('[data-reader-id]')).find(candidate => candidate.dataset.readerId === question.id);
    const stickyTop = scrollRoot.getBoundingClientRect().top + toolbarHeight;
    currentQuestion = element && element.getBoundingClientRect().bottom <= stickyTop + 2 ? question.text : '';
  }
  function save() {
    if (!mounted || loading) return;
    const anchor = visibleAnchor();
    writeReaderState(localStorage, workspace, conversationID, { location: anchor ? locationFor(anchor.event, '', source) : undefined, inset: anchor?.inset || 0,
      expanded: [...expanded], query, role, navigation, updated: Date.now() });
  }
  function scheduleSave() { if (savePending) return; savePending = true; restoreTimer = setTimeout(() => { savePending = false; save(); }, 300); }
  async function reveal(id: string, inset?: number) {
    scrollIntent = '';
    await tick(); if (destroyed) return;
    const element = Array.from(host.querySelectorAll<HTMLElement>('[data-reader-id]')).find(element => element.dataset.readerId === id);
    if (!element) return;
    if (inset !== undefined && scrollRoot) scrollRoot.scrollTop += element.getBoundingClientRect().top - scrollRoot.getBoundingClientRect().top - inset;
    else element.scrollIntoView({ block: 'center', behavior: 'instant' });
    refreshStickyQuestion();
    lastScrollTop = scrollRoot?.scrollTop || 0;
  }
  async function load(params = new URLSearchParams(), direction: ReaderPageDirection | '' = '', followOnly = false) {
    fullRequest?.abort();
    request?.abort(); const controller = new AbortController(); request = controller;
    loading = true; error = ''; failedPage = ''; paging = direction; scrollIntent = '';
    try {
      let page = await json(`${endpoint}?${params}`, controller.signal);
      // 前页可能因字节上限提前结束，先接续到当前窗口，再衔接，避免漏掉中间记录。
      if (direction === 'before' && items[0]) {
        const firstID = items[0].id; const gathered = [...page.items]; const previousCursor = page.previous_cursor; const visited = new Set<string>();
        while (!gathered.some(item => item.id === firstID)) {
          if (!page.has_more || !page.next_cursor || visited.has(page.next_cursor) || gathered.length > maxVisibleMessages) throw new Error('前后历史无法衔接，请重新定位后重试');
          visited.add(page.next_cursor);
          page = await json(`${endpoint}?${new URLSearchParams({ cursor: page.next_cursor })}`, controller.signal);
          gathered.push(...page.items);
        }
        page = { ...page, items: gathered, previous_cursor: previousCursor };
      }
      if (controller !== request || destroyed || followOnly && !following) return false;
      const anchor = direction ? visibleAnchor() : null;
      if (direction) {
        const merged = mergeReaderItems(items, page.items, direction); items = merged.items;
        if (direction === 'before') {
          previous = page.previous_cursor || ''; trimmedBefore = false;
          if (merged.trimmed) { next = ''; hasMore = true; }
        } else {
          next = page.next_cursor; hasMore = page.has_more;
          if (merged.trimmed) { previous = ''; trimmedBefore = true; }
        }
      } else {
        items = page.items; previous = page.previous_cursor || ''; trimmedBefore = false;
        next = page.next_cursor; hasMore = page.has_more;
      }
      diagnostics = [...new Set(page.diagnostics || [])]; complete = page.complete;
      if (page.source_stamp) stamp = page.source_stamp;
      source = page.source || source; mainline = page.mainline || ''; versions = page.versions || versions;
      if (anchor && items.some(item => item.id === anchor.event.id)) {
        await reveal(anchor.event.id, anchor.inset);
        // 首次滚动可能出现当前提问提示，等提示布局稳定后再次对齐同一条消息。
        onScroll(); await reveal(anchor.event.id, anchor.inset);
      }
      if (page.source_changed) status = '读取期间有更新，已保留当前窗口';
      return true;
    } catch (cause) { if (!controller.signal.aborted) { error = cause instanceof Error ? cause.message : '无法读取正文'; failedPage = direction; } }
    finally { if (controller === request) { loading = false; paging = ''; scrollIntent = ''; await tick(); lastScrollTop = scrollRoot?.scrollTop || 0; } }
    return false;
  }
  async function more() {
    if (loading || !hasMore) return;
    const last = items[items.length - 1];
    if (next) await load(new URLSearchParams({ cursor: next }), 'after');
    else if (last) await load(new URLSearchParams({ path: last.relative_path, start: String(last.start_offset), anchor: last.anchor }), 'after');
  }
  async function earlier() {
    if (loading) return;
    if (previous) await load(new URLSearchParams({ cursor: previous }), 'before');
    else if (trimmedBefore && items[0]) await load(new URLSearchParams({ path: items[0].relative_path, start: String(items[0].start_offset), before: 'true' }), 'before');
  }
  async function jump(target: ReaderLocation, restoreInset?: number) {
    if (target.source) source = target.source;
    if (target.query) { query = target.query; searchOpen = true; }
    const params = new URLSearchParams({ path: target.path, start: String(target.start) }); if (target.anchor) params.set('anchor', target.anchor);
    if (!await load(params)) return;
    const found = items.find(item => item.relative_path === target.path && item.start_offset === target.start);
    if (found) {
      if (!isReaderMessage(found) && restoreInset === undefined) showRecords = true;
      activeID = found.id;
      if (restoreInset === undefined) expanded = new Set([...expanded, found.id]);
      if (target.query && found.truncated && !found.text.toLocaleLowerCase().includes(target.query.toLocaleLowerCase())) await full(found);
      await reveal(found.id, restoreInset);
      if (target.query && restoreInset === undefined) host.querySelector('.reader-active mark')?.scrollIntoView({ block: 'center', behavior: 'instant' });
    }
    else if (!error) status = '定位内容未形成可读消息，可查看原始证据';
  }
  async function search(reset = true) {
    const text = query.trim(); if (!text) { matches = []; searchDone = true; return; }
    searchRequest?.abort(); const controller = new AbortController(); searchRequest = controller; searching = true;
    if (reset) { matches = []; searchCursor = ''; searchDone = false; searchPending = false; matchIndex = -1; }
    try {
      // 按小窗口协作搜索，可立即取消；每次最多 20 页，之后由用户继续。
      for (let pageIndex = 0; pageIndex < 20; pageIndex++) {
        const params = new URLSearchParams({ q: text, role }); if (searchCursor) params.set('cursor', searchCursor);
        const page = await json(`${endpoint}/search?${params}`, controller.signal);
        if (controller !== searchRequest || destroyed) return;
        matches = [...new Map([...matches, ...page.items].map(item => [item.id, item])).values()].slice(0, 500);
        searchCursor = page.next_cursor; searchPending = page.pending; searchDone = !page.has_more && !page.pending;
        if (matches.length >= 500 || page.items.length || searchDone || page.pending || !searchCursor) break;
      }
      if ((reset || matchIndex < 0) && matches.length) { matchIndex = 0; await jump(locationFor(matches[0], text, source)); }
      scheduleSave();
    } catch (cause) { if (!controller.signal.aborted) status = cause instanceof Error ? cause.message : '搜索失败'; }
    finally { if (controller === searchRequest) searching = false; }
  }
  async function nextMatch(delta: number) {
    if (!matches.length) return; matchIndex = (matchIndex + delta + matches.length) % matches.length;
    await jump(locationFor(matches[matchIndex], query, source));
  }
  function toggle(id: string, open: boolean) { const values = new Set(expanded); if (open) values.add(id); else values.delete(id); expanded = values; scheduleSave(); }
  async function full(event: ReaderEvent) {
    fullRequest?.abort(); const controller = new AbortController(); fullRequest = controller;
    fullBusy = event.id; status = '正在读取完整消息…';
    try { const page = await json(`${endpoint}?${new URLSearchParams({ path: event.relative_path, start: String(event.start_offset), anchor: event.anchor, full: 'true' })}`,controller.signal);
      if (controller !== fullRequest || destroyed) return;
      if (page.items[0]) { items = items.map(item => item.id === event.id ? page.items[0] : item); status = '已读取完整消息'; }
    } catch (cause) { if (!controller.signal.aborted) status = cause instanceof Error ? cause.message : '读取失败'; } finally { if (controller === fullRequest) fullBusy = ''; }
  }
  async function share(event: ReaderEvent) {
    const url = new URL(window.location.href); url.pathname = `/sessions/${encodeURIComponent(conversationID)}`;
    if (source) url.searchParams.set('read_source', source);
    url.searchParams.delete('view'); url.searchParams.set('read_path', event.relative_path); url.searchParams.set('read_start', String(event.start_offset)); url.searchParams.set('read_anchor', event.anchor);
    if (query.trim()) url.searchParams.set('read_q', query.trim()); else url.searchParams.delete('read_q');
    try { await navigator.clipboard.writeText(url.toString()); status = '定位链接已复制'; } catch { status = '无法复制，请检查剪贴板权限'; }
  }
  async function poll() {
    if (pollBusy || document.hidden || !host?.getClientRects().length || loading) return;
    pollBusy = true; const controller = new AbortController(); liveRequest = controller;
    try {
      const page = await json(`${endpoint}?probe=true`, controller.signal);
      if (!stamp) stamp = page.source_stamp || '';
      else if (stamp !== page.source_stamp) { hasNew = true; if (following) await latest(true); }
    } catch { /* 短暂断连保留已读窗口，手动读取仍有明确错误。 */ }
    finally { pollBusy = false; }
  }
  async function latest(automatic = false) {
    if (!await load(new URLSearchParams({ tail: 'true' }), '', automatic)) return;
    hasNew = false;
    const visible = showRecords ? items : items.filter(isReaderMessage);
    if (visible.length) { const last = visible[visible.length - 1]; expanded = new Set([...expanded, last.id]); await reveal(last.id); }
  }
  function closeMore() { moreOpen = false; moreTrigger?.focus(); }
  function dismissMore(event: Event) {
    if (moreOpen && event.target instanceof Node && !moreMenu?.contains(event.target)) moreOpen = false;
  }
  function pageAtEdge(direction: ReaderPageDirection) {
    if (!scrollRoot || loading || destroyed || moreOpen) return;
    const edge = direction === 'before' ? beforeEdge : afterEdge;
    if (!edge?.isConnected) return;
    const root = scrollRoot.getBoundingClientRect(); const rect = edge.getBoundingClientRect();
    const near = direction === 'after' ? rect.top <= root.bottom + 96 && rect.bottom >= root.top + toolbarHeight
      : rect.bottom >= root.top + toolbarHeight - 96 && (rect.top <= root.top + toolbarHeight + 96 || scrollRoot.scrollTop <= 1 && rect.top < root.bottom);
    if (near) { scrollIntent = ''; void (direction === 'before' ? earlier() : more()); }
  }
  function intendScroll(direction: ReaderPageDirection, target: EventTarget | null) {
    if (loading || !(target instanceof Element) || target.closest('input,textarea,select,.reader-more-panel')) return;
    // 代码块和目录自身还可滚动时，不把其中的操作当作整段对话翻页。
    for (let element: Element | null = target; element && element !== scrollRoot; element = element.parentElement) {
      if (element.scrollHeight > element.clientHeight + 1 && /^(auto|scroll)$/.test(getComputedStyle(element).overflowY)
        && (direction === 'before' ? element.scrollTop > 1 : element.scrollTop + element.clientHeight < element.scrollHeight - 1)) return;
    }
    scrollIntent = direction; pageAtEdge(direction);
  }
  function onScroll() {
    const top = scrollRoot?.scrollTop || 0; const delta = top - lastScrollTop; lastScrollTop = top;
    refreshStickyQuestion();
    scheduleSave();
    if (delta) { const direction = delta < 0 ? 'before' : 'after'; if (scrollbarDragging || scrollIntent === direction) pageAtEdge(direction); }
  }
  function onKeydown(event: KeyboardEvent) {
    if (event.key === 'Escape' && moreOpen) { event.preventDefault(); event.stopPropagation(); closeMore(); return; }
    if ((event.metaKey || event.ctrlKey) && event.key.toLowerCase() === 'f') { event.preventDefault(); event.stopPropagation(); moreOpen = false; searchOpen = true; void tick().then(() => searchInput?.focus()); }
    if (event.key === 'Escape' && searchOpen) { event.stopPropagation(); searchRequest?.abort(); searching = false; searchOpen = false; host.focus(); }
  }
  onMount(() => {
    mounted = true; scrollRoot = host.closest<HTMLElement>('.detail-content');
    const state = readReaderState(localStorage, workspace, conversationID);
    if (state) { expanded = new Set(state.expanded); query = state.query; role = state.role; navigation = state.navigation; source = state.location?.source || ''; }
    const explicit = location || readerLocationFromURL(window.location.search); lastLocation = location;
    if (explicit) void jump(explicit);
    else if (state?.location) void jump(state.location, state.inset).then(async () => { if (error) { status = '上次阅读位置已失效，已回到开头'; await load(); } });
    else void load();
    scrollRoot?.addEventListener('scroll', onScroll, { passive: true });
    const pauseFollowing = (event: WheelEvent) => { if (event.deltaY < 0) following = false; if (event.deltaY) intendScroll(event.deltaY < 0 ? 'before' : 'after', event.target); };
    scrollRoot?.addEventListener('wheel', pauseFollowing, { passive: true });
    const pausePointer = (event: PointerEvent) => { if (event.target === scrollRoot || event.pointerType === 'touch') following = false; scrollbarDragging = event.target === scrollRoot && event.pointerType !== 'touch'; };
    scrollRoot?.addEventListener('pointerdown', pausePointer);
    const releasePointer = () => { scrollbarDragging = false; };
    window.addEventListener('pointerup', releasePointer); window.addEventListener('pointercancel', releasePointer);
    const touchStart = (event: TouchEvent) => { touchY = event.touches[0]?.clientY || 0; };
    const touchMove = (event: TouchEvent) => { if (event.touches.length !== 1) return; const y = event.touches[0].clientY; const delta = touchY - y; touchY = y; if (delta < 0) following = false; if (Math.abs(delta) > 2) intendScroll(delta < 0 ? 'before' : 'after', event.target); };
    scrollRoot?.addEventListener('touchstart', touchStart, { passive: true }); scrollRoot?.addEventListener('touchmove', touchMove, { passive: true });
    const pauseKeys = (event: KeyboardEvent) => {
      if (event.defaultPrevented || event.ctrlKey || event.metaKey || event.altKey || (event.target as Element)?.closest('input,textarea,select,button,summary,[role="tab"]')) return;
      const before = ['ArrowUp', 'PageUp', 'Home'].includes(event.key) || event.key === ' ' && event.shiftKey;
      if (before) following = false;
      if (before || ['ArrowDown', 'PageDown', 'End', ' '].includes(event.key)) intendScroll(before ? 'before' : 'after', event.target);
    };
    scrollRoot?.addEventListener('keydown', pauseKeys);
    host.addEventListener('keydown', onKeydown);
    document.addEventListener('pointerdown', dismissMore);
    document.addEventListener('focusin', dismissMore);
    window.addEventListener('pagehide', save);
    liveTimer = setInterval(() => void poll(), 3000);
    return () => { save(); destroyed = true; request?.abort(); searchRequest?.abort(); liveRequest?.abort(); fullRequest?.abort(); directoryRequest?.abort(); clearInterval(liveTimer); clearTimeout(restoreTimer);
      scrollRoot?.removeEventListener('scroll', onScroll); scrollRoot?.removeEventListener('wheel', pauseFollowing); scrollRoot?.removeEventListener('pointerdown', pausePointer); scrollRoot?.removeEventListener('keydown', pauseKeys);
      scrollRoot?.removeEventListener('touchstart', touchStart); scrollRoot?.removeEventListener('touchmove', touchMove); window.removeEventListener('pointerup', releasePointer); window.removeEventListener('pointercancel', releasePointer);
      host.removeEventListener('keydown', onKeydown); document.removeEventListener('pointerdown', dismissMore); document.removeEventListener('focusin', dismissMore); window.removeEventListener('pagehide', save); };
  });
</script>

<div class="chronicle-reader" bind:this={host} style={`--reader-toolbar-height: ${toolbarHeight}px`} tabindex="-1" role="region" aria-label="对话阅读器">
  <div class="reader-controls" bind:offsetHeight={toolbarHeight}>
    <slot name="view-tabs" />
    <span class="reader-window-count" title={`当前窗口已载入 ${messageCount} 条用户和助手消息`}>{messageCount} 条消息</span>
    <div class="reader-actions">
    <slot name="summary-toggle" />
    <button type="button" class="reader-directory-toggle" aria-label="提问目录" aria-expanded={directory === 'turns'} title="按提问浏览当前历史来源，点击目录项跳到对应消息" on:click={() => showDirectory('turns')}><List aria-hidden="true" /><span>提问目录</span></button>
    <button type="button" class="reader-search-toggle" aria-label="搜索本对话" aria-expanded={searchOpen} title="搜索当前历史来源的完整内容，不限于已载入的消息" on:click={async () => { searchOpen = !searchOpen; await tick(); searchInput?.focus(); }}><Search aria-hidden="true" /><span>搜索本对话</span></button>
    <details class="reader-more" class:reader-more-active={showRecords || following || directory === 'files'} bind:this={moreMenu} bind:open={moreOpen}>
      <summary bind:this={moreTrigger} title="执行记录、文件改动与跟随最新" aria-expanded={moreOpen}>更多<span class="reader-more-chevron" aria-hidden="true"></span></summary>
      <div class="reader-more-panel" role="group" aria-label="更多阅读功能">
        <button type="button" class="reader-records-toggle" aria-pressed={showRecords} title="显示或隐藏工具、命令、思考和系统记录；数量仅统计当前已载入部分" on:click={() => { showRecords = !showRecords; closeMore(); }}><span>{showRecords ? '收起执行记录' : '展开执行记录'}</span><small>{recordCount}</small></button>
        <button type="button" class="reader-files-toggle" aria-expanded={directory === 'files'} title="查看对话记录中的文件改动，点击文件定位当时的执行记录" on:click={() => { closeMore(); void showDirectory('files'); }}>文件改动</button>
        <button type="button" class="reader-follow-toggle" class:reader-following={following} aria-pressed={following} title="跳到当前最新消息，并在记录更新时自动跟随；向上滚动会暂停跟随" on:click={() => { following = !following; closeMore(); if (following) void latest(); }}>{following ? '停止跟随最新' : '跟随最新'}</button>
      </div>
    </details>
    </div>
  </div>
  <slot name="before-content" />
  {#if searchOpen}<form class="reader-search" on:submit|preventDefault={() => search(true)}>
    <input aria-label="搜索当前会话全文" placeholder="搜索当前会话全文" bind:this={searchInput} bind:value={query} maxlength="4096" on:input={() => { searchRequest?.abort(); searching = false; matches = []; matchIndex = -1; searchDone = false; searchPending = false; searchCursor = ''; }} on:keydown={(event) => { if (event.key === 'Enter' && (event.shiftKey || matches.length && !searching)) { event.preventDefault(); void nextMatch(event.shiftKey ? -1 : 1); } }} />
    <select aria-label="搜索消息角色" bind:value={role} on:change={() => { searchRequest?.abort(); searching = false; matches = []; matchIndex = -1; searchCursor = ''; searchDone = false; searchPending = false; }}><option value="">全部</option><option value="user">用户</option><option value="assistant">助手</option></select>
    <button type="submit" disabled={searching}>{searching ? '搜索中…' : '搜索'}</button><button type="button" aria-label="上一处命中" disabled={!matches.length} on:click={() => nextMatch(-1)}>↑</button><button type="button" aria-label="下一处命中" disabled={!matches.length} on:click={() => nextMatch(1)}>↓</button>
    <button type="button" aria-label="关闭本对话搜索" on:click={() => { searchRequest?.abort(); searching = false; searchOpen = false; host.focus(); }}>×</button>
    <span class="reader-search-status" role="status">{matches.length ? `${Math.max(matchIndex + 1, 1)} / ${matches.length}${matches.length >= 500 ? '，最多显示 500 处' : searchPending ? '，末尾仍在写入' : searchDone ? '' : '，仍有范围未搜索'}` : searching ? '正在搜索…' : searchPending ? '完整记录中未命中，末尾仍在写入' : searchDone ? '没有找到匹配内容' : '搜索范围：整个当前会话'}</span>
    {#if !searchDone && searchCursor}<button type="button" disabled={searching || matches.length >= 500} on:click={() => search(false)}>继续搜索</button>{/if}
    {#if searching}<button type="button" on:click={() => { searchRequest?.abort(); searching = false; }}>停止</button>{/if}
  </form>{/if}
  {#if hasNew}<div class="reader-new" role="status"><span>有新内容</span><button type="button" on:click={() => latest()}>查看最新</button></div>{/if}
  {#if status}<p class="reader-status" role="status">{fullBusy ? '正在读取完整消息…' : status}</p>{/if}
  {#if !complete || diagnostics.length}<details class="reader-diagnostics" open={!complete}><summary>{complete ? '读取提示' : '历史不完整'}</summary>{#each diagnostics as message}<p>{message}</p>{/each}</details>{/if}
  {#if error && !failedPage}<div class="reader-error" role="alert"><span>{error}</span><button type="button" on:click={() => load()}>重新加载</button></div>{/if}
  {#if versions.length > 1}<div class="reader-version"><label for="reader-source">历史来源</label><select id="reader-source" value={source} on:change={(e) => switchSource(e.currentTarget.value)}>{#each versions as version}<option value={version.path}>{version.mainline ? '当前主线' : '历史来源'} · {version.path.split('/').pop()}</option>{/each}</select>{#if mainline && source !== mainline}<small>正在阅读旧版本</small>{:else if !mainline}<small>当前主线未确认</small>{/if}</div>{/if}
  {#if directory}<nav class="reader-directory" aria-label={directory === 'turns' ? '提问目录' : '会话文件改动'}>
    <div class="reader-directory-heading"><strong>{directory === 'turns' ? '提问目录' : '文件改动'}<small>{directoryTotal} 项</small></strong><button type="button" aria-label="关闭目录" on:click={() => { directoryRequest?.abort(); directory = ''; navigation = false; }}>×</button></div>
    {#if directoryError}<p class="reader-error" role="alert">{directoryError}<button type="button" on:click={() => showDirectory(directory || 'turns')}>重试</button></p>{/if}
    {#if directory === 'files'}<p class="reader-directory-note">来自对话记录，点击定位证据。</p>{/if}
    <ol>{#if directory === 'turns'}{#each allTurns as turn}<li><button type="button" title={turn.caption || turn.id} on:click={() => jumpReference(turn.message || turn.start)}><span class="reader-directory-label">{turn.caption || '无提问的记录轮次'}</span><small>{turn.status === 'interrupted' ? '已中断' : turn.inferred ? '提问分组' : ''}</small></button></li>{/each}
      {:else}{#each files as file}<li><button type="button" title={file.move_path ? `${file.path} → ${file.move_path}` : file.path} on:click={() => jumpReference(file.source)}><small>{({create:'新增',modify:'修改',delete:'删除',rename:'重命名'} as Record<string,string>)[file.kind] || '修改'}</small><code class="reader-directory-label">{file.path}{#if file.move_path} → {file.move_path}{/if}</code><small class:reader-tool-failed={file.status === 'failed'}>{file.status === 'completed' ? `+${file.added} −${file.removed}` : file.status === 'failed' ? '失败' : file.status === 'interrupted' ? '已中断' : '未确认'}</small></button></li>{/each}{/if}</ol>
    {#if directoryBusy}<p class="reader-status" role="status">正在读取目录…</p>{:else if !directoryError && !directoryTotal}<p class="reader-status">{directory === 'files' ? '没有记录到文件改动。' : '没有可导航的轮次。'}</p>{/if}
    {#if directoryMore}<button type="button" disabled={directoryBusy} on:click={() => showDirectory(directory || 'turns', true)}>加载更多</button>{/if}
  </nav>{/if}
  {#if currentQuestion}<div class="reader-sticky-question"><details><summary>{currentQuestion.slice(0, 180)}</summary><p>{currentQuestion}</p></details></div>{/if}
  {#if previous || trimmedBefore}<div class="reader-page-edge" data-reader-direction="before" bind:this={beforeEdge} role="status" title={beforePageHint} aria-busy={paging === 'before'}><ReaderPageIndicator direction="before" busy={paging === 'before'} failed={failedPage === 'before'} disabled={loading} label={beforePageHint} on:retry={earlier} /></div>{/if}
  <div class="reader-messages">
    {#each visibleItems as event, index (event.id)}
      {#if index === 0 || event.turn_id !== visibleItems[index - 1]?.turn_id}<div class="reader-turn-marker"><span>{event.turn_inferred ? '提问分组' : event.turn_id ? '轮次' : '上下文'}</span>{#if event.turn_inferred}<small>根据已记录提问推断</small>{/if}</div>{/if}
      {#if isReaderMessage(event)}
        <ReaderMessage {event} open={expanded.has(event.id)} {query} active={activeID === event.id} on:toggle={(e) => toggle(e.detail.id, e.detail.open)} on:full={(e) => full(e.detail)} on:share={(e) => share(e.detail)} on:evidence={(e) => dispatch('evidence', e.detail)} />
      {:else}<ReaderTool {event} open={expanded.has(event.id)} active={activeID === event.id} busy={fullBusy === event.id} on:toggle={(e) => toggle(e.detail.id, e.detail.open)} on:full={(e) => full(e.detail)} on:share={(e) => share(e.detail)} on:evidence={(e) => dispatch('evidence', e.detail)} />{/if}
    {/each}
  </div>
  {#if indexing || loading && !paging}<div class="reader-status" role="status">{indexing ? `正在整理有效历史 ${totalBytes ? Math.min(100, Math.round(indexedBytes / totalBytes * 100)) : 0}%` : '正在读取消息…'}{#if indexing}<progress value={indexedBytes} max={totalBytes || 1} aria-label="有效历史整理进度"></progress><button type="button" on:click={stopWaiting}>停止等待</button>{/if}</div>{:else if !visibleItems.length && !error && !loading}<div class="empty-note">{recordCount ? '当前窗口只有执行记录，可展开查看或继续加载消息。' : '当前窗口没有可读消息。'}</div>{/if}
  {#if hasMore}<div class="reader-page-edge" data-reader-direction="after" bind:this={afterEdge} role="status" title={afterPageHint} aria-busy={paging === 'after'}><ReaderPageIndicator direction="after" busy={paging === 'after'} failed={failedPage === 'after'} disabled={loading} label={afterPageHint} on:retry={more} /></div>{/if}
</div>
