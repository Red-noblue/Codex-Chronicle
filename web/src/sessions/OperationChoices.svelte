<!-- 中文说明：单条和批量审核共用执行方式表单，选项约束、预览和执行仍由上层处理。 -->
<script lang="ts">
  import { createEventDispatcher } from 'svelte';
  export let options: { archive: boolean; archive_local?: boolean; archive_remote?: boolean; encrypted: boolean; trash: boolean; permanent_delete: boolean };
  export let trashOnly = false;
  export let realConversations = false;
  export let archivePassword = '';
  export let summary: string;
  export let writesEnabled = false;
  export let sourceReadOnly = false;
  const dispatch = createEventDispatcher<{ optionchange: { option: keyof typeof options; checked: boolean }; invalidate: void }>();
</script>

<section class="operation-picker library-operation-choices" aria-label="审核方式">
  <h3 class="subheading">处理方式</h3>
  <div class="operation-options">
    <label class:disabled={trashOnly}><input type="checkbox" disabled={trashOnly} checked={options.archive} on:change={(event) => dispatch('optionchange', { option: 'archive', checked: event.currentTarget.checked })} />归档</label>
    <label class:disabled={trashOnly || !options.archive}><input type="checkbox" checked={options.encrypted} disabled={trashOnly || !options.archive} on:change={(event) => dispatch('optionchange', { option: 'encrypted', checked: event.currentTarget.checked })} />客户端加密</label>
    <label class:disabled={sourceReadOnly}><input type="checkbox" disabled={sourceReadOnly} checked={options.trash} on:change={(event) => dispatch('optionchange', { option: 'trash', checked: event.currentTarget.checked })} />移入回收站</label>
    <label class="danger-option"><input type="checkbox" disabled={trashOnly || sourceReadOnly} checked={options.permanent_delete} on:change={(event) => dispatch('optionchange', { option: 'permanent_delete', checked: event.currentTarget.checked })} />删除</label>
  </div>
  {#if options.archive}
    <fieldset class="archive-locations"><legend>归档保存位置</legend>
      <label><input type="checkbox" checked={Boolean(options.archive_local)} on:change={event => dispatch('optionchange', { option: 'archive_local', checked: event.currentTarget.checked })} />本地归档</label>
      <label><input type="checkbox" checked={Boolean(options.archive_remote)} on:change={event => dispatch('optionchange', { option: 'archive_remote', checked: event.currentTarget.checked })} />云端归档</label>
      {#if !options.archive_local && !options.archive_remote}<span class="archive-location-warning" role="status">归档至少选择一个保存位置</span>{:else}<span>可同时保存到多个位置。</span>{/if}
    </fieldset>
    <p class="evidence-note">{options.trash || options.permanent_delete ? '所选位置全部保存并校验成功后，再移除 .codex 中的源版本。' : '保留 .codex 中的源版本。'}</p>
  {/if}
  <div class="operation-summary">当前方式：<strong>{summary}</strong></div>
  {#if options.encrypted}
    <label class="operation-field"><span>客户端加密密码</span><input type="password" bind:value={archivePassword} autocomplete="new-password" spellcheck="false" placeholder="执行时使用" aria-label="客户端加密密码" /></label>
  {/if}
  {#if trashOnly}<p class="evidence-note">仅支持已授权会话移入废纸篓。</p>{/if}
  {#if realConversations}<details class="operation-scope"><summary>支持范围</summary><p class="evidence-note">归档包含正文、目标数据库行和索引记录。正在使用或关联检查未通过的会话不会处理。</p></details>{/if}
  {#if options.permanent_delete}<p class="evidence-note">删除 .codex 中的源文件和专属记录，跳过回收站。{options.archive ? '所选归档副本保留。' : '未选择归档，完成后无法撤销。'}</p>{/if}
  {#if sourceReadOnly}<p class="evidence-note">已开启只读保护，无法删除原始记录。可在“数据格式兼容性”中调整。</p>{/if}
  {#if !writesEnabled}<p class="evidence-note">当前服务提供只读影响预览，实际执行受服务端权限限制。</p>{/if}
</section>

<style>
  .operation-scope { margin-top: var(--space-2); color: var(--text-muted); font-size: var(--font-caption); line-height: 1.6; }
  summary { cursor: pointer; }
  summary:focus-visible { outline: 2px solid var(--accent); outline-offset: 2px; }
  .archive-locations { display: flex; flex-wrap: wrap; align-items: center; gap: var(--space-3); margin: var(--space-3) 0; padding: var(--space-2) var(--space-3); border: 1px solid var(--border); border-radius: var(--radius-control); font-size: var(--font-ui); }
  legend { color: var(--text-secondary); font-size: var(--font-caption); padding-inline: 4px; }
  .archive-locations label { display: inline-flex; align-items: center; gap: 6px; }
  .archive-locations span { color: var(--text-muted); font-size: var(--font-caption); }
  .archive-locations .archive-location-warning { color: var(--warning); }
</style>
