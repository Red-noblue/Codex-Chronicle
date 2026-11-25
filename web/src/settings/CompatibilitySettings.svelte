<!-- 中文说明：集中展示当前工作区的格式读取状态，并保存由服务端执行的源数据访问策略。 -->
<script lang="ts">
  import { createEventDispatcher, onDestroy } from 'svelte';
  import { CircleCheck, CircleAlert, LoaderCircle, RefreshCw } from '@lucide/svelte';
  type Policy = 'automatic' | 'readonly';
  type Compatibility = {
    root: string; policy: Policy; persisted: boolean;
    source_status: string; checked_at: string; stale: boolean;
    resolution: { descriptor: { capabilities?: Record<string, boolean> } };
  };
  export let root: string;
  export let request: typeof fetch;
  export let helpText = '';
  const dispatch = createEventDispatcher<{ policychange: { root: string; policy: Policy } }>();
  let state: Compatibility | null = null;
  let activeRoot: string | null = null;
  let controller: AbortController | undefined;
  let loading = true;
  let detecting = false;
  let saving = false;
  let message = '';
  let error = '';
  $: if (root !== activeRoot) {
    activeRoot = root;
    controller?.abort();
    controller = new AbortController();
    state = null;
    message = error = '';
    detecting = saving = false;
    if (root) void load(controller); else loading = false;
  }
  $: canRead = Boolean(state?.resolution.descriptor.capabilities?.read);
  $: canSearch = Boolean(state?.resolution.descriptor.capabilities?.search_projection);
  $: protectionText = state?.policy === 'readonly'
    ? '禁止删除、恢复和修改原始记录。阅读、搜索与备份不受影响。'
    : state?.source_status === '未启用源数据修改'
      ? '当前服务只支持查看，暂不能删除或恢复原始记录。'
      : '删除或恢复原始记录前，仍需检查并由你确认。';
  $: helpText = [
    'Codex 更新后，应用会自动尝试读取会话，无需手动选择版本。',
    '只读保护会阻止本项目修改 Codex 原始记录，包括删除和恢复；不会限制 Codex 自身的操作。',
    '关闭保护后，删除和恢复仍需先检查、再由你确认。遇到不支持的数据格式，会停止操作。'
  ].join('\n\n');
  async function json(path: string, control: AbortController, init: RequestInit = {}) {
    const response = await request(path, { ...init, signal: control.signal });
    const body = await response.json();
    if (!response.ok) throw new Error(body.error || `请求失败（HTTP ${response.status}）`);
    return body;
  }
  function apply(next: Compatibility) {
    if (next.root !== activeRoot) throw new Error('工作区已变化，请重新读取兼容状态');
    state = next;
    dispatch('policychange', { root: next.root, policy: next.policy });
  }
  async function load(control: AbortController) {
    loading = true;
    try {
      const next = await json('/api/v1/compatibility', control);
      if (!control.signal.aborted) apply(next);
    } catch (cause) {
      if (!control.signal.aborted) error = cause instanceof Error ? cause.message : '读取兼容状态失败';
    } finally {
      if (!control.signal.aborted) loading = false;
    }
  }
  async function detect() {
    const control = controller;
    if (!control || detecting || saving) return;
    detecting = true;
    message = error = '';
    try {
      let { task } = await json('/api/v1/compatibility/detect', control, {
        method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ root })
      });
      while (task.status === 'queued' || task.status === 'running') {
        await new Promise(resolve => setTimeout(resolve, 500));
        if (control.signal.aborted) return;
        ({ task } = await json(`/api/v1/tasks/${encodeURIComponent(task.id)}`, control));
      }
      if (task.status !== 'completed') throw new Error(task.error || '检测未完成，请重试');
      const next = await json('/api/v1/compatibility', control);
      if (control.signal.aborted) return;
      apply(next);
      message = '检测完成';
    } catch (cause) {
      if (!control.signal.aborted) error = cause instanceof Error ? cause.message : '检测失败';
    } finally {
      if (!control.signal.aborted) detecting = false;
    }
  }
  async function save(policy: Policy) {
    const control = controller;
    if (!control || !state || saving || detecting) return;
    saving = true;
    message = error = '';
    try {
      const next = await json('/api/v1/compatibility', control, {
        method: 'PUT', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ root, policy })
      });
      if (control.signal.aborted) return;
      if (next.root !== activeRoot) throw new Error('工作区已变化，请重新读取兼容状态');
      state = { ...state, policy: next.policy, source_status: next.policy === 'readonly' ? '已设为仅查看' : '需重新读取状态' };
      dispatch('policychange', { root: next.root, policy: next.policy });
      message = next.policy === 'readonly' ? '只读保护已开启' : '只读保护已关闭';
      await load(control);
    } catch (cause) {
      if (!control.signal.aborted) {
        error = cause instanceof Error ? cause.message : '未能保存，请重新读取设置。';
      }
    } finally {
      if (!control.signal.aborted) saving = false;
    }
  }
  onDestroy(() => controller?.abort());
</script>

