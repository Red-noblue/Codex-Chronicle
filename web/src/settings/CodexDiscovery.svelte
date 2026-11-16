<!-- 中文说明：展示服务设备的 Codex 目录识别结果；添加或切换只改变读取源，不授予对话写入权限。 -->
<script context="module" lang="ts">
  export type CodexHomeCandidate = {
    root: string;
    name: string;
    sources: string[];
    status: string;
    markers: string[];
    reason?: string;
    workspace_id?: string;
    current: boolean;
  };
</script>

<script lang="ts">
  import { createEventDispatcher } from 'svelte';
  import { RefreshCw } from '@lucide/svelte';
  export let candidates: CodexHomeCandidate[] = [];
  export let loading = false;
  export let busy = false;
  export let persisted: boolean | null = null;
  export let registryPath = '';
  export let serviceAddress = '';
  export let message = '';
  const dispatch = createEventDispatcher<{ refresh: void; select: CodexHomeCandidate }>();
  const labels: Record<string, string> = { available: '可读取', missing: '未找到', empty: '空目录', unavailable: '不可用', unrecognized: '待核对' };
</script>

<div class="codex-discovery">
  <div class="discovery-service"><span>当前服务</span><code>{serviceAddress}</code><span class:unavailable={persisted === false}>{persisted === true ? '工作区自动保存' : persisted === false ? '未启用工作区保存' : '正在读取配置…'}</span></div>
  {#if registryPath}<details class="discovery-registry"><summary>查看保存位置</summary><code>{registryPath}</code></details>{/if}
  {#if persisted === false}<p class="discovery-warning" role="status">此服务使用旧的单目录配置，暂时不能添加或切换工作区。请更新并重启服务，或启动时指定 --workspace-config。若打开的是旧实验端口，请切换到正式服务地址。</p>{/if}
  <div class="discovery-heading"><strong>自动识别 Codex 目录</strong><button class="secondary-button" type="button" on:click={() => dispatch('refresh')} disabled={loading || busy}><RefreshCw size={14} /><span>{loading ? '识别中…' : '重新识别'}</span></button></div>
  <p class="discovery-note">检查服务所在设备的 CODEX_HOME 和用户目录；远程访问时不是浏览器所在设备。</p>
  {#if message}<p class="discovery-warning" role="status">{message}</p>{/if}
  <div class="discovery-candidates" aria-busy={loading}>
    {#each candidates as candidate (candidate.root)}
      <div class="discovery-candidate" class:current={candidate.current}>
        <div class="discovery-candidate-heading"><strong>{candidate.name}</strong><span class="discovery-status" class:available={candidate.status === 'available'}>{candidate.current ? '正在读取' : labels[candidate.status] || candidate.status}</span></div>
        <code title={candidate.root}>{candidate.root}</code>
        <div class="discovery-candidate-bottom"><small>{candidate.sources.map(source => source === 'user_home' ? '用户主目录' : source).join(' · ')}{candidate.reason ? ` · ${candidate.reason}` : ''}</small><button class="secondary-button" type="button" on:click={() => dispatch('select', candidate)} disabled={busy || loading || persisted !== true || candidate.current || candidate.status !== 'available'}>{candidate.current ? '当前目录' : candidate.workspace_id ? '切换读取' : '添加并读取'}</button></div>
      </div>
    {/each}
  </div>
  {#if !loading && !message && candidates.length === 0}<p class="discovery-note">未发现默认目录，可在下方手动添加。</p>{/if}
</div>

<style>
  .codex-discovery { min-width: 0; display: grid; gap: var(--space-3); margin-bottom: var(--space-4); }
  .discovery-service { display: flex; align-items: baseline; flex-wrap: wrap; gap: var(--space-2); font-size: var(--font-caption); color: var(--text-secondary); }
  code { min-width: 0; font-family: var(--font-mono, ui-monospace, SFMono-Regular, Menlo, monospace); overflow-wrap: anywhere; font-size: var(--font-caption); }
  .discovery-service code { color: var(--text-primary); }
  .discovery-service .unavailable, .discovery-warning { color: var(--warning); }
  .discovery-registry { color: var(--text-muted); font-size: var(--font-caption); }
  .discovery-registry summary { cursor: pointer; }
  .discovery-registry code { display: block; margin-top: var(--space-2); }
  .discovery-warning { margin: 0; padding: var(--space-3); border: 1px solid var(--warning-border); border-radius: var(--radius-control); background: var(--warning-bg); font-size: var(--font-caption); line-height: 1.5; overflow-wrap: anywhere; }
  .discovery-heading, .discovery-candidate-heading, .discovery-candidate-bottom { display: flex; align-items: center; justify-content: space-between; flex-wrap: wrap; gap: var(--space-2); min-width: 0; }
  .discovery-heading strong, .discovery-candidate strong { font-size: var(--font-ui); overflow-wrap: anywhere; }
  .discovery-heading button { display: inline-flex; align-items: center; gap: var(--space-2); }
  .discovery-note { margin: 0; color: var(--text-muted); font-size: var(--font-caption); line-height: 1.5; }
  .discovery-candidates { display: grid; gap: var(--space-2); }
  .discovery-candidate { display: grid; gap: var(--space-2); padding: var(--space-3); border: 1px solid var(--border); border-radius: var(--radius-card); background: var(--bg-subtle); }
  .discovery-candidate.current { border-color: var(--border-strong); }
  .discovery-candidate code { color: var(--text-secondary); }
  .discovery-status { flex: 0 0 auto; color: var(--text-muted); font-size: var(--font-caption); }
  .discovery-status.available { color: var(--success); }
  .discovery-candidate-bottom small { flex: 1 1 160px; min-width: 0; color: var(--text-muted); font-size: var(--font-caption); overflow-wrap: anywhere; }
  .discovery-candidate-bottom button { flex: 0 0 auto; }
</style>
