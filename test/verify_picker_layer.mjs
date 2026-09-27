// 一次性验证：内置选择器在「设置」面板之上的实际层叠关系。
//
// 用真实 Chrome 量，而不是只看 CSS 数字 —— 层级问题只在合成之后才见分晓。
// 判定标准是**元素命中测试**：选择器中心点上最顶层的元素必须属于选择器自己。
//
//   node test/verify_picker_layer.mjs
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
  throw new Error('找不到页面 target');
}
const t = await target();
const ws = await new Promise((res, rej) => {
  const s = new WebSocket(t.webSocketDebuggerUrl);
  s.onopen = () => res(s); s.onerror = () => rej(new Error('CDP 连接失败'));
});
let seq = 0; const waiting = new Map();
ws.onmessage = (e) => {
  const m = JSON.parse(e.data);
  if (m.id && waiting.has(m.id)) { const w = waiting.get(m.id); waiting.delete(m.id); m.error ? w.rej(new Error(m.error.message)) : w.res(m.result); }
};
const send = (method, params) => new Promise((res, rej) => { const id = ++seq; waiting.set(id, { res, rej }); ws.send(JSON.stringify({ id, method, params: params || {} })); });
const ev = async (expression) => {
  const r = await send('Runtime.evaluate', { expression, returnByValue: true, awaitPromise: true });
  if (r.exceptionDetails) throw new Error('页面内异常: ' + JSON.stringify(r.exceptionDetails.text));
  return r.result.value;
};

const passed = []; const failed = [];
function check(name, cond, detail) {
  (cond ? passed : failed).push(name);
  console.log((cond ? '  ✓ ' : '  ✗ ') + name + (detail ? '   ' + detail : ''));
}

await send('Page.enable');
await send('Page.navigate', { url: APP });
await sleep(2600);

const r = await ev(`(async () => {
  // 打开设置面板，再打开内置选择器（模拟「设置 → 外观 → 选择图片」的层级）
  // 先用同步创建选择器的入口（＋菜单 → 浏览当前目录的文件）。
  // 「添加文件」走 /api/pick_file 异步接口，Windows 上还会弹原生对话框，不适合无头验证。
  const bw = document.getElementById('more-browse-workspace');
  if (!bw) return { err: 'more-browse-workspace 不存在' };
  bw.click();
  await new Promise((r) => setTimeout(r, 300));
  // 再打开设置面板 —— 复现「选择器从设置面板里弹出」的层叠场景
  document.getElementById('open-settings').click();
  const s = document.getElementById('settings-overlay');
  s.classList.remove('hidden');
  const p = document.getElementById('picker-overlay');
  if (!p) return { err: '选择器尚未创建（先触发一次 openBuiltinPicker）' };
  p.classList.remove('hidden');

  const sz = parseInt(getComputedStyle(s).zIndex, 10);
  const pz = parseInt(getComputedStyle(p).zIndex, 10);
  // 两者是否同在一个层叠上下文里（#app 若形成层叠上下文就会隔开）
  const appCS = getComputedStyle(document.getElementById('app'));
  const appIsCtx = appCS.zIndex !== 'auto' || (appCS.transform !== 'none') || (appCS.filter !== 'none')
    || (appCS.contain !== 'none') || (appCS.perspective !== 'none');
  const parent = p.parentElement ? (p.parentElement.tagName + (p.parentElement.id ? '#' + p.parentElement.id : '')) : 'null';

  // 关键：命中测试。取选择器头、列表、底部按钮三个点，看最顶层是谁。
  const pts = [];
  const probe = (sel, tag) => {
    const el = p.querySelector(sel);
    if (!el) return;
    const b = el.getBoundingClientRect();
    if (b.width < 1 || b.height < 1) return;
    const hit = document.elementFromPoint(b.left + b.width / 2, b.top + b.height / 2);
    pts.push({ tag, hit: hit ? (hit.closest('#picker-overlay') ? 'picker' : (hit.id || hit.className || hit.tagName)) : 'null' });
  };
  probe('.picker-head', '头');
  probe('#picker-list', '列表');
  probe('.picker-actions', '按钮');
  return { sz, pz, appIsCtx, parent, pts };
})()`);

if (r.err) {
  console.log('  ✗ ' + r.err);
  failed.push(r.err);
} else {
  console.log('\n层级数据：设置 z-index=' + r.sz + '，选择器 z-index=' + r.pz +
    '，挂载在 ' + r.parent + '，#app 形成层叠上下文=' + r.appIsCtx);
  check('选择器 z-index 高于设置面板', r.pz > r.sz, r.pz + ' > ' + r.sz);
  check('选择器是 body 的直接子节点（不受 #app 的 overflow 裁剪）', r.parent === 'BODY', r.parent);
  check('#app 不形成层叠上下文（否则两者被隔开、400 不再与 300 直接比较）', r.appIsCtx === false);
  for (const p of r.pts) {
    check('命中测试 · ' + p.tag + '：最顶层是选择器自己', p.hit === 'picker', '实际 ' + p.hit);
  }
}

console.log('\n' + '-'.repeat(52));
console.log('通过 ' + passed.length + ' / 失败 ' + failed.length);
try { ws.close(); } catch (_) {}
process.exit(failed.length ? 1 : 0);
