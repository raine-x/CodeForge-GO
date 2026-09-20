// 验证「写」路径：就地编辑模型保存、新建供应商保存。
//
//   node test/verify_model_prov_save.mjs
//
// 为什么单独测写：
//   读路径错了顶多显示不对，写路径错了会**改坏用户的模型库/密钥**。
//   尤其要盯住「就地编辑模型时不能把密钥冲掉」—— 模型列表是脱敏视图，
//   前端拿不到明文，一旦把空的 key_value 提交上去，已存的密钥就没了。
//
// 两个接口都用 CDP Fetch 拦下来，**不真的写库**。

const PORT = process.env.CDP_PORT || 9320;
const BASE = 'http://127.0.0.1:' + PORT;
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

async function pageTarget() {
  for (let i = 0; i < 40; i++) {
    try {
      const l = await (await fetch(BASE + '/json/list')).json();
      const t = l.find((x) => x.type === 'page' && x.webSocketDebuggerUrl);
      if (t) return t;
    } catch (e) {}
    await sleep(500);
  }
  throw new Error('找不到可调试的页面 target');
}

let seq = 0;
const waiting = new Map();
const send = (ws, m, p) => new Promise((res, rej) => {
  const id = ++seq; waiting.set(id, { res, rej });
  ws.send(JSON.stringify({ id, method: m, params: p || {} }));
});
async function ev(ws, e) {
  const r = await send(ws, 'Runtime.evaluate', { expression: e, returnByValue: true, awaitPromise: true });
  if (r.exceptionDetails) throw new Error(r.exceptionDetails.exception?.description || r.exceptionDetails.text);
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
  s.onopen = () => res(s); s.onerror = () => rej(new Error('CDP 连接失败'));
});

const seen = { modelSave: null, provSave: null };
ws.onmessage = async (m0) => {
  const m = JSON.parse(m0.data);
  if (m.id && waiting.has(m.id)) {
    const { res, rej } = waiting.get(m.id); waiting.delete(m.id);
    m.error ? rej(new Error(m.error.message)) : res(m.result);
    return;
  }
  if (m.method === 'Fetch.requestPaused') {
    const { requestId, request } = m.params;
    let body = { ok: true };
    if (request.url.indexOf('/api/models/save') >= 0) {
      seen.modelSave = request.postData || '';
      body = { ok: true };
    } else if (request.url.indexOf('/api/providers/save') >= 0) {
      seen.provSave = request.postData || '';
      body = { ok: true, hot: false };
    }
    await send(ws, 'Fetch.fulfillRequest', {
      requestId, responseCode: 200,
      responseHeaders: [{ name: 'Content-Type', value: 'application/json' }],
      body: Buffer.from(JSON.stringify(body)).toString('base64')
    });
  }
};

await send(ws, 'Page.enable');
await send(ws, 'Runtime.enable');
await send(ws, 'Fetch.enable', { patterns: [
  { urlPattern: '*/api/models/save' },
  { urlPattern: '*/api/providers/save' }
] });
await send(ws, 'Page.navigate', { url: 'http://127.0.0.1:8420/' });
await sleep(3500);

await ev(ws, `(function(){
  document.getElementById('open-settings').click();
  document.getElementById('nav-models').click();
  return true;
})()`);
await sleep(800);

console.log('① 就地编辑模型 → 保存');
await ev(ws, `document.querySelector('.models-tab[data-mtab="list"]').click(); true`);
await sleep(800);
const modelEdit = await ev(ws, `(async function(){
  var btn = [].slice.call(document.querySelectorAll('#model-items .mi-actions button'))
    .filter(function(b){ return b.textContent.trim() === '编辑'; })[0];
  if (!btn) return { missing: true };
  btn.click();
  var ed = document.querySelector('#model-items .model-editor');
  if (!ed) return { noEditor: true };
  var ins = ed.querySelectorAll('input');
  // ins: [0]=名称 [1]=id [2]=ctx_in [3]=ctx_out（档位是按钮，不占 input）
  ins[0].value = '改过的名字';
  // 点「512K」档位按钮 → 数值写进输入框
  var chip = [].slice.call(ed.querySelectorAll('.ctx-chip'))
    .filter(function(b){ return b.textContent.trim() === '512K'; })[0];
  if (!chip) return { noChip: true };
  chip.click();
  var save = [].slice.call(ed.querySelectorAll('.me-foot button'))
    .filter(function(b){ return b.textContent.trim() === '保存修改'; })[0];
  save.click();
  await new Promise(function(r){ setTimeout(r, 900); });
  return { saved: true };
})()`);
console.log('     ' + JSON.stringify(modelEdit));
await sleep(400);

