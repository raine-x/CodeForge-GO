// 验证模型 / 供应商管理改造：添加供应商、获取模型列表、勾选批量添加、折叠、就地编辑。
//
//   node test/verify_model_prov_ui.mjs
//
// ⚠️ 用 CDP Fetch 把两个接口拦下来返回假数据：
//   - /api/models/discover   → 不真的去问上游（省额度，也不依赖网络）
//   - /api/models/save_batch → 不真的写库（不动用户的模型库）
// 这样才能既验证前端逻辑、又不产生副作用。

const PORT = process.env.CDP_PORT || 9300;
const BASE = 'http://127.0.0.1:' + PORT;
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

const seen = { discover: null, batch: null };
ws.onmessage = async (ev) => {
  const m = JSON.parse(ev.data);
  if (m.id && waiting.has(m.id)) {
    const { res, rej } = waiting.get(m.id);
    waiting.delete(m.id);
    m.error ? rej(new Error(m.error.message)) : res(m.result);
    return;
  }
  // 拦截并伪造响应
  if (m.method === 'Fetch.requestPaused') {
    const { requestId, request } = m.params;
    const url = request.url;
    let body = { ok: true };
    if (url.indexOf('/api/models/discover') >= 0) {
      seen.discover = request.postData || '';
      body = {
        ok: true, count: 3, key_from_active: false,
        models: [
          { id: 'alpha-model', name: 'Alpha', in_library: false },
          { id: 'beta-model', name: 'Beta', in_library: false },
          { id: 'gamma-model', name: '', in_library: true }
        ]
      };
    } else if (url.indexOf('/api/models/save_batch') >= 0) {
      seen.batch = request.postData || '';
      body = { ok: true, added: 2, skipped: 0 };
    }
    const payload = Buffer.from(JSON.stringify(body)).toString('base64');
    await send(ws, 'Fetch.fulfillRequest', {
      requestId, responseCode: 200,
      responseHeaders: [{ name: 'Content-Type', value: 'application/json' }],
      body: payload
    });
  }
};

await send(ws, 'Page.enable');
await send(ws, 'Runtime.enable');
await send(ws, 'Fetch.enable', { patterns: [
  { urlPattern: '*/api/models/discover*' },
  { urlPattern: '*/api/models/save_batch*' }
] });
await send(ws, 'Page.navigate', { url: 'http://127.0.0.1:8420/' });
await sleep(3500);

// 打开设置 → 模型页
await evaluate(ws, `(function(){
  document.getElementById('open-settings').click();
  document.querySelector('.nav-item[data-page="memory"]').click();  // 先切走，确认 nav 正常
  var nav = document.getElementById('nav-models');
  nav.click();
  return true;
})()`);
await sleep(800);

console.log('① 「供应商机制」横幅');
const banner = await evaluate(ws, `document.querySelectorAll('.prov-banner').length`);
ck('横幅已删除', banner === 0, banner);

console.log('\n② 供应商管理：添加供应商');
await evaluate(ws, `document.querySelector('.models-tab[data-mtab="prov"]').click(); true`);
await sleep(600);
const beforeAdd = await evaluate(ws, `(function(){
  return { title: (document.querySelector('#prov-detail .mf-title') || {}).textContent,
           items: document.querySelectorAll('#prov-list .prov-item').length };
})()`);
console.log('     ' + JSON.stringify(beforeAdd));
const afterAdd = await evaluate(ws, `(async function(){
  var add = document.querySelector('#prov-list .prov-add');
  if (!add) return { missing: true };
  add.click();
  // ⚠️ renderProviders 内部走 loadLibrary().then(...)，是**异步**的。
  // 点完立刻同步读 DOM 会读到旧内容，误判成「点了没反应」—— 探针第一版就这么错的。
  await new Promise(function (r) { setTimeout(r, 500); });
  var fresh = document.querySelector('#prov-list .prov-add');
  return { title: (document.querySelector('#prov-detail .mf-title') || {}).textContent,
           addActive: fresh ? fresh.classList.contains('active') : null,
           items: document.querySelectorAll('#prov-list .prov-item').length };
})()`);
await sleep(300);
console.log('     ' + JSON.stringify(afterAdd));
ck('点「＋ 添加供应商」切到新建态（不再毫无反应）',
  afterAdd.title === '新建供应商', afterAdd.title);
