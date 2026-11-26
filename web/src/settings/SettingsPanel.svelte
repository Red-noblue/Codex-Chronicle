<!-- 中文说明：统一设置分组的标题、作用范围和内容容器，不替业务表单保存或确认。 -->
<script lang="ts">
  import HelpBubble from '../HelpBubble.svelte';
  import { settingsItems, type SettingsItemId } from './catalog';
  export let id: SettingsItemId;
  export let helpText = '';
  $: item = settingsItems[id];
</script>

<section class="settings-panel" data-setting={id} aria-labelledby={`setting-${id}`}>
  <header class="settings-panel-heading"><div><h4 id={`setting-${id}`}>{item.title}{#if helpText}<HelpBubble id={`setting-${id}-help`} label={`${item.title}说明`} text={helpText} />{/if}</h4><p>{item.description}</p></div><span class="settings-scope">{item.scope}</span></header>
  <div class="settings-panel-body"><slot /></div>
</section>

<style>
  h4 { display: flex; align-items: center; gap: 8px; }
  h4 :global(.help-bubble-panel) { font-weight: 400; }
</style>
