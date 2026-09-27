// 运行时验证：上一轮已交付但**从未在真机点过**的三项改动
//   ① 内置选择器层级（z-index 400 > 设置面板 300）且真在最上层
//   ② 「.. 返回上一级」可用，且在根目录上自动隐藏
//   ③ MCP 列表显示中文别名、内部标识退为小字
//   ④ 常规页「刷新页面」按钮存在且真的重载
//
// 用法（服务 + headless Chrome 都要先起好）：
//   node test/verify_shipped_ui.mjs
// CDP 端口 9333，服务 8420。

const CDP = process.env.CDP_PORT || 'http://127.0.0.1:9333';
const APP = 'http://127.0.0.1:8420/';
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

async function target() {
  for (let i = 0; i < 40; i++) {
    try {
      const list = await (await fetch(CDP + '/json/list')).json();
      const t = list.find((x) => x.type === 'page' && x.webSocketDebuggerUrl);
      if (t) return t;
    } catch (_) {}
    await sleep(400);
  }
  throw new Error('找不到可调试的页面 target');
}
function connect(url) {
  return new Promise((res, rej) => {
    const ws = new WebSocket(url);
    ws.onopen = () => res(ws);
    ws.onerror = () => rej(new Error('CDP WebSocket 连接失败'));
  });
}
let seq = 0;
const waiting = new Map();
function send(ws, method, params) {
  const id = ++seq;
  return new Promise((res, rej) => {
    waiting.set(id, { res, rej });
    ws.send(JSON.stringify({ id, method, params: params || {} }));
  });
}
const t = await target();
const ws = await connect(t.webSocketDebuggerUrl);
ws.onmessage = (e) => {
  const m = JSON.parse(e.data);
  if (m.id && waiting.has(m.id)) {
    const w = waiting.get(m.id); waiting.delete(m.id);
    m.error ? w.rej(new Error(m.error.message)) : w.res(m.result);
  }
};
async function ev(expr) {
  const r = await send(ws, 'Runtime.evaluate', { expression: expr, returnByValue: true, awaitPromise: true });
  if (r.exceptionDetails) {
    const d = r.exceptionDetails.exception;
    throw new Error('页面内异常: ' + (d && d.description ? d.description : r.exceptionDetails.text));
  }
  return r.result.value;
}

const passed = [];
const failed = [];
function check(name, cond, detail) {
  (cond ? passed : failed).push(name + (detail ? '  → ' + detail : ''));
  console.log((cond ? '  ✓ ' : '  ✗ ') + name + (detail ? '   ' + detail : ''));
}
function group(n) { console.log('\n' + n); }

await send(ws, 'Page.enable');
await send(ws, 'Page.navigate', { url: APP });
await sleep(2600);

// ---------------------------------------------------------------------------
// 前置：选一个工作区。
// 「＋菜单 → 浏览当前目录的文件」在没有工作区时会直接拒绝（ui.js:4522），
// 拿不到选择器就无从验证层级与「..」。这里直接走 REST 选仓库根，再重载页面。
group('⓪ 前置：选定工作区');
const wsSet = await ev(`(async () => {
  const r = await fetch('/api/workspace', {
    method: 'POST', headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ path: ${JSON.stringify(process.env.WS_ROOT || 'C:/Users/26536/Desktop/Code/CodeForge-GO/CodeForge-go').replace(/\\/g, '\\\\')} })
  });
  const d = await r.json();
  const g = await (await fetch('/api/workspace')).json();
  return { ok: r.ok, d, root: g.root };
})()`);
check('工作区已选定', !!wsSet.root, wsSet.root || JSON.stringify(wsSet.d));
if (wsSet.root) {
  await send(ws, 'Page.navigate', { url: APP });
  await sleep(2200);
}

// ---------------------------------------------------------------------------
group('① 内置选择器：层级必须高于设置面板');

const z = await ev(`(() => {
  // 打开设置面板，再打开内置选择器（＋菜单 → 浏览工作区）
  document.getElementById('open-settings').click();
  const s = document.getElementById('settings-overlay');
  s.classList.remove('hidden');
  const more = document.getElementById('more-browse-workspace');
  if (!more) return { err: 'more-browse-workspace 不存在' };
  more.click();
  const p = document.getElementById('picker-overlay');
  if (!p) return { err: '选择器未创建' };
  p.classList.remove('hidden');
  const sz = parseInt(getComputedStyle(s).zIndex, 10);
  const pz = parseInt(getComputedStyle(p).zIndex, 10);
  // 关键：选择器中心点上最顶层的元素必须属于选择器自己
  const r = p.querySelector('.picker-modal').getBoundingClientRect();
  const hit = document.elementFromPoint(r.left + r.width / 2, r.top + 24);
  const topmost = hit ? (hit.closest('#picker-overlay') ? 'picker' : (hit.id || hit.className)) : 'null';
  return { sz, pz, topmost };
})()`);
check('选择器 z-index 高于设置面板', !z.err && z.pz > z.sz, z.err || `picker=${z.pz} settings=${z.sz}`);
check('选择器中心点最顶层元素属于选择器（真盖在上面，不是被遮住）',
  !z.err && z.topmost === 'picker', z.err || 'elementFromPoint=' + z.topmost);