ck('「添加供应商」按钮高亮', afterAdd.addActive === true, afterAdd.addActive);
ck('供应商列表本身没被清空', afterAdd.items === beforeAdd.items, beforeAdd.items + ' → ' + afterAdd.items);

console.log('\n③ 选中一个供应商 → 详情里应有「获取模型列表」');
const pick = await evaluate(ws, `(function(){
  var items = document.querySelectorAll('#prov-list .prov-item:not(.prov-add)');
  if (!items.length) return { missing: true };
  items[0].click();
  return { name: items[0].textContent.trim() };
})()`);
await sleep(600);
const detail = await evaluate(ws, `(function(){
  var btns = [].slice.call(document.querySelectorAll('#prov-detail .prov-actions button'));
  return { texts: btns.map(function(b){ return b.textContent.trim(); }),
           hasDisc: !!document.querySelector('#prov-detail .pv-disc') };
})()`);
console.log('     ' + JSON.stringify(detail));
ck('详情里有「获取模型列表」按钮', detail.texts.indexOf('获取模型列表') >= 0, JSON.stringify(detail.texts));
ck('拉取面板是就地元素（不是弹窗）', detail.hasDisc === true, detail.hasDisc);

console.log('\n④ 点「获取模型列表」→ 勾选 → 批量添加');
const disc = await evaluate(ws, `(async function(){
  var btn = [].slice.call(document.querySelectorAll('#prov-detail .prov-actions button'))
    .filter(function(b){ return b.textContent.trim() === '获取模型列表'; })[0];
  btn.click();
  await new Promise(function(r){ setTimeout(r, 700); });
  var box = document.querySelector('#prov-detail .pv-disc');
  var rows = box.querySelectorAll('.disc-item');
  return { visible: !box.classList.contains('hidden'), rows: rows.length,
           disabled: [].slice.call(rows).filter(function(r){ return r.querySelector('input').disabled; }).length };
})()`);
console.log('     ' + JSON.stringify(disc));
ck('面板就地展开', disc.visible === true, disc.visible);
ck('列出 3 个上游模型', disc.rows === 3, disc.rows);
ck('已在库中的条目被禁用（不可重复添加）', disc.disabled === 1, disc.disabled);

const committed = await evaluate(ws, `(async function(){
  var rows = document.querySelectorAll('#prov-detail .disc-item');
  rows[0].querySelector('input').click();
  rows[1].querySelector('input').click();
  await new Promise(function(r){ setTimeout(r, 100); });
  var addBtn = document.querySelector('#prov-detail .disc-foot button');
  var label = addBtn.textContent.trim();
  addBtn.click();
  // ⚠️ 回执要等 loadLibrary() 的异步链跑完、renderProviders 重绘后才出现，
  // 而它显示一次就被清空 —— 单次定时读取容易踩空，所以这里轮询 3 秒。
  var flash = '', note = '';
  for (var i = 0; i < 30; i++) {
    await new Promise(function(r){ setTimeout(r, 100); });
    var f = document.querySelector('#prov-detail .prov-flash');
    if (f) { flash = f.textContent; break; }
    var n = document.querySelector('#prov-detail .disc-foot .mf-test-result');
    if (n && n.textContent) note = n.textContent;
  }
  return { label: label, flash: flash, panelNote: note };
})()`);
console.log('     ' + JSON.stringify(committed));
ck('按钮显示已选数量', /2 个/.test(committed.label), committed.label);
ck('批量添加后有跨重绘的可见回执',
  /已添加 2 个/.test(committed.flash || '') || /已添加 2 个/.test(committed.panelNote || ''),
  committed.flash || committed.panelNote);
