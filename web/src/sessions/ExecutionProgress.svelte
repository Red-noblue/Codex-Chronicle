<!-- 中文说明：显示服务端持久化的执行进度及恢复定位；关闭页面不会取消任务，停止按钮仅阻止后续项。 -->
<script lang="ts">
  import { createEventDispatcher } from 'svelte';
  import { executionResultLabel, executionStateLabels, type ExecutionTask } from './execution';
  export let task: ExecutionTask;
  export let message = '';
  const dispatch = createEventDispatcher<{ stop: void; refresh: void }>();
</script>

<section class="execution-progress" aria-label="执行任务">
  <div class="progress-heading"><strong>{task.state === 'completed' ? executionResultLabel(task) : executionStateLabels[task.state]}</strong><span>{task.completed} / {task.preview.items.length} 个对话</span><button class="secondary-button" type="button" title="进度会自动更新；点击可立即核对" on:click={() => dispatch('refresh')}>核对进度</button></div>
  <progress max={task.preview.items.length} value={task.completed} aria-label="已完成的对话数"></progress>
  {#if task.state === 'running'}<div class="progress-actions"><small>{task.restricted && !task.sequential ? '受限计划作为一组执行，当前事务不中断' : `第 ${task.current_batch || 1} / ${Math.ceil(task.preview.items.length / task.batch_size)} 批 · 每批最多 ${task.batch_size} 个`}</small>{#if !task.restricted || task.sequential || !task.current_item}<button class="secondary-button" type="button" disabled={task.stop_requested} on:click={() => dispatch('stop')}>{task.stop_requested ? '等待当前项结束' : '停止后续项'}</button>{/if}</div>{/if}
  {#if task.error}<p class="execution-error" role="alert">{task.error}</p>{/if}
  {#if message}<p role="status">{message}</p>{/if}
  <details>
    <summary>执行记录与恢复位置</summary>
    {#if task.packaged_trash}
      {#if task.trash_package_path}<p>目标文件、数据库行和索引记录已汇入一个回收文件夹。</p><details><summary>回收文件夹位置</summary><code>{task.trash_package_path}</code></details>{/if}
      <p>{task.recovery_purged ? '临时备份已清理，项目目录只留执行凭据。' : task.state === 'running' ? '处理中暂存恢复材料，成功后自动清理。' : '临时恢复材料仍保留，请先核对本次未完成的操作。'}</p>
    {/if}
    {#if task.recovery_path}
      {#if !task.packaged_trash}<p>{task.recovery_purged ? '删除已完成，临时恢复正文已清理。' : task.strategy.permanent_delete ? '临时恢复材料将在全部成功后清理；中断时保留供复核。' : '此历史任务仍保留旧版恢复备份，额外占用磁盘空间。'}</p>{/if}
      <details><summary>{task.recovery_purged ? '执行凭据位置' : '临时恢复材料位置'}</summary><code>{task.recovery_path}</code></details>
    {/if}
    {#if task.recovery_command}
      <details><summary>高级：命令行恢复</summary><p>在项目根目录运行。恢复会同时处理对话文件和数据库记录，并重新检查权限、活动和数据冲突。</p><code>{task.recovery_command}</code></details>
    {/if}
    <details><summary>任务标识与源目录</summary><code>{task.id}</code><code>{task.source_root}</code></details>
    <ul>{#each task.preview.items as item}<li><span>{item.completed ? '已完成' : item.result?.needs_review ? '需复核' : '未完成'} · {item.title || item.source_id || item.conversation_id}</span>{#if item.result?.journal_path}<details><summary>执行日志位置</summary><code>{item.result.journal_path}</code></details>{/if}</li>{/each}</ul>
  </details>
</section>

<style>
  .execution-progress { min-width: 0; margin: var(--space-3) 0; padding: var(--space-3); border: 1px solid var(--border-strong); border-radius: var(--radius-control); background: var(--bg-surface); font-size: var(--font-ui); }
  .progress-heading, .progress-actions { display: flex; align-items: center; flex-wrap: wrap; gap: var(--space-2); }
  .progress-heading strong { flex: 1; } .progress-heading span, small { color: var(--text-secondary); font-size: var(--font-caption); }
  progress { width: 100%; height: 6px; margin: var(--space-3) 0; accent-color: var(--accent); }
  .progress-actions { justify-content: space-between; } .execution-error { color: var(--danger); }
  p { margin: var(--space-2) 0; line-height: 1.5; overflow-wrap: anywhere; font-size: var(--font-caption); }
  details { margin-top: var(--space-3); font-size: var(--font-caption); } summary { cursor: pointer; color: var(--text-secondary); }
  code { display: block; margin: var(--space-2) 0; overflow-wrap: anywhere; white-space: pre-wrap; font-size: var(--font-caption); }
  ul { max-height: 200px; overflow: auto; margin: var(--space-2) 0 0; padding: 0; list-style: none; } li { padding: var(--space-2) 0; border-top: 1px solid var(--border); overflow-wrap: anywhere; }
</style>