// ---------------------------------------------------------------------------
group('② 「.. 返回上一级」');

// ⚠️ 必须等 browse() 的 fetch 回来：openBuiltinPicker 是同步建 DOM、异步填列表，
// 断言跑在它前面就会看到空列表（第一轮实测踩过：label 与 path 都是空）。
async function waitPickerList(ws, ms) {
  const deadline = Date.now() + (ms || 6000);
  while (Date.now() < deadline) {
    const n = await ev(`(function () {
      var p = document.getElementById('picker-overlay');
      if (!p) return -1;
      var l = document.getElementById('picker-list');
      if (!l) return -1;
      return l.children.length;
    })()`);
    if (n > 0) return n;
    await sleep(200);
  }
  return 0;
}
const listed = await waitPickerList(ws, 6000);
check('选择器列表已加载（browse 的 fetch 回来了）', listed > 0, '子节点 ' + listed);

const up1 = await ev(`(() => {
  const p = document.getElementById('picker-overlay');
  const rows = [...p.querySelectorAll('.picker-item')];
  const up = rows.find((x) => x.classList.contains('picker-up'));
  return { hasUp: !!up, label: up ? up.textContent : '', current: p.dataset.current || '', path: document.getElementById('picker-path').textContent };
})()`);
check('列表顶部有「返回上一级」', up1.hasUp, up1.label + ' @ ' + up1.path);

const up2 = await ev(`(async () => {
  const p = document.getElementById('picker-overlay');
  const before = p.dataset.current;
  const up = [...p.querySelectorAll('.picker-item')].find((x) => x.classList.contains('picker-up'));
  if (!up) return { err: '没有 .. 行' };
  up.click();
  await new Promise((r) => setTimeout(r, 900));
  return { before, after: p.dataset.current, text: document.getElementById('picker-path').textContent };
})()`);
check('点「..」后真的进入上一级',
  !up2.err && up2.after && up2.after !== up2.before, up2.err || (up2.before + ' → ' + up2.after));

// ⚠️ 不要用 path=/ 来找根：Windows 上 filepath.IsAbs("/") 为 false，
// 它会被当成「工作区内的相对路径」，返回的还是工作区根（实测踩过）。
// 正确做法是**一路点 .. 往上走**，直到 .. 自己消失 —— 这也正是要验证的用户可见行为。
const up3 = await ev(`(async () => {
  const p = document.getElementById('picker-overlay');
  const pathEl = document.getElementById('picker-path');
  const seen = [];
  for (let i = 0; i < 40; i++) {
    const up = [...p.querySelectorAll('.picker-item')].find((x) => x.classList.contains('picker-up'));
    if (!up) break;
    const before = pathEl.textContent;
    up.click();
    await new Promise((r) => setTimeout(r, 260));
    seen.push(before);
    if (seen.length > 1 && pathEl.textContent === before) break; // 没动 = 到顶了
  }
  const up = [...p.querySelectorAll('.picker-item')].find((x) => x.classList.contains('picker-up'));
  const at = pathEl.textContent;
  // 到顶时服务端给的 parent 要么为空、要么等于自身
  const r = await fetch('/api/tree?depth=1&picker=1&path=' + encodeURIComponent(at));
  const d = await r.json();
  return {
    hops: seen.length, at,
    upHidden: !up,
    parentAtRoot: d.parent === '' || d.parent == null || d.parent === d.path,
    respParent: JSON.stringify(d.parent)
  };
})()`);
check('反复点「..」能一路上到文件系统根', up3.hops > 0, '跳了 ' + up3.hops + ' 次，停在 ' + up3.at);
check('到根后「..」自动隐藏', up3.upHidden, '停在 ' + up3.at + '，仍有 .. 行=' + !up3.upHidden);
check('根目录上服务端 parent 为空或等于自身', up3.parentAtRoot, 'parent=' + up3.respParent);

