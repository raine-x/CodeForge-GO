// 验证「深浅色自适应」：背景图偏暗时文字必须转白，且对比度达标。
//
//   node test/verify_bg_theme.mjs
//
// 判据不是「滑条值」而是**实际对比度**：取文字颜色与它背后真正渲染出来的像素，
// 算 WCAG 对比度（AA 正文要求 ≥ 4.5）。这样即使以后改了阈值/配色，
// 也能立刻看出「文字到底还读不读得出来」，而不是靠肉眼看截图猜。
//
// 需要环境变量 DARK_IMG / LIGHT_IMG 指向两张测试图（深色、浅色各一张）。

import fs from 'node:fs';

const PORT = process.env.CDP_PORT || 9236;
const BASE = 'http://127.0.0.1:' + PORT;
const OUT = process.env.SHOT_DIR || '.';
const DARK_IMG = process.env.DARK_IMG || '';
const LIGHT_IMG = process.env.LIGHT_IMG || '';
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
// ⚠️ 必须先导航到应用再调 fetch：页面还是 about:blank 时相对 URL 解析不了，
// 会报 "Failed to parse URL from /api/appearance"（本脚本第一版就踩了）。
await send(ws, 'Page.navigate', { url: 'http://127.0.0.1:8420/' });
await sleep(3000);

// 采样：读「文字颜色」（CSS 变量）+「文字背后的真实像素」，算 WCAG 对比度。
const PROBE = `(async function(){
  // ⚠️ CSS 变量拿到的是 **hex**（#eef0f6），不能直接抠数字 ——
  // 那样会把 "eef0f6" 里的 0 和 6 当成 RGB 分量（本脚本第一版就是这么错的，
  // 输出 rgb(0,6,undefined)，对比度全成了 null）。hex 与 rgb() 都要认。
  function parseColor(s){
    s = String(s).trim();
    if (s.charAt(0) === '#') {
      var h = s.slice(1);
      if (h.length === 3) h = h[0]+h[0]+h[1]+h[1]+h[2]+h[2];
      if (h.length < 6) return null;
      return [parseInt(h.slice(0,2),16), parseInt(h.slice(2,4),16), parseInt(h.slice(4,6),16)];
    }
    var m = s.match(/(\\d+(?:\\.\\d+)?)/g);
    return (m && m.length >= 3) ? [+m[0], +m[1], +m[2]] : null;
  }
  function relLum(c){
    var f = function(v){ v/=255; return v <= 0.03928 ? v/12.92 : Math.pow((v+0.055)/1.055, 2.4); };
    return 0.2126*f(c[0]) + 0.7152*f(c[1]) + 0.0722*f(c[2]);
  }
  function ratio(a, b){
    if (!a || !b) return null;
    var la = relLum(a), lb = relLum(b);
    var hi = Math.max(la, lb), lo = Math.min(la, lb);
    return (hi + 0.05) / (lo + 0.05);
  }
  // 用页面自己的解码器把截图读进 canvas，取指定点像素
  async function px(x, y){
    var im = new Image();
    im.src = window.__shot;
    await im.decode();
    var cv = document.createElement('canvas');
    cv.width = im.naturalWidth; cv.height = im.naturalHeight;
    var ctx = cv.getContext('2d'); ctx.drawImage(im, 0, 0);
    var d = ctx.getImageData(x, y, 1, 1).data;
    return [d[0], d[1], d[2]];
  }
  var cs = getComputedStyle(document.body);
  var sb = document.getElementById('sidebar').getBoundingClientRect();
  var mn = document.getElementById('main').getBoundingClientRect();
  // 侧栏空白处（避开胶囊/按钮）：取靠近底部、只有文字和背景的位置
  var bgSidebar = await px(Math.round(sb.left + sb.width/2), Math.round(sb.top + sb.height*0.62));
  var bgMain    = await px(Math.round(mn.right - 60), Math.round(mn.top + mn.height*0.55));
  var text = parseColor(cs.getPropertyValue('--text'));
  var dim  = parseColor(cs.getPropertyValue('--text-dim'));
  return {
    bgDark: document.body.classList.contains('bg-dark'),
    hasBg: document.body.classList.contains('has-bg'),
    text: text, dim: dim, bgSidebar: bgSidebar, bgMain: bgMain,
    cTextSidebar: ratio(text, bgSidebar) === null ? null : +ratio(text, bgSidebar).toFixed(2),
    cDimSidebar:  ratio(dim,  bgSidebar) === null ? null : +ratio(dim,  bgSidebar).toFixed(2),
    cTextMain:    ratio(text, bgMain)    === null ? null : +ratio(text, bgMain).toFixed(2)
  };
})()`;

