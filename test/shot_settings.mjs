// 截设置面板 / 弹层（带背景图）——检查全局光晕在「不透明卡片」上有没有副作用。
//
//   node test/shot_settings.mjs
//
// 背景：`body.has-bg { text-shadow: ... }` 是**全局**的，卡片（设置面板、输入卡、
// 弹窗）上的文字也会带上光晕。理论上在浅底/深底卡片上光晕几乎不可见，
// 但这种事只能看图，不能靠推理。

import fs from 'node:fs';

const PORT = process.env.CDP_PORT || 9260;
const BASE = 'http://127.0.0.1:' + PORT;
const OUT = process.env.SHOT_DIR || '.';
const IMG = process.env.TEST_IMG || '';
const MODE = process.env.MODE || 'light';
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

await evaluate(ws, `fetch('/api/appearance', {
  method:'POST', headers:{'Content-Type':'application/json'},
  body: JSON.stringify({ background_path: ${JSON.stringify(IMG)}, brightness: ${MODE === 'dark' ? 45 : 100}, blur: 0 })
}).then(function(r){ return r.json(); })`);
await send(ws, 'Page.navigate', { url: 'http://127.0.0.1:8420/' });
await sleep(3500);

// 打开设置 → 外观页
await evaluate(ws, `(function(){
  var o = document.getElementById('open-settings');
  if (o) o.click();
  var nav = document.querySelector('.nav-item[data-page="appearance"]');
  if (nav) nav.click();
  return true;
})()`);
await sleep(1200);

const state = await evaluate(ws, `(function(){
  var ov = document.getElementById('settings-overlay');
  return {
    open: ov && !ov.classList.contains('hidden'),
    bgDark: document.body.classList.contains('bg-dark'),
    hasBg: document.body.classList.contains('has-bg'),
    shadow: getComputedStyle(document.body).textShadow
  };
})()`);
console.log('状态: ' + JSON.stringify(state));

const shot = await send(ws, 'Page.captureScreenshot', { format: 'png' });
const p = OUT.replace(/[\\/]+$/, '') + '/cf-ui-settings-' + MODE + '.png';
fs.writeFileSync(p, Buffer.from(shot.data, 'base64'));
console.log('截图: ' + p);
ws.close();
