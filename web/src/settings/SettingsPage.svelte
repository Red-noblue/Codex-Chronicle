<!-- 中文说明：设置页只管理分类、检索和内容布局，业务表单通过插槽提供，草稿由上层持有。 -->
<script lang="ts">
  import { createEventDispatcher } from 'svelte';
  import { Search, X } from '@lucide/svelte';
  import { findSettings, settingsCategories, settingsItems, type SettingsCategory } from './catalog';

  const dispatch = createEventDispatcher<{ categorychange: SettingsCategory }>();
  export let category: SettingsCategory = 'appearance';
  let query = '';
  let content: HTMLElement;
  $: current = settingsCategories.find(item => item.id === category)!;
  $: visibleIDs = findSettings(query, category);

  function selectCategory(value: SettingsCategory) {
    category = value;
    query = '';
    content?.scrollTo({ top: 0 });
    dispatch('categorychange', value);
  }
</script>

<main class="settings-page" aria-label="设置">
  <header class="settings-page-header">
    <div><h2>设置</h2><p>外观偏好、数据位置与运行配置</p></div>
    <div class="settings-search">
      <Search aria-hidden="true" />
      <input bind:value={query} type="search" aria-label="搜索设置" placeholder="搜索设置，例如：回收站、密度" on:keydown={(event) => { if (event.key === 'Escape') query = ''; }} />
      {#if query}<button type="button" aria-label="清除设置搜索" title="清除设置搜索" on:click={() => query = ''}><X aria-hidden="true" /></button>{/if}
    </div>
  </header>
  <div class="settings-layout">
    <nav class="settings-navigation" aria-label="设置分类">
      {#each settingsCategories as item}
        <button type="button" class:active={!query.trim() && category === item.id} aria-current={!query.trim() && category === item.id ? 'page' : undefined} data-settings-category={item.id} on:click={() => selectCategory(item.id)}>
          <span>{item.label}</span>
          {#if query.trim()}<small>{visibleIDs.filter(id => settingsItems[id].category === item.id).length}</small>{/if}
        </button>
      {/each}
    </nav>
    <div class="settings-content" bind:this={content}>
      <div class="settings-content-inner">
        <header class="settings-category-heading">
          <h3>{query.trim() ? '搜索结果' : current.label}</h3>
          <p aria-live="polite">{query.trim() ? `找到 ${visibleIDs.length} 组设置` : current.description}</p>
        </header>
        {#if visibleIDs.length}
          <slot {visibleIDs} />
        {:else}
          <div class="settings-empty"><Search aria-hidden="true" /><strong>没有找到相关设置</strong><p>试试“工作区”“回收站”或“更新”。</p><button type="button" class="secondary-button" on:click={() => query = ''}>清除搜索</button></div>
        {/if}
      </div>
    </div>
  </div>
</main>
