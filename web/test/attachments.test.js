'use strict';

const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');
const src = fs.readFileSync(path.join(__dirname, '../dist/ui.js'), 'utf8');

function extract(name) {
  const start = src.search(new RegExp('^  (?:async )?function ' + name + '\\(', 'm'));
  assert.notEqual(start, -1, name);
  const end = src.indexOf('\n  }', start);
  assert.notEqual(end, -1, name);
  return src.slice(start, end + 4);
}

function harness() {
  const events = {}, sent = [], errors = [], infos = [], users = [], requests = [];
  const input = {
    value: '', selectionStart: 0,
    focus() {},
    setSelectionRange(a, b) { this.selectionStart = a; this.selectionEnd = b; },
    addEventListener(type, fn) { events[type] = fn; }
  };
  class Socket {
    static OPEN = 1;
    constructor() { this.readyState = 1; this.events = {}; }
    send(text) { sent.push(JSON.parse(text)); }
    addEventListener(type, fn) { this.events[type] = fn; }
  }
  const c = vm.createContext({
    input, inputMirror: null, form: input, WebSocket: Socket,
    location: { protocol: 'http:', host: 'localhost' },
    FormData: class { constructor() { this.entries = []; } append(...entry) { this.entries.push(entry); } },
    fetch(url, options) { requests.push({ url, options }); return c.reply(url, options); },
    reply() { throw new Error('Unexpected fetch'); },
    addError(text) { errors.push(text); }, addInfo(text) { infos.push(text); },
    // addUser 必须**返回行节点**：submitMessage 记乐观气泡要用 lastAddedUserRow()，
    // 它取的是 addUser 的返回值。早先这里只 push 不返回，于是 submitMessage 走到
    // `optimisticBubble = lastAddedUserRow()` 时 ReferenceError，被自己的 catch
    // 吞掉并 `sending = false` —— 表现是「第一次已发出，紧接着的第二次提交没被
    // 拦住」（sent.length 2 !== 1），排查时极难指向这里。
    addUser(text) { users.push(text); return {}; }, showThinking() {}, resetPlanActions() {},
    // syncSendBtn 走 classList.toggle(cls, force) + setAttribute(aria-label)：
    // 桩要跟着补齐，否则打桩比真实 DOM 少方法，报错会指向附件逻辑、实际是桩不全。
    sendBtn: { classList: { add() {}, remove() {}, toggle() {} }, setAttribute() {} },
    // 按钮三态不是本组关注点（由 render_md.test.js 锁），打桩即可。
    syncSendBtn() {},
    sessionsCache: [], loadSessionList() {}, messagesEl: {}, subagentCards: new Map(),
    syncComposerMode() {}, renderSessions() {}, scrollBottom() {},
    removeThinking() {}, removeRetry() {}, removeResumeRing() {}, showResumeRing() {}, settleActiveTool() {}, foldReason() {}, closeText() {},
    resetSubagentCards() {}, syncRunBadges() {}, maybeShowPlanActions() {},
    // submitMessage 开头会问「现在是不是编辑态」——这里只测附件路径，
    // 编辑重发由 checkpoints 测试覆盖，本框架恒为非编辑态。
    // syncUserEditButtons 由 busy 事件触发，steerNow 在「运行中再按发送」时触发，
    // 两者都不在本框架的覆盖范围内。
    isEditing() { return false; }, syncUserEditButtons() {}, steerNow() {},
    // 乐观气泡（提交成功后记、error/idle 时撤）。
    //
    // 这里打桩而不是抽取源码里的实现：lastAddedUserRow 依赖
    // userQuestionRows → messagesEl.querySelectorAll，是纯 DOM 视图层；本组测的是
    // 「发送时机 / 附件暂存 / 重复提交拦截」，与气泡渲染无关。
    //
    // ⚠️ 早先没提供它们，submitMessage 走到 `optimisticBubble = lastAddedUserRow()`
    // 就 ReferenceError，被自己的 catch 吞掉并 `sending = false` —— 于是**紧接着的
    // 第二次提交没被拦住**（sent.length 2 !== 1）。而 sent[0] 早已写出、users 也已
    // 追加，看起来「第一次发送是成功的」，排查时极难指向这个缺失依赖。
    lastAddedUserRow() { return null; }, dropOptimisticBubble() { return false; },
    // 审批卡作废（busy 分支会调）。纯视图层：改审批按钮的禁用态。
    // ⚠️ 缺它会让 h.event('busy') 直接抛 ReferenceError —— WS 的 onmessage 没有
    // try/catch，一抛就整个测试炸在那里，看不出跟「发送时机」有关。
    expireApprovals() {},
    // idle/error 分支会调它收尾本轮可视元素（工具卡 / 思考提示 / 重试圆环…）。纯视图层。
    clearRunVisuals() {},
    // pendingEdit：编辑重发已发出、尚未收到服务端权威快照的标志。idle/error 分支会读写它。
    pendingEdit: false,
    setTimeout() {},
    thinkingVal: 'high', composerSnap: false, lastUserText: '', lastReply: '',
    runSessionID: '', runReason: '', runText: '', pendingToolEl: null
  });
  const state = src.slice(src.indexOf('  let ws = null;'), src.indexOf('  let currentTextEl = null;'));
  const names = ['mentionBody', 'mentionToken', 'pruneFileAliases', 'expandFileAliases',
    'insideWorkspace', 'attachmentKey', 'composerSnapshot', 'sameComposer', 'fileMentions',
    'prepareMentions', 'syncInputMirror', 'setInputValue', 'insertIntoInput', 'insertUploadedFile', 'pasteFiles',
    'submitMessage', 'wsSend', 'runAway', 'setWorkspace', 'loadSession', 'doNewSession',
    'deleteWorkspace', 'connectWS'];
  vm.runInContext(state + '\n' + src.match(/  const MENTION_SPLIT = .*;/)[0] + '\n' +
    'const fileAlias = new Map(); let workspaceRoot = "C:/work"; let running = false;\n' +
    names.map(extract).join('\n') + '\n' +
    src.match(/  input.addEventListener\('paste', pasteFiles\);/)[0] + '\n' +
    src.match(/  form.addEventListener\('submit', submitMessage\);/)[0] + '\n' +
    'connectWS(); wsReady = true; sessionID = "s1";', c);
  function run(code) { return vm.runInContext(code, c); }
  function text(value) { input.value = value; input.setSelectionRange(value.length, value.length); }
  function paste(files = [], items = [], plain = '') {
    const e = { clipboardData: { files, items, getData() { return plain; } }, prevented: false,
      preventDefault() { this.prevented = true; } };
    return { e, done: events.paste(e) };
  }
  return { c, run, input, text, paste, sent, errors, infos, users, requests,
    submit() { return events.submit({ preventDefault() {} }); },
    event(type) { return run('ws.events.message({ data: ' + JSON.stringify(JSON.stringify({ type })) + ' });'); }
  };
}