let p = null;
try { p = JSON.parse(seen.modelSave || 'null'); } catch (e) {}
console.log('     载荷: ' + (seen.modelSave || '(无请求)'));
ck('保存请求已发出', !!seen.modelSave, seen.modelSave ? 'ok' : '无');
ck('带上改名后的名称', !!p && p.name === '改过的名字', p && p.name);
ck('选中预置档「512K」后写进载荷 524288', !!p && p.ctx_in === 524288, p && p.ctx_in);
ck('带 provider_id（归属不变）', !!p && typeof p.provider_id === 'string' && p.provider_id.length > 0, p && p.provider_id);
ck('inherit_key=true（清掉条目上遗留的自带密钥）', !!p && p.inherit_key === true, p && p.inherit_key);
ck('⚠️ 绝不提交 key_value（列表是脱敏视图，提交空值会冲掉已存密钥）',
  !!p && !('key_value' in p), p ? JSON.stringify(Object.keys(p)) : '');
ck('不提交 base_url（连接信息属于供应商）',
  !!p && !('base_url' in p), p ? JSON.stringify(Object.keys(p)) : '');

console.log('\n② 新建供应商 → 保存');
await ev(ws, `document.querySelector('.models-tab[data-mtab="prov"]').click(); true`);
await sleep(700);
const provSave = await ev(ws, `(async function(){
  var add = document.querySelector('#prov-list .prov-add');
  add.click();
  await new Promise(function(r){ setTimeout(r, 500); });
  var host = document.getElementById('prov-detail');
  if (!/新建供应商/.test(host.textContent)) return { notNew: true, head: host.textContent.slice(0, 40) };
  var ins = host.querySelectorAll('.prov-form input');
  // ins: [0]=名称 [1]=BaseURL [2]=密钥相关…
  ins[0].value = '测试供应商';
  ins[1].value = 'https://example.test/v1';
  var save = [].slice.call(host.querySelectorAll('.prov-actions button'))
    .filter(function(b){ return b.textContent.trim() === '保存供应商'; })[0];
  if (!save) return { noSaveBtn: true };
  save.click();
  await new Promise(function(r){ setTimeout(r, 900); });
  return { clicked: true };
})()`);
console.log('     ' + JSON.stringify(provSave));
await sleep(400);
let pp = null;
try { pp = JSON.parse(seen.provSave || 'null'); } catch (e) {}
console.log('     载荷: ' + (seen.provSave || '(无请求)'));
ck('新建态能进到表单', !provSave.notNew, JSON.stringify(provSave));
ck('保存请求已发出', !!seen.provSave, seen.provSave ? 'ok' : '无');
ck('带上供应商名称', !!pp && pp.name === '测试供应商', pp && pp.name);
ck('带上 Base URL', !!pp && pp.base_url === 'https://example.test/v1', pp && pp.base_url);
ck('自动生成了 id', !!pp && typeof pp.id === 'string' && pp.id.length > 0, pp && pp.id);
ck('带上协议与密钥来源字段',
  !!pp && typeof pp.protocol === 'string' && typeof pp.key_source === 'string',
  pp ? (pp.protocol + '/' + pp.key_source) : '');

