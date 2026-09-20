// 外观页全量审计：每个控件是否「真的生效 + 真的记住」。
//
//   node test/verify_appearance.mjs
//
// 起因：背景图这一个功能就连续暴露了两个 bug（图层被遮、选完不落库），
// 所以把同一页的其余控件也逐个走一遍，看有没有同类问题（静默无效 / 不持久化）。
//
// 覆盖：皮肤、液态玻璃、对话框宽/高、背景模糊/亮度、以及
//       「开了背景图之后哪些面板是透明的」（此前遗留的疑问）。

import fs from 'node:fs';

const PORT = process.env.CDP_PORT || 9229;
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

// 采样屏幕像素。做法是把截图当 data URL 塞回页面，用浏览器自己的解码器
// 画到 canvas 上再 getImageData —— 省得在 Node 里手写 PNG 解码（含各种 filter）。
//
// ⚠️ 为什么必须看像素：`getComputedStyle(#sidebar).backgroundColor` 返回的是
// **不透明浅灰**，但 #bg-layer 若 z-index 不为负，就会在绘制顺序上盖住它 ——
// 计算样式完全看不出来，只有像素能揭穿（2026-09-19 就是这么发现侧栏被铺满的）。
async function samplePixels(ws, points) {
  const shot = await send(ws, 'Page.captureScreenshot', { format: 'png' });
  const dataUrl = 'data:image/png;base64,' + shot.data;
  return evaluate(ws, `(async function(){
    var pts = ${JSON.stringify(points)};
    var im = new Image();
    im.src = ${JSON.stringify(dataUrl)};
    await im.decode();
    var cv = document.createElement('canvas');
    cv.width = im.naturalWidth; cv.height = im.naturalHeight;
    var ctx = cv.getContext('2d');
    ctx.drawImage(im, 0, 0);
    return { size: [im.naturalWidth, im.naturalHeight], px: pts.map(function(p){
      var d = ctx.getImageData(p[0], p[1], 1, 1).data;
      return { x: p[0], y: p[1], r: d[0], g: d[1], b: d[2] };
    })};
  })()`);
}

// 品红的判据：R 与 B 都远高于 G（测试图是 #FF00FF，被 brightness 压暗后仍是紫红色）
const isMagenta = (p) => p.g < 80 && p.r > 60 && p.b > 60 && Math.abs(p.r - p.b) < 60;

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
await sleep(3500);

// ---------------------------------------------------------------------------
console.log('\n① 皮肤分段选择');
const skinBefore = await evaluate(ws, `localStorage.getItem('cf_skin') || '(未设置)'`);
const target = await evaluate(ws, `(function(){
  var btns = document.querySelectorAll('#skin-seg button');
  var other = null;
  for (var i = 0; i < btns.length; i++) {
    if (btns[i].dataset.skin !== (localStorage.getItem('cf_skin') || 'meteor')) { other = btns[i]; break; }
  }
  if (!other) return null;
  var name = other.dataset.skin;
  other.click();
  return name;
})()`);
await sleep(400);
const skinAfter = await evaluate(ws, `(function(){
  var active = document.querySelector('#skin-seg button.active');
  return {
    stored: localStorage.getItem('cf_skin'),
    activeBtn: active ? active.dataset.skin : null,
    sliderClass: (document.getElementById('level-slider') || {}).className || ''
  };
})()`);
console.log('     ' + skinBefore + ' → ' + JSON.stringify(skinAfter));
ck('点皮肤按钮后写入 localStorage', target !== null && skinAfter.stored === target,
  skinBefore + ' → ' + skinAfter.stored);
ck('分段控件选中态跟着切', skinAfter.activeBtn === target, skinAfter.activeBtn);
ck('滑条按皮肤重建（skin-forge 类）',
  target !== 'forge' || /skin-forge/.test(skinAfter.sliderClass), skinAfter.sliderClass);

// ---------------------------------------------------------------------------
console.log('\n② 液态玻璃开关');
const lg = await evaluate(ws, `(function(){
  var el = document.getElementById('opt-liquid-glass');
  var was = el.checked;
  el.checked = !was;
  el.dispatchEvent(new Event('change', { bubbles: true }));
  return {
    toggled: !was,
    bodyClass: document.body.classList.contains('liquid-glass'),
    stored: localStorage.getItem('cf_liquid_glass'),
    checked: el.checked
  };
})()`);
console.log('     ' + JSON.stringify(lg));
ck('切换后 body 类跟着变', lg.bodyClass === lg.toggled, 'toggled=' + lg.toggled + ' class=' + lg.bodyClass);
ck('状态写入 localStorage', lg.stored === (lg.toggled ? '1' : '0'), lg.stored);
// 还原为关闭，避免影响后续断言
await evaluate(ws, `(function(){
  var el = document.getElementById('opt-liquid-glass');
  el.checked = false; el.dispatchEvent(new Event('change', { bubbles: true }));
})(); true`);

// ---------------------------------------------------------------------------
console.log('\n③ 对话框宽度 / 高度滑条');
const sz = await evaluate(ws, `(function(){
  var w = document.getElementById('opt-comp-w'), h = document.getElementById('opt-comp-h');
  var w2 = w.value === '480' ? '1000' : '480';
  var h2 = h.value === '44' ? '200' : '44';
  w.value = w2; w.dispatchEvent(new Event('input', { bubbles: true }));
  h.value = h2; h.dispatchEvent(new Event('input', { bubbles: true }));
  var cs = getComputedStyle(document.documentElement);
  return {
    w2: w2, h2: h2,
    varW: cs.getPropertyValue('--composer-max-w').trim(),
    varH: cs.getPropertyValue('--composer-min-h').trim(),
    lsW: localStorage.getItem('cf_composer_w'),
    lsH: localStorage.getItem('cf_composer_h')
  };
})()`);
console.log('     ' + JSON.stringify(sz));
ck('宽度写入 CSS 变量 --composer-max-w', sz.varW === sz.w2 + 'px', sz.varW);
ck('高度写入 CSS 变量 --composer-min-h', sz.varH === sz.h2 + 'px', sz.varH);
ck('宽高都记住了', sz.lsW === sz.w2 && sz.lsH === sz.h2, sz.lsW + '/' + sz.lsH);