function response(data, ok = true) { return { ok, json: async () => data }; }
function deferred() { let resolve, reject; const promise = new Promise((a, b) => { resolve = a; reject = b; }); return { promise, resolve, reject }; }
function file(name = 'a.png', size = 10) { return { name, size, type: 'image/png' }; }
function plain(value) { return JSON.parse(JSON.stringify(value)); }
const tick = () => new Promise(resolve => setImmediate(resolve));
const tests = [];
function test(name, fn) { tests.push({ name, fn }); }

test('root files and quoted paths are mentions; skills are not', () => {
  const h = harness();
  assert.deepEqual(plain(h.c.fileMentions('@a.png @plan @技能 @codeforge-build @notes.txt @"a b.jpg" @src/main.go')),
    ['a.png', 'notes.txt', 'a b.jpg', 'src/main.go']);
});

test('workspace attachments deduplicate relative and absolute Windows paths', async () => {
  const h = harness();
  const p = await h.c.prepareMentions('@a.png @./a.png @C:/work/a.png @C:\\work\\A.png @notes.txt', 'display');
  assert.deepEqual(plain(p.attachments), ['a.png', 'notes.txt']);
  assert.equal(p.display, 'display');
  assert.equal(h.requests.length, 0);
});

test('external staging uses final paths and quoted mention tokens, once per file', async () => {
  const h = harness();
  h.c.reply = async () => response({ ok: true, staged_path: 'attachments/a b.png', name: 'a b.png' });
  const p = await h.c.prepareMentions('@"D:/a b.png" @"D:/a b.png" @attachments/a.png', '@original');
  assert.equal(h.requests.length, 1);
  assert.deepEqual(JSON.parse(h.requests[0].options.body), { path: 'D:/a b.png' });
  assert.equal(p.text, '@"attachments/a b.png" @"attachments/a b.png" @attachments/a.png');
  assert.deepEqual(plain(p.attachments), ['attachments/a b.png', 'attachments/a.png']);
  assert.equal(p.display, '@original');
  assert.equal(p.notes.length, 1);
});

