<!-- 中文说明：用单个可访问按钮切换亮暗色，悬停提示说明下一步操作。 -->
<script lang="ts">
  import { Moon, Sun } from '@lucide/svelte';
  import { setTheme, type Theme } from './theme';

  let theme: Theme = document.documentElement.dataset.theme === 'dark' ? 'dark' : 'light';

  function chooseTheme(value: Theme) {
    theme = value;
    setTheme(value);
  }
</script>

<div class="theme-switcher">
  <button type="button" aria-label="深色主题" aria-pressed={theme === 'dark'} title={theme === 'dark' ? '切换为亮色' : '切换为暗色'} on:click={() => chooseTheme(theme === 'dark' ? 'light' : 'dark')}>
    {#if theme === 'dark'}<Sun aria-hidden="true" />{:else}<Moon aria-hidden="true" />{/if}
  </button>
</div>

