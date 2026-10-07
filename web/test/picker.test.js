'use strict';

// 内置目录选择器的行为测试（不是源码正则）。
//
// 安卓上「要么点好几次弹不出来、要么弹出来一个空挂载」这两个症状都会
// 自我放大，且**都不是样式问题**，纯正则断言看不出来：
//   · 列举期间界面是一片空白 → 用户以为没弹出 → 再点；
//   · 重复点击会叠出多个面板 + 多条并发 tree 请求；
//   · 迟到的响应会把列表拉回用户早已离开的目录；
//   · 服务端把「目录读不出内容」吞成空列表，前端只能显示「无子目录」。
// 这里用一个最小 DOM 桩把 openBuiltinPicker / fetchTreeInto 真的跑起来，
// 断言上面四条在行为上被挡住。

const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const src = fs.readFileSync(path.join(__dirname, '../dist/ui.js'), 'utf8');

function extract(name) {
  const start = src.indexOf('function ' + name + '(');
  assert.notEqual(start, -1, '在 ui.js 中找不到 ' + name + '()');
  let i = src.indexOf('{', start);
  let depth = 0;
  for (; i < src.length; i++) {
    if (src[i] === '{') depth++;
    else if (src[i] === '}') { depth--; if (depth === 0) break; }
  }
  assert.equal(depth, 0, name + '() 花括号不配对');
  return src.slice(start, i + 1);
}

// ---------------------------------------------------------------------------
// 最小 DOM 桩：只覆盖选择器用到的 API。
// 元素有 classList / dataset / children，querySelector 支持 '#id' 与 '.class'
// 两种形式（源码只用了这两种），insertAdjacentHTML 只追加一个 div 占位。
// ---------------------------------------------------------------------------
class El {
  constructor(tag) {
    this.tagName = (tag || 'div').toUpperCase();
    this.children = [];
    this.parent = null;
    this.dataset = {};
    this._class = new Set();
    this._listeners = {};
    this._html = '';
    this.textContent = '';
    this.title = '';
    this.disabled = false;
  }
  get classList() {
    const set = this._class;
    const self = this;
    return {
      add(...c) { c.forEach((x) => set.add(x)); },
      remove(...c) { c.forEach((x) => set.delete(x)); },
      contains(c) { return set.has(c); },
      toggle(c, force) { if (force === undefined) { set.has(c) ? set.delete(c) : set.add(c); } else if (force) set.add(c); else set.delete(c); },
    };
  }
  get className() { return [...this._class].join(' '); }
  set className(v) { this._class = new Set(String(v).split(/\s+/).filter(Boolean)); }
  get innerHTML() { return this._html; }
  set innerHTML(v) { this._html = String(v); this.children = []; }
  appendChild(el) { el.parent = this; this.children.push(el); return el; }
  // 空态提示走的是 insertAdjacentHTML，内容同样要能被 text() 读到
  insertAdjacentHTML(_pos, html) {
    const el = new El('div');
    el._html = String(html);
    this.appendChild(el);
  }
  addEventListener(type, fn) { (this._listeners[type] = this._listeners[type] || []).push(fn); }
  removeEventListener() {}
  click() { (this._listeners.click || []).forEach((f) => f({ target: this })); }
  matches() { return false; }
  querySelector(sel) { return findIn(this, sel); }
  querySelectorAll(sel) {
    const out = [];
    (function walk(el) {
      for (const c of el.children) {
        if (match(c, sel)) out.push(c);
        walk(c);
      }
    })(this);
    return out;
  }
  // 取可见文本。innerHTML 是桩里唯一的内容载体（不解析成节点），
  // 所以它必须计入 —— 否则「读取中…」这类纯 innerHTML 文案全都测不到。
  text() {
    return this._html + ' ' + this.textContent + ' ' + this.children.map((c) => c.text()).join(' ');
  }
  // 列出直接子节点的 class，便于断言条目渲染
  classes() { return this.children.map((c) => [...c._class].join(' ')); }
}

// 支持源码实际用到的三种形式：'#id' / '.class' / '.class:not(.other)'
function match(el, sel) {
  const not = /:not\(([^)]+)\)/.exec(sel);
  if (not && match(el, not[1])) return false;
  const base = not ? sel.slice(0, not.index) : sel;
  if (base.startsWith('#')) return el.id === base.slice(1);
  if (base.startsWith('.')) return el._class.has(base.slice(1));
  return el.tagName === base.toUpperCase();
}
function findIn(root, sel) {
  const stack = [...root.children];
  while (stack.length) {
    const el = stack.shift();
    if (match(el, sel)) return el;
    stack.unshift(...el.children);
  }
  return null;
}

