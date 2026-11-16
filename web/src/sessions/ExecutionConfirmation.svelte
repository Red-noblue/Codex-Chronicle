<!-- 中文说明：单条与批量共用的危险操作确认，原生 dialog 管理焦点与 Escape；两次数字只用于防止误操作。 -->
<script lang="ts">
  import { createEventDispatcher, onMount } from 'svelte';
  import { ShieldCheck, X } from '@lucide/svelte';
  import type { ExecutionConfirmation } from './execution';

  export let open = false;
  export let challenge: ExecutionConfirmation | null = null;
  export let action = '';
  export let sourceRoot = '';
  export let destination = '';
  export let permanent = false;
  export let restricted = false;
  export let packagedTrash = false;
  export let archived = false;
  export let targets: Array<{ id: string; title: string; automaticallyIncluded?: boolean; subagent?: boolean }> = [];
  export let busy = false;
  export let message = '';
  let dialog: HTMLDialogElement;
  let code = '';
  let additionalCode = '';
  let step = 1;
  let previousChallenge = '';
  let now = Date.now();
  const dispatch = createEventDispatcher<{ confirm: { code: string; additionalCode: string }; refresh: void; cancel: void }>();

  onMount(() => {
    const timer = setInterval(() => now = Date.now(), 1000);
    return () => clearInterval(timer);
  });
  $: expired = challenge ? now >= Date.parse(challenge.expires_at) : true;
  $: currentCode = step === 1 ? challenge?.code : challenge?.additional_code;
  $: inputValid = Boolean(challenge && !expired && (!challenge.code || (step === 1 ? code === challenge.code : additionalCode === challenge.additional_code)));
  $: if (challenge?.confirmation_id !== previousChallenge) {
    previousChallenge = challenge?.confirmation_id || '';
    code = ''; additionalCode = ''; step = 1; now = Date.now();
  }
  $: if (dialog) {
    if (open && !dialog.open) { dialog.showModal(); dialog.querySelector<HTMLButtonElement>('[data-cancel]')?.focus(); }
    else if (!open && dialog.open) dialog.close();
  }
  function cancel() { open = false; dispatch('cancel'); }
  function submit() {
    if (!inputValid || busy) return;
    if (step === 1 && challenge?.additional_code) {
      step = 2;
      requestAnimationFrame(() => dialog.querySelector<HTMLInputElement>('#execution-additional-code')?.focus());
      return;
    }
    dispatch('confirm', { code, additionalCode });
  }
</script>

