<!-- 中文说明：三档密度滑块保留原生拖动和键盘行为，保存由上层处理。 -->
<script lang="ts">
  import { createEventDispatcher } from 'svelte';
  type Density = 'compact' | 'comfortable' | 'reading';
  export let value: Density = 'comfortable';
  const options: { value: Density; label: string }[] = [
    { value: 'compact', label: '紧凑' },
    { value: 'comfortable', label: '舒适' },
    { value: 'reading', label: '阅读' }
  ];
  const dispatch = createEventDispatcher<{ change: Density }>();
  $: step = options.findIndex(option => option.value === value);
</script>

<div class="density-control" title="拖动或点击切换，也可使用方向键">
  <div class="density-slider" style={`--density-step: ${step}`}>
    <span class="density-thumb" aria-hidden="true"></span>
    <input class="density-range" type="range" min="0" max="2" step="1" value={step} aria-label="信息密度" aria-valuetext={options[step].label} on:input={(event) => dispatch('change', options[event.currentTarget.valueAsNumber].value)} />
    <div class="density-labels" aria-hidden="true">{#each options as option}<span class:active={value === option.value}>{option.label}</span>{/each}</div>
  </div>
</div>
