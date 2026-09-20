// 切换会话的 DOM 实测：点击后侧栏到底有没有 .active？以及切换耗时。
//
//   node test/probe_switch_dom.mjs <session_id>
//
// 背景：renderSessions 是按 `s.id === sessionID` 给 li 加 .active 的。
// 探针已确认服务端回的 session_id 与请求一致，所以要么是重绘时机，
// 要么是「加了类但没有对应样式」。这里直接把 DOM 状态和耗时打出来。

import fs from 'node:fs';

const PORT = process.env.CDP_PORT || 9270;
const BASE = 'http://127.0.0.1:' + PORT;
const SID = process.argv[2] || '';
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

// ⚠️ 必须在页面脚本之前包住 WebSocket：ui.js 用 `ws.onmessage = fn` 赋值，
// 页面加载后再挂钩就抓不到入站帧了。收发都记，才能区分「服务端没回」与「回了但没重绘」。
await send(ws, 'Page.addScriptToEvaluateOnNewDocument', {
  source: `(function(){
    window.__out = []; window.__in = [];
    var Orig = window.WebSocket;
    function Patched(url, protocols) {
      var inst = protocols === undefined ? new Orig(url) : new Orig(url, protocols);
      window.__wsInst = inst;
      inst.addEventListener('message', function (ev) {
        try { window.__in.push(String(ev.data).slice(0, 120)); } catch (e) {}
      });
      return inst;
    }
    Patched.prototype = Orig.prototype;
    // ⚠️ 必须把静态常量一并搬过来！应用里写的是 ws.readyState !== WebSocket.OPEN，
    // 只换构造函数不复制 OPEN/CONNECTING/…，这些常量会变成 undefined，
    // 于是 wsSend 永远 return false —— 整个页面再也发不出任何消息。
    // 探针第一版就栽在这：把「发送被拦」误判成了应用 bug。
    ['CONNECTING', 'OPEN', 'CLOSING', 'CLOSED'].forEach(function (k) {
      Patched[k] = Orig[k];
    });
    window.WebSocket = Patched;
    var origSend = Orig.prototype.send;
    Orig.prototype.send = function (d) {
      try { window.__out.push(String(d).slice(0, 120)); } catch (e) {}
      return origSend.apply(this, arguments);
    };
  })();`,
});

await send(ws, 'Page.navigate', { url: 'http://127.0.0.1:8420/' });
await sleep(3500);

const dump = `(function(){
  var out = [];
  document.querySelectorAll('.session-item').forEach(function (li) {
    out.push({ id: li.dataset.id, text: (li.textContent || '').trim().slice(0, 12),
               cls: li.className,
               bg: getComputedStyle(li).backgroundColor,
               color: getComputedStyle(li).color });
  });
  return out;
})()`;

console.log('=== 切换前 ===');
(await evaluate(ws, dump)).forEach((r) => console.log('  ' + JSON.stringify(r)));

// 页面自打开以来收到的全部事件（看 ready 里有没有 sessions、history 有没有来过）
const inbound = await evaluate(ws, `window.__in.map(function(s){
  try { var o = JSON.parse(s); return o.type + (o.session_id ? ':' + o.session_id : '') + (o.items ? ' items=' + o.items.length : '') + (o.sessions ? ' sessions=' + o.sessions.length : '') + (o.messages ? ' msgs=' + o.messages.length : ''); }
  catch (e) { return '?' + s.slice(0, 40); }
})`);
const outbound = await evaluate(ws, `window.__out.map(function(s){
  try { var o = JSON.parse(s); return o.type + (o.session_id ? ':' + o.session_id : ''); }
  catch (e) { return '?' + s.slice(0, 40); }
})`);
console.log('\n=== 页面自打开以来的收发 ===');
console.log('  收: ' + JSON.stringify(inbound));
console.log('  发: ' + JSON.stringify(outbound));

