<!-- 中文说明：统一导航的线性 SVG 图标，文字标签承担可访问名称。 -->
<script lang="ts">
  import {
    Archive,
    ChartNoAxesColumn,
    Database,
    House,
    Inbox,
    LibraryBig,
    SlidersHorizontal
  } from '@lucide/svelte';

  export let name: string;
</script>

{#if name === 'home'}<House />
{:else if name === 'sessions'}<LibraryBig />
{:else if name === 'archives'}<Archive />
{:else if name === 'pending'}<Inbox />
{:else if name === 'analytics'}<ChartNoAxesColumn />
{:else if name === 'maintenance'}<Database />
{:else}<SlidersHorizontal />
{/if}