test('inside stage result deduplicates against final path without copy note', async () => {
  const h = harness();
  h.c.reply = async () => response({ ok: true, staged_path: 'a.png', inside: true });
  const p = await h.c.prepareMentions('@../work/a.png @a.png');
  assert.deepEqual(plain(p.attachments), ['a.png']);
  assert.equal(p.text, '@a.png @a.png');
  assert.equal(p.notes.length, 0);
});

test('staging HTTP, protocol, JSON and network failures retain input and aliases', async () => {
  for (const reply of [
    async () => response({ error: 'denied' }, false),
    async () => response({ ok: true }),
    async () => ({ ok: true, json: async () => { throw new Error('bad JSON'); } }),
    async () => { throw new Error('offline'); }
  ]) {
    const h = harness();
    h.text('@a.png keep');
    h.run('fileAlias.set("@a.png", "D:/a.png")');
    h.c.reply = reply;
    await h.submit();
    assert.equal(h.input.value, '@a.png keep');
    assert.equal(h.run('fileAlias.get("@a.png")'), 'D:/a.png');
    assert.equal(h.sent.length, 0);
    assert.equal(h.users.length, 0);
    assert.equal(h.errors.length, 1);
    assert.equal(h.run('sending'), false);
  }
});

test('missing workspace blocks attachments but not skill-only text', async () => {
  const h = harness();
  h.run('workspaceRoot = ""');
  await assert.rejects(h.c.prepareMentions('@a.png'), /工作区/);
  assert.deepEqual(plain((await h.c.prepareMentions('@plan')).attachments), []);
  const p = h.paste([file()]);
  await p.done;
  assert.equal(h.requests.length, 0);
  assert.equal(h.input.value, '');
  assert.match(h.errors[0], /工作区/);
});

test('text-only and null getAsFile paste preserve native default', () => {
  const h = harness();
  for (const items of [[], [{ kind: 'string' }], [{ kind: 'file', getAsFile: () => null }]]) {
    const p = h.paste([], items, 'hello');
    assert.equal(p.e.prevented, false);
  }
  assert.equal(h.requests.length, 0);
});

test('clipboard real file is sent in multipart, fallback items and no duplicate extraction', async () => {
  for (const useItems of [false, true]) {
    const h = harness(), f = file('原 图.png');
    let calls = 0;
    const items = [{ kind: 'file', getAsFile() { calls++; return f; } }];
    h.c.reply = async () => response({ ok: true, staged_path: 'attachments/random.png', name: f.name });
    const p = h.paste(useItems ? [] : [f], items);
    assert.equal(p.e.prevented, true);
    await p.done;
    assert.equal(calls, useItems ? 1 : 0);
    assert.equal(h.requests.length, 1);
    const req = h.requests[0];
    assert.equal(req.url, '/api/upload_file');
    assert.equal(req.options.method, 'POST');
    assert.equal(req.options.headers, undefined);
    assert.equal(req.options.body.entries[0][0], 'file');
    assert.equal(req.options.body.entries[0][1], f);
    assert.equal(h.input.value, '@"原 图.png" ');
    assert.equal(h.run('expandFileAliases(input.value)'), '@attachments/random.png ');
    assert.equal(h.input.value.includes('random'), false);
  }
});

