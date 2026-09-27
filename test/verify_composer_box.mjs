// 一次性验证：设置 → 外观 → 输入框外观（黑 / 白 / 透明）
//
// 要证明的是「换底色的同时前景色也换了」——输入文字不是 textarea 画的，
// 而是 .input-mirror 用 var(--text) 画的，只换背景会在浅色主题下白底白字。
// 所以每档都要在 **body.bg-dark 开与关两种主题** 下各测一次。
//
// 用法：先起服务（8420），再起带调试端口的 Chrome，然后
//   node test/verify_composer_box.mjs
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
  throw new Error('找不到可连接的页面 target（Chrome 是否带 --remote-debugging-port 启动？）');
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

const passed = []; const failed = [];
function check(name, cond, detail) {
  (cond ? passed : failed).push(name + (detail ? '  → ' + detail : ''));
  console.log((cond ? '  ✓ ' : '  ✗ ') + name + (detail ? '   ' + detail : ''));
}
function group(n) { console.log('\n' + n); }

// 看门狗：宁可超时报错并把已跑过的结果打出来，也不要静默挂死
// （踩过一次：页面内 location.reload() 销毁执行上下文，awaitPromise 永不落定）。
const watchdog = setTimeout(() => {
  console.log('\n✗ 超时（150s）。已跑过的结果：');
  passed.forEach((p) => console.log('  ✓ ' + p));
  failed.forEach((f) => console.log('  ✗ ' + f));
  process.exit(2);
}, 150000);
watchdog.unref?.();

await send(ws, 'Page.enable');
await send(ws, 'Page.navigate', { url: APP });
await sleep(2600);

// 在页面里采样：卡片底色 / 正文色 / placeholder 色 / 实心按钮色，以及各自相对亮度。
// 相对亮度用 WCAG 的 sRGB→linear 公式：只有这样「深底浅字」才是可判定的数字，
// 而不是靠肉眼看两个 hex。
const SAMPLE = `(() => {
  function parse(c) {
    const m = String(c).match(/rgba?\\(([^)]+)\\)/); if (!m) return null;
    const p = m[1].split(',').map((x) => parseFloat(x));
    return { r: p[0], g: p[1], b: p[2], a: p.length > 3 ? p[3] : 1 };
  }
  function lum(c) {
    if (!c) return null;
    const f = (v) => { v /= 255; return v <= 0.03928 ? v / 12.92 : Math.pow((v + 0.055) / 1.055, 2.4); };
    return 0.2126 * f(c.r) + 0.7152 * f(c.g) + 0.0722 * f(c.b);
  }
  // WCAG 对比度：判定「看不看得清」只有这个指标有意义。
  // 早先用「亮度 > 0.4」判 placeholder 是错的 —— 0.354 听着像低，
  // 但对 L=0.006 的近黑底换算过来是 7.2:1，实际很清楚。
  function ratio(fg, bg) {
    const a = lum(fg); const b = lum(bg);
    if (a === null || b === null) return null;
    const hi = Math.max(a, b); const lo = Math.min(a, b);
    return (hi + 0.05) / (lo + 0.05);
  }
  const box = document.getElementById('composer');
  const ta = document.getElementById('input');
  const mir = document.getElementById('input-mirror');
  const send = document.getElementById('send-btn');
  const cs = getComputedStyle(box);
  const bgC = parse(cs.backgroundColor);
  const textC = parse(getComputedStyle(mir).color);
  const phC = parse(getComputedStyle(ta, '::placeholder').color);
  return {
    cls: box.className,
    bg: bgC,
    bgLum: lum(bgC),
    text: textC,
    textLum: lum(textC),
    ph: phC,
    textRatio: ratio(textC, bgC),
    phRatio: ratio(phC, bgC),
    sendBg: send ? parse(getComputedStyle(send).backgroundColor) : null,
    dark: document.body.classList.contains('bg-dark'),
  };
})()`;

async function setMode(v) {
  await ev(`(() => { localStorage.setItem('cf_composer_box', ${JSON.stringify(v)}); })()`);
  await send(ws, 'Page.navigate', { url: APP });
  await sleep(1500);
}
async function setTheme(dark) {
  await ev(`(() => { document.body.classList.toggle('bg-dark', ${!!dark}); })()`);
  await sleep(150);
}
const fmt = (c) => (c ? 'rgb(' + [c.r, c.g, c.b].map(Math.round).join(',') + ')' : '?');

