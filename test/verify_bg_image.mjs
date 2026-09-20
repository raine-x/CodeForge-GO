// 验证「自定义背景图真的可见」：图层状态 + 图片可解码 + 截图
//
//   node test/verify_bg_image.mjs      # 需与「起服务 / 设背景图 / Chrome」在同一次 Bash 调用
//
// 前次只验证了前置条件（body 透明、z-index=0），那只能说明「图层没被遮住」，
// 不能说明「图真的画出来了」。这里补上决定性证据：截图 + 图像可解码。

import fs from 'node:fs';

const PORT = process.env.CDP_PORT || 9226;
const BASE = 'http://127.0.0.1:' + PORT;
const OUT = process.env.SHOT_DIR || '.';
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

console.log('\n背景图层运行时状态');
const st = await evaluate(ws, `(async function(){
  var bl = document.getElementById('bg-layer');
  var cs = getComputedStyle(bl);
  var bodyCs = getComputedStyle(document.body);
  // 决定性证据之一：这个 URL 真的能解码出一张图（而不是 404 / 空响应）
  var ok = false, nw = 0, nh = 0, err = '';
  try {
    var im = new Image();
    im.src = '/api/appearance/background?probe=' + Date.now();
    await im.decode();
    ok = true; nw = im.naturalWidth; nh = im.naturalHeight;
  } catch (e) { err = String(e); }
  return {
    cls: bl.className,
    display: cs.display,
    visibility: cs.visibility,
    opacity: cs.opacity,
    zIndex: cs.zIndex,
    bgImage: cs.backgroundImage.slice(0, 60),
    filter: cs.filter,
    bgSize: cs.backgroundSize,
    bodyBg: bodyCs.backgroundColor,
    bodyHasBgClass: document.body.classList.contains('has-bg'),
    decodable: ok, natW: nw, natH: nh, decodeErr: err
  };
})()`);
console.log('     ' + JSON.stringify(st));

ck('图层带 .on（display:block）', st.cls.includes('on') && st.display === 'block', st.cls + '/' + st.display);
ck('图层可见（visibility:visible, opacity:1）', st.visibility === 'visible' && st.opacity === '1',
  st.visibility + '/' + st.opacity);
ck('图层已挂上背景图 URL', /url\(/.test(st.bgImage), st.bgImage);
// ⚠️ z-index 必须是负值：写成 0 会让图层盖住侧栏底色（曾实际踩到）
ck('图层 z-index=-1 且 body 透明（不被遮住，也不盖面板）',
  st.zIndex === '-1' && st.bodyBg === 'rgba(0, 0, 0, 0)', st.zIndex + '/' + st.bodyBg);
ck('body 带 has-bg（主区已透明化让图透出）', st.bodyHasBgClass === true, st.bodyHasBgClass);
ck('模糊/亮度滤镜已生效', /blur\(/.test(st.filter) && /brightness\(/.test(st.filter), st.filter);
ck('背景图 URL 真的能解码出图像（不是 404）', st.decodable === true,
  st.natW + 'x' + st.natH + (st.decodeErr ? ' err=' + st.decodeErr : ''));

// 截图：这是最终、最直观的证据 —— 看一眼就知道图有没有显示出来
const shot = await send(ws, 'Page.captureScreenshot', { format: 'png' });
const p = OUT.replace(/[\\/]+$/, '') + '/cf-ui-bgimage.png';
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
