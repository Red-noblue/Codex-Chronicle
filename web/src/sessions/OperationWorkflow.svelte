<!-- 中文说明：单条和批量操作共用固定的下一步入口；只触发预览或打开数字确认，不直接执行删除。 -->
<script lang="ts">
  import { createEventDispatcher } from 'svelte';
  import { ArrowRight, Check, LoaderCircle, RefreshCw } from '@lucide/svelte';

  export let count = 0;
  export let selected = false;
  export let previewed = false;
  export let blocked = false;
  export let ready = false;
  export let loading = false;
  export let confirming = false;
  export let running = false;
  export let writesEnabled = false;
  export let needsPassword = false;
  export let destructive = true;
  export let selectionError = '';
  export let message = '';
  const dispatch = createEventDispatcher<{ preview: void; confirm: void }>();
  $: steps = ['选择方式', '查看影响', destructive ? '数字确认' : '确认归档'];

  $: current = !selected || !count || selectionError ? 1 : ready ? 3 : 2;
  $: disabled = !selected || !count || loading || confirming || running || Boolean(selectionError) || (previewed && needsPassword);
  $: hint = loading ? '正在检查文件、数据库和活动状态…'
    : confirming ? '正在准备执行前确认…'
    : running ? '已有任务正在执行，请等待完成或停止后续项。'
    : !count ? '请先在会话库选择要处理的对话。'
    : !selected ? '请先勾选一种处理方式。'
    : selectionError ? selectionError
    : blocked ? message || '检查未通过，请处理预览中的阻断项后重新检查。'
    : !previewed ? message || '先检查影响范围，不会移动或删除对话。'
    : !writesEnabled ? '当前服务只允许预览，尚未启用执行。'
    : needsPassword ? '请先在上方填写归档密码。'
    : ready ? destructive ? '核对文件与记录清单后，继续输入确认数字。' : '请核对保存位置和归档内容，再确认归档。'
    : message || '当前预览不可执行，请重新检查影响。';
  $: label = loading ? '正在检查影响…'
    : confirming ? '正在准备确认…'
    : running ? '任务执行中'
    : !selected || !count ? '下一步：查看影响'
    : selectionError ? '请选择归档位置'
    : ready ? destructive ? '下一步：输入确认数字' : '下一步：确认归档'
    : previewed ? '重新检查影响'
    : message ? '影响检查失败 · 点击重试'
    : `下一步：查看影响（${count} 项）`;
</script>

<section class="operation-workflow" aria-label="处理步骤">
  <ol aria-label="操作流程">
    {#each steps as step, index}
      <li class:current={current === index + 1} class:complete={current > index + 1} aria-current={current === index + 1 ? 'step' : undefined}>
        <span class="step-number">{#if current > index + 1}<Check aria-hidden="true" />{:else}{index + 1}{/if}</span><span>{step}</span>
      </li>
    {/each}
  </ol>
  <p class:blocked={blocked || Boolean(selectionError) || (!previewed && Boolean(message) && !loading)} role="status" aria-live="polite" aria-atomic="true">{hint}</p>
  <div class="workflow-actions">
    {#if ready}<button type="button" class="workflow-recheck" disabled={disabled} on:click={() => dispatch('preview')}><RefreshCw aria-hidden="true" />重新检查</button>{/if}
    <button type="button" class="operation-next-button" data-step={ready ? 'confirm' : 'preview'} {disabled} on:click={() => dispatch(ready ? 'confirm' : 'preview')}>
      <span>{label}</span>{#if loading || confirming}<LoaderCircle aria-hidden="true" />{:else}<ArrowRight aria-hidden="true" />{/if}
    </button>
  </div>
</section>

<style>
  .operation-workflow { display: grid; gap: var(--space-3); min-width: 0; }
  ol { display: flex; align-items: center; justify-content: space-between; gap: var(--space-2); margin: 0; padding: 0; list-style: none; font-size: var(--font-caption); }
  li { display: flex; align-items: center; gap: var(--space-1); min-width: 0; color: var(--text-muted); }
  .step-number { display: grid; place-items: center; flex-shrink: 0; width: 20px; height: 20px; border: 1px solid var(--border-strong); border-radius: 50%; font-variant-numeric: tabular-nums; }
  li.current { color: var(--accent); font-weight: 600; }
  li.current .step-number { border-color: var(--accent); background: var(--bg-selected); }
  li.complete { color: var(--text-secondary); }
  p { margin: 0; color: var(--text-secondary); font-size: var(--font-caption); line-height: 1.6; overflow-wrap: anywhere; max-height: 5em; overflow: auto; }
  p.blocked { color: var(--warning); }
  .workflow-actions { display: flex; align-items: stretch; gap: var(--space-2); min-width: 0; }
  button { display: flex; align-items: center; justify-content: center; gap: var(--space-2); min-width: 0; min-height: var(--control-touch, 44px); padding: var(--space-2) var(--space-3); border: 1px solid var(--border-strong); border-radius: var(--radius-control); background: var(--bg-surface); color: var(--text-secondary); font: inherit; font-size: var(--font-ui); cursor: pointer; transition: background var(--duration-fast), border-color var(--duration-fast); }
  button:hover { background: var(--bg-hover); }
  .operation-next-button { flex: 1; justify-content: space-between; color: var(--on-accent); background: var(--accent-solid); border-color: var(--accent-solid); font-weight: 600; }
  .operation-next-button:hover { background: var(--accent-solid-hover); border-color: var(--accent-solid-hover); }
  button:disabled { cursor: not-allowed; opacity: .55; }
  button span { min-width: 0; overflow-wrap: anywhere; text-align: start; }
  .operation-workflow :global(svg) { width: 16px; height: 16px; flex-shrink: 0; }
  .step-number :global(svg) { width: 12px; height: 12px; }
  @media (max-width: 479px) {
    ol { gap: var(--space-1); }
    li { flex: 1; flex-direction: column; text-align: center; }
    .workflow-recheck { padding-inline: var(--space-2); }
  }
  @media (prefers-reduced-motion: reduce) { button { transition: none; } }
</style>
