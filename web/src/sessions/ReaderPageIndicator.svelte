<!-- 中文说明：双向补载共用图形提示，方向箭头、等待转圈和失败重试保持相同尺寸；文字仅用于悬停和无障碍说明。 -->
<script lang="ts">
  import { createEventDispatcher } from 'svelte';
  import { ChevronsDown, ChevronsUp, CircleAlert, LoaderCircle, RotateCcw } from '@lucide/svelte';
  import type { ReaderPageDirection } from './reader-state';
  export let direction: ReaderPageDirection;
  export let busy = false;
  export let failed = false;
  export let disabled = false;
  export let label: string;
  const dispatch = createEventDispatcher<{ retry: void }>();
</script>

{#if failed}
  <span class="reader-edge-error-icon" aria-hidden="true"><CircleAlert /></span>
  <button type="button" class="reader-edge-retry" {disabled} title={label} aria-label={label} on:click={() => dispatch('retry')}><RotateCcw aria-hidden="true" /></button>
{:else}
  <span class="reader-edge-symbol" aria-hidden="true">
    {#if busy}<LoaderCircle class="reader-edge-spinner" />{:else if direction === 'before'}<ChevronsUp />{:else}<ChevronsDown />{/if}
  </span>
{/if}
<span class="reader-edge-label">{label}</span>