// ---------------------------------------------------------------------------
// 搭建被测环境：document 里预置 #picker-overlay 的骨架（源码里它是懒创建的，
// 这里预置一份好让测试直接驱动；重建路径另有 openBuiltinPicker 的用例覆盖）。
// ---------------------------------------------------------------------------
function harness(opts) {
  opts = opts || {};
  const body = new El('body');
  const picker = new El('div');
  picker.id = 'picker-overlay';
  picker.className = 'hidden';
  const pathEl = new El('span'); pathEl.id = 'picker-path';
  const listEl = new El('div'); listEl.id = 'picker-list';
  const cancel = new El('button'); cancel.id = 'picker-cancel';
  const ok = new El('button'); ok.id = 'picker-ok';
  const modal = new El('div');
  modal.appendChild(pathEl); modal.appendChild(listEl);
  modal.appendChild(cancel); modal.appendChild(ok);
  picker.appendChild(modal);
  body.appendChild(picker);

  const document = {
    body,
    getElementById: (id) => (id === 'picker-overlay' ? picker : null),
    createElement: (t) => new El(t),
    addEventListener() {},
  };

  const pending = [];   // 尚未 resolve 的 tree 响应
  const requested = []; // 请求过的 path，按发出顺序

  const c = {
    document,
    pickerMode: 'dir', pickerOnPick: null, pickerSel: '',
    fetch(url) {
      const m = /path=([^&]*)/.exec(url);
      const p = m ? decodeURIComponent(m[1]) : '';
      requested.push(p);
      if (opts.failFetch) return Promise.reject(new Error('连接被拒绝'));
      return new Promise((resolve) => {
        pending.push({ path: p, resolve: () => resolve({ json: () => Promise.resolve(opts.reply(p)) }) });
      });
    },
    // 让第 i 个挂起的请求按顺序落地（模拟乱序返回）
    settle(i, data) {
      const job = pending[i];
      if (!job) throw new Error('没有挂起的请求 #' + i);
      job.resolve();
      return job.path;
    },
  };
  c.pending = pending;
  c.requested = requested;
  c.picker = picker;
  c.listEl = listEl;
  c.pathEl = pathEl;
  c.ok = ok;
  c.cancel = cancel;
  c.overlays = () => body.children.filter((x) => x.id === 'picker-overlay').length;

  c.__settleAll = async function (reply) {
    // 依次 resolve 全部挂起请求并让微任务跑完
    while (c.pending.length) {
      const job = c.pending.shift();
      job.resolve();
      await tick();
    }
  };
  return c;
}

const tick = () => new Promise((r) => setTimeout(r, 0));

// 把 openBuiltinPicker / fetchTreeInto 装进同一上下文并求值
function load(c, onPick) {
  const sandbox = vm.createContext(c);
  vm.runInContext(extract('fetchTreeInto'), sandbox);
  // pickerMode / pickerOnPick / pickerSel 声明在 openBuiltinPicker **之前**，
  // 测试上下文已预置同名全局；这里把 `let` 去掉，避免遮蔽掉它们。
  vm.runInContext(
    extract('openBuiltinPicker').replace('let pickerMode', '/*let pickerMode'),
    sandbox
  );
  c.__open = (startPath, mode, cb) => sandbox.openBuiltinPicker(startPath, mode, cb || onPick);
  return sandbox;
}

// ---------------------------------------------------------------------------
// 用例
// ---------------------------------------------------------------------------
const tests = [];
const test = (name, fn) => tests.push({ name, fn });

test('列举期间显示「读取中」而不是空白列表（空白会被当成没弹出）', async () => {
  const c = harness({ reply: () => ({ items: [] }) });
  load(c);
  c.__open('/home', 'dir', () => {});
  assert.match(c.listEl.text(), /读取中/, '列举期间应有明确反馈');
  await c.__settleAll();
  assert.doesNotMatch(c.listEl.text(), /读取中/, '落地后不应还显示读取中');
});

test('read_error 单独渲染为警告，不显示成「无子目录」', async () => {
  const c = harness({
    reply: () => ({ path: '/storage/emulated/0', parent: '/storage/emulated', read_error: '无法读取手机存储：permission denied。请在 Termux 执行 termux-setup-storage 后重试' }),
  });
  load(c);
  c.__open('/storage/emulated/0', 'dir', () => {});
  await c.__settleAll();
  const txt = c.listEl.text();
  assert.match(txt, /termux-setup-storage/, '应显示可执行的下一步，实际: ' + txt);
  assert.doesNotMatch(txt, /无子目录/, '「读不到」不能被渲染成「无子目录」');
});

test('真空目录仍然显示「无子目录」（修复没有把空当成错误）', async () => {
  const c = harness({ reply: () => ({ path: '/home/empty', parent: '/home', items: [] }) });
  load(c);
  c.__open('/home/empty', 'dir', () => {});
  await c.__settleAll();
  assert.match(c.listEl.text(), /无子目录/);
});

test('重复点击不会叠出第二个面板，复位到新的起始目录', async () => {
  const c = harness({ reply: (p) => ({ path: p, parent: '/', items: [] }) });
  load(c);
  c.__open('/a', 'dir', () => {});
  c.__open('/b', 'dir', () => {});
  // 面板 DOM 只建一次：源码里 getElementById 始终返回同一个节点，
  // 这里断言两次调用后 body 下仍只有一个 picker-overlay。
  assert.equal(c.overlays(), 1, '重复点击不得叠出第二个面板');
  await c.__settleAll();
  assert.deepEqual(c.requested, ['/a', '/b'], '两次点击各发一次请求，但都打在同一个面板上');
});

