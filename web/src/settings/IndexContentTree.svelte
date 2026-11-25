<!-- 中文说明：并排显示索引内容分支；正文时间范围使用独立的单选分支。 -->
<script lang="ts" module>
  export type IndexContentNode = { id: string; label: string; hint?: string; children?: IndexContentNode[] };

  export function indexContentLeaves(nodes: IndexContentNode[]): string[] {
    return nodes.flatMap(node => node.children?.length ? indexContentLeaves(node.children) : [node.id]);
  }
</script>

<script lang="ts">
  import { tick } from 'svelte';
  import { Check, ChevronRight, Clock3, Layers3, Minus } from '@lucide/svelte';

  export let nodes: IndexContentNode[];
  export let selected: Set<string>;
  export let disabled = false;
  export let onSelect: (ids: string[]) => void;
  export let recentContext: boolean;
  export let rangeApplicable: boolean;
  export let onRangeSelect: (recent: boolean) => void;

  type BranchGroup = { id: string; label: string; nodes: IndexContentNode[] };
  const rangeBranch: IndexContentNode = {
    id: 'dialogue-range',
    label: '正文时间范围',
    children: [
      { id: 'history-all', label: '全部历史', hint: '索引所选正文，不限制时段' },
      { id: 'history-recent', label: '最近约一个窗口', hint: '从会话结尾向前估算一个模型窗口，必要时跨过压缩点' },
    ],
  };
  const branchPalette = [255, 165, 310, 85, 215, 135, 285];
  let expanded = new Set(['dialogue']);
  let viewport: HTMLDivElement;
  let hoveredBranch: string | undefined;
  let focusedBranch: string | undefined;
  $: columns = buildColumns(nodes, expanded);
  $: branchHues = buildBranchHues(nodes);
  $: linkedBranch = hoveredBranch ?? focusedBranch;

  function displayedChildren(node: IndexContentNode) {
    const children = node.children ?? [];
    // 只在渲染的树中插入设置项，不参与内容计数或父级全选。
    return node.id === 'dialogue' ? [rangeBranch, ...children] : children;
  }

  function buildColumns(roots: IndexContentNode[], open: Set<string>) {
    let groups: BranchGroup[] = [{ id: 'root', label: '内容分类', nodes: roots }];
    const levels: BranchGroup[][] = [];
    while (groups.length) {
      levels.push(groups);
      groups = groups.flatMap(group => group.nodes.flatMap(node =>
        open.has(node.id) && node.children?.length
          ? [{ id: node.id, label: node.label, nodes: displayedChildren(node) }]
          : []
      ));
    }
    return levels;
  }

  function buildBranchHues(level: IndexContentNode[]) {
    // 以整层为准分配颜色，开关同层其他分支时标记保持稳定。
    const hues = new Map<string, number>();
    while (level.length) {
      const branches = level.filter(node => node.children?.length);
      branches.forEach((node, index) => hues.set(node.id, branchPalette[index % branchPalette.length]));
      level = branches.flatMap(displayedChildren);
    }
    return hues;
  }

  function branchAt(target: EventTarget | null) {
    return target instanceof Element ? target.closest<HTMLElement>('[data-index-branch]')?.dataset.indexBranch : undefined;
  }

  async function showBranch(node: IndexContentNode, depth: number, toggleExpanded = false) {
    if (expanded.has(node.id)) {
      if (toggleExpanded) {
        const next = new Set(expanded);
        next.delete(node.id);
        expanded = next;
      }
      return;
    }
    expanded = new Set([...expanded, node.id]);
    await tick();
    // 只横向滚动到新增的相邻列。
    const nextColumn = viewport?.querySelector(`[data-depth="${depth + 1}"]`);
    if (nextColumn) {
      const right = viewport.getBoundingClientRect().left + viewport.clientLeft + viewport.clientWidth;
      viewport.scrollLeft += Math.max(0, Math.ceil(nextColumn.getBoundingClientRect().right - right));
    }
  }

  function toggle(node: IndexContentNode, ids: string[], depth: number) {
    const allSelected = ids.every(id => selected.has(id));
    const next = new Set(selected);
    for (const id of ids) {
      if (allSelected) next.delete(id);
      else next.add(id);
    }
    onSelect([...next]);
    if (node.children?.length) void showBranch(node, depth);
  }
</script>

<div
  class="index-tree"
  bind:this={viewport}
  role="group"
  aria-label="内容分类树"
  on:pointerover={event => hoveredBranch = branchAt(event.target)}
  on:pointerleave={() => hoveredBranch = undefined}
  on:focusin={event => focusedBranch = branchAt(event.target)}
  on:focusout={event => focusedBranch = branchAt(event.relatedTarget)}
