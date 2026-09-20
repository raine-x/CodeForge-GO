// 全 UI 对比度审计：找出「文字看不清」的元素。
//
//   node test/verify_contrast.mjs
//
// 动机：深色主题是**覆盖 CSS 变量**实现的，凡是写死颜色的组件都不会跟着翻 ——
// 那类地方就是「白底白字」的温床，而且只在你恰好打开那个面板时才看得见。
// 与其一个个面板点开用肉眼看，不如遍历所有可见元素，逐个算 WCAG 对比度。
//
// 判据：
//   元素的 color  vs  它**背后真正生效的背景色**
//   背景 = 从自身向上逐层合成 backgroundColor（半透明按 alpha 混合），
//          全部透明则视为「背景图」——此时取该元素框内的众数色。
//
// 阈值：正文 ≥ 4.5，次要文字（--text-dim 系）≥ 3.0。
//
// ⚠️ **已知局限：本审计看不见 text-shadow**。
// 文字压在背景图上时，靠的是「深字 + 浅光晕」在字形周围造局部反差
// （见 app.css 的 body.has-bg 规则）。光晕是渲染效果，不在「颜色 vs 背景色」的模型里，
// 所以中间调区域仍会报 < 4.5 —— 那是**数学上限**：
// 在 rgb(120,120,134) 这类中调底上，深字 ≈3.2、浅字 ≈3.7，换任何颜色都到不了 4.5。
// 这类残留请**看截图判断**（test/shot_full_bg.mjs 会出侧栏 2x 近景），不要盲改颜色。
// 真正能彻底解决的只有「加遮罩」，但那会牺牲背景图的通透感 —— 属于产品取舍，不是 bug。

import fs from 'node:fs';

const PORT = process.env.CDP_PORT || 9244;
const BASE = 'http://127.0.0.1:' + PORT;
const IMG = process.env.TEST_IMG || '';
const MODE = process.env.MODE || 'dark';   // dark | light
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

// 页面内的审计函数：遍历可见元素，逐个算对比度
const AUDIT = `(async function(){
  function parseColor(s){
    s = String(s).trim();
    if (!s || s === 'transparent') return null;
    if (s.charAt(0) === '#') {
      var h = s.slice(1);
      if (h.length === 3) h = h[0]+h[0]+h[1]+h[1]+h[2]+h[2];
      if (h.length < 6) return null;
      return [parseInt(h.slice(0,2),16), parseInt(h.slice(2,4),16), parseInt(h.slice(4,6),16), 1];
    }
    var m = s.match(/([\\d.]+)/g);
    if (!m || m.length < 3) return null;
    return [+m[0], +m[1], +m[2], m.length > 3 ? +m[3] : 1];
  }
  function relLum(c){
    var f = function(v){ v/=255; return v <= 0.03928 ? v/12.92 : Math.pow((v+0.055)/1.055, 2.4); };
    return 0.2126*f(c[0]) + 0.7152*f(c[1]) + 0.0722*f(c[2]);
  }
  function ratio(a, b){
    var la = relLum(a), lb = relLum(b);
    var hi = Math.max(la, lb), lo = Math.min(la, lb);
    return (hi + 0.05) / (lo + 0.05);
  }
  // 从元素自身向上合成背景；遇到第一层不透明的就停。
  // 返回 null 表示「背景其实是背景图」→ 调用方改用截图采样。
  function composedBg(el){
    var hasBg = document.body.classList.contains('has-bg');
    var layers = [];
    var node = el;
    while (node && node.nodeType === 1) {
      // ⚠️ 开了背景图时，<html> 的背景会被 #bg-layer 整个盖住 —— 走到 body 就停。
      // 否则会把 html 的白色当成「真实背景」，于是所有压在图上的文字
      // 都被误判成「白底」，报出一堆假失败（本脚本第一版就这么错了）。
      if (hasBg && node === document.documentElement) break;
      var c = parseColor(getComputedStyle(node).backgroundColor);
      if (c && c[3] > 0) {
        layers.push(c);
        if (c[3] >= 1) break;          // 不透明，再往上也没意义
      }
      node = node.parentElement;
    }
    if (!layers.length || layers[layers.length - 1][3] < 1) return null;  // 没找到不透明底
    var base = [255, 255, 255];
    for (var i = layers.length - 1; i >= 0; i--) {
      var L = layers[i];
      base = [L[0]*L[3] + base[0]*(1-L[3]), L[1]*L[3] + base[1]*(1-L[3]), L[2]*L[3] + base[2]*(1-L[3])];
    }
    return [Math.round(base[0]), Math.round(base[1]), Math.round(base[2])];
  }
  // 直接渲染出的文字（排除纯容器）
  function hasOwnText(el){
    for (var i = 0; i < el.childNodes.length; i++) {
      var n = el.childNodes[i];
      if (n.nodeType === 3 && n.nodeValue && n.nodeValue.trim()) return true;
    }
    return false;
  }
  function visible(el){
    var cs = getComputedStyle(el);
    if (cs.display === 'none' || cs.visibility === 'hidden') return false;
    if (parseFloat(cs.opacity) < 0.15) return false;
    var r = el.getBoundingClientRect();
    return r.width > 1 && r.height > 1 && r.bottom > 0 && r.top < window.innerHeight;
  }
  function desc(el){
    var s = el.tagName.toLowerCase();
    if (el.id) s += '#' + el.id;
    var cls = (el.className && typeof el.className === 'string') ? el.className.trim().split(/\\s+/).slice(0, 2).join('.') : '';
    if (cls) s += '.' + cls;
    return s;
  }

  // 背景图区域的像素：**取元素框内的众数色**当作背景。
  // ⚠️ 不能只采一个点 —— 采到字形上就会得出「字色 == 底色、对比度 1.0」的假结果
  // （本脚本第一版就这么错了）。这里在框内打网格，量化后取出现最多的颜色。
  var _cv = null, _ctx = null;
  async function ensureCanvas(){
    if (_ctx) return true;
    if (!window.__shot) return false;
    var im = new Image(); im.src = window.__shot;
    await im.decode();
    _cv = document.createElement('canvas');
    _cv.width = im.naturalWidth; _cv.height = im.naturalHeight;
    _ctx = _cv.getContext('2d'); _ctx.drawImage(im, 0, 0);
    return true;
  }
  function dominantBg(r){
    var x0 = Math.max(0, Math.round(r.left)), y0 = Math.max(0, Math.round(r.top));
    var w = Math.max(1, Math.round(r.width)), h = Math.max(1, Math.round(r.height));
    w = Math.min(w, _cv.width - x0); h = Math.min(h, _cv.height - y0);
    if (w <= 0 || h <= 0) return null;
    var d = _ctx.getImageData(x0, y0, w, h).data;
    var tally = {};
    var step = 4 * Math.max(1, Math.floor(Math.sqrt(w * h / 400)));  // 最多约 400 个采样点
    for (var i = 0; i < d.length; i += step) {
      var key = (d[i] >> 4) + ',' + (d[i+1] >> 4) + ',' + (d[i+2] >> 4);
      if (!tally[key]) tally[key] = { n: 0, r: 0, g: 0, b: 0 };
      var t = tally[key];
      t.n++; t.r += d[i]; t.g += d[i+1]; t.b += d[i+2];
    }
    var best = null;
    for (var k in tally) if (!best || tally[k].n > best.n) best = tally[k];
    if (!best) return null;
    return [Math.round(best.r / best.n), Math.round(best.g / best.n), Math.round(best.b / best.n)];
  }

  var out = [];
  await ensureCanvas();
  var all = document.querySelectorAll('body *');
  for (var i = 0; i < all.length; i++) {
    var el = all[i];
    if (!visible(el) || !hasOwnText(el)) continue;
    var cs = getComputedStyle(el);
    var fg = parseColor(cs.color);
    if (!fg || fg[3] < 0.15) continue;   // 全透明文字（镜像层等）跳过
    var bg = composedBg(el);
    if (!bg) {
      // 一路透明 → 背后是背景图：取元素框内的众数色
      bg = dominantBg(el.getBoundingClientRect());
      if (!bg) continue;
    }
    var cr = ratio([fg[0], fg[1], fg[2]], bg);
    var dim = cs.color.indexOf('text-dim') >= 0;
    var threshold = dim ? 3.0 : 4.5;
    if (cr < threshold) {
      out.push({
        sel: desc(el),
        text: (el.textContent || '').trim().slice(0, 24),
        color: 'rgb(' + fg.slice(0,3) + ')',
        bg: 'rgb(' + bg + ')',
        contrast: +cr.toFixed(2),
        threshold: threshold
      });
    }
  }
  return out;
})()`;

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
await sleep(3000);