test('multiple paste events upload sequentially and preserve text typed during upload', async () => {
  const h = harness(), first = deferred();
  let count = 0;
  h.c.reply = async () => ++count === 1 ? first.promise : response({ ok: true, staged_path: 'attachments/' + count + '.png' });
  h.text('before');
  const a = h.paste([file(), file()]);
  const b = h.paste([file()]);
  await tick();
  assert.equal(h.requests.length, 1);
  assert.equal(h.run('pendingUploads'), 3);
  h.text('before typed');
  await h.submit();
  assert.match(h.infos[0], /上传/);
  assert.equal(h.sent.length, 0);
  const change = await h.c.setWorkspace('D:/other');
  assert.equal(change.ok, false);
  assert.equal(h.run('workspaceRoot'), 'C:/work');
  assert.equal(h.requests.length, 1);
  first.resolve(response({ ok: true, staged_path: 'attachments/1.png' }));
  await a.done; await b.done;
  assert.equal(h.input.value, 'before typed @a.png @"a (2).png" @"a (3).png" ');
  assert.equal(h.requests.length, 3);
  assert.equal(h.run('pendingUploads'), 0);
  await h.submit();
  assert.deepEqual(h.sent[0].attachments, ['attachments/1.png', 'attachments/2.png', 'attachments/3.png']);
  assert.equal(h.users[0], 'before typed @a.png @"a (2).png" @"a (3).png"');
});

test('typed root mention and existing alias cannot be overwritten by uploaded names', async () => {
  const h = harness();
  h.text('@a.png @"a (2).png"');
  h.run('fileAlias.set(\'@"a (2).png"\', "C:/work/old.png")');
  h.c.reply = async () => response({ ok: true, staged_path: 'attachments/random.png' });
  await h.paste([file()]).done;
  assert.equal(h.input.value, '@a.png @"a (2).png" @"a (3).png" ');
  await h.submit();
  assert.deepEqual(h.sent[0].attachments, ['a.png', 'C:/work/old.png', 'attachments/random.png']);
});

test('upload failure preserves mixed clipboard text and adds no fake reference', async () => {
  for (const reply of [
    async () => response({ error: 'too large' }, false),
    async () => response({ ok: true }),
    async () => { throw new Error('offline'); },
    async () => ({ ok: true, json: async () => { throw new Error('bad JSON'); } })
  ]) {
    const h = harness();
    h.text('keep'); h.c.reply = reply;
    await h.paste([file()], [], 'pasted text').done;
    assert.equal(h.input.value, 'keep pasted text ');
    assert.equal(h.run('fileAlias.size'), 0);
    assert.equal(h.run('pendingUploads'), 0);
    assert.equal(h.errors.length, 1);
  }
});

test('20MiB limit is inclusive and failed files do not stop following uploads', async () => {
  const h = harness();
  h.c.reply = async () => response({ ok: true, staged_path: 'attachments/ok.png' });
  await h.paste([file('large.png', 20 * 1024 * 1024 + 1), file('ok.png', 20 * 1024 * 1024)]).done;
  assert.equal(h.requests.length, 1);
  assert.equal(h.input.value, '@ok.png ');
  assert.match(h.errors[0], /20MiB/);
  assert.equal(h.run('pendingUploads'), 0);
});

test('late upload and remaining queue are discarded after session switch intent', async () => {
  const h = harness(), gate = deferred();
  h.c.reply = () => gate.promise;
  const p = h.paste([file(), file()]);
  await tick();
  h.c.loadSession('s2');
  h.text('new session text');
  gate.resolve(response({ ok: true, staged_path: 'attachments/late.png' }));
  await p.done;
  assert.equal(h.requests.length, 1);
  assert.equal(h.input.value, 'new session text');
  assert.equal(h.run('fileAlias.size'), 0);
  assert.equal(h.run('pendingUploads'), 0);
});

test('workspace snapshot and epoch prevent late insertion even after switching back', async () => {
  for (const change of ['workspaceRoot = "D:/other"', 'composerEpoch += 2']) {
    const h = harness(), gate = deferred();
    h.c.reply = () => gate.promise;
    const p = h.paste([file()]); await tick();
    h.run(change); h.text('new input');
    gate.resolve(response({ ok: true, staged_path: 'attachments/late.png' }));
    await p.done;
    assert.equal(h.input.value, 'new input');
    assert.equal(h.run('fileAlias.size'), 0);
  }
});