>
  {#each columns as column, depth (depth)}
    <section class="index-tree-column" class:range-column={column.some(group => group.id === 'dialogue')} data-depth={depth} aria-label={depth === 0 ? '内容分类' : `第 ${depth + 1} 层索引内容`}>
      {#each column as group (group.id)}
          {@const rangeChoices = group.id === rangeBranch.id}
          <div
            class="index-tree-group"
            class:child-group={depth > 0}
            class:linked={linkedBranch === group.id}
            data-index-branch={depth > 0 ? group.id : undefined}
            id={depth > 0 ? `index-tree-branch-${group.id}` : undefined}
            style:--index-tree-hue={branchHues.get(group.id)}
            role={rangeChoices ? 'radiogroup' : 'group'}
            aria-label={group.label}
          >
            {#if depth === 0}
              <strong class="index-tree-heading">{group.label}</strong>
            {:else}
              <span class="index-tree-guide" aria-hidden="true" title={group.label}></span>
            {/if}
            <ul>
              {#each group.nodes as node (node.id)}
                {@const isRange = node === rangeBranch}
                {@const rangeSelected = rangeChoices && recentContext === (node.id === 'history-recent')}
                {@const ids = isRange || rangeChoices ? [] : indexContentLeaves([node])}
                {@const count = ids.filter(id => selected.has(id)).length}
                {@const branch = Boolean(node.children?.length)}
                {@const opened = expanded.has(node.id)}
                <li class:opened class:marked={rangeSelected || count > 0} data-index-branch={opened ? node.id : undefined}>
                  {#if isRange}
                    <button type="button" class="index-tree-choice index-tree-range"
                      data-index-branch={node.id} aria-expanded={opened}
                      aria-controls={opened ? `index-tree-branch-${node.id}` : undefined}
                      title="只限制用户提问和助手消息"
                      on:click={() => void showBranch(node, depth, true)}>
                      <span class="index-tree-mark"><Clock3 size={13} aria-hidden="true" /></span>
                      <span class="index-tree-label">{node.label}</span>
                      <small>{recentContext ? '最近窗口' : '全部历史'}</small>
                      <span class="index-tree-expand" style:--index-tree-hue={branchHues.get(node.id)}>
                        <ChevronRight size={13} aria-hidden="true" />
                      </span>
                    </button>
                  {:else if rangeChoices}
                    <label class="index-tree-choice index-tree-radio" title={node.hint}>
                      <input type="radio" name="chronicle-dialogue-range" value={node.id}
                        disabled={disabled || !rangeApplicable} checked={rangeSelected}
                        on:change={() => onRangeSelect(node.id === 'history-recent')} />
                      <span class="index-tree-mark">{#if rangeSelected}<Check size={13} aria-hidden="true" />{/if}</span>
                      <span class="index-tree-label">{node.label}</span>
                    </label>
                  {:else}
                  <button
                    type="button"
                    class="index-tree-choice"
                    aria-pressed={count === ids.length ? true : count ? 'mixed' : false}
                    title={(depth > 0 ? group.label + ' / ' : '') + node.label + (node.hint ? ' · ' + node.hint : '')}
                    {disabled}
                    on:click={() => toggle(node, ids, depth)}
                  >
                    <span class="index-tree-mark">
                      {#if count === ids.length}<Check size={13} aria-hidden="true" />
                      {:else if count}<Minus size={13} aria-hidden="true" />
                      {:else if branch}<Layers3 size={13} aria-hidden="true" />{/if}
                    </span>
                    <span class="index-tree-label">{node.label}</span>
                    {#if branch}<small aria-label={'已选 ' + count + ' 项，共 ' + ids.length + ' 项'}>{count}/{ids.length}</small>{/if}
                  </button>
                  {#if branch}
                    <button
                      type="button"
                      class="index-tree-expand"
                      class:linked={opened && linkedBranch === node.id}
                      data-index-branch={node.id}
                      style:--index-tree-hue={branchHues.get(node.id)}
                      aria-expanded={opened}
                      aria-controls={opened ? `index-tree-branch-${node.id}` : undefined}
                      aria-label={(opened ? '折叠' : '展开') + node.label}
                      on:click={() => void showBranch(node, depth, true)}
                    ><ChevronRight size={13} aria-hidden="true" /></button>
                  {/if}
                  {/if}
                </li>
              {/each}
            </ul>
            {#if rangeChoices && !rangeApplicable}<p class="index-tree-note">未选用户提问或助手消息</p>{/if}
          </div>
      {/each}
    </section>
  {/each}
</div>

<style>
  .index-tree {
    --index-tree-row: 1.625rem;
    display: flex;
    min-width: 0;
    overflow-x: auto;
    scrollbar-width: thin;
    border: 1px solid var(--border);
    border-radius: var(--radius-control);
    background: var(--bg-surface);
  }
  .index-tree-column {
    display: flex;
    flex: 1 0 10.75rem;
    flex-direction: column;
    max-width: 17rem;
    min-width: 0;
    padding: var(--space-1);
  }
  .index-tree-column.range-column { flex-basis: 12.75rem; }
  .index-tree-column + .index-tree-column { border-left: 1px solid var(--border); }
  .index-tree-heading { display: block; height: var(--index-tree-row); padding-inline: var(--space-2); overflow: hidden; color: var(--text-secondary); font-size: var(--font-caption); font-weight: 600; line-height: calc(var(--index-tree-row) - 1px); text-overflow: ellipsis; white-space: nowrap; }
  .child-group, .index-tree-expand {
    --index-tree-guide-color: color-mix(in oklab, var(--text-primary) 22%, oklch(63% .14 var(--index-tree-hue, 255)));
  }
  .child-group { position: relative; padding-inline-start: .75rem; }
  .index-tree-guide {
    position: absolute;
    inset-block: var(--space-1);
    inset-inline-start: 1px;
    width: .4375rem;
    border-block: 1px solid;
    border-inline-start: 1px solid;
    border-start-start-radius: 3px;
    border-end-start-radius: 3px;
    color: var(--index-tree-guide-color, var(--accent));
  }
  .child-group.linked .index-tree-guide { border-width: 2px; }
  ul { margin: 0; padding: 0; list-style: none; }
  li { display: flex; align-items: stretch; height: var(--index-tree-row); border: 1px solid transparent; border-radius: var(--radius-control); }
  li.marked { background: var(--bg-selected); }
  li.opened { border-color: var(--border-strong); }
  li :is(button, .index-tree-radio) { min-width: 0; min-height: 0; padding: 0 var(--space-1); border: 0; border-radius: var(--radius-control); background: transparent; color: var(--text-primary); font: inherit; cursor: pointer; }
  li :is(button:disabled, .index-tree-radio:has(input:disabled)) { cursor: default; opacity: .7; }
  li :is(button:not(:disabled), .index-tree-radio:has(input:enabled)):hover { background: var(--bg-hover); }
  li button:focus-visible, .index-tree-radio:has(input:focus-visible) { outline-offset: -2px; }
  .index-tree-radio { position: relative; }
  .index-tree-radio input { position: absolute; width: 1px; height: 1px; margin: 0; opacity: 0; }
  .index-tree-radio:has(input:focus-visible) { outline: 2px solid var(--accent); }
  .index-tree-note { margin: var(--space-1); color: var(--text-muted); font-size: var(--font-caption); }
  .index-tree-choice { flex: 1; display: flex; align-items: center; gap: var(--space-1); text-align: left; }
  .index-tree-label { min-width: 0; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
  .index-tree-choice small { margin-left: auto; color: var(--text-muted); font-size: var(--font-caption); font-variant-numeric: tabular-nums; white-space: nowrap; }
  .index-tree-mark { display: grid; flex: 0 0 13px; place-items: center; color: var(--text-muted); }
  .marked .index-tree-mark, .opened .index-tree-expand { color: var(--accent); }
  .index-tree-expand { position: relative; display: grid; flex: 0 0 var(--index-tree-row); place-items: center; padding: 0; color: var(--text-muted); }
  .index-tree-range .index-tree-expand { align-self: stretch; margin-inline-end: calc(-1 * var(--space-1)); }
  .opened .index-tree-expand { box-shadow: inset 1px 0 var(--border); }
  .opened .index-tree-expand :global(svg) { transform: rotate(90deg); }
  .opened .index-tree-expand::after {
    position: absolute;
    inset-block: 25%;
    inset-inline-end: 1px;
    width: 2px;
    border-radius: 2px;
    background: var(--index-tree-guide-color, var(--accent));
    content: '';
  }
  .index-tree-expand.linked { background: var(--bg-hover); }
  .index-tree-expand.linked::after { width: 3px; }
  @media (pointer: coarse) { .index-tree { --index-tree-row: var(--control-touch); } }
</style>