ck('请求带上 provider_id（按供应商归类）',
  !!seen.batch && /"provider_id"\s*:\s*"[^"]+"/.test(seen.batch), (seen.batch || '').slice(0, 90));
ck('请求带上勾选的两个 id',
  !!seen.batch && /alpha-model/.test(seen.batch) && /beta-model/.test(seen.batch),
  (seen.batch || '').slice(0, 90));

console.log('\n⑤ 模型列表：折叠 + 缩进 + 就地编辑');
await evaluate(ws, `document.querySelector('.models-tab[data-mtab="list"]').click(); true`);
await sleep(800);
const list = await evaluate(ws, `(function(){
  var groups = document.querySelectorAll('#model-items .model-group');
  var kids = document.querySelectorAll('#model-items .model-children');
  var fold = document.querySelector('#model-items .mg-fold');
  if (!groups.length || !fold) return { missing: true, groups: groups.length };
  var kidsBefore = kids.length;
  fold.click();
  return { groups: groups.length, kidsBefore: kidsBefore, foldClass: fold.className };
})()`);
await sleep(400);
const afterFold = await evaluate(ws, `(function(){
  return { kids: document.querySelectorAll('#model-items .model-children').length,
           foldedCls: (document.querySelector('#model-items .mg-fold') || {}).className,
           indent: (function(){
             var c = document.querySelector('#model-items .model-children');
             return c ? getComputedStyle(c).marginLeft : null;
           })() };
})()`);
console.log('     ' + JSON.stringify(afterFold));
ck('供应商分组有折叠箭头', !!list.foldClass, list.foldClass);
ck('子模型包在独立容器里（可缩进）', list.kidsBefore > 0, list.kidsBefore);
ck('折叠后子模型整组不再渲染', afterFold.kids === list.kidsBefore - 1, list.kidsBefore + ' → ' + afterFold.kids);
ck('折叠态有视觉标记（箭头旋转）', /folded/.test(afterFold.foldedCls || ''), afterFold.foldedCls);

// 展开回去再验编辑
await evaluate(ws, `document.querySelector('#model-items .mg-fold').click(); true`);
await sleep(400);
const edit = await evaluate(ws, `(function(){
  var btn = [].slice.call(document.querySelectorAll('#model-items .mi-actions button'))
    .filter(function(b){ return b.textContent.trim() === '编辑'; })[0];
  if (!btn) return { missing: true };
  btn.click();
  var ed = document.querySelector('#model-items .model-editor');
  var tabNow = (document.querySelector('.mtab-pane.active') || {}).id;
  return { opened: !!ed, tabNow: tabNow,
           fields: ed ? ed.querySelectorAll('input').length : 0,
           ctxNums: ed ? ed.querySelectorAll('.ctx-num').length : 0,
           ctxChips: ed ? ed.querySelectorAll('.ctx-chip').length : 0,
           inheritText: ed ? (ed.querySelector('.me-inherit') || {}).textContent : '' };
})()`);
console.log('     ' + JSON.stringify(edit));
ck('编辑就地展开', edit.opened === true, edit.opened);
ck('没有跳到「添加模型」标签页', edit.tabNow === 'mtab-list', edit.tabNow);
ck('编辑区含名称/id 两个文本框 + 两个上下文档位输入框 + 档位按钮',
  edit.fields === 4 && edit.ctxNums === 2 && edit.ctxChips === 7,
  edit.fields + ' inputs / ' + edit.ctxNums + ' ctx-num / ' + edit.ctxChips + ' chips');
ck('显示连接信息继承自哪个供应商', /继承自该供应商/.test(edit.inheritText || ''), edit.inheritText);

console.log('\n' + '-'.repeat(52));
if (failed.length) {
  console.log('失败 ' + failed.length + ' 项 / 通过 ' + passed.length + ' 项');
  failed.forEach((f) => console.log('  - ' + f));
  process.exitCode = 1;
} else {
  console.log('全部通过（' + passed.length + ' 项）');
}
ws.close();
