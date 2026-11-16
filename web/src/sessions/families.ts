// 中文说明：将已载入会话投影为可折叠的父子列表；分页缺少父节点时仍保留子代理。
export type FamilyConversation = {
  id: string;
  parent_conversation_id?: string;
  child_conversation_ids?: string[];
  family_conflict?: boolean;
};

// 预览先展示用户选择的对象，执行顺序仍由服务端的子代理优先计划决定。
export function reviewFamilyItems<T extends { automatically_included?: boolean; thread_kind?: string }>(items: readonly T[]) {
  return [...items].sort((a, b) => Number(Boolean(a.automatically_included)) - Number(Boolean(b.automatically_included)) || Number(a.thread_kind === 'subagent') - Number(b.thread_kind === 'subagent'));
}

export function projectFamilies<T extends FamilyConversation>(items: T[], expanded: ReadonlySet<string>, matches?: ReadonlySet<string>) {
  const byID = new Map(items.map(item => [item.id, item]));
  const children = new Map<string, T[]>();
  const included = new Set<string>();
  if (matches) {
    for (const id of matches) {
      let current = byID.get(id);
      const seen = new Set<string>();
      while (current && !seen.has(current.id)) {
        seen.add(current.id); included.add(current.id);
        current = byID.get(current.parent_conversation_id || '');
      }
    }
  }
  for (const item of items) {
    if (matches && !included.has(item.id)) continue;
    const parent = item.parent_conversation_id;
    if (parent && byID.has(parent) && !item.family_conflict) {
      const group = children.get(parent) || [];
      group.push(item); children.set(parent, group);
    }
  }
  const rows: Array<T & { familyDepth: number; familyHasChildren: boolean; familyExpanded: boolean }> = [];
  const visited = new Set<string>();
  function visit(item: T, depth: number) {
    if (visited.has(item.id) || matches && !included.has(item.id)) return;
    visited.add(item.id);
    const group = children.get(item.id) || [];
    const open = Boolean(matches) || expanded.has(item.id);
    rows.push({ ...item, familyDepth: depth, familyHasChildren: group.length > 0, familyExpanded: open });
    // 折叠时也标记后代，避免将它们再次当作根节点渲染。
    for (const child of group) {
      if (open) visit(child, depth + 1);
      else hide(child);
    }
  }
  function hide(item: T) {
    if (visited.has(item.id)) return;
    visited.add(item.id);
    for (const child of children.get(item.id) || []) hide(child);
  }
  for (const item of items) {
    if (!item.parent_conversation_id || !byID.has(item.parent_conversation_id) || item.family_conflict) visit(item, 0);
  }
  // 损坏的循环关系也保持可见，不能因列表投影而丢失会话。
  for (const item of items) if (!visited.has(item.id)) visit(item, 0);
  return rows;
}
