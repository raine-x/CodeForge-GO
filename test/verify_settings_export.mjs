// 校验导出的设置界面：导航、开关、分段控件、滑条是否真的能交互。
//
//   node test/verify_settings_export.mjs <导出的 html 路径>
//
// 交付物是「给别的模型重构的 UI 稿」，所以「打开有样子」不够 ——
// 用户明确要求保留切换功能，这里逐项点一遍并断言状态真的变了。

const PORT = process.env.CDP_PORT || 9290;
const BASE = 'http://127.0.0.1:' + PORT;
const FILE = process.argv[2] || '';
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
await send(ws, 'Page.navigate', { url: 'file:///' + FILE.replace(/\\/g, '/') });
await sleep(1500);

// 加载时就该报错的地方：脚本异常会让下面全部失败，先单独抓一次
const errs = await evaluate(ws, `(function(){
  return { hasOverlay: !!document.getElementById('settings-overlay'),
           pages: document.querySelectorAll('.settings-page').length,
           navs: document.querySelectorAll('.nav-item').length,
           navsWithPage: document.querySelectorAll('.nav-item[data-page]').length };
})()`);
console.log('结构: ' + JSON.stringify(errs));
ck('面板已渲染', errs.hasOverlay === true);
ck('页面数 = 9', errs.pages === 9, errs.pages);
ck('导航项 = 10（含「返回」与无 data-page 的「模型」）', errs.navs === 10, errs.navs);
ck('其中带 data-page 的 = 8', errs.navsWithPage === 8, errs.navsWithPage);

console.log('\n① 左导航切换（带 data-page 的项）');
const nav = await evaluate(ws, `(function(){
  var before = (document.querySelector('.settings-page.active') || {}).id;
  document.querySelector('.nav-item[data-page="memory"]').click();
  var after = (document.querySelector('.settings-page.active') || {}).id;
  var act = document.querySelector('.nav-item.active');
  return { before: before, after: after,
           navActive: act && act.dataset ? act.dataset.page : null,
           activeCount: document.querySelectorAll('.settings-page.active').length };
})()`);
console.log('     ' + JSON.stringify(nav));
ck('点击「记忆」后页面切过去了', nav.after === 'page-memory', nav.before + ' → ' + nav.after);
ck('同一时刻只有一个页面 active', nav.activeCount === 1, nav.activeCount);
ck('导航项高亮跟着走', nav.navActive === 'memory', nav.navActive);

// 回归点：模型项没有 data-page，漏掉它点「模型」会毫无反应
console.log('\n①b 点「模型」（无 data-page，靠 id 特判）');
const navModels = await evaluate(ws, `(function(){
  var item = document.getElementById('nav-models');
  if (!item) return { missing: true };
  item.click();
  var act = document.querySelector('.settings-page.active');
  return { after: act ? act.id : null, navActiveId: (document.querySelector('.nav-item.active') || {}).id };
})()`);
console.log('     ' + JSON.stringify(navModels));
ck('点「模型」能切到 page-models', navModels.after === 'page-models', navModels.after);
ck('「模型」项本身高亮', navModels.navActiveId === 'nav-models', navModels.navActiveId);

console.log('\n①c 点「返回工作区」不应切页');
const navBack = await evaluate(ws, `(function(){
  var before = (document.querySelector('.settings-page.active') || {}).id;
  document.getElementById('nav-back').click();
  var after = (document.querySelector('.settings-page.active') || {}).id;
  return { before: before, after: after };
})()`);
ck('「返回」不改变当前页', navBack.before === navBack.after, navBack.before + ' → ' + navBack.after);

console.log('\n② 开关（原生 checkbox）');
const sw = await evaluate(ws, `(function(){
  var el = document.getElementById('settings-notify');
  if (!el) return { missing: true };
  var before = el.checked;
  el.click();
  var mid = el.checked;
  el.click();
  return { before: before, afterFirstClick: mid, afterSecondClick: el.checked };
})()`);
console.log('     ' + JSON.stringify(sw));
ck('开关存在且可切换', !sw.missing && sw.afterFirstClick !== sw.before, sw.before + ' → ' + sw.afterFirstClick);
ck('再点一次能切回', sw.afterSecondClick === sw.before, sw.afterSecondClick);

console.log('\n③ 分段控件（思考皮肤）');
const seg = await evaluate(ws, `(function(){
  var seg = document.querySelector('.skin-seg');
  if (!seg) return { missing: true };
  var btns = seg.querySelectorAll('button');
  var target = null;
  for (var i = 0; i < btns.length; i++) { if (!btns[i].classList.contains('active')) { target = btns[i]; break; } }
  if (!target) return { missing: true, reason: '没有非激活项' };
  var want = target.dataset.skin || target.textContent.trim();
  target.click();
  var act = seg.querySelectorAll('button.active');
  return { want: want, activeCount: act.length, activeIsTarget: act.length === 1 && act[0] === target };
})()`);
console.log('     ' + JSON.stringify(seg));
ck('分段控件可选中', !seg.missing && seg.activeIsTarget === true, JSON.stringify(seg));
ck('分段控件是单选（只有一个 active）', seg.activeCount === 1, seg.activeCount);

console.log('\n④ 滑条数值同步');
const sl = await evaluate(ws, `(function(){
  var r = document.getElementById('opt-bg-bright');
  if (!r) return { missing: true };
  r.value = 33;
  r.dispatchEvent(new Event('input', { bubbles: true }));
  var out = document.getElementById('opt-bg-bright-val');
  return { text: out ? out.textContent : null };
})()`);
console.log('     ' + JSON.stringify(sl));
ck('亮度滑条同步到 33%', sl.text === '33%', sl.text);

console.log('\n⑤ MCP 分栏切换');
const mcp = await evaluate(ws, `(function(){
  var page = document.getElementById('page-mcp');
  if (!page) return { missing: true };
  var tabs = page.querySelectorAll('.mcp-tab');
  if (tabs.length < 2) return { missing: true, reason: 'tab 不足' };
  tabs[1].click();
  var pane = tabs[1].dataset.pane;
  var act = page.querySelectorAll('.mcp-pane.active');
  return { pane: pane, activeCount: act.length, activeId: act[0] ? act[0].id : null };
})()`);
console.log('     ' + JSON.stringify(mcp));
ck('MCP 分栏切到目标面板', !mcp.missing && mcp.activeId === mcp.pane, mcp.activeId);
ck('MCP 面板同时只有一个 active', mcp.activeCount === 1, mcp.activeCount);

console.log('\n' + '-'.repeat(52));
if (failed.length) {
  console.log('失败 ' + failed.length + ' 项 / 通过 ' + passed.length + ' 项');
  failed.forEach((f) => console.log('  - ' + f));
  process.exitCode = 1;
} else {
  console.log('全部通过（' + passed.length + ' 项）');
}
ws.close();
