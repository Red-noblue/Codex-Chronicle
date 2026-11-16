<!-- 中文说明：连接向导优先展示账户操作，将开发者配置和传输参数收进高级设置；令牌不进入表单。 -->
<script lang="ts">
  import { createEventDispatcher, onMount } from 'svelte';
  import { Check, CircleAlert, LoaderCircle, RefreshCw } from '@lucide/svelte';

  type ConnectionSettings = {
    client_id: string; oauth_base_url: string; api_base_url: string; proxy_url: string;
    archive_root: string; branch: string; chunk_mib: number; timeout_seconds: number;
  };
  type SettingsResponse = {
    settings?: ConnectionSettings; editable: boolean; persisted?: boolean;
    source_archive_supported?: boolean; source_backup_supported?: boolean; recovery_root?: string;
  };
  type ConnectionCheck = { label: string; ok: boolean; elapsed_ms: number; error?: string };

  export let request: typeof fetch;
  const dispatch = createEventDispatcher<{ saved: void }>();
  let settings: ConnectionSettings = {
    client_id: '', oauth_base_url: 'https://github.com', api_base_url: 'https://api.github.com',
    proxy_url: '', archive_root: 'codex-chronicle', branch: '', chunk_mib: 8, timeout_seconds: 120
  };
  let saved = '';
  let status: SettingsResponse | null = null;
  let loading = true;
  let saving = false;
  let checking = false;
  let message = '';
  let failed = false;
  let checks: ConnectionCheck[] = [];
  $: dirty = JSON.stringify(settings) !== saved;
  $: busy = loading || saving || checking;

  async function readResponse(response: Response) {
    const body = await response.json();
    if (!response.ok) throw new Error(body.error || `请求失败（HTTP ${response.status}）`);
    return body;
  }

  async function load() {
    loading = true;
    message = '';
    try {
      status = await readResponse(await request('/api/v1/github/settings')) as SettingsResponse;
      if (status.settings) settings = { ...status.settings };
      saved = JSON.stringify(settings);
      failed = false;
    } catch (error) {
      failed = true;
      message = error instanceof Error ? error.message : '无法读取 GitHub 设置';
    } finally { loading = false; }
  }

  async function save() {
    saving = true;
    message = '';
    checks = [];
    try {
      status = await readResponse(await request('/api/v1/github/settings', {
        method: 'PUT', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(settings)
      })) as SettingsResponse;
      if (status.settings) settings = { ...status.settings };
      saved = JSON.stringify(settings);
      failed = false;
      message = '已保存并生效';
      dispatch('saved');
    } catch (error) {
      failed = true;
      message = error instanceof Error ? error.message : '保存失败';
    } finally { saving = false; }
  }

  async function testConnection() {
    checking = true;
    checks = [];
    message = '';
    try {
      const result = await readResponse(await request('/api/v1/github/connection-test', { method: 'POST' })) as { checks: ConnectionCheck[] };
      checks = result.checks;
    } catch (error) {
      failed = true;
      message = error instanceof Error ? error.message : '连接检查失败';
    } finally { checking = false; }
  }

  onMount(load);
</script>

