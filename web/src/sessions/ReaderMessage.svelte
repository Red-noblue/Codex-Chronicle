<!-- 中文说明：消息默认展示两行折叠预览，展开时角色与操作共用一行；富文本、复制和命中标记不改写源数据。 -->
<script lang="ts">
  import { createEventDispatcher, tick } from 'svelte';
  import { Check, Copy, FileText, Link } from '@lucide/svelte';
  import type { ReaderEvent } from './reader-state';
  import { renderReaderMarkdown, highlightReaderCode } from './reader-markdown';
  export let event: ReaderEvent;
  export let open = false;
  export let query = '';
  export let active = false;
  const dispatch = createEventDispatcher<{ toggle: { id: string; open: boolean }; full: ReaderEvent; evidence: ReaderEvent; share: ReaderEvent }>();
  let body: HTMLDivElement; let notice = ''; let generation = 0;
  $: html = open ? renderReaderMarkdown(event.text) : '';
  $: if (html && open) void decorate(++generation, html, query);
  async function decorate(token: number, _html: string, needle: string) {
    await tick(); if (!body || token !== generation) return;
    if (needle.trim()) {
      const walker = document.createTreeWalker(body, NodeFilter.SHOW_TEXT); const nodes: Text[] = []; let node: Node | null;
      while ((node = walker.nextNode())) nodes.push(node as Text);
      for (const text of nodes) {
        if (text.parentElement?.closest('mark,script,style,button')) continue;
        const value = text.data; const lower = value.toLocaleLowerCase(); const find = needle.trim().toLocaleLowerCase(); let start = 0; let at = lower.indexOf(find);
        if (at < 0) continue;
        const fragment = document.createDocumentFragment(); let count = 0;
        while (at >= 0 && count++ < 200) { fragment.append(value.slice(start, at)); const mark = document.createElement('mark'); mark.textContent = value.slice(at, at + find.length); fragment.append(mark); start = at + find.length; at = lower.indexOf(find, start); }
        fragment.append(value.slice(start)); text.replaceWith(fragment);
      }
    } else await highlightReaderCode(body, () => token !== generation || !open);
  }
  async function copy(text: string) { try { await navigator.clipboard.writeText(text); notice = '已复制'; } catch { notice = '复制失败，请手动选择文字'; } }
  function codeActions(event: MouseEvent) {
    const button = (event.target as Element)?.closest<HTMLButtonElement>('[data-copy-code]');
    if (button) void copy(button.parentElement?.querySelector('code')?.textContent || '');
  }
  function installCodeButtons(node: HTMLDivElement, _html: string) {
    function update() { for (const pre of Array.from(node.querySelectorAll('pre'))) { if (pre.querySelector('button')) continue; const button = document.createElement('button'); button.type = 'button'; button.textContent = '复制代码'; button.dataset.copyCode = ''; pre.prepend(button); } }
    update(); return { update: (_html: string) => update() };
  }
</script>

<details class="reader-message" class:reader-user={event.role === 'user'} class:reader-active={active} data-reader-id={event.id} data-reader-path={event.relative_path} data-reader-start={event.start_offset} {open}
  on:toggle={(e) => dispatch('toggle', { id: event.id, open: e.currentTarget.open })}>
  <summary><strong>{event.kind === 'agent_task' ? '代理任务' : event.role === 'user' ? '用户' : event.role === 'assistant' ? '助手' : event.tool_name || '工具与系统'}</strong>
    <span class="reader-message-preview">{event.text.trim().slice(0, 600)}</span>
    {#if open}
      <span class="reader-message-actions reader-message-inline-actions">
        <button type="button" aria-label="复制正文" title={notice || '复制本条消息的正文'} on:click|preventDefault|stopPropagation={() => copy(event.text)}>{#if notice === '已复制'}<Check aria-hidden="true" />{:else}<Copy aria-hidden="true" />{/if}<span>复制正文</span></button>
        <button type="button" aria-label="复制定位链接" title="复制链接，重新打开可定位到本条消息" on:click|preventDefault|stopPropagation={() => dispatch('share', event)}><Link aria-hidden="true" /><span>复制定位链接</span></button>
        <button type="button" aria-label="原始证据" title="在技术证据中查看本条消息的原始记录" on:click|preventDefault|stopPropagation={() => dispatch('evidence', event)}><FileText aria-hidden="true" /><span>原始证据</span></button>
        <span class="reader-message-copy-status" role="status">{notice}</span>
      </span>
    {/if}
    {#if event.timestamp && !event.timestamp.startsWith('0001-')}<time datetime={event.timestamp}>{new Date(event.timestamp).toLocaleTimeString('zh-CN', { hour: '2-digit', minute: '2-digit' })}</time>{/if}
  </summary>
  {#if open}
    {#key query}<div class="reader-prose" bind:this={body} use:installCodeButtons={html} on:click={codeActions} role="presentation">{@html html}</div>{/key}
    {#if event.truncated}<button class="reader-full-button" type="button" on:click={() => dispatch('full', event)}>读取完整消息</button>{/if}
  {/if}
</details>
