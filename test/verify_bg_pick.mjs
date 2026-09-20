// 验证「选图后生效」的完整链路，重点是前端那层兜底。
//
//   node test/verify_bg_pick.mjs
//
// ⚠️ 前置条件：**当前不能已设置背景图**。脚本第一件事就是断言「初始 #bg-layer 不显示」，
// 若上一步刚设过图，这条会假失败（2026-09-19 回归时踩到）。
// 需要先清空：POST /api/appearance {"clear":true}
//
// 做法：把 /api/appearance/pick 拦下来，返回一个**没有 background:true 确认**的响应
// （复刻 Windows 分支漏写 cfg 的老服务端行为）。此时前端必须自己补一次保存、
// 并等保存完成后再去拉背景图 —— 否则就是用户看到的「选完图片啥也没有」。

import fs from 'node:fs';

const PORT = process.env.CDP_PORT || 9227;
const BASE = 'http://127.0.0.1:' + PORT;
const OUT = process.env.SHOT_DIR || '.';
const IMG = process.env.TEST_IMG || '';
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

async function pageTarget() {
  for (let i = 0; i < 40; i++) {
    try {
      const list = await (await fetch(BASE + '/json/list')).json();
      const t = list.find((x) => x.type === 'page' && x.webSocketDebuggerUrl);
      if (t) return t;
    } catch (e) {}
    await sleep(500);
  }
  throw new Error('找不到可调试的页面 target');
}

let seq = 0;
const waiting = new Map();
const send = (ws, method, params) =>
  new Promise((res, rej) => {
    const id = ++seq;
    waiting.set(id, { res, rej });
    ws.send(JSON.stringify({ id, method, params: params || {} }));
  });

async function evaluate(ws, expr) {
  const r = await send(ws, 'Runtime.evaluate', { expression: expr, returnByValue: true, awaitPromise: true });
  if (r.exceptionDetails) {
    const d = r.exceptionDetails.exception;
    throw new Error('页面内异常: ' + (d && d.description ? d.description : r.exceptionDetails.text));
  }
  return r.result.value;
}

const passed = [], failed = [];
function ck(name, ok, extra) {
  (ok ? passed : failed).push(name);
  console.log((ok ? '  \u2713 ' : '  \u2717 ') + name + (extra !== undefined ? '   [' + extra + ']' : ''));
}

const t = await pageTarget();
const ws = await new Promise((res, rej) => {
  const s = new WebSocket(t.webSocketDebuggerUrl);
  s.onopen = () => res(s);
  s.onerror = () => rej(new Error('CDP 连接失败'));
});
ws.onmessage = (ev) => {
  const m = JSON.parse(ev.data);
  if (m.id && waiting.has(m.id)) {
    const { res, rej } = waiting.get(m.id);
    waiting.delete(m.id);
    m.error ? rej(new Error(m.error.message)) : res(m.result);
  }
};
await send(ws, 'Page.enable');
await send(ws, 'Runtime.enable');
await send(ws, 'Page.navigate', { url: 'http://127.0.0.1:8420/' });
await sleep(4000);

console.log('\n前置：确认初始无背景图');
const before = await evaluate(ws, `getComputedStyle(document.getElementById('bg-layer')).display`);
ck('初始 #bg-layer 不显示', before === 'none', before);

console.log('\n拦截选图接口，返回「未落库」的响应（复刻老服务端漏写 cfg）');
await evaluate(ws, `(function(){
  var orig = window.fetch;
  window.__pickCalls = 0;
  window.__saveCalls = 0;
  window.__calls = [];
  window.fetch = function (url, opts) {
    var u = String(url);
    var method = (opts && opts.method) || 'GET';
    window.__calls.push({ url: u.slice(0, 60), method: method, body: opts && opts.body ? String(opts.body).slice(0, 80) : '' });
    if (u.indexOf('/api/appearance/pick') === 0) {
      window.__pickCalls++;
      // ⚠️ 关键：没有 background:true —— 老服务端就是这样，只回 path 不落库
      return Promise.resolve(new Response(
        JSON.stringify({ ok: true, path: ${JSON.stringify(IMG)} }),
        { status: 200, headers: { 'Content-Type': 'application/json' } }));
    }
    if (u.indexOf('/api/appearance') === 0 && method === 'POST') {
      window.__saveCalls++;
    }
    return orig.apply(this, arguments);
  };
})(); true`);

await evaluate(ws, `document.getElementById('bg-pick').click(); true`);
await sleep(2500);   // 等 补保存 → applyBgImage → 图片加载

console.log('\n结果');
const r = await evaluate(ws, `(async function(){
  var bl = document.getElementById('bg-layer');
  var cs = getComputedStyle(bl);
  // 直接问服务端：背景图接口到底通不通
  var httpStatus = 0, natW = 0, natH = 0, err = '';
  try {
    var resp = await fetch('/api/appearance/background?probe=' + Date.now());
    httpStatus = resp.status;
    if (resp.ok) {
      var blob = await resp.blob();
      var url = URL.createObjectURL(blob);
      var im = new Image();
      im.src = url;
      await im.decode();
      natW = im.naturalWidth; natH = im.naturalHeight;
      URL.revokeObjectURL(url);
    }
  } catch (e) { err = String(e); }
  return {
    pickCalls: window.__pickCalls,
    saveCalls: window.__saveCalls,
    calls: window.__calls,
    cls: bl.className,
    display: cs.display,
    bgImage: cs.backgroundImage.slice(0, 50),
    bodyHasBg: document.body.classList.contains('has-bg'),
    httpStatus: httpStatus, natW: natW, natH: natH, err: err
  };
})()`);
console.log('     ' + JSON.stringify({ pickCalls: r.pickCalls, saveCalls: r.saveCalls, cls: r.cls, httpStatus: r.httpStatus }));
console.log('     fetch 调用记录:');
(r.calls || []).forEach((c) => console.log('       ' + c.method + ' ' + c.url + (c.body ? '  body=' + c.body : '')));

ck('选图接口被调用一次', r.pickCalls === 1, r.pickCalls);
ck('前端补了一次显式保存（兜底生效）', r.saveCalls >= 1, r.saveCalls);
ck('图层已开启', r.cls.includes('on') && r.display === 'block', r.cls + '/' + r.display);
ck('主区已透明化（has-bg）', r.bodyHasBg === true, r.bodyHasBg);
ck('图层已挂上背景图 URL', /url\(/.test(r.bgImage), r.bgImage);
ck('背景图接口返回 200（未落库时这里是 404）', r.httpStatus === 200, r.httpStatus);
ck('图片真的解码出来了', r.natW > 0 && r.natH > 0, r.natW + 'x' + r.natH + (r.err ? ' err=' + r.err : ''));

const shot = await send(ws, 'Page.captureScreenshot', { format: 'png' });
const p = OUT.replace(/[\\/]+$/, '') + '/cf-ui-bgpick.png';
fs.writeFileSync(p, Buffer.from(shot.data, 'base64'));
console.log('\n     截图: ' + p);

console.log('\n' + '-'.repeat(52));
if (failed.length) {
  console.log('失败 ' + failed.length + ' 项 / 通过 ' + passed.length + ' 项');
  failed.forEach((f) => console.log('  - ' + f));
  process.exitCode = 1;
} else {
  console.log('全部通过（' + passed.length + ' 项）');
}
ws.close();