<dialog class="execution-dialog" bind:this={dialog} aria-labelledby="execution-confirm-title" aria-describedby="execution-confirm-description" on:cancel={cancel} on:close={() => open = false}>
  <header><div><span class="eyebrow"><ShieldCheck aria-hidden="true" />{challenge?.additional_code ? `第 ${step} / 2 次确认` : '执行前确认'}</span><h2 id="execution-confirm-title">{action} · {targets.length} 个对话</h2></div><button class="close-button" type="button" aria-label="关闭执行确认" on:click={cancel}><X aria-hidden="true" /></button></header>
  <form on:submit|preventDefault={submit}>
    <div class="confirmation-body">
      <p id="execution-confirm-description" class:permanent>{permanent ? `删除 .codex 中的源文件和专属记录，跳过回收站。${archived ? '归档副本校验成功后才删除，归档保留。' : '未选择归档，完成后无法撤销。'}` : challenge?.code ? '请核对对象和目的地，再输入下方数字。' : '将正文、目标数据库行和索引保存至所选位置，保留 .codex 中的源版本。'}</p>
      <dl><div><dt>读取源</dt><dd><code>{sourceRoot}</code></dd></div>{#if destination}<div><dt>{permanent ? '恢复凭据' : '目的地'}</dt><dd><code>{destination}</code></dd></div>{/if}<div><dt>执行方式</dt><dd>每批最多 {challenge?.batch_size || 10} 个，逐项执行；失败即停。</dd></div></dl>
      {#if packagedTrash}<p>对话文件、目标数据库行和索引记录将集中放入一个回收文件夹。成功后清理临时备份；清空回收站前仍占磁盘空间。</p>{/if}
      {#if restricted}<p>本机受限计划共用恢复包，作为一组执行；开始后不会主动中断当前事务。</p>{/if}
      {#if targets.some(target => target.subagent)}<p>本次含 {targets.filter(target => !target.subagent).length} 个主会话、{targets.filter(target => target.subagent).length} 个子代理。确认数字按全部实际线程计数。</p>{/if}
      <details open={targets.some(target => target.automaticallyIncluded)}><summary>核对 {targets.length} 个对话</summary><ol>{#each targets as target}<li><strong title={target.title}>{target.automaticallyIncluded ? '自动纳入 · ' : ''}{target.subagent && !target.title.startsWith('子代理') ? '子代理 · ' : ''}{target.title || '未命名对话'}</strong><code>{target.id}</code></li>{/each}</ol></details>
      {#if challenge?.code}
        {#if step === 2}<p class="additional-notice">本次超过 10 个对话。再次确认后，按 {Math.ceil(targets.length / 10)} 批顺序处理。</p>{/if}
        <div class="code-section"><label for={step === 1 ? 'execution-code' : 'execution-additional-code'}>{step === 1 ? '请输入本次确认数字' : '请输入第二组确认数字'}</label><code class="challenge-code" aria-label={`确认数字：${currentCode?.split('').join(' ')}`}>{currentCode}</code>
          {#if step === 1}<input id="execution-code" type="text" bind:value={code} inputmode="numeric" pattern={'[0-9]{6}'} maxlength="6" autocomplete="off" spellcheck="false" aria-describedby="execution-code-help" />{:else}<input id="execution-additional-code" type="text" bind:value={additionalCode} inputmode="numeric" pattern={'[0-9]{6}'} maxlength="6" autocomplete="off" spellcheck="false" aria-describedby="execution-code-help" />{/if}
          <small id="execution-code-help">{expired ? '确认已过期，请重新获取数字。' : '数字仅用于本次计划，5 分钟内有效。支持复制和粘贴。'}</small>
        </div>
      {/if}
      {#if message}<p class="confirmation-error" role="alert">{message}</p>{/if}
    </div>
    <footer><button class="secondary-button" data-cancel type="button" on:click={cancel}>取消</button>{#if expired || message}<button class="secondary-button" type="button" disabled={busy} on:click={() => dispatch('refresh')}>重新确认</button>{:else}<button class="confirm-button" class:archive-confirm={!challenge?.code} type="submit" disabled={!inputValid || busy}>{busy ? '正在提交…' : step === 1 && challenge?.additional_code ? '继续第二次确认' : `确认${action}`}</button>{/if}</footer>
  </form>
</dialog>

<style>
  .execution-dialog { width: min(540px, calc(100vw - 24px)); max-height: calc(100dvh - 32px); margin: auto; padding: 0; border: 1px solid var(--border-strong); border-radius: var(--radius-dialog); background: var(--bg-surface); color: var(--text-primary); box-shadow: var(--shadow); overflow: auto; }
  .execution-dialog::backdrop { background: var(--overlay-backdrop, #0008); }
  header { display: flex; gap: var(--space-3); align-items: start; padding: var(--space-4); border-bottom: 1px solid var(--border); }
  header > div { flex: 1; min-width: 0; }
  .eyebrow { display: flex; align-items: center; gap: var(--space-2); color: var(--text-secondary); font-size: var(--font-caption); }
  :global(.execution-dialog svg) { width: 16px; height: 16px; }
  h2 { margin: var(--space-2) 0 0; font-size: var(--font-heading); overflow-wrap: anywhere; }
  .close-button { width: var(--control-md); height: var(--control-md); display: grid; place-items: center; border: 0; border-radius: var(--radius-control); color: var(--text-secondary); background: transparent; cursor: pointer; }
  .close-button:hover { background: var(--bg-hover); }
  .confirmation-body { display: grid; gap: var(--space-4); padding: var(--space-4); min-width: 0; }
  p, dl { margin: 0; font-size: var(--font-ui); line-height: 1.6; }
  p { color: var(--text-secondary); }
  .permanent, .confirmation-error { color: var(--danger); }
  dl { display: grid; gap: var(--space-2); font-size: var(--font-caption); }
  dl > div { display: grid; grid-template-columns: 66px minmax(0, 1fr); gap: var(--space-2); }
  dt { color: var(--text-muted); } dd { margin: 0; min-width: 0; }
  code { font-family: var(--font-mono, monospace); overflow-wrap: anywhere; white-space: pre-wrap; }
  details { border: 1px solid var(--border); border-radius: var(--radius-control); padding: var(--space-3); font-size: var(--font-caption); }
  summary { cursor: pointer; }
  ol { max-height: 160px; overflow: auto; padding: 0; margin: var(--space-3) 0 0; list-style: none; }
  li { display: grid; gap: 4px; padding: var(--space-2) 0; border-bottom: 1px solid var(--border); }
  li strong { font-weight: 500; white-space: nowrap; text-overflow: ellipsis; overflow: hidden; }
  li code { color: var(--text-muted); }
  .code-section { display: grid; gap: var(--space-2); padding: var(--space-3); background: var(--bg-subtle); border-radius: var(--radius-control); }
  label { font-size: var(--font-ui); font-weight: 500; }
  .challenge-code { font-size: 28px; letter-spacing: .22em; line-height: 1.5; font-variant-numeric: tabular-nums; }
  input { box-sizing: border-box; min-width: 0; width: 100%; height: var(--control-lg, 40px); padding: 8px 12px; color: var(--text-primary); background: var(--bg-surface); border: 1px solid var(--border-strong); border-radius: var(--radius-control); font: inherit; font-family: monospace; letter-spacing: .2em; }
  small { color: var(--text-muted); font-size: var(--font-caption); }
  .additional-notice { padding-left: var(--space-3); border-left: 2px solid var(--accent); }
  footer { display: flex; flex-wrap: wrap; justify-content: end; gap: var(--space-2); padding: var(--space-4); border-top: 1px solid var(--border); }
  .confirm-button { padding: 8px 14px; border: 1px solid var(--danger); border-radius: var(--radius-control); background: var(--danger); color: var(--bg-surface); font-size: var(--font-ui); font-weight: 600; cursor: pointer; }
  .archive-confirm { border-color: var(--accent-solid); background: var(--accent-solid); color: var(--on-accent); }
  button:disabled { opacity: .5; cursor: not-allowed; }
  button:focus-visible, input:focus-visible, summary:focus-visible { outline: 2px solid var(--accent); outline-offset: 3px; }
</style>
