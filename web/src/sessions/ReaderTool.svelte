<!-- 中文说明：工具卡片只展示已记录的命令、结果与文件差异，展开时按需读取结果，不执行历史操作。 -->
<script lang="ts">
  import { createEventDispatcher } from 'svelte';
  import type { ReaderEvent } from './reader-state';
  export let event: ReaderEvent;
  export let open = false;
  export let active = false;
  export let busy = false;
  const dispatch = createEventDispatcher<{ toggle: { id: string; open: boolean }; full: ReaderEvent; evidence: ReaderEvent; share: ReaderEvent }>();
  const statusLabels: Record<string,string> = { completed: '已完成', failed: '失败', interrupted: '已中断', running: '进行中', unconfirmed: '未确认' };
  const kindLabels: Record<string,string> = { create: '新增', modify: '修改', delete: '删除', rename: '重命名' };
  let notice = ''; let loadedResult = false;
  $: title = event.tool?.title || ({ reasoning: '思考记录', context_compaction: '上下文压缩', injected_context: '运行上下文', plan: '计划', agent_task: '代理任务' } as Record<string,string>)[event.kind] || '系统记录';
  $: target = event.tool?.target || (event.changes?.length ? event.changes[0].path : event.tool ? event.tool_name || '' : event.text.slice(0,120));
  $: needsResult = !!event.tool?.result && !event.tool.output;
  async function copy(value: string) { try { await navigator.clipboard.writeText(value); notice = '已复制'; } catch { notice = '无法复制，请手动选择'; } }
  function toggle(isOpen: boolean) {
    dispatch('toggle', { id: event.id, open: isOpen });
    if (isOpen && needsResult && !loadedResult) { loadedResult = true; dispatch('full', event); }
  }
  function resultEvidence() {
    const ref = event.tool?.result; if (ref) dispatch('evidence', { ...event, relative_path: ref.path, start_offset: ref.start, end_offset: ref.end, line: ref.line });
  }
</script>

<details class="reader-tool" class:reader-active={active} data-reader-id={event.id} data-reader-path={event.relative_path} data-reader-start={event.start_offset} {open} on:toggle={(e) => toggle(e.currentTarget.open)}>
  <summary><strong>{title}</strong><span class="reader-tool-target" title={target}>{target}</span>{#if event.tool}<small class="reader-tool-status" class:reader-tool-failed={event.status === 'failed'} class:reader-tool-completed={event.status === 'completed'}>{statusLabels[event.status || 'unconfirmed'] || '未确认'}{#if event.exit_code !== undefined} · {event.exit_code}{/if}</small>{/if}</summary>
  {#if open}
    <div class="reader-tool-body">
      <div class="reader-message-actions"><button type="button" on:click={() => dispatch('share', event)}>复制定位链接</button><button type="button" on:click={() => dispatch('evidence', event)}>调用原文</button>{#if event.tool?.result}<button type="button" on:click={resultEvidence}>结果原文</button>{/if}<span role="status">{notice}</span></div>
      {#if event.tool?.cwd}<div class="reader-tool-cwd"><span>工作目录</span><code title={event.tool.cwd}>{event.tool.cwd}</code><button type="button" on:click={() => copy(event.tool?.cwd || '')}>复制</button></div>{/if}
      {#if event.changes?.length}<div class="reader-file-changes">{#each event.changes as change}<details><summary><small>{kindLabels[change.kind] || '修改'}</small><code title={change.path}>{change.path}{#if change.move_path} → {change.move_path}{/if}</code><span>+{change.added} −{change.removed}</span></summary>{#if change.diff}<pre class="reader-diff">{change.diff}</pre>{:else}<p class="reader-status">此记录没有保存差异正文。</p>{/if}{#if change.truncated}<p class="reader-status">差异预览已截断，可查看调用原文。</p>{/if}</details>{/each}</div>{/if}
      {#if event.tool?.output}<div class="reader-tool-section"><div><strong>输出</strong><button type="button" on:click={() => copy(event.tool?.output || '')}>复制输出</button></div><pre>{event.tool.output}</pre></div>{/if}
      {#if busy}<p class="reader-status" role="status">正在读取结果…</p>{:else if needsResult}<button type="button" on:click={() => dispatch('full', event)}>读取结果</button>{/if}
      {#if event.truncated || event.tool?.output_truncated}<button type="button" disabled={busy} on:click={() => dispatch('full', event)}>读取完整记录</button>{/if}
      {#if event.tool}<details class="reader-tool-arguments"><summary>参数与原始文本</summary><button type="button" on:click={() => copy(event.text)}>复制</button><pre>{event.text}</pre></details>{:else}<pre>{event.text}</pre>{/if}
    </div>
  {/if}
</details>