test('迟到的旧响应不覆盖用户已经切走的目录', async () => {
  const c = harness({
    reply: (p) => ({
      path: p, parent: '/',
      items: [{ name: 'marker-' + p.replace(/\W/g, ''), path: p + '/x', is_dir: true }],
    }),
  });
  load(c);
  c.__open('/slow', 'dir', () => {});   // 请求 0：慢
  c.__open('/fast', 'dir', () => {});   // 请求 1：快
  // 先落地第 1 个（用户已经点了 /fast），再落地第 0 个（迟到的 /slow）
  c.pending[1].resolve();
  await tick();
  assert.match(c.listEl.text(), /marker-fast/, '应显示 /fast 的内容');
  c.pending[0].resolve();
  await tick();
  assert.match(c.listEl.text(), /marker-fast/, '迟到的 /slow 不该把列表拉回去，实际: ' + c.listEl.text());
  assert.doesNotMatch(c.listEl.text(), /marker-slow/);
});

// 面板关闭（确认 / 取消）后再打开必须真的弹出来。
// 「怎么点都不弹」是安卓上最恼人的那种故障，所以两条关闭路径都要钉住。
test('确认关闭后再次打开仍能弹出', async () => {
  const c = harness({ reply: (p) => ({ path: p, parent: '/', items: [] }) });
  load(c, () => {});
  c.__open('/a', 'dir', () => {});
  await c.__settleAll();
  c.ok.click();                       // 确认 → 关闭
  c.picker.classList.add('hidden');   // hideWithAnim 的效果
  c.__open('/b', 'dir', () => {});    // 再次打开
  assert.ok(!c.picker.classList.contains('hidden'), '再次打开必须真的弹出来');
  await c.__settleAll();
  assert.equal(c.pathEl.textContent, '/b');
});

test('取消关闭后再次打开仍能弹出', async () => {
  const c = harness({ reply: (p) => ({ path: p, parent: '/', items: [] }) });
  load(c, () => {});
  c.__open('/a', 'dir', () => {});
  await c.__settleAll();
  c.cancel.click();
  c.picker.classList.add('hidden');
  c.__open('/b', 'dir', () => {});
  assert.ok(!c.picker.classList.contains('hidden'));
  await c.__settleAll();
  assert.equal(c.pathEl.textContent, '/b');
});

// 重复点击不该叠面板，但**每次点击都要重新列举**到各自的起始目录 ——
// 若第二次点击被静默吞掉，用户看到的还是上一次的目录（正是「点了没反应」）。
test('重复点击各自复位到自己的起始目录（第二次不被静默忽略）', async () => {
  const c = harness({ reply: (p) => ({ path: p, parent: '/', items: [] }) });
  load(c);
  c.__open('/first', 'dir', () => {});
  c.__open('/second', 'dir', () => {});
  await c.__settleAll();
  assert.equal(c.overlays(), 1, '不得叠出第二个面板');
  assert.equal(c.pathEl.textContent, '/second', '最后一次点击的目录必须胜出');
});

test('dir 模式「选择当前目录」始终可点（只有 file 模式才禁用）', async () => {
  const c = harness({ reply: (p) => ({ path: p, parent: '/', items: [] }) });
  load(c);
  c.__open('/a', 'dir', () => {});
  await c.__settleAll();
  assert.equal(c.ok.disabled, false, 'dir 模式下按钮必须在第一帧就可点');
  c.__open('/a', 'file', () => {});
  await c.__settleAll();
  assert.equal(c.ok.disabled, true, 'file 模式没选中文件前应禁用');
});

test('请求失败显示原因，且不把面板锁死', async () => {
  const c = harness({ failFetch: true });
  load(c);
  c.__open('/a', 'dir', () => {});
  await tick(); await tick();
  assert.match(c.listEl.text(), /读取失败/, '失败要有原因，实际: ' + c.listEl.text());
  c.picker.classList.add('hidden');
  c.__open('/b', 'dir', () => {});
  assert.ok(!c.picker.classList.contains('hidden'), '失败后仍可再次打开');
});

test('.. 返回上一级仍可用（parent 由服务端给）', async () => {
  const c = harness({
    reply: (p) => ({ path: p, parent: '/home', items: [{ name: 'sub', path: p + '/sub', is_dir: true }] }),
  });
  load(c);
  c.__open('/home/x', 'dir', () => {});
  await c.__settleAll();
  const up = c.listEl.children.find((x) => x._class.has('picker-up'));
  assert.ok(up, '应有「.. 返回上一级」行');
  assert.equal(up.textContent, '📂 .. 返回上一级');
});

// ---------------------------------------------------------------------------
(async () => {
  let pass = 0;
  const failed = [];
  for (const t of tests) {
    try {
      await t.fn();
      pass++;
      console.log('PASS ' + t.name);
    } catch (err) {
      failed.push(t.name);
      console.log('FAIL ' + t.name + '\n      ' + (err && err.message));
    }
  }
  console.log('\n' + pass + '/' + tests.length + ' picker tests passed');
  if (failed.length) process.exit(1);
})();