// 中文说明：验证阅读状态有界存储、跨工作区隔离、URL 定位和危险 Markdown 的处理。
import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import { test } from 'node:test';
import ts from 'typescript';
async function moduleAt(path, imports = false) {
  const source = await readFile(new URL(path, import.meta.url), 'utf8');
  let { outputText } = ts.transpileModule(source, { compilerOptions: { target: ts.ScriptTarget.ES2022, module: ts.ModuleKind.ESNext } });
  if (imports) outputText = outputText.replace("from 'markdown-it'", `from '${import.meta.resolve('markdown-it')}'`);
  return import(`data:text/javascript;base64,${Buffer.from(outputText).toString('base64')}`);
}
const { isReaderMessage, mergeReaderItems, maxVisibleMessages, readReaderState, writeReaderState, readerLocationFromURL } = await moduleAt('../src/sessions/reader-state.ts');
const { renderReaderMarkdown } = await moduleAt('../src/sessions/reader-markdown.ts', true);
test('正文保留用户、助手与问答回复，工具和运行上下文独立显示', () => {
  assert.equal(isReaderMessage({ role: 'user', kind: 'message' }), true);
  assert.equal(isReaderMessage({ role: 'assistant', kind: 'message' }), true);
  assert.equal(isReaderMessage({ role: 'user', kind: 'question_reply' }), true);
  assert.equal(isReaderMessage({ role: 'user', kind: 'agent_task' }), true);
  for (const event of [
    { role: 'tool', kind: 'function_call' },
    { role: 'assistant', kind: 'command_execution', tool: { kind: 'command' } },
    { role: 'user', kind: 'injected_context' },
    { role: 'context', kind: 'reasoning' },
    { role: 'developer', kind: 'message' }
  ]) assert.equal(isReaderMessage(event), false);
});
test('双向补载按顺序合并重叠消息，前页不越过当前窗口写入乱序消息', () => {
  const events = (...ids) => ids.map(id => ({ id, text: String(id) }));
  const current = events(3, 4, 5); current[0].text = '已读取完整正文';
  const before = mergeReaderItems(current, events(1, 2, 3, 4, 5, 6), 'before');
  assert.deepEqual(before.items.map(item => item.id), [1, 2, 3, 4, 5]);
  assert.equal(before.items[2].text, '已读取完整正文');
  assert.equal(before.trimmed, false);
  const after = mergeReaderItems(current, events(5, 6, 7), 'after');
  assert.deepEqual(after.items.map(item => item.id), [3, 4, 5, 6, 7]);
  assert.equal(after.items[0], current[0]);
});
test('长对话滚动窗口不超过上限，按加载方向回收另一端', () => {
  const events = (start, length) => Array.from({ length }, (_, i) => ({ id: start + i }));
  const initial = events(100, maxVisibleMessages);
  const before = mergeReaderItems(initial, events(40, 80), 'before');
  assert.equal(before.items.length, maxVisibleMessages);
  assert.equal(before.items[0].id, 40);
  assert.equal(before.items.at(-1).id, 439);
  assert.equal(before.trimmed, true);
  const after = mergeReaderItems(before.items, events(439, 61), 'after');
  assert.equal(after.items.length, maxVisibleMessages);
  assert.equal(after.items[0].id, 100);
  assert.equal(after.items.at(-1).id, 499);
  assert.equal(after.trimmed, true);
});
test('阅读状态不包含正文、最多 100 个会话且工作区隔离', () => {
  let raw = ''; const storage = { getItem: () => raw, setItem: (_key, value) => { raw = value; } };
  const state = { inset: 12, expanded: ['m'], query: '中文', role: '', navigation: true, updated: 0 };
  writeReaderState(storage, 'a', 'one', state);
  assert.equal(readReaderState(storage, 'b', 'one'), null);
  assert.equal(readReaderState(storage, 'a', 'one').query, '中文');
  for (let i = 0; i < 105; i++) writeReaderState(storage, 'a', String(i), state);
  assert.equal(Object.keys(JSON.parse(raw)).length, 100);
});
test('定位链接验证字节位置', () => {
  assert.equal(readerLocationFromURL('?read_path=x&read_start=-1'), null);
  assert.deepEqual(readerLocationFromURL('?read_path=x&read_start=12&read_anchor=abc'), { path: 'x', start: 12, anchor: 'abc', query: undefined, source: undefined });
});
test('历史版本随定位链接和阅读状态保存', () => {
  const location = readerLocationFromURL('?read_path=base.jsonl&read_start=12&read_source=old.jsonl');
  assert.equal(location.source, 'old.jsonl');
  let raw = ''; const storage = { getItem: () => raw, setItem: (_key, value) => { raw = value; } };
  writeReaderState(storage, 'workspace', 'id', { location, inset: 0, expanded: [], query: '', role: '', navigation: false, updated: 0 });
  assert.equal(readReaderState(storage, 'workspace', 'id').location.source, 'old.jsonl');
});
test('Markdown 禁止 HTML、危险协议和远程图片自动请求，保留表格和代码', () => {
  const html = renderReaderMarkdown('<script>alert(1)</script>\n\n[x](javascript:alert(1))\n\n![远程](https://invalid.example/a.png)\n\n|甲|乙|\n|-|-|\n|1|2|\n\n```js\nconst x = 1;\n```');
  assert(!html.includes('<script>')); assert(!html.includes('href="javascript:')); assert(!html.includes('<img'));
  assert(html.includes('<table>')); assert(html.includes('<pre>')); assert(html.includes('未自动加载'));
  assert(!renderReaderMarkdown('[命令](command:delete)').includes('href="command:'));
});
