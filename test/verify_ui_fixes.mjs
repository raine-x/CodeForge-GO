// 三个前端修复的运行时验证（CDP 驱动无头 Chrome，零依赖）
//
//   node test/verify_ui_fixes.mjs            # 需先在同一条 Bash 调用里起服务 + Chrome
//
// 为什么需要它：web/test/render_md.test.js 只能锁「代码形状 / 选择器契约」，
// 证明不了「真的画出来了」「真的在上面」「真的不再往回拽」。
// 本脚本在真实浏览器里跑，直接量几何与计算样式。
//
// 覆盖：
//   ① 背景图：body 必须透明（不透明会把 #bg-layer 永久遮住）、图层 z-index=0
//   ② 任务清单：默认 display:none；派发 todo 事件后出现且在输入卡片**上方**
//   ③ 滚动：往上滚后「回到底部」出现，点击后真的回到最底

import fs from 'node:fs';

const PORT = process.env.CDP_PORT || 9223;
const BASE = 'http://127.0.0.1:' + PORT;
const OUT = process.env.SHOT_DIR || '.';

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

async function pageTarget() {
  for (let i = 0; i < 40; i++) {
    try {
      const list = await (await fetch(BASE + '/json/list')).json();
      const t = list.find((x) => x.type === 'page' && x.webSocketDebuggerUrl);
      if (t) return t;
    } catch (e) { /* Chrome 还没起来 */ }
    await sleep(500);
  }
  throw new Error('找不到可调试的页面 target');
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

async function evaluate(ws, expr) {
  const r = await send(ws, 'Runtime.evaluate', {
    expression: expr, returnByValue: true, awaitPromise: true,
  });
  if (r.exceptionDetails) {
    const d = r.exceptionDetails.exception;
    throw new Error('页面内异常: ' + (d && d.description ? d.description : r.exceptionDetails.text));
  }
  return r.result.value;
}

async function shot(ws, name) {
  const r = await send(ws, 'Page.captureScreenshot', { format: 'png' });
  const p = OUT.replace(/[\\/]+$/, '') + '/' + name;
  fs.writeFileSync(p, Buffer.from(r.data, 'base64'));
  return p;
}

// ---------------------------------------------------------------------------
const passed = [];
const failed = [];

function ck(name, ok, extra) {
  (ok ? passed : failed).push(name + (extra !== undefined && !ok ? '  → 实际: ' + extra : ''));
  console.log((ok ? '  \u2713 ' : '  \u2717 ') + name + (extra !== undefined ? '   [' + extra + ']' : ''));
}

const main = async () => {
  const t = await pageTarget();
  const ws = await connect(t.webSocketDebuggerUrl);
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

  // 在 ui.js 之前装好钩子：包一层 WebSocket 拿到实例，并把收到的帧记下来，
  // 这样我们既能读出发言用的 session_id，也能自己派发 todo 事件。
  await send(ws, 'Page.addScriptToEvaluateOnNewDocument', {
    source: `(function(){
      window.__frames = [];
      var Orig = window.WebSocket;
      function Patched(url, protocols) {
        var inst = protocols === undefined ? new Orig(url) : new Orig(url, protocols);
        window.__ws = inst;
        inst.addEventListener('message', function (ev) {
          try { window.__frames.push(JSON.parse(ev.data)); } catch (e) {}
        });
        return inst;
      }
      Patched.prototype = Orig.prototype;
      ['CONNECTING','OPEN','CLOSING','CLOSED'].forEach(function(k){ Patched[k] = Orig[k]; });
      window.WebSocket = Patched;
      window.__fire = function (obj) {
        if (!window.__ws) return false;
        window.__ws.dispatchEvent(new MessageEvent('message', { data: JSON.stringify(obj) }));
        return true;
      };
    })();`,
  });

  await send(ws, 'Page.navigate', { url: 'http://127.0.0.1:8420/' });
  await sleep(3500);   // 等 WS 连上、会话回放完

  console.log('\n① 背景图（body 必须透明，否则图层被永久遮住）');
  const bg = await evaluate(ws, `(function(){
    var bl = document.getElementById('bg-layer');
    var cs = getComputedStyle(bl);
    return {
      layerZ: cs.zIndex,
      layerPos: cs.position,
      layerPE: cs.pointerEvents,
      bodyBg: getComputedStyle(document.body).backgroundColor,
      htmlBg: getComputedStyle(document.documentElement).backgroundColor,
      isFirstChild: document.body.firstChild === bl
    };
  })()`);
  ck('图层 position:fixed 且 pointer-events:none', bg.layerPos === 'fixed' && bg.layerPE === 'none',
    bg.layerPos + '/' + bg.layerPE);
  // ⚠️ 必须是负值：写成 0 会让图层在绘制顺序上晚于在流块背景，
  // 从而盖住侧栏底色（曾实际踩到）。这条断言锁死这个回归。
  ck('图层 z-index=-1（画在在流块背景之下，不盖面板）', bg.layerZ === '-1', bg.layerZ);
  ck('图层是 body 首个子节点（DOM 顺序垫底）', bg.isFirstChild === true, bg.isFirstChild);
  ck('body 背景透明（关键：不透明会盖住图层）', bg.bodyBg === 'rgba(0, 0, 0, 0)', bg.bodyBg);
  ck('html 有不透明底色（页面配色不丢）', bg.htmlBg === 'rgb(255, 255, 255)', bg.htmlBg);

  console.log('\n② 任务清单：默认关闭');
  const d0 = await evaluate(ws, `(function(){
    var tb = document.getElementById('todo-bar');
    return {
      className: tb.className,
      count: document.querySelectorAll('#todo-bar').length,
      hasHidden: tb.classList.contains('hidden'),
      display: getComputedStyle(tb).display,
      rows: tb.querySelectorAll('.todo-row').length,
      pad: getComputedStyle(document.documentElement).getPropertyValue('--composer-pad-bottom').trim()
    };
  })()`);
  console.log('     (class="' + d0.className + '", 元素数=' + d0.count + ')');
  ck('默认 display:none', d0.hasHidden && d0.display === 'none',
    'class=' + d0.className + ' display=' + d0.display);
  ck('默认无任何任务行（不是空壳）', d0.rows === 0, d0.rows);
  ck('底部留白变量已由 JS 实测写入', /^\d+px$/.test(d0.pad), d0.pad);
  await shot(ws, 'cf-ui-default.png');

  console.log('\n③ 任务清单：AI 调用后出现在输入卡片上方');
  const sid = await evaluate(ws, `(function(){
    var f = (window.__frames || []).filter(function(x){ return x && x.session_id; });
    return f.length ? f[f.length - 1].session_id : '';
  })()`);
  console.log('     (会话 id: ' + (sid || '(空)') + ')');
  await evaluate(ws, `window.__fire({type:'todo', session_id:${JSON.stringify(sid)}, todos:[
    {content:'分析现有代码', status:'completed'},
    {content:'实现检查点落库', status:'in_progress'},
    {content:'补充测试', status:'pending'}
  ]}); true`);
  await sleep(400);
  // ⚠️ 几何比较必须拿 #composer（表单）当基准，不能拿 #composer-wrap ——
  // 面板就在 wrap 内部，wrap 的 top 天然在面板之上，用 wrap 比必然失败（曾误判成代码 bug）。
  const d1 = await evaluate(ws, `(function(){
    var tb = document.getElementById('todo-bar');
    var fm = document.getElementById('composer');
    var a = tb.getBoundingClientRect(), b = fm.getBoundingClientRect();
    return {
      display: getComputedStyle(tb).display,
      rows: tb.querySelectorAll('.todo-row').length,
      count: tb.querySelector('.todo-count').textContent,
      aboveComposer: a.bottom <= b.top + 1,
      gapToComposer: Math.round(b.top - a.bottom),
      tbWidth: Math.round(a.width),
      fmWidth: Math.round(b.width),
      sameWidth: Math.abs(a.width - b.width) <= 1,
      rowsText: Array.prototype.map.call(tb.querySelectorAll('.todo-row'), function(r){ return r.textContent; })
    };
  })()`);
  ck('面板显形（display:flex）', d1.display === 'flex', d1.display);
  ck('列出 3 条任务', d1.rows === 3, d1.rows);
  ck('完成计数 1/3', d1.count === '1/3', d1.count);
  ck('面板整体位于输入卡片上方', d1.aboveComposer === true,
    '间距 ' + d1.gapToComposer + 'px');
  ck('面板宽度与输入卡片一致', d1.sameWidth === true,
    d1.tbWidth + 'px vs ' + d1.fmWidth + 'px');
  ck('状态图标正确（✓/◐/○）',
    /^✓分析现有代码$/.test(d1.rowsText[0]) && /^◐实现检查点落库$/.test(d1.rowsText[1]) &&
    /^○补充测试$/.test(d1.rowsText[2]), JSON.stringify(d1.rowsText));
  await shot(ws, 'cf-ui-todo.png');

  console.log('\n④ 任务清单：清空后重新隐藏');
  await evaluate(ws, `window.__fire({type:'todo', session_id:${JSON.stringify(sid)}, todos:[]}); true`);
  await sleep(400);
  const d2 = await evaluate(ws, `(function(){
    var tb = document.getElementById('todo-bar');
    return { className: tb.className, display: getComputedStyle(tb).display,
             rows: tb.querySelectorAll('.todo-row').length };
  })()`);
  ck('清空后 display:none', d2.display === 'none',
    'class=' + d2.className + ' display=' + d2.display);
  ck('旧条目已清掉（不留残影）', d2.rows === 0, d2.rows);

  console.log('\n⑤ 滚动：向上翻历史 + 回到底部');
  const s0 = await evaluate(ws, `(function(){
    var m = document.getElementById('messages');
    var col = m.querySelector('.msg-col') || m;
    for (var i = 0; i < 60; i++) {
      var d = document.createElement('div');
      d.className = 'msg-assistant';
      d.textContent = '填充行 ' + i + ' ' + new Array(30).join('x');
      col.appendChild(d);
    }
    m.scrollTop = 0;
    m.dispatchEvent(new Event('scroll'));
    var tbb = document.getElementById('to-bottom');
    return {
      scrollable: m.scrollHeight > m.clientHeight + 100,
      scrollTop: Math.round(m.scrollTop),
      showsBtn: tbb.classList.contains('show'),
      btnPE: getComputedStyle(tbb).pointerEvents
    };
  })()`);
  ck('消息区已可滚动（构造出长会话）', s0.scrollable === true, s0.scrollable);
  ck('滚到顶部后 scrollTop 停在顶部（没被拽回底部）', s0.scrollTop < 20, s0.scrollTop);
  ck('「回到底部」按钮出现', s0.showsBtn === true, s0.showsBtn);
  ck('按钮可点（pointer-events:auto）', s0.btnPE === 'auto', s0.btnPE);
  await shot(ws, 'cf-ui-scrolled.png');

  // 内容继续增长时不该把视口拽回底部（旧 bug 的核心症状）
  const s1 = await evaluate(ws, `(function(){
    var m = document.getElementById('messages');
    var col = m.querySelector('.msg-col') || m;
    var d = document.createElement('div');
    d.className = 'msg-assistant';
    d.textContent = '追加的一行';
    col.appendChild(d);
    m.dispatchEvent(new Event('scroll'));
    return { scrollTop: Math.round(m.scrollTop) };
  })()`);
  ck('内容增长后仍停在原位（跟随态已解除）', s1.scrollTop < 20, s1.scrollTop);

  await evaluate(ws, `document.getElementById('to-bottom').click(); true`);
  await sleep(1000);   // 等平滑滚动结束
  const s2 = await evaluate(ws, `(function(){
    var m = document.getElementById('messages');
    var tbb = document.getElementById('to-bottom');
    return {
      gap: Math.round(m.scrollHeight - m.scrollTop - m.clientHeight),
      showsBtn: tbb.classList.contains('show')
    };
  })()`);
  ck('点按钮后回到最底部', s2.gap <= 4, s2.gap + 'px');
  ck('到底后按钮自动收起', s2.showsBtn === false, s2.showsBtn);

  console.log('\n' + '-'.repeat(52));
  if (failed.length) {
    console.log('失败 ' + failed.length + ' 项 / 通过 ' + passed.length + ' 项：');
    failed.forEach((f) => console.log('  - ' + f));
    process.exitCode = 1;
  } else {
    console.log('全部通过（' + passed.length + ' 项运行时检查）');
  }
  ws.close();
};

main().catch((e) => { console.error('验证脚本出错: ' + e.message); process.exitCode = 1; });
