<!-- 中文说明：用于术语旁的圆形帮助按钮，支持悬停、键盘和触屏。 -->
<script lang="ts">
  import { afterUpdate, onDestroy, tick } from 'svelte';

  export let id: string;
  export let label: string;
  export let text: string;
  export let onOpen: (() => void) | undefined = undefined;

  type OpenMethod = 'hover' | 'focus' | 'press';
  let method: OpenMethod | null = null;
  let trigger: HTMLButtonElement;
  let panel: HTMLSpanElement;
  let timer: ReturnType<typeof setTimeout> | undefined;

  function place() {
    const anchor = trigger.getBoundingClientRect();
    const viewport = document.documentElement;
    const gap = 6;
    const inset = 10;
    const left = Math.min(
      Math.max(anchor.left - inset, inset),
      viewport.clientWidth - panel.offsetWidth - inset
    );
    const preferredTop = anchor.bottom + gap + panel.offsetHeight <= viewport.clientHeight - inset
      ? anchor.bottom + gap
      : anchor.top - gap - panel.offsetHeight;
    panel.style.left = Math.max(inset, Math.round(left)) + 'px';
    panel.style.top = Math.max(inset, Math.round(preferredTop)) + 'px';
  }

  async function show(next: OpenMethod) {
    clearTimeout(timer);
    if (method) {
      method = next;
      return;
    }
    method = next;
    await tick();
    if (!method || !panel?.isConnected) return;
    panel.showPopover?.();
    place();
    onOpen?.();
  }

  function close() {
    clearTimeout(timer);
    if (!method) return;
    method = null;
    panel?.hidePopover?.();
  }

  function hover(event: PointerEvent) {
    if (event.pointerType === 'touch') return;
    clearTimeout(timer);
    if (!method) timer = setTimeout(() => void show('hover'), 170);
  }

  function leave() {
    clearTimeout(timer);
    // 留出从图标跨到浮层的时间，经过间隙时不会闪退。
    if (method === 'hover') timer = setTimeout(close, 180);
  }

  function focus() {
    if (trigger.matches(':focus-visible') && method !== 'press') void show('focus');
  }

  function dismissOutside(event: PointerEvent) {
    if (!method || !(event.target instanceof Node)) return;
    if (!trigger.contains(event.target) && !panel?.contains(event.target)) close();
  }

  function escape(event: KeyboardEvent) {
    if (event.key !== 'Escape') return;
    clearTimeout(timer);
    if (!method) return;
    event.preventDefault();
    event.stopPropagation();
    close();
  }

  function dismissOnScroll(event: Event) {
    if (method && event.target !== panel) close();
  }

  afterUpdate(() => {
    if (method && panel?.offsetHeight) place();
  });
  onDestroy(() => clearTimeout(timer));
</script>

<svelte:window on:pointerdown|capture={dismissOutside} on:keydown|capture={escape} on:resize={close} on:scroll|capture={dismissOnScroll} />
<button
  bind:this={trigger}
  type="button"
  class="help-bubble-trigger"
  aria-label={label}
  aria-describedby={id}
  aria-expanded={method !== null}
  on:pointerenter={hover}
  on:pointerleave={leave}
  on:focus={focus}
  on:blur={() => { if (method === 'focus' || method === 'press') close(); }}
  on:click={(event) => {
    if (event instanceof PointerEvent && event.pointerType === 'mouse') {
      void show('hover');
    } else if (method === 'press') {
      close();
    } else {
      void show('press');
    }
  }}
><span aria-hidden="true">!</span></button>
<span
  bind:this={panel}
  {id}
  class="help-bubble-panel"
  role="tooltip"
  popover="manual"
  hidden={!method}
  on:pointerenter={() => clearTimeout(timer)}
  on:pointerleave={leave}
>{text}</span>

<style>
  .help-bubble-trigger {
    display: inline-grid;
    flex: 0 0 auto;
    place-items: center;
    width: 1.625rem;
    height: 1.625rem;
    min-height: 1.625rem;
    padding: 0;
    border: 0;
    border-radius: 50%;
    background: transparent;
    color: var(--text-secondary);
    cursor: help;
  }
  .help-bubble-trigger > span {
    display: grid;
    width: 1rem;
    height: 1rem;
    place-items: center;
    border: 1px solid var(--border-strong);
    border-radius: 50%;
    background: var(--bg-subtle);
    font-family: inherit;
    font-size: .6875rem;
    font-weight: 700;
    line-height: 1;
    transition: color 130ms ease, background-color 130ms ease, border-color 130ms ease;
  }
  .help-bubble-trigger:hover > span, .help-bubble-trigger[aria-expanded="true"] > span {
    border-color: var(--accent);
    background: var(--bg-selected);
    color: var(--accent);
  }
  .help-bubble-trigger:focus-visible { outline: 2px solid var(--accent); outline-offset: 1px; }
  .help-bubble-panel {
    position: fixed;
    z-index: 15;
    inset: auto;
    width: min(19rem, calc(100vw - 20px));
    max-height: calc(100vh - 20px);
    margin: 0;
    padding: 10px 12px;
    overflow: auto;
    border: 1px solid var(--border-strong);
    border-top-color: var(--accent);
    border-radius: var(--radius-control);
    background: var(--bg-surface);
    color: var(--text-primary);
    box-shadow: var(--overlay-shadow);
    font-size: var(--font-ui);
    line-height: 1.65;
    white-space: pre-line;
    overflow-wrap: anywhere;
    animation: help-reveal 135ms ease-out;
  }
  .help-bubble-panel[hidden] { display: none; }
  @keyframes help-reveal { from { opacity: 0; } to { opacity: 1; } }
  @media (pointer: coarse) {
    .help-bubble-trigger { width: var(--control-touch); height: var(--control-touch); }
  }
  @media (prefers-reduced-motion: reduce) {
    .help-bubble-trigger > span { transition: none; }
    .help-bubble-panel { animation: none; }
  }
</style>