// 设图 + 亮度（dark 模式用低亮度触发 bg-dark）
await evaluate(ws, `fetch('/api/appearance', {
  method:'POST', headers:{'Content-Type':'application/json'},
  body: JSON.stringify({ background_path: ${JSON.stringify(IMG)}, brightness: ${MODE === 'dark' ? 45 : 100}, blur: 0 })
}).then(function(r){ return r.json(); })`);
await send(ws, 'Page.navigate', { url: 'http://127.0.0.1:8420/' });
await sleep(3500);

async function runPass(label, prepare) {
  await evaluate(ws, prepare);
  await sleep(900);
  const shot = await send(ws, 'Page.captureScreenshot', { format: 'png' });
  await evaluate(ws, `window.__shot = 'data:image/png;base64,${shot.data}'; true`);
  const bad = await evaluate(ws, AUDIT);
  const theme = await evaluate(ws, `document.body.classList.contains('bg-dark')`);
  console.log('\n' + label + '   （bg-dark=' + theme + '）');
  if (!bad.length) {
    console.log('  ✓ 没有对比度不达标的文字');
  } else {
    console.log('  ✗ 发现 ' + bad.length + ' 处对比度不足：');
    bad.forEach((b) => console.log('     ' + b.contrast + ' (< ' + b.threshold + ')  ' + b.sel +
      '  "' + b.text + '"  字 ' + b.color + ' / 底 ' + b.bg));
  }
  return bad;
}

let allBad = [];
allBad = allBad.concat(await runPass('① 主界面', 'true'));
allBad = allBad.concat(await runPass('② 设置面板 · 外观页',
  `(function(){
     var o = document.getElementById('open-settings');
     if (o) o.click();
     var nav = document.querySelector('.nav-item[data-page="appearance"]');
     if (nav) nav.click();
     return true;
   })()`));
allBad = allBad.concat(await runPass('③ 设置面板 · 模型页',
  `(function(){
     var nav = document.querySelector('.nav-item[data-page="models"]');
     if (nav) nav.click();
     return true;
   })()`));

console.log('\n' + '-'.repeat(52));
if (allBad.length) {
  console.log('合计 ' + allBad.length + ' 处对比度不足（模式=' + MODE + '）');
  process.exitCode = 1;
} else {
  console.log('全部通过：模式=' + MODE + ' 下没有对比度不足的文字');
}
ws.close();