<div class="compatibility" aria-busy={loading || detecting || saving}>
  {#if state}
    <div class="read-status" class:limited={!canRead || !canSearch}>
      {#if canRead && canSearch}<CircleCheck size={18} aria-hidden="true" />{:else}<CircleAlert size={18} aria-hidden="true" />{/if}
      <div><strong>{canRead && canSearch ? '支持阅读与搜索' : canRead ? '可以阅读，搜索受限' : '暂时无法读取会话'}</strong>
        {#if !canRead || !canSearch}<p>部分内容可能无法显示，可重新检测。</p>{/if}
      </div>
    </div>
    <div class="protection-row">
      <div class="protection-copy"><span id="compatibility-protection-label">只读保护</span><p id="compatibility-protection-description">{protectionText}</p></div>
      <div class="switch-control"><span aria-hidden="true">{saving ? '保存中…' : state.policy === 'readonly' ? '已开启' : '未开启'}</span>
        <button type="button" class="protection-switch" role="switch" aria-checked={state.policy === 'readonly'} aria-labelledby="compatibility-protection-label" aria-describedby="compatibility-protection-description" disabled={!state.persisted || loading || saving || detecting} on:click={() => save(state?.policy === 'readonly' ? 'automatic' : 'readonly')}><span></span></button>
      </div>
    </div>
    <div class="check-row">
      <span class="checked-at">{state.stale ? '上次检测' : '最近检测'}：{state.checked_at && !state.checked_at.startsWith('0001-') ? new Date(state.checked_at).toLocaleString('zh-CN', { hour12: false, year: 'numeric', month: '2-digit', day: '2-digit', hour: '2-digit', minute: '2-digit' }) : '尚未完成'}{state.stale ? ' · 建议重新检测' : ''}</span>
      <div class="check-actions"><span class="notice" role="status">{error ? '' : message}</span>
        <button type="button" class="secondary-button" disabled={loading || detecting || saving} on:click={detect}>
          {#if detecting}<LoaderCircle size={14} class="spinning" />{:else}<RefreshCw size={14} />{/if}{detecting ? '检测中…' : '重新检测'}
        </button>
      </div>
    </div>
    {#if !state.persisted}<p class="notice">当前无法保存设置，暂不能调整只读保护。</p>{/if}
  {:else if loading}<p class="notice" role="status">正在检查会话读取状态…</p>
  {:else if !root}<p class="notice">请先选择工作区。</p>
  {/if}
  {#if error}<div class="error" role="alert"><span>{error}</span><button type="button" class="secondary-button" disabled={loading || detecting || saving} on:click={() => { error = ''; if (controller) void load(controller); }}>重新读取</button></div>{/if}
</div>

<style>
  .compatibility { display: grid; gap: 18px; font-size: var(--font-ui); }
  .read-status { display: flex; align-items: center; gap: 9px; color: var(--success); }
  .read-status strong { font-weight: 500; }
  .read-status.limited { color: var(--warning); }
  .read-status p { margin-top: 4px; color: var(--text-secondary); }
  .protection-row { display: flex; align-items: center; justify-content: space-between; gap: 20px; padding: 14px 16px; border: 1px solid var(--border); border-radius: var(--radius-control); }
  .protection-copy { min-width: 0; }
  .protection-copy > span { font-weight: 500; }
  .protection-copy p { margin-top: 5px; color: var(--text-muted); }
  .switch-control { display: flex; align-items: center; gap: 10px; flex-shrink: 0; }
  .switch-control > span, .check-row, .notice { color: var(--text-muted); font-size: var(--font-caption); }
  .protection-switch { display: flex; align-items: center; width: 36px; height: 22px; min-height: 22px; padding: 2px; border: 1px solid var(--border-strong); border-radius: 12px; background: var(--bg-hover); cursor: pointer; }
  .protection-switch > span { display: block; width: 16px; height: 16px; border-radius: 50%; background: var(--text-muted); transition: transform 120ms ease; }
  .protection-switch[aria-checked="true"] { background: var(--accent-solid); border-color: var(--accent-solid); }
  .protection-switch[aria-checked="true"] > span { transform: translateX(14px); background: var(--on-accent); }
  .protection-switch:focus-visible { outline: 2px solid var(--accent); outline-offset: 3px; }
  .protection-switch:disabled { opacity: .6; cursor: default; }
  .check-row, .check-actions { display: flex; align-items: center; gap: 10px; flex-wrap: wrap; }
  .check-row { justify-content: space-between; }
  .checked-at { line-height: 1.6; }
  .secondary-button { display: inline-flex; align-items: center; gap: 6px; white-space: nowrap; }
  p { margin: 0; line-height: 1.6; }
  .error { display: flex; align-items: center; justify-content: space-between; gap: 12px; color: var(--danger); line-height: 1.6; }
  :global(.compatibility .spinning) { animation: compatibility-spin 1s linear infinite; }
  @keyframes compatibility-spin { to { transform: rotate(360deg); } }
  @media (max-width: 520px) {
    .protection-row { align-items: flex-start; gap: 12px; padding: 12px; }
    .switch-control { flex-direction: column-reverse; gap: 5px; }
    .check-row { align-items: flex-start; }
    .check-actions { margin-left: auto; }
    .error { align-items: flex-start; flex-wrap: wrap; }
  }
  @media (prefers-reduced-motion: reduce) {
    .protection-switch > span { transition: none; }
    :global(.compatibility .spinning) { animation: none; }
  }
</style>