async function scenario(label, img, brightness) {
  // 设图 + 亮度，然后重载让前端按正常流程走一遍
  await evaluate(ws, `fetch('/api/appearance', {
    method:'POST', headers:{'Content-Type':'application/json'},
    body: JSON.stringify({ background_path: ${JSON.stringify(img)}, brightness: ${brightness}, blur: 0 })
  }).then(function(r){ return r.json(); })`);
  await send(ws, 'Page.navigate', { url: 'http://127.0.0.1:8420/' });
  await sleep(3200);

  const shot = await send(ws, 'Page.captureScreenshot', { format: 'png' });
  await evaluate(ws, `window.__shot = 'data:image/png;base64,${shot.data}'; true`);
  const r = await evaluate(ws, PROBE);
  r.shot = shot.data;
  console.log('     ' + JSON.stringify({
    bgDark: r.bgDark, text: r.text, bg: r.bgSidebar,
    cTextSidebar: r.cTextSidebar, cDimSidebar: r.cDimSidebar,
  }));
  return r;
}

const results = {};

// ① 深色图 + 满亮度：页面本来就是暗的 → 必须切深色主题
console.log('\n① 深色图 + 亮度 100（页面本就暗）');
results.darkFull = await scenario('dark/100', DARK_IMG, 100);
ck('切到深色主题（bg-dark）', results.darkFull.bgDark === true, results.darkFull.bgDark);
ck('文字颜色为浅色（白系）', results.darkFull.text[0] > 200 && results.darkFull.text[2] > 200,
  'rgb(' + results.darkFull.text + ')');
ck('正文对比度 ≥ 4.5（WCAG AA）', results.darkFull.cTextSidebar >= 4.5, results.darkFull.cTextSidebar);
ck('次要文字对比度 ≥ 3.0', results.darkFull.cDimSidebar >= 3.0, results.darkFull.cDimSidebar);

// ② 浅色图 + 满亮度：页面是亮的 → 必须保持浅色主题（深色文字）
console.log('\n② 浅色图 + 亮度 100（页面是亮的）');
results.lightFull = await scenario('light/100', LIGHT_IMG, 100);
ck('保持浅色主题（不切 bg-dark）', results.lightFull.bgDark === false, results.lightFull.bgDark);
ck('文字颜色为深色', results.lightFull.text[0] < 80, 'rgb(' + results.lightFull.text + ')');
ck('正文对比度 ≥ 4.5', results.lightFull.cTextSidebar >= 4.5, results.lightFull.cTextSidebar);

// ③ 用户核心诉求：亮度调到很低 → 页面变暗 → 文字必须转白
//    用浅色图 + 低亮度，最能体现「图本身不暗，是亮度把它压暗了」
console.log('\n③ 浅色图 + 亮度 20（亮度压到最低 —— 用户核心诉求）');
results.lightLow = await scenario('light/20', LIGHT_IMG, 20);
ck('切到深色主题（bg-dark）', results.lightLow.bgDark === true, results.lightLow.bgDark);
ck('文字转为浅色（白系）', results.lightLow.text[0] > 200 && results.lightLow.text[2] > 200,
  'rgb(' + results.lightLow.text + ')');
ck('正文对比度 ≥ 4.5', results.lightLow.cTextSidebar >= 4.5, results.lightLow.cTextSidebar);
ck('次要文字对比度 ≥ 3.0', results.lightLow.cDimSidebar >= 3.0, results.lightLow.cDimSidebar);

// ④ 深色图 + 低亮度：双重压暗，同样要白字
console.log('\n④ 深色图 + 亮度 20');
results.darkLow = await scenario('dark/20', DARK_IMG, 20);
ck('切到深色主题', results.darkLow.bgDark === true, results.darkLow.bgDark);
ck('正文对比度 ≥ 4.5', results.darkLow.cTextSidebar >= 4.5, results.darkLow.cTextSidebar);

// 截图（深色主题下的两个极端，便于肉眼看）
for (const [name, r] of [['cf-ui-dark-theme.png', results.lightLow], ['cf-ui-light-theme.png', results.lightFull]]) {
  fs.writeFileSync(OUT.replace(/[\\/]+$/, '') + '/' + name, Buffer.from(r.shot, 'base64'));
  console.log('     截图: ' + name);
}

console.log('\n' + '-'.repeat(52));
if (failed.length) {
  console.log('失败 ' + failed.length + ' 项 / 通过 ' + passed.length + ' 项');
  failed.forEach((f) => console.log('  - ' + f));
  process.exitCode = 1;
} else {
  console.log('全部通过（' + passed.length + ' 项）');
}
ws.close();