test('workspace switch in progress blocks paste/send and rolls back failed switch', async () => {
  const h = harness(), gate = deferred();
  h.c.reply = () => gate.promise;
  const change = h.c.setWorkspace('D:/other');
  h.text('keep');
  await h.paste([file()]).done;
  await h.submit();
  assert.equal(h.requests.length, 1);
  assert.equal(h.sent.length, 0);
  gate.resolve(response({ error: 'bad path' }, false));
  assert.equal((await change).ok, false);
  assert.equal(h.run('workspaceRoot'), 'C:/work');
  assert.equal(h.run('workspaceChanging'), false);
});

test('uploaded aliases remain bound to original workspace', async () => {
  const h = harness();
  h.c.reply = async () => response({ ok: true, staged_path: 'attachments/random.png' });
  await h.paste([file()]).done;
  h.run('workspaceRoot = "D:/other"');
  await h.submit();
  assert.equal(h.input.value, '@a.png ');
  assert.equal(h.sent.length, 0);
  assert.match(h.errors[0], /其他工作区/);
});

test('WS payload uses display-independent text and attachments, locks duplicate submit until busy', async () => {
  const h = harness(), gate = deferred();
  h.text('@"D:/original name.png"');
  h.c.reply = () => gate.promise;
  const send = h.submit();
  await h.submit();
  assert.equal(h.requests.length, 1);
  assert.equal((await h.c.setWorkspace('D:/other')).ok, false);
  gate.resolve(response({ ok: true, staged_path: 'attachments/model name.png', name: 'original name.png' }));
  await send;
  assert.deepEqual(h.sent, [{ type: 'user_message', session_id: 's1', text: '@"attachments/model name.png"', attachments: ['attachments/model name.png'], thinking: 'high' }]);
  assert.deepEqual(h.users, ['@"D:/original name.png"']);
  h.text('next'); await h.submit();
  assert.equal(h.sent.length, 1);
  h.event('busy');
  assert.equal(h.run('sending'), false);
  // 运行中再按发送：输入框有字会走「转向」而非打断，这里要验的是打断路径，先清空。
  h.text('');
  await h.submit();
  assert.equal(h.sent[1].type, 'cancel');
  h.event('idle');
  h.text('第二轮');
  await h.submit();
  assert.deepEqual(h.sent[2].attachments, []);
});

test('input edits, changed session and lost socket during staging never send or erase draft', async () => {
  for (const mutate of [h => h.text('edited draft'), h => h.run('sessionID = "s2"'), h => h.run('ws.readyState = 3')]) {
    const h = harness(), gate = deferred();
    h.text('@D:/a.png'); h.c.reply = () => gate.promise;
    const send = h.submit(); mutate(h);
    const draft = h.input.value;
    gate.resolve(response({ ok: true, staged_path: 'attachments/a.png' }));
    await send;
    assert.equal(h.sent.length, 0);
    assert.equal(h.input.value, draft);
    assert.equal(h.errors.length, 1);
    assert.equal(h.run('sending'), false);
  }
});

test('alias deletion prunes workspace binding and does not hijack later typed mentions', async () => {
  const h = harness();
  h.c.reply = async () => response({ ok: true, staged_path: 'attachments/random.png' });
  await h.paste([file()]).done;
  h.text(''); h.c.syncInputMirror();
  assert.equal(h.run('uploadAliasWorkspace.size'), 0);
  h.text('@a.png'); await h.submit();
  assert.deepEqual(h.sent[0].attachments, ['a.png']);
});

test('delete workspace guard blocks upload and releases lock on network failure', async () => {
  const h = harness();
  h.run('pendingUploads = 1');
  await h.c.deleteWorkspace('C:/work');
  assert.equal(h.requests.length, 0);
  h.run('pendingUploads = 0');
  h.c.reply = async () => { throw new Error('offline'); };
  await h.c.deleteWorkspace('C:/work');
  assert.equal(h.run('workspaceChanging'), false);
});

(async function () {
  let passed = 0;
  for (const { name, fn } of tests) {
    try { await fn(); passed++; console.log('PASS ' + name); }
    catch (err) { console.error('FAIL ' + name); console.error(err); process.exitCode = 1; }
  }
  console.log(passed + '/' + tests.length + ' attachment tests passed');
})();
