<!-- 中文说明：首次操作的目的地检查弹窗；创建探针、用户目视确认和更改路径均为显式动作，关闭弹窗不会执行真实操作。 -->
<script lang="ts">
  import { createEventDispatcher } from 'svelte';
  import { Check, FlaskConical, FolderOpen, X } from '@lucide/svelte';
  import type { OperationDestinations } from './destinations';
  export let open = false;
  export let status: OperationDestinations | null = null;
  export let busy = false;
  export let message = '';
  export let customPath = '';
  export let allowCustomPath = true;
  export let needsArchive = false;
  export let needsTrash = true;
  let dialog: HTMLDialogElement;
  let correctPath = false;
  const dispatch = createEventDispatcher<{ probe: void; found: boolean; savepath: string; confirmarchive: void; refresh: void }>();
  $: if (dialog) {
    if (open && !dialog.open) { correctPath = false; dialog.showModal(); dialog.querySelector<HTMLButtonElement>('header button')?.focus(); }
    else if (!open && dialog.open) dialog.close();
  }
</script>

<dialog class="destination-dialog" bind:this={dialog} aria-label="检查处理目的地" on:cancel={() => open = false} on:close={() => open = false}>
  <header><div><h2>检查处理目的地</h2><p>先用模拟样本确认位置，再返回影响预览。</p></div><button class="destination-close" type="button" aria-label="关闭目的地检查" on:click={() => open = false}><X aria-hidden="true" /></button></header>
  <div class="destination-body" aria-busy={busy}>
    {#if status}
      {#if needsTrash}
      <section class="destination-section">
        <div class="destination-heading"><h3><FolderOpen aria-hidden="true" />{status.mode !== 'native' ? '自定义隔离目录' : '系统回收站 / 废纸篓'}</h3><span class:verified={status.trash_verified}>{status.trash_verified ? '已确认' : '待验证'}</span></div>
        <code class="destination-path">{status.trash_path || '由系统在移动时确定'}</code>
        <p>{status.mode !== 'native' ? '文件进入这个目录；普通目录可能不会显示在系统废纸篓中。' : '测试文件与当前工作区位于同一磁盘，由系统回收站接收。'}</p>
        {#if status.reason}<p>{status.reason}</p>{/if}
        <button class="secondary-button destination-probe" type="button" on:click={() => dispatch('probe')} disabled={busy}><FlaskConical aria-hidden="true" />{busy ? '处理中…' : status.probe ? '重新发送测试文件' : '发送测试文件'}</button>
        {#if status.probe}
          <div class="destination-sample">
            <strong>查找这个测试文件</strong><code>{status.probe.file_name}</code>
            {#if status.probe.trash_path}<details><summary>实际位置</summary><code>{status.probe.trash_path}</code></details>{/if}
            {#if status.probe.state === 'moved' && !status.trash_verified}<div class="destination-actions"><button class="secondary-button" type="button" on:click={() => dispatch('found', true)} disabled={busy}><Check aria-hidden="true" />我已找到测试文件</button><button class="destination-text-button" type="button" on:click={() => { correctPath = true; dispatch('found', false); }} disabled={busy}>没有找到 / 位置不对</button></div>{/if}
            {#if status.probe.state !== 'moved'}<p>上次测试未完整完成，请重新测试。</p>{/if}
          </div>
        {/if}
        {#if allowCustomPath && status.mode !== 'isolation'}
          <details class="destination-correction" open={correctPath}><summary>更改回收站位置</summary><label for="destination-custom-path">自定义目录的绝对路径</label><input id="destination-custom-path" bind:value={customPath} placeholder="留空使用系统默认位置" autocomplete="off" spellcheck="false" /><div class="destination-actions"><button class="secondary-button" type="button" disabled={busy} on:click={() => dispatch('savepath', customPath)}>保存并重新检测</button><button class="destination-text-button" type="button" disabled={busy} on:click={() => dispatch('savepath', '')}>恢复系统默认</button></div><p>更改后需要重新发送样本并确认。Linux 应选择 Trash/files 目录。</p></details>
        {:else if !allowCustomPath}<p>Windows 由系统 Shell 管理回收站，不支持改成普通目录。</p>{:else}<p>此隔离目录由服务启动配置指定。</p>{/if}
      </section>
      {/if}
      {#if needsArchive}
        <section class="destination-section"><div class="destination-heading"><h3><FolderOpen aria-hidden="true" />本地归档</h3><span class:verified={status.archive_confirmed}>{status.archive_confirmed ? '已确认' : '待确认'}</span></div><code class="destination-path">{status.archive_path || '尚未配置本地归档目录'}</code>{#if !status.archive_confirmed && status.archive_path}<button class="secondary-button" type="button" disabled={busy} on:click={() => dispatch('confirmarchive')}>确认使用这个归档目录</button>{/if}</section>
      {/if}
    {:else}<div class="destination-empty"><p>{busy ? '正在读取工作区和目的地…' : '暂时无法读取目的地。'}</p>{#if !busy}<button class="secondary-button" type="button" on:click={() => dispatch('refresh')}>重新检测</button>{/if}</div>{/if}
    {#if message}<p class="destination-message" role="status">{message}</p>{/if}
  </div>
  <footer><small>此检查仅移动新建的测试文件。不会归档或删除真实对话。</small><button class="secondary-button" type="button" on:click={() => open = false}>{status && (!needsTrash || status.trash_verified) && (!needsArchive || status.archive_confirmed) ? '完成检查' : '稍后继续'}</button></footer>
</dialog>

<style>
  .destination-dialog { width: min(560px, calc(100vw - 24px)); max-height: calc(100dvh - 32px); padding: 0; border: 1px solid var(--border-strong); border-radius: var(--radius-dialog); background: var(--bg-surface); color: var(--text-primary); box-shadow: var(--shadow); overflow: auto; }
  .destination-dialog::backdrop { background: var(--overlay-backdrop, #0008); }
  header, footer { display: flex; align-items: start; gap: var(--space-3); padding: var(--space-4); }
  header { border-bottom: 1px solid var(--border); }
  header > div { flex: 1; min-width: 0; }
  h2 { margin: 0; font-size: var(--font-heading); }
  h3 { display: flex; align-items: center; gap: var(--space-2); margin: 0; font-size: var(--font-ui); font-weight: 600; }
  p { margin: var(--space-2) 0; font-size: var(--font-caption); color: var(--text-secondary); line-height: 1.6; }
  .destination-dialog :global(svg) { width: 16px; height: 16px; flex-shrink: 0; }
  button { display: inline-flex; align-items: center; justify-content: center; gap: var(--space-2); cursor: pointer; }
  .destination-close { width: var(--control-md); height: var(--control-md); border: 0; border-radius: var(--radius-control); background: transparent; color: var(--text-secondary); }
  .destination-close:hover { background: var(--bg-hover); }
  .destination-body { display: grid; gap: var(--space-4); padding: var(--space-4); }
  .destination-section { display: grid; min-width: 0; gap: var(--space-2); }
  .destination-heading { display: flex; align-items: center; justify-content: space-between; gap: var(--space-2); }
  .destination-heading > span { flex-shrink: 0; font-size: var(--font-caption); color: var(--text-muted); }
  .destination-heading > span.verified { color: var(--success); }
  code { display: block; min-width: 0; font-size: var(--font-caption); line-height: 1.6; overflow-wrap: anywhere; white-space: pre-wrap; }
  .destination-path, .destination-sample { padding: var(--space-3); border: 1px solid var(--border); border-radius: var(--radius-control); background: var(--bg-subtle); }
  .destination-sample { display: grid; gap: var(--space-2); }
  .destination-sample strong { font-size: var(--font-caption); }
  .destination-actions { display: flex; flex-wrap: wrap; align-items: center; gap: var(--space-2); }
  .destination-text-button { border: 0; padding: var(--space-2); background: transparent; color: var(--text-secondary); font-size: var(--font-caption); }
  .destination-correction { min-width: 0; padding-top: var(--space-2); }
  summary, label { font-size: var(--font-caption); color: var(--text-secondary); }
  summary { cursor: pointer; }
  label { display: block; margin-top: var(--space-3); }
  input { width: 100%; min-width: 0; margin: var(--space-2) 0; padding: var(--space-2); border: 1px solid var(--border-strong); border-radius: var(--radius-control); background: var(--bg-subtle); color: var(--text-primary); font: inherit; font-size: var(--font-caption); }
  .destination-message { padding: var(--space-3); border-radius: var(--radius-control); background: var(--bg-hover); }
  footer { align-items: center; border-top: 1px solid var(--border); }
  footer small { flex: 1; min-width: 0; color: var(--text-muted); font-size: var(--font-caption); line-height: 1.5; }
  footer button { flex-shrink: 0; }
  button:focus-visible, input:focus-visible, summary:focus-visible { outline: 2px solid var(--accent); outline-offset: 3px; }
  @media (max-width: 420px) { footer { flex-wrap: wrap; } footer small { flex-basis: 100%; } }
</style>