// 到顶后往回走两级，确认 .. 还能重新出现（不是一次性消耗）
const up4 = await ev(`(async () => {
  const p = document.getElementById('picker-overlay');
  const pathEl = document.getElementById('picker-path');
  for (let i = 0; i < 2; i++) {
    const it = [...p.querySelectorAll('.picker-item')].find((x) => !x.classList.contains('picker-up'));
    if (!it) break;
    it.click();
    await new Promise((r) => setTimeout(r, 300));
  }
  const up = [...p.querySelectorAll('.picker-item')].find((x) => x.classList.contains('picker-up'));
  return { back: !!up, at: pathEl.textContent };
})()`);
check('往下走进子目录后「..」重新出现', up4.back, '当前 ' + up4.at);

// 收尾：关掉选择器与设置面板
await ev(`(() => {
  const p = document.getElementById('picker-overlay'); if (p) p.classList.add('hidden');
  const s = document.getElementById('settings-overlay'); if (s) s.classList.add('hidden');
  return true;
})()`);

// ---------------------------------------------------------------------------
group('③ MCP 显示别名');

const api = await ev(`(async () => {
  const r = await fetch('/api/plugins'); const d = await r.json();
  return (d.items || []).map((x) => ({ name: x.name, display_name: x.display_name, type: x.type }));
})()`);
check('接口下发 display_name', api.length > 0 && api.every((x) => typeof x.display_name === 'string' && x.display_name),
  api.map((x) => x.name + '→' + x.display_name).join(', '));
check('显示名不是英文代号（用户反馈「显示代号不好看」）',
  api.some((x) => x.display_name && x.display_name !== x.name),
  api.filter((x) => x.display_name && x.display_name !== x.name).map((x) => x.display_name).join('/'));

const mcpUi = await ev(`(async () => {
  document.getElementById('open-settings').click();
  const s = document.getElementById('settings-overlay');
  s.classList.remove('hidden');
  const nav = s.querySelector('.nav-item[data-page="mcp"]');
  if (nav) nav.click();
  await new Promise((r) => setTimeout(r, 1200));
  const names = [...document.querySelectorAll('#mcp-items .mi-name')].map((x) => ({
    text: x.childNodes[0] ? x.childNodes[0].textContent.trim() : x.textContent.trim(),
    alias: x.querySelector('.mi-alias') ? x.querySelector('.mi-alias').textContent : null
  }));
  return names;
})()`);
check('设置页 MCP 列表渲染出条目', mcpUi.length > 0, JSON.stringify(mcpUi));
check('条目主文案是中文别名，标识退为小字',
  mcpUi.some((x) => x.alias && !/^[A-Za-z]/.test(x.text)),
  mcpUi.map((x) => x.text + (x.alias ? '(' + x.alias + ')' : '')).join(' | '));

// 侧栏 MCP 入口
const side = await ev(`(async () => {
  await new Promise((r) => setTimeout(r, 800));
  const li = [...document.querySelectorAll('#mcp-list li')];
  return li.map((x) => x.textContent.trim());
})()`);
check('侧栏 MCP 入口也用显示名', side.length === 0 || !/^[a-z_]+$/.test(side[0]),
  side.length ? side.join(' | ') : '（当前无启用的 MCP 插件）');

// ---------------------------------------------------------------------------
group('④ 常规页「刷新页面」');

const reload = await ev(`(() => {
  const s = document.getElementById('settings-overlay');
  s.classList.remove('hidden');
  const nav = s.querySelector('.nav-item[data-page="general"]');
  if (nav) nav.click();
  const btn = document.getElementById('reload-page');
  return { exists: !!btn, visible: btn ? getComputedStyle(btn).display !== 'none' : false, inGeneral: btn ? !!btn.closest('#page-general') : false };
})()`);
check('刷新按钮存在于常规页', reload.exists && reload.inGeneral, JSON.stringify(reload));

// 真的点它：CDP 能扛住导航，用 performance 的 navigationStart 变化判定重载发生
const before = await ev(`performance.timeOrigin`);
await ev(`document.getElementById('reload-page').click()`);
await sleep(2500);
const after = await ev(`performance.timeOrigin`);
check('点击后页面确实重载', after !== before, 'timeOrigin ' + before + ' → ' + after);
const afterState = await ev(`(() => ({
  composer: !!document.getElementById('composer'),
  reloadBtn: !!document.getElementById('reload-page')
}))()`);
check('重载后界面完好', afterState.composer && afterState.reloadBtn, JSON.stringify(afterState));

// ---------------------------------------------------------------------------
console.log('\n----------------------------------------------------');
console.log((failed.length ? '失败 ' + failed.length + ' 项：通过 ' + passed.length + ' 项：\n  - ' : '全部通过（') + (failed.length ? failed.join('\n  - ') : passed.length + ' 项断言'));
ws.close();
process.exit(failed.length ? 1 : 0);
