// 三档各截一张图，供人工核对（浅色主题下最容易看出「白底白字」）
//   node test/shoot_composer_box.mjs
import fs from 'fs';
const CDP = process.env.CDP_PORT || 'http://127.0.0.1:9333';
const APP = 'http://127.0.0.1:8420/';
const OUT = 'test';
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
  throw new Error('找不到页面 target');
}
const t = await target();
const ws = await new Promise((res, rej) => {
  const s = new WebSocket(t.webSocketDebuggerUrl);
  s.onopen = () => res(s); s.onerror = () => rej(new Error('CDP 连接失败'));
});
let seq = 0; const waiting = new Map();
ws.onmessage = (e) => {
  const m = JSON.parse(e.data);
  if (m.id && waiting.has(m.id)) { const w = waiting.get(m.id); waiting.delete(m.id); m.error ? w.rej(new Error(m.error.message)) : w.res(m.result); }
};
const send = (method, params) => new Promise((res, rej) => { const id = ++seq; waiting.set(id, { res, rej }); ws.send(JSON.stringify({ id, method, params: params || {} })); });
const ev = async (expression) => {
  const r = await send('Runtime.evaluate', { expression, returnByValue: true, awaitPromise: true });
  if (r.exceptionDetails) throw new Error('页面内异常: ' + JSON.stringify(r.exceptionDetails.text));
  return r.result.value;
};

await send('Page.enable');
await send('Emulation.setDeviceMetricsOverride', { width: 1280, height: 800, deviceScaleFactor: 1, mobile: false });

for (const v of ['black', 'white', 'clear']) {
  await ev(`(() => { localStorage.setItem('cf_composer_box', ${JSON.stringify(v)}); })()`);
  await send('Page.navigate', { url: APP });
  await sleep(2000);
  // 往输入框里打点字，截图才有内容可看
  await ev(`(() => {
    const ta = document.getElementById('input');
    ta.value = '在这里输入文字，看看底色与文字的对比';
    ta.dispatchEvent(new Event('input', { bubbles: true }));
  })()`);
  await sleep(500);
  const r = await send('Page.captureScreenshot', { format: 'png' });
  fs.writeFileSync(`${OUT}/cbox-${v}.png`, Buffer.from(r.data, 'base64'));
  console.log('已写 test/cbox-' + v + '.png');
}
try { ws.close(); } catch (_) {}
process.exit(0);
