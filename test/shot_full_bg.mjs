// 全页截图（带背景图）—— 用于**肉眼看可读性**，不做断言。
//
//   node test/shot_full_bg.mjs
//
// 纯色测试图能验证「图有没有透出来」，但判断不了「文字压在图上看不看得清」。
// 换一张明暗渐变的图截全页，才能看出侧栏/主区的文字在各区域是否还读得出来。

import fs from 'node:fs';

const PORT = process.env.CDP_PORT || 9234;
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

// 用接口设背景（这条路径本身已单独验证过），再重新加载让前端按正常流程应用
await evaluate(ws, `fetch('/api/appearance', {
  method:'POST', headers:{'Content-Type':'application/json'},
  body: JSON.stringify({ background_path: ${JSON.stringify(IMG)}, brightness: 100, blur: 0 })
}).then(function(r){ return r.json(); })`);
await send(ws, 'Page.navigate', { url: 'http://127.0.0.1:8420/' });
await sleep(3500);

// 顺便量一下侧栏文字的**实际渲染对比度**：在侧栏区域内找最亮/最暗像素，
// 两者差太小说明文字和背景糊在一起了（粗略但有效）。
const shot = await send(ws, 'Page.captureScreenshot', { format: 'png' });
const dataUrl = 'data:image/png;base64,' + shot.data;
const contrast = await evaluate(ws, `(async function(){
  var im = new Image(); im.src = ${JSON.stringify(dataUrl)};
  await im.decode();
  var cv = document.createElement('canvas');
  cv.width = im.naturalWidth; cv.height = im.naturalHeight;
  var ctx = cv.getContext('2d'); ctx.drawImage(im, 0, 0);
  function lum(d, i){ return 0.2126*d[i] + 0.7152*d[i+1] + 0.0722*d[i+2]; }
  function region(x, y, w, h){
    var d = ctx.getImageData(x, y, w, h).data;
    var lo = 255, hi = 0;
    // ⚠️ 必须按 i 取像素：写成 lum(d) 会永远读第 0 个像素，
    // 结果 lo===hi 看着像「区域纯色」，其实是量错了（本脚本第一版就踩了）。
    for (var i = 0; i < d.length; i += 4) { var L = lum(d, i); if (L < lo) lo = L; if (L > hi) hi = L; }
    return { lo: Math.round(lo), hi: Math.round(hi), range: Math.round(hi - lo) };
  }
  var sb = document.getElementById('sidebar').getBoundingClientRect();
  var mn = document.getElementById('main').getBoundingClientRect();
  return {
    viewport: [window.innerWidth, window.innerHeight],
    sidebar: region(Math.round(sb.left)+8, Math.round(sb.top)+60, Math.round(sb.width)-16, Math.round(sb.height)-120),
    main: region(Math.round(mn.left)+40, Math.round(mn.top)+40, Math.round(mn.width)-80, Math.round(mn.height)-260)
  };
})()`);
console.log('视口: ' + JSON.stringify(contrast.viewport));
console.log('侧栏区域亮度范围: ' + JSON.stringify(contrast.sidebar) + '   （range 越小 = 文字与背景越糊）');
console.log('主区区域亮度范围: ' + JSON.stringify(contrast.main));

const p = OUT.replace(/[\\/]+$/, '') + '/cf-ui-full-bg.png';
fs.writeFileSync(p, Buffer.from(shot.data, 'base64'));
console.log('全页截图: ' + p);

// 再截一张侧栏的近景（放大 2 倍），肉眼看文字到底还读不读得出来
const sbRect = await evaluate(ws, `(function(){
  var r = document.getElementById('sidebar').getBoundingClientRect();
  return { x: Math.round(r.left), y: Math.round(r.top), w: Math.round(r.width), h: Math.round(r.height) };
})()`);
const clip = await send(ws, 'Page.captureScreenshot', {
  format: 'png',
  clip: { x: sbRect.x, y: sbRect.y, width: sbRect.w, height: sbRect.h, scale: 2 },
});
const p2 = OUT.replace(/[\\/]+$/, '') + '/cf-ui-sidebar-zoom.png';
fs.writeFileSync(p2, Buffer.from(clip.data, 'base64'));
console.log('侧栏近景(2x): ' + p2);
ws.close();