console.log('\n③ 上下文档位：输入框保留 + 档位直接点 + 非整档值不被静默改掉');
await ev(ws, `document.querySelector('.models-tab[data-mtab="list"]').click(); true`);
await sleep(800);
seen.modelSave = null;   // ⚠️ 这是 Node 侧的变量，不能在页面表达式里赋值
const ctx = await ev(ws, `(async function(){
  // 挑一个 ctx_in 不在预置档里的模型（实测 deepseek-flash 是 1040000）
  var rows = document.querySelectorAll('#model-items .model-wrap');
  var target = null, targetCtx = null;
  for (var i = 0; i < rows.length; i++) {
    var txt = rows[i].querySelector('.mi-id') ? rows[i].querySelector('.mi-id').textContent : '';
    var m = txt.match(/上下文\\s*([\\d.]+)([MK]?)/);
    if (m) {
      var v = parseFloat(m[1]) * (m[2] === 'M' ? 1000000 : m[2] === 'K' ? 1000 : 1);
      if (v % 1024 !== 0) { target = rows[i]; targetCtx = txt; break; }
    }
  }
  if (!target) return { noNonPresetModel: true };
  var btn = [].slice.call(target.querySelectorAll('.mi-actions button'))
    .filter(function(b){ return b.textContent.trim() === '编辑'; })[0];
  btn.click();
  var ed = target.querySelector('.model-editor');
  if (!ed) return { noEditor: true };
  var picks = ed.querySelectorAll('.ctx-pick');
  var nums = [].slice.call(ed.querySelectorAll('.ctx-num'));
  var chipsIn = [].slice.call(picks[0].querySelectorAll('.ctx-chip')).map(function(b){ return b.textContent.trim(); });
  var chipsOut = [].slice.call(picks[1].querySelectorAll('.ctx-chip')).map(function(b){ return b.textContent.trim(); });
  var valBefore = nums[0].value;
  var onBefore = picks[0].querySelectorAll('.ctx-chip.on').length;
  // 点「1M」→ 输入框应变成 1048576，且该档高亮
  var chip1M = [].slice.call(picks[0].querySelectorAll('.ctx-chip'))
    .filter(function(b){ return b.textContent.trim() === '1M'; })[0];
  chip1M.click();
  var valAfter = nums[0].value;
  var onAfter = picks[0].querySelectorAll('.ctx-chip.on');
  // 再保存一次，确认「点档位 → 输入框 → 载荷」整条路是通的
  var save = [].slice.call(ed.querySelectorAll('.me-foot button'))
    .filter(function(b){ return b.textContent.trim() === '保存修改'; })[0];
  save.click();
  await new Promise(function(r){ setTimeout(r, 900); });
  return { targetCtx: targetCtx, valBefore: valBefore, valAfter: valAfter,
           onBefore: onBefore, onCount: onAfter.length,
           onLabel: onAfter[0] ? onAfter[0].textContent.trim() : null,
           chipsIn: chipsIn, chipsOut: chipsOut,
           hasInput: nums.length === 2 };
})()`);
console.log('     ' + JSON.stringify(ctx));
await sleep(300);
let pc = null;
try { pc = JSON.parse(seen.modelSave || 'null'); } catch (e) {}
console.log('     保存载荷 ctx: ' + (pc ? (pc.ctx_in + ' / ' + pc.ctx_out) : '(无请求)'));
ck('找到非整档的真实模型（用例成立）', !ctx.noNonPresetModel && !ctx.noEditor, ctx.targetCtx);
ck('保留了数字输入框（两个）', ctx.hasInput === true, ctx.hasInput);
ck('输入档位按钮 = 1M / 512K / 256K / 128K',
  ['1M', '512K', '256K', '128K'].every(function (t) { return (ctx.chipsIn || []).indexOf(t) >= 0; }),
  JSON.stringify(ctx.chipsIn));
ck('输出档位按钮 = 384K / 256K / 128K',
  ['384K', '256K', '128K'].every(function (t) { return (ctx.chipsOut || []).indexOf(t) >= 0; }),
  JSON.stringify(ctx.chipsOut));
ck('非整档的既有值原样落在输入框里（1040000，没被改成 1M）',
  String(ctx.valBefore) === '1040000', ctx.valBefore);
ck('非整档时没有任何档位高亮（表示这是自定义值）', ctx.onBefore === 0, ctx.onBefore);
ck('点「1M」把 1048576 写进输入框', String(ctx.valAfter) === '1048576', ctx.valAfter);
ck('点完该档高亮，且只有一个档高亮',
  ctx.onCount === 1 && ctx.onLabel === '1M', ctx.onCount + '/' + ctx.onLabel);
ck('「点档位 → 输入框 → 保存载荷」整条路通（载荷 ctx_in = 1048576）',
  !!pc && pc.ctx_in === 1048576, pc && pc.ctx_in);

console.log('\n' + '-'.repeat(52));
if (failed.length) {
  console.log('失败 ' + failed.length + ' 项 / 通过 ' + passed.length + ' 项');
  failed.forEach((f) => console.log('  - ' + f));
  process.exitCode = 1;
} else {
  console.log('全部通过（' + passed.length + ' 项）');
}
ws.close();
