// 中文说明：验证首次导入浏览器设置、跨端口恢复、部分更新顺序及同步失败提示。
import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import { test } from 'node:test';
import ts from 'typescript';

let sequence = 0;
async function loadPreferences(initial = {}) {
  const storage = new Map(Object.entries(initial));
  globalThis.localStorage = {
    getItem: key => storage.get(key) ?? null,
    setItem: (key, value) => storage.set(key, value)
  };
  globalThis.window = new EventTarget();
  const source = await readFile(new URL('../src/preferences.ts', import.meta.url), 'utf8');
  const { outputText } = ts.transpileModule(source, { compilerOptions: { target: ts.ScriptTarget.ES2022, module: ts.ModuleKind.ESNext } });
  const module = await import(`data:text/javascript;base64,${Buffer.from(outputText).toString('base64')}#${sequence++}`);
  return { module, storage };
}

const theme = 'codex-chronicle.theme.v1';
const density = 'codex-chronicle.density.v1';
const filters = 'codex-chronicle.saved-filters.v1';

test('首次导入已有偏好，换端口后从服务恢复，旧浏览器缓存不会覆盖已保存设置', async () => {
  const saved = {};
  const request = async (_url, init) => {
    if (init) Object.assign(saved, JSON.parse(init.body));
    return Response.json({ available: true, preferences: { ...saved } });
  };
  const first = await loadPreferences({ [theme]: 'dark', [filters]: '{"workspace":[]}' });
  await first.module.initializePreferences(request);
  await first.module.savePreference(density, 'reading');
  const second = await loadPreferences({ [theme]: 'light' });
  await second.module.initializePreferences(request);
  assert.equal(second.module.readPreference(theme), 'dark');
  assert.equal(second.module.readPreference(density), 'reading');
  assert.equal(second.module.readPreference(filters), '{"workspace":[]}');
  assert.equal(second.storage.get(theme), 'dark');
  assert.equal(saved[theme], 'dark');
});

test('快速连续修改按顺序保存，不能让较早请求覆盖最后一次选择', async () => {
  const saved = { [theme]: 'light' };
  let release;
  const blocked = new Promise(resolve => { release = resolve; });
  const request = async (_url, init) => {
    if (init) {
      const patch = JSON.parse(init.body);
      if (patch[theme] === 'dark') await blocked;
      Object.assign(saved, patch);
    }
    return Response.json({ available: true, preferences: { ...saved } });
  };
  const { module } = await loadPreferences();
  await module.initializePreferences(request);
  const first = module.savePreference(theme, 'dark');
  const second = module.savePreference(theme, 'light');
  release();
  await Promise.all([first, second]);
  assert.equal(saved[theme], 'light');
});

test('浏览器禁止缓存时仍能保存到应用，服务端失败会通知界面且保留当前选择', async () => {
  const { module } = await loadPreferences();
  globalThis.localStorage.setItem = () => { throw new Error('blocked'); };
  let failed = false;
  let warnings = 0;
  const saved = {};
  window.addEventListener('chronicle-preferences-error', () => { warnings++; });
  await module.initializePreferences(async (_url, init) => {
    if (init) {
      if (failed) return new Response('', { status: 500 });
      Object.assign(saved, JSON.parse(init.body));
    }
    return Response.json({ available: true, preferences: { ...saved } });
  });
  await module.savePreference(theme, 'dark');
  assert.equal(saved[theme], 'dark');
  assert.equal(warnings, 0);
  failed = true;
  await module.savePreference(density, 'compact');
  assert.equal(warnings, 1);
  assert.equal(module.readPreference(density), 'compact');
});