// 记录点击 → 消息区重建完成 的耗时；同时抓「客户端到底有没有把 load_session 发出去」
// —— 这能区分两种完全不同的故障：
//   a) 点击被 loadSession 的守卫拦下（sending/sessionChanging/workspaceChanging）→ 根本没发
//   b) 发出去了但服务端没回 / 回了但没重绘
// 抓异常：replayHistory 里 sessionID 已更新、消息也重建了，但末尾的 renderSessions 似乎没跑到。
// 若中途抛异常，就能解释「会话切了但侧栏高亮不动」。
await send(ws, 'Runtime.enable');
const exceptions = [];
ws.addEventListener('message', (ev) => {
  let m;
  try { m = JSON.parse(ev.data); } catch (e) { return; }
  if (m.method === 'Runtime.exceptionThrown') {
    const d = m.params && m.params.exceptionDetails;
    exceptions.push((d && (d.exception && d.exception.description || d.text)) || 'unknown');
  }
});

const timing = await evaluate(ws, `(async function(){
  var li = document.querySelector('.session-item[data-id="${SID}"]');
  if (!li) return { error: '列表里找不到该 session id' };

  var outBefore = window.__out.length, inBefore = window.__in.length;
  var msgs = document.getElementById('messages');
  var htmlBefore = msgs ? msgs.innerHTML.length : 0;
  var t0 = performance.now();
  li.click();

  // 等「messages 内容真的变了」（用 innerHTML 长度 + active 归属，别用子元素个数 ——
  // 那可能是个固定包裹层，永远不变，会误判成超时）
  var t1 = null;
  for (var i = 0; i < 300; i++) {
    await new Promise(function (r) { setTimeout(r, 10); });
    var act = document.querySelector('.session-item.active');
    if (act && act.dataset.id === '${SID}') { t1 = performance.now(); break; }
  }
  await new Promise(function (r) { setTimeout(r, 300); });

  var txt = document.body.innerText || '';
  return {
    clicked: true,
    switched: t1 !== null,
    ms: t1 === null ? null : Math.round(t1 - t0),
    htmlLenBefore: htmlBefore,
    htmlLenAfter: msgs ? msgs.innerHTML.length : 0,
    sentThisClick: window.__out.slice(outBefore).map(function (s) { return s.slice(0, 80); }),
    recvThisClick: window.__in.slice(inBefore).map(function (s) { return s.slice(0, 80); }),
    sawBusyToast: /请稍候|正在发送|正在切换/.test(txt)
  };
})()`);
console.log('\n=== 点击切换 ===');
console.log('  发出: ' + JSON.stringify(timing.sentThisClick));
console.log('  收到: ' + JSON.stringify(timing.recvThisClick));
console.log('  是否切换成功: ' + timing.switched + '   耗时: ' + timing.ms + ' ms');
console.log('  消息区 innerHTML 长度: ' + timing.htmlLenBefore + ' → ' + timing.htmlLenAfter);
console.log('  看到忙碌提示: ' + timing.sawBusyToast);
await sleep(400);
console.log('\n=== 切换期间抛出的异常 ===');
if (!exceptions.length) {
  console.log('  无异常 → 那 renderSessions 为什么没生效要另找原因');
} else {
  exceptions.forEach((e) => console.log('  ❌ ' + String(e).split('\n').slice(0, 3).join(' | ')));
}
await sleep(200);

console.log('\n=== 切换后 ===');
const after = await evaluate(ws, dump);
after.forEach((r) => console.log('  ' + JSON.stringify(r)));

const act = after.filter((r) => /(^|\s)active(\s|$)/.test(r.cls));
console.log('\n--- 结论 ---');
console.log('  带 active 的条目数: ' + act.length + (act.length ? ' → ' + JSON.stringify(act.map((a) => a.text)) : ''));
const target = after.find((r) => r.id === SID);
if (target) {
  console.log('  目标条目 class=' + JSON.stringify(target.cls) + '  bg=' + target.bg + '  color=' + target.color);
} else {
  console.log('  ❌ 目标 session 根本不在列表里');
}
ws.close();