<div class="github-connection">
  {#if loading}
    <div class="connection-feedback" role="status"><LoaderCircle class="connection-spinner" size={15} />正在读取连接设置…</div>
  {:else if !status}
    <div class="connection-feedback failed" role="alert"><CircleAlert size={15} />{message}<button class="secondary-button" on:click={load}>重试</button></div>
  {:else if !status.editable}
    <p class="connection-feedback">当前服务使用其他远端存储，GitHub 连接不可编辑。</p>
  {:else}
    <slot />
    <details class="connection-advanced">
      <summary>高级设置 · 代理、登录应用与传输</summary>
      <form on:submit|preventDefault={save}>
      <div class="connection-fields">
        <label>公开 Client ID<span class="connection-optional">维护者或自建应用</span><input bind:value={settings.client_id} autocomplete="off" spellcheck="false" maxlength="200" placeholder="发行包可内置，无需普通用户注册应用" disabled={busy} /></label>
        <label>代理地址<span class="connection-optional">可选</span><input bind:value={settings.proxy_url} autocomplete="off" spellcheck="false" placeholder="http://127.0.0.1:端口 或 socks5://…" disabled={busy} /></label>
      </div>
        <div class="connection-fields">
          <label>授权站点<input bind:value={settings.oauth_base_url} type="url" required disabled={busy} /></label>
          <label>API 地址<input bind:value={settings.api_base_url} type="url" required disabled={busy} /></label>
          <label>仓库内归档目录<input bind:value={settings.archive_root} required disabled={busy} /></label>
          <label>分支<span class="connection-optional">留空使用默认分支</span><input bind:value={settings.branch} placeholder="默认分支" disabled={busy} /></label>
          <label>分片大小（MiB）<input bind:value={settings.chunk_mib} type="number" min="1" max="50" required disabled={busy} /></label>
          <label>单次请求超时（秒）<input bind:value={settings.timeout_seconds} type="number" min="5" max="300" required disabled={busy} /></label>
        </div>
      <div class="connection-actions">
        <button type="submit" class="secondary-button" disabled={busy || (!dirty && status.persisted)}>{saving ? '保存中…' : '保存连接'}</button>
        <button type="button" class="secondary-button" on:click={testConnection} disabled={busy || dirty}><RefreshCw size={14} class={checking ? 'connection-spinner' : ''} />{checking ? '检查中…' : '检查连接'}</button>
        {#if dirty}<span class="connection-optional">有未保存更改</span>{:else if status.persisted}<span class="connection-saved"><Check size={14} />已保存</span>{/if}
      </div>
      </form>
    {#if message}<div class="connection-feedback" class:failed role={failed ? 'alert' : 'status'}>{message}</div>{/if}
    {#if checks.length}
      <ul class="connection-checks" aria-label="连接检查结果">
        {#each checks as item}<li class:failed={!item.ok}>{#if item.ok}<Check size={15} />{:else}<CircleAlert size={15} />{/if}<span>{item.label}</span><small>{item.ok ? `${item.elapsed_ms} ms` : item.error}</small></li>{/each}
      </ul>
    {/if}
      <p class="connection-limitation">普通用户使用发行包内置的登录应用即可。自建应用需启用 Device Flow；这里只填写公开 Client ID。访问令牌由系统凭证库保存。</p>
      <p class="connection-limitation">检查连接仅验证网络可达性。更换登录应用或服务地址后需重新连接账户。</p>
    </details>
    {#if status.source_backup_supported}<p class="connection-limitation">在会话库选择会话后使用“云端备份”。支持压缩、分片续传与隔离下载，保留本地源。</p>{:else if !status.source_archive_supported}<p class="connection-limitation">当前服务尚未配置完整会话快照；可管理已有远端归档。</p>{/if}
    {#if status.recovery_root}<details class="connection-help"><summary>归档下载位置</summary><code>{status.recovery_root}</code></details>{/if}
  {/if}
</div>

<style>
  .github-connection, form { display: grid; gap: var(--space-3); min-width: 0; }
  .github-connection { margin-top: var(--space-3); }
  .connection-fields { display: grid; grid-template-columns: repeat(2, minmax(0, 1fr)); gap: var(--space-3); }
  label { min-width: 0; display: flex; flex-wrap: wrap; align-items: baseline; gap: var(--space-1); color: var(--text-secondary); font-size: var(--font-caption); }
  input { flex: 1 0 100%; width: 100%; min-width: 0; min-height: var(--control-md); padding: var(--space-1) var(--space-2); border: 1px solid var(--border-strong); border-radius: var(--radius-control); background: var(--bg-subtle); color: var(--text-primary); font: inherit; }
  input:focus-visible { outline: 2px solid var(--accent); outline-offset: 1px; }
  .connection-optional, .connection-limitation { color: var(--text-muted); font-size: var(--font-caption); }
  .connection-actions, .connection-feedback, .connection-saved { display: flex; align-items: center; flex-wrap: wrap; gap: var(--space-2); font-size: var(--font-caption); }
  .connection-actions button { display: inline-flex; align-items: center; gap: var(--space-1); }
  .connection-saved { color: var(--success); }
  .connection-feedback { color: var(--text-secondary); }
  .connection-advanced, .connection-help { min-width: 0; color: var(--text-muted); font-size: var(--font-caption); }
  summary { cursor: pointer; padding-block: var(--space-1); }
  .connection-advanced[open] summary { margin-bottom: var(--space-2); }
  .connection-limitation { margin: 0; line-height: 1.6; }
  .connection-help code { display: block; overflow-wrap: anywhere; }
  .connection-checks { margin: 0; padding: var(--space-2); list-style: none; background: var(--bg-subtle); border-radius: var(--radius-control); }
  .connection-checks li { display: flex; align-items: center; gap: var(--space-2); min-width: 0; padding: var(--space-1); color: var(--success); font-size: var(--font-caption); }
  .connection-checks small { margin-left: auto; color: var(--text-muted); overflow-wrap: anywhere; }
  .connection-checks :global(svg) { flex-shrink: 0; }
  .connection-feedback.failed, .connection-checks li.failed { color: var(--danger); }
  :global(.connection-spinner) { animation: connection-spin 1s linear infinite; }
  @keyframes connection-spin { to { transform: rotate(360deg); } }
  @media (max-width: 620px) { .connection-fields { grid-template-columns: minmax(0, 1fr); } }
  @media (prefers-reduced-motion: reduce) { :global(.connection-spinner) { animation: none; } }
</style>