for (const dark of [false, true]) {
  group((dark ? '深色主题' : '浅色主题') + '（body.bg-dark = ' + dark + '）');
  for (const v of ['black', 'white', 'clear']) {
    await setMode(v);
    await setTheme(dark);
    const s = await ev(SAMPLE);
    const tag = v.padEnd(5);
    if (v === 'black') {
      check(tag + ' 底色为深色', s.bgLum !== null && s.bgLum < 0.05, fmt(s.bg) + ' L=' + (s.bgLum === null ? '?' : s.bgLum.toFixed(3)));
    } else if (v === 'white') {
      check(tag + ' 底色为浅色', s.bgLum !== null && s.bgLum > 0.85, fmt(s.bg) + ' L=' + (s.bgLum === null ? '?' : s.bgLum.toFixed(3)));
    } else {
      check(tag + ' 底色完全透明', s.bg && s.bg.a === 0, fmt(s.bg) + ' a=' + (s.bg ? s.bg.a : '?'));
    }
    if (v !== 'clear') {
      // 正文对比度：AA 对正文的门槛是 4.5:1
      check(tag + ' 正文对比度 ≥ 4.5:1（看得清）', s.textRatio !== null && s.textRatio >= 4.5,
        fmt(s.text) + ' 对 ' + fmt(s.bg) + ' = ' + (s.textRatio === null ? '?' : s.textRatio.toFixed(2) + ':1'));
      // placeholder 是弱提示，但低到读不出来就成了「灰条上放灰字」
      check(tag + ' placeholder 对比度 ≥ 3:1', s.phRatio !== null && s.phRatio >= 3,
        fmt(s.ph) + ' = ' + (s.phRatio === null ? '?' : s.phRatio.toFixed(2) + ':1'));
    }
  }
}

group('设置页控件');
const seg = await ev(`(() => {
  const el = document.getElementById('composer-seg');
  if (!el) return { err: 'composer-seg 不存在' };
  const bs = [...el.querySelectorAll('button')].map((b) => ({ cbox: b.dataset.cbox, txt: b.textContent.trim(), active: b.classList.contains('active') }));
  return { bs };
})()`);
check('三档按钮齐全', !seg.err && seg.bs.length === 3, seg.err || JSON.stringify(seg.bs.map((b) => b.txt)));
check('当前档有 active 标记', !seg.err && seg.bs.filter((b) => b.active).length === 1,
  seg.err || JSON.stringify(seg.bs.filter((b) => b.active).map((b) => b.cbox)));

group('默认档是黑色（清掉 localStorage 后）');
// 必须先删掉存储再刷新：默认值只在「从没选过 / 存了非法值」时生效，
// 否则测的是上一次跑残留的值。
const fresh = await (async () => {
  await ev(`(() => { localStorage.removeItem('cf_composer_box'); })()`);
  await send(ws, 'Page.reload');
  await sleep(2000);
  return ev(SAMPLE);
})();
check('无存储时落在黑色', /cbox-black/.test(fresh.cls), fresh.cls);
check('默认黑档底色为深色', fresh.bgLum !== null && fresh.bgLum < 0.05, fmt(fresh.bg) + ' L=' + (fresh.bgLum === null ? '?' : fresh.bgLum.toFixed(3)));
check('默认黑档正文对比度 ≥ 4.5:1', fresh.textRatio !== null && fresh.textRatio >= 4.5,
  fmt(fresh.text) + ' = ' + (fresh.textRatio === null ? '?' : fresh.textRatio.toFixed(2) + ':1'));

group('点击切换即时生效（不必刷新）');
const clicked = await ev(`(async () => {
  document.getElementById('open-settings').click();
  const seg = document.getElementById('composer-seg');
  const btn = [...seg.querySelectorAll('button')].find((b) => b.dataset.cbox === 'white');
  btn.click();
  await new Promise((r) => setTimeout(r, 120));
  const box = document.getElementById('composer');
  return {
    cls: box.className,
    stored: localStorage.getItem('cf_composer_box'),
    active: [...seg.querySelectorAll('button')].filter((b) => b.classList.contains('active')).map((b) => b.dataset.cbox),
  };
})()`);
check('点「白色」后 class 立刻切换', /cbox-white/.test(clicked.cls) && !/cbox-black/.test(clicked.cls), clicked.cls);
check('点「白色」后写入 localStorage', clicked.stored === 'white', String(clicked.stored));
check('点「白色」后分段控件高亮同步', (clicked.active || []).join() === 'white', JSON.stringify(clicked.active));

group('非法值落回默认（不给 #composer 挂野 class）');
// ⚠️ 用 CDP 的 Page.reload，不要在页面里 location.reload()：
// 页面内刷新会销毁执行上下文，Runtime.evaluate 里 awaitPromise 的 promise
// 永远不会落定 —— 整个脚本会静默挂死（踩过一次）。
await ev(`(() => { localStorage.setItem('cf_composer_box', 'chartreuse'); })()`);
await send(ws, 'Page.reload');
await sleep(2000);
const after = await ev(SAMPLE);
check('存了非法值时回落到默认黑色', /cbox-black/.test(after.cls), after.cls);
check('回落时底色是深色（不是透明，也不是野 class）', after.bgLum !== null && after.bgLum < 0.05, fmt(after.bg));

console.log('\n' + '-'.repeat(52));
console.log('通过 ' + passed.length + ' / 失败 ' + failed.length);
if (failed.length) { failed.forEach((f) => console.log('  - ' + f)); }
// 必须显式收尾：WebSocket 句柄会让事件循环一直活着，进程不退出，
// 于是看门狗在结果打完之后又空跑一次，把成功的运行报成超时。
clearTimeout(watchdog);
try { ws.close(); } catch (_) {}
process.exit(failed.length ? 1 : 0);
