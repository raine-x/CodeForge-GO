// 功能校验（非看效果）：打开设置时主界面是否真的被藏起来。
//
//   node test/probe_settings_overlap.mjs
//
// 为什么必须实测：方案依赖 CSS 的 :has()。**它不被支持时选择器会被整条忽略**，
// 表现成「代码看着对、重叠依旧」—— 纯静态断言查不出来。这里直接量计算样式。

const PORT = process.env.CDP_PORT || 9280;
const BASE = 'http://127.0.0.1:' + PORT;
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
await sleep(3200);

const probe = `(function(){
  var app = document.getElementById('app');
  var ov = document.getElementById('settings-overlay');
  return {
    hasSupport: (function(){ try { return CSS.supports('selector(:has(*))'); } catch (e) { return false; } })(),
    ovHidden: ov.classList.contains('hidden'),
    appVisibility: getComputedStyle(app).visibility,
    appDisplay: getComputedStyle(app).display,
    selectorMatches: app.matches('#app:has(~ #settings-overlay:not(.hidden))')
  };
})()`;

console.log('\n① 设置关闭时');
const closed = await evaluate(ws, probe);
console.log('     ' + JSON.stringify(closed));
ck('浏览器支持 :has()', closed.hasSupport === true, closed.hasSupport);
ck('主界面可见（正常显示）', closed.appVisibility === 'visible', closed.appVisibility);

console.log('\n② 打开设置');
await evaluate(ws, `document.getElementById('open-settings').click(); true`);
await sleep(800);
const opened = await evaluate(ws, probe);
console.log('     ' + JSON.stringify(opened));
ck('overlay 已显示（未带 hidden）', opened.ovHidden === false, opened.ovHidden);
ck('选择器命中主界面', opened.selectorMatches === true, opened.selectorMatches);
ck('主界面被藏起来（visibility:hidden）', opened.appVisibility === 'hidden', opened.appVisibility);
ck('没有用 display:none（布局与滚动位置得以保留）', opened.appDisplay !== 'none', opened.appDisplay);

console.log('\n③ 关闭设置（Esc 路径）');
await evaluate(ws, `document.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape', bubbles: true })); true`);
await sleep(900);
const closed2 = await evaluate(ws, probe);
console.log('     ' + JSON.stringify(closed2));
ck('主界面恢复可见', closed2.appVisibility === 'visible', closed2.appVisibility);

console.log('\n' + '-'.repeat(52));
if (failed.length) {
  console.log('失败 ' + failed.length + ' 项 / 通过 ' + passed.length + ' 项');
  failed.forEach((f) => console.log('  - ' + f));
  process.exitCode = 1;
} else {
  console.log('全部通过（' + passed.length + ' 项）');
}
ws.close();
