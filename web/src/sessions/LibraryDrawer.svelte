<!-- 中文说明：会话库共用的原生侧滑面板，浏览器负责焦点约束、Escape 关闭和背景隔离。 -->
<script lang="ts">
  import { X } from '@lucide/svelte';
  export let open = false;
  export let title: string;
  export let description = '';
  let dialog: HTMLDialogElement;

  $: if (dialog) {
    if (open && !dialog.open) {
      dialog.showModal();
      dialog.querySelector<HTMLButtonElement>('.library-drawer-heading button')?.focus();
    }
    else if (!open && dialog.open) dialog.close();
  }
</script>

<dialog class="library-drawer-overlay" bind:this={dialog} aria-label={title} on:cancel={() => open = false} on:close={() => open = false}>
  <button type="button" class="library-drawer-dismiss-area" tabindex="-1" aria-label={`点击空白处关闭${title}`} on:click={() => open = false}></button>
  <section class="library-drawer">
    <header class="library-drawer-heading">
      <div><h2>{title}</h2>{#if description}<p>{description}</p>{/if}</div>
      <button type="button" class="library-icon-button" aria-label={`关闭${title}`} on:click={() => open = false}><X aria-hidden="true" /></button>
    </header>
    <div class="library-drawer-content"><slot /></div>
    {#if $$slots.footer}<footer class="library-drawer-footer"><slot name="footer" /></footer>{/if}
  </section>
  <button type="button" class="library-drawer-close" aria-label={`关闭${title}（大按钮）`} title={`关闭${title} · Esc`} on:click={() => open = false}><X aria-hidden="true" /></button>
</dialog>