// ---------------------------------------------------------------------------
console.log('\n④ 背景模糊 / 亮度（即时生效 + 落库）');
const fx = await evaluate(ws, `(async function(){
  var b = document.getElementById('opt-bg-blur'), br = document.getElementById('opt-bg-bright');
  var b2 = b.value === '40' ? '12' : '40';
  b.value = b2; b.dispatchEvent(new Event('input', { bubbles: true }));
  br.value = '88'; br.dispatchEvent(new Event('input', { bubbles: true }));
  await new Promise(function (r) { setTimeout(r, 900); });   // 等 400ms 防抖落库
  var resp = await fetch('/api/appearance');
  var d = await resp.json();
  return {
    b2: b2, lsBlur: localStorage.getItem('cf_bg_blur'), lsBright: localStorage.getItem('cf_bg_bright'),
    srvBlur: d.blur, srvBright: d.brightness,
    layerFilter: getComputedStyle(document.getElementById('bg-layer')).filter
  };
})()`);
console.log('     ' + JSON.stringify(fx));
ck('模糊即时写进图层滤镜', /blur\(/.test(fx.layerFilter), fx.layerFilter);
ck('模糊值写 localStorage', fx.lsBlur === fx.b2, fx.lsBlur);
ck('模糊值落库到服务端', String(fx.srvBlur) === fx.b2, 'srv=' + fx.srvBlur);
ck('亮度落库到服务端', String(fx.srvBright) === '88', 'srv=' + fx.srvBright);

// ---------------------------------------------------------------------------
console.log('\n⑤ 设背景图后，哪些面板是透明的？（此前遗留的疑问）');
if (IMG) {
  await evaluate(ws, `fetch('/api/appearance', {
    method:'POST', headers:{'Content-Type':'application/json'},
    body: JSON.stringify({ background_path: ${JSON.stringify(IMG)} })
  }).then(function(r){ return r.json(); })`);
  await send(ws, 'Page.navigate', { url: 'http://127.0.0.1:8420/' });
  await sleep(3500);

  const panels = await evaluate(ws, `(function(){
    var ids = ['sidebar', 'main', 'app', 'messages'];
    var out = { hasBg: document.body.classList.contains('has-bg'),
                layerOn: document.getElementById('bg-layer').classList.contains('on') };
    ids.forEach(function (id) {
      var el = document.getElementById(id);
      out[id] = el ? getComputedStyle(el).backgroundColor : '(缺失)';
    });
    return out;
  })()`);
  console.log('     ' + JSON.stringify(panels));

  const opaque = (v) => v !== 'rgba(0, 0, 0, 0)';
  ck('背景图已开启（has-bg + 图层 on）', panels.hasBg && panels.layerOn,
    panels.hasBg + '/' + panels.layerOn);
  // 用户要求左右两块面板都透明，让背景图铺满整个窗口
  ck('#main 透明（让背景图透出）', !opaque(panels.main), panels.main);
  ck('#app 透明', !opaque(panels.app), panels.app);
  ck('#sidebar 透明（与主区一致）', !opaque(panels.sidebar), panels.sidebar);

  // ---- 像素级验证：计算样式会说谎，绘制顺序只能看像素 ----
  const geom = await evaluate(ws, `(function(){
    var sb = document.getElementById('sidebar').getBoundingClientRect();
    var mn = document.getElementById('main').getBoundingClientRect();
    return { sb: [sb.left, sb.top, sb.right, sb.bottom], mn: [mn.left, mn.top, mn.right, mn.bottom] };
  })()`);
  const midY = Math.round((geom.sb[1] + geom.sb[3]) / 2);
  const pts = [
    [Math.round(geom.sb[0] + geom.sb[2]) / 2 | 0, midY],          // 侧栏中部
    [Math.round(geom.sb[2]) - 8, midY],                            // 侧栏右缘内侧
    [Math.round(geom.mn[2]) - 40, Math.round((geom.mn[1] + geom.mn[3]) / 2)], // 主区右侧空白
  ];
  const sampled = await samplePixels(ws, pts);
  const rgb = (p) => 'rgb(' + [p.r, p.g, p.b] + ')';
  console.log('     采样: ' + JSON.stringify(sampled.px));
  ck('侧栏中部**实际透出**背景图（与主区一致）', isMagenta(sampled.px[0]), rgb(sampled.px[0]));
  ck('侧栏右缘内侧同样透出背景图', isMagenta(sampled.px[1]), rgb(sampled.px[1]));
  ck('主区空白处透出背景图', isMagenta(sampled.px[2]), rgb(sampled.px[2]));

  // 侧栏区域的裁剪截图，便于肉眼确认文字仍可读
  const shot = await send(ws, 'Page.captureScreenshot', {
    format: 'png', clip: { x: 0, y: 0, width: 260, height: 320, scale: 1 },
  });
  const p = OUT.replace(/[\\/]+$/, '') + '/cf-ui-sidebar-clip.png';
  fs.writeFileSync(p, Buffer.from(shot.data, 'base64'));
  console.log('     侧栏裁剪截图: ' + p);
} else {
  console.log('     （未提供 TEST_IMG，跳过）');
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
