/* ══════════════════════════════════════════════════════════════════
   CodeForge 独立设置页
   —— 从 web/dist/ui.js 的设置部分提取；模型页重排为
      「供应商 ▸ 模型 ▸ 详情」三栏，其余各页保持原有行为。
   —— 所有请求走同源 /api/*，由 serve.py 反向代理到 codeforge 服务
      并注入鉴权 Cookie，因此这里不需要处理 token。
   ══════════════════════════════════════════════════════════════════ */
(function () {
'use strict';

/* ════════ 通用 ════════ */

function $(sel, root) { return (root || document).querySelector(sel); }
function $$(sel, root) { return Array.prototype.slice.call((root || document).querySelectorAll(sel)); }

function el(tag, cls, text) {
  var e = document.createElement(tag);
  if (cls) e.className = cls;
  if (text !== undefined && text !== null) e.textContent = text;
  return e;
}
function button(text, cls, fn) {
  var b = el('button', cls, text);
  b.type = 'button';
  if (fn) b.addEventListener('click', fn);
  return b;
}
function input(type, value, placeholder) {
  var i = el('input');
  i.type = type;
  if (value) i.value = value;
  if (placeholder) i.placeholder = placeholder;
  return i;
}
function field(label, control, wide) {
  var w = el('label', 'f-field' + (wide ? ' wide' : ''));
  w.appendChild(el('span', null, label));
  w.appendChild(control);
  return w;
}
function fmtCtx(n) {
  n = Number(n) || 0;
  if (n >= 1000000) return (n / 1000000).toFixed(2).replace(/\.?0+$/, '') + 'M';
  if (n >= 1000) return Math.round(n / 1000) + 'K';
  return String(n);
}
function fmtCtxSlot(n) {
  n = Number(n) || 0;
  if (n >= 1048576 && n % 1048576 === 0) return (n / 1048576) + 'M';
  if (n >= 1024 && n % 1024 === 0) return (n / 1024) + 'K';
  return String(n);
}
function setNote(node, ok, msg) {
  if (!node) return;
  node.className = 'note' + (ok === true ? ' ok' : ok === false ? ' fail' : '');
  node.textContent = msg || '';
}
var bannerTimer = null;
function showBanner(msg, level) {
  var b = $('#banner');
  b.className = 'banner' + (level ? ' ' + level : '');
  b.textContent = msg;
  clearTimeout(bannerTimer);
  bannerTimer = setTimeout(function () { b.className = 'banner hidden'; }, level === 'error' ? 7000 : 4000);
}
function setConn(on, text) {
  var dot = $('#conn-dot');
  dot.className = 'dot ' + (on ? 'on' : 'off');
  $('#conn-text').textContent = text || (on ? '已连接' : '未连接');
}

/* 统一 API：JSON 解包 + 401/网络错误提示 */
function api(url, opts) {
  opts = opts || {};
  if (opts.body && typeof opts.body !== 'string') {
    opts.headers = Object.assign({ 'Content-Type': 'application/json' }, opts.headers || {});
    opts.body = JSON.stringify(opts.body);
  }
  return fetch(url, opts).then(function (r) {
    return r.text().then(function (t) {
      var data = {};
      try { data = t ? JSON.parse(t) : {}; } catch (e) { data = { error: t.slice(0, 300) }; }
      if (r.status === 401) setConn(false, '未授权');
      return { ok: r.ok, status: r.status, data: data };
    });
  }).catch(function (e) {
    setConn(false, '服务不可达');
    return { ok: false, status: 0, data: { error: '服务不可达：' + e } };
  });
}

/* ════════ 页面切换 ════════ */

var pageLoaders = {};
var currentPage = '';
function showPage(id) {
  currentPage = id;
  $$('.nav-item').forEach(function (n) { n.classList.toggle('active', n.dataset.page === id); });
  $$('.page').forEach(function (p) { p.classList.toggle('active', p.id === 'page-' + id); });
  var fn = pageLoaders[id];
  if (fn) fn();
}
$$('.nav-item').forEach(function (n) {
  n.addEventListener('click', function () { showPage(n.dataset.page); });
});
/* 刷新：重测连接 + 重新加载当前页数据（不跳页、不丢选中的模型/供应商） */
$('#btn-refresh').addEventListener('click', function () {
  api('/api/health').then(function (res) {
    setConn(!!res.ok, res.ok ? '已连接' : '服务异常');
  });
  var fn = pageLoaders[currentPage];
  if (fn) fn();
  showBanner('已刷新');
});

/* ════════ 常规 ════════ */

function loadGeneral() {
  api('/api/workspace').then(function (res) {
    var root = (res.data || {}).root || '--';
    var e = $('#settings-workdir');
    e.textContent = root; e.title = root;
  });
  api('/api/models/list').then(function (res) {
    var d = res.data || {};
    var active = d.active || '';
    var hit = (d.models || []).filter(function (m) { return m.id === active; })[0];
    var e = $('#settings-model');
    e.textContent = hit ? (hit.name || hit.id) : (active || '--');
    e.title = active;
  });
  api('/api/notify').then(function (res) {
    if (res.ok) $('#settings-notify').checked = res.data.enabled !== false;
  });
  api('/api/config').then(function (res) {
    var cfg = res.data || {};
    $('#max-steps').value = cfg.max_steps > 0 ? cfg.max_steps : 500;
    $('#retry-attempts').value = cfg.retry_max_attempts || 5;
    $('#retry-mode').value = cfg.retry_mode === 'fixed' ? 'fixed' : 'backoff';
    $('#retry-interval').value = cfg.retry_interval_sec > 0 ? cfg.retry_interval_sec : 1;
  });
}
pageLoaders.general = loadGeneral;

$('#settings-notify').addEventListener('change', function () {
  var e = this, want = e.checked;
  e.disabled = true;
  api('/api/notify', { method: 'POST', body: { enabled: want } }).then(function (res) {
    if (!res.ok) { e.checked = !want; showBanner('通知设置保存失败：' + (res.data.error || '未知错误'), 'error'); }
  }).then(function () { e.disabled = false; });
});
$('#max-steps-save').addEventListener('click', function () {
  var b = this, note = $('#max-steps-result');
  var v = Math.max(1, Number($('#max-steps').value) || 500);
  b.disabled = true; setNote(note, null, '保存中…');
  api('/api/config', { method: 'POST', body: { max_steps: v } }).then(function (res) {
    if (res.ok) setNote(note, true, '已保存');
    else { setNote(note, false, res.data.error || '保存失败'); showBanner('轮数设置保存失败：' + (res.data.error || '未知错误'), 'error'); }
  }).then(function () { b.disabled = false; });
});
$('#retry-save').addEventListener('click', function () {
  var b = this, note = $('#retry-result');
  var body = {
    retry_max_attempts: Math.max(1, Math.min(15, Number($('#retry-attempts').value) || 5)),
    retry_mode: $('#retry-mode').value,
    retry_interval_sec: Math.max(1, Math.min(60, Number($('#retry-interval').value) || 1))
  };
  b.disabled = true; setNote(note, null, '保存中…');
  api('/api/config', { method: 'POST', body: body }).then(function (res) {
    if (res.ok) setNote(note, true, '已保存');
    else { setNote(note, false, res.data.error || '保存失败'); showBanner('重试设置保存失败：' + (res.data.error || '未知错误'), 'error'); }
  }).then(function () { b.disabled = false; });
});

/* ════════ 外观 ════════ */

(function initAppearance() {
  function applyComposerSize(w, h) {
    document.documentElement.style.setProperty('--composer-max-w', w + 'px');
    document.documentElement.style.setProperty('--composer-min-h', h + 'px');
  }
  var wEl = $('#opt-comp-w'), hEl = $('#opt-comp-h');
  var w = parseInt(localStorage.getItem('cf_composer_w'), 10) || 760;
  var h = parseInt(localStorage.getItem('cf_composer_h'), 10) || 60;
  wEl.value = w; hEl.value = h;
  $('#opt-comp-w-val').textContent = w + 'px';
  $('#opt-comp-h-val').textContent = h + 'px';
  wEl.addEventListener('input', function () {
    w = +wEl.value; $('#opt-comp-w-val').textContent = w + 'px';
    applyComposerSize(w, h); localStorage.setItem('cf_composer_w', w);
  });
  hEl.addEventListener('input', function () {
    h = +hEl.value; $('#opt-comp-h-val').textContent = h + 'px';
    applyComposerSize(w, h); localStorage.setItem('cf_composer_h', h);
  });
  applyComposerSize(w, h);

  var blurEl = $('#opt-bg-blur'), brightEl = $('#opt-bg-bright');
  var blurVal = el('span', 'slider-val'), brightVal = el('span', 'slider-val');
  function loadBg() {
    api('/api/appearance').then(function (res) {
      if (!res.ok) return;
      var d = res.data || {};
      blurEl.value = typeof d.blur === 'number' ? d.blur : 0;
      brightEl.value = typeof d.brightness === 'number' ? d.brightness : 100;
      var set = !!d.background_set;
      $('#bg-path').placeholder = set ? '当前已设置背景；填新路径可更换' : '如 /sdcard/Pictures/bg.jpg';
      setNote($('#bg-result'), set ? true : null, set ? '当前：已设置背景图' : '');
    });
  }
  pageLoaders.appearance = loadBg;
  $('#bg-save').addEventListener('click', function () {
    var b = this, note = $('#bg-result');
    var path = $('#bg-path').value.trim();
    if (!path) { setNote(note, false, '请先填写图片绝对路径'); return; }
    b.disabled = true; setNote(note, null, '保存中…');
    api('/api/appearance', {
      method: 'POST',
      body: { background_path: path, blur: +blurEl.value, brightness: +brightEl.value }
    }).then(function (res) {
      if (res.ok) { setNote(note, true, '已保存，主界面刷新后生效'); loadBg(); }
      else setNote(note, false, res.data.error || '保存失败');
    }).then(function () { b.disabled = false; });
  });
  $('#bg-clear').addEventListener('click', function () {
    api('/api/appearance', { method: 'POST', body: { clear: true } }).then(function (res) {
      if (res.ok) { setNote($('#bg-result'), true, '已清除背景'); $('#bg-path').value = ''; loadBg(); }
      else setNote($('#bg-result'), false, res.data.error || '清除失败');
    });
  });
})();

/* ════════ 记忆 ════════ */

function loadMemories() {
  var box = $('#mem-items');
  api('/api/memory').then(function (res) {
    var items = (res.data || {}).items || [];
    box.innerHTML = '';
    if (!items.length) {
      box.appendChild(el('div', 'mem-empty', '还没有记忆。对话中让 AI「记住…」，或用上方输入框手动添加。'));
      return;
    }
    items.forEach(function (m) {
      var row = el('div', 'mem-item');
      row.appendChild(el('span', 'mem-text', m.content));
      row.appendChild(el('span', 'mem-time',
        new Date((m.updated_at || m.created_at) * 1000).toLocaleDateString()));
      row.appendChild(button('✎', 'btn small ghost', function () {
        var v = prompt('编辑记忆：', m.content);
        if (v == null || !v.trim() || v.trim() === m.content) return;
        api('/api/memory', { method: 'PATCH', body: { id: m.id, content: v.trim() } })
          .then(function (r) { if (r.data.ok) loadMemories(); });
      }));
      var del = button('✕', 'btn small ghost', function () {
        var confirmBtn = button('确认删除', 'btn small confirming', function () {
          confirmBtn.disabled = true;
          api('/api/memory?id=' + encodeURIComponent(m.id), { method: 'DELETE' })
            .then(function (r) {
              if (r.data && r.data.ok === false) { showBanner('删除失败：' + (r.data.error || '未知错误'), 'error'); return; }
              loadMemories();
            });
        });
        var cancelBtn = button('取消', 'btn small ghost', function () {
          row.replaceChild(del, confirmBtn);
          row.removeChild(cancelBtn);
        });
        row.replaceChild(confirmBtn, del);
        row.appendChild(cancelBtn);
      });
      row.appendChild(del);
      box.appendChild(row);
    });
  });
}
pageLoaders.memory = loadMemories;
$('#mem-add').addEventListener('click', function () {
  var v = $('#mem-new').value.trim();
  if (!v) return;
  api('/api/memory', { method: 'POST', body: { content: v } }).then(function (r) {
    if (r.data.ok) { $('#mem-new').value = ''; loadMemories(); }
    else showBanner('添加失败：' + (r.data.error || '未知错误'), 'error');
  });
});
$('#mem-new').addEventListener('keydown', function (e) {
  if (e.key === 'Enter') $('#mem-add').click();
});

/* ════════ 技能 ════════ */

function loadSkills() {
  var box = $('#skill-items');
  api('/api/skills').then(function (res) {
    var items = (res.data || {}).items || [];
    box.innerHTML = '';
    if (!items.length) {
      box.appendChild(el('div', 'mem-empty', '当前项目还没有技能。在 <工作区>/.codeforge/skills/<名称>/SKILL.md 创建即可被识别。'));
      return;
    }
    items.forEach(function (sk) {
      var row = el('div', 'skill-item');
      row.appendChild(el('div', 'sk-name', (sk.enabled ? '⚡ ' : '⏸ ') + sk.name));
      if (sk.description) row.appendChild(el('div', 'sk-desc', sk.description));
      if (sk.triggers && sk.triggers.length) row.appendChild(el('div', 'sk-triggers', '触发词：' + sk.triggers.join('、')));
      if (sk.path) row.appendChild(el('div', 'sk-path', sk.path));
      box.appendChild(row);
    });
  });
}
pageLoaders.skills = loadSkills;

/* ════════ 插件（内置） ════════ */

function loadBuiltin() {
  var box = $('#builtin-items');
  box.innerHTML = '';
  box.appendChild(el('div', 'mem-empty', '加载中…'));
  api('/api/builtin-plugins').then(function (res) {
    var items = (res.data || {}).items || [];
    box.innerHTML = '';
    if (!items.length) { box.appendChild(el('div', 'mem-empty', '没有可用的内置插件')); return; }
    items.forEach(function (p) {
      var row = el('div', 'bp-item');
      var top = el('div', 'bp-top');
      var main = el('div', 'bp-main');
      main.appendChild(el('div', 'bp-name', p.name));
      if (p.purpose) main.appendChild(el('div', 'bp-purpose', p.purpose));
      if (p.when_to_use) main.appendChild(el('div', 'bp-when', '调用时机：' + p.when_to_use));
      top.appendChild(main);
      var sw = el('label', 'switch');
      var cb = input('checkbox');
      cb.checked = !!p.enabled;
      var track = el('span', 'switch-track');
      track.appendChild(el('span', 'switch-knob'));
      sw.appendChild(cb); sw.appendChild(track);
      cb.addEventListener('change', function () {
        api('/api/builtin-plugins', { method: 'PATCH', body: { id: p.id, enabled: cb.checked } })
          .then(function (r) {
            if (!r.data.ok) { cb.checked = !cb.checked; showBanner('切换插件失败：' + (r.data.error || '未知错误'), 'error'); }
            else p.enabled = cb.checked;
          });
      });
      top.appendChild(sw);
      row.appendChild(top);
      if (p.setting && typeof p.setting.min === 'number') {
        var st = p.setting;
        var wrap = el('div', 'bp-setting');
        wrap.appendChild(el('span', null, st.label || '参数'));
        var num = input('number', String(st.value));
        num.min = String(st.min); num.max = String(st.max); num.step = '1';
        num.addEventListener('change', function () {
          var v = parseInt(num.value, 10);
          if (isNaN(v)) { num.value = String(st.value); return; }
          v = Math.max(st.min, Math.min(st.max, v));
          num.value = String(v);
          if (v === st.value) return;
          api('/api/builtin-plugins', { method: 'PATCH', body: { id: p.id, value: v } })
            .then(function (r) {
              if (r.data.ok) { st.value = v; }
              else { num.value = String(st.value); showBanner('保存失败：' + (r.data.error || '未知原因'), 'error'); }
            });
        });
        wrap.appendChild(num);
        if (st.unit) wrap.appendChild(el('span', null, st.unit));
        row.appendChild(wrap);
      }
      box.appendChild(row);
    });
  });
}
pageLoaders.builtin = loadBuiltin;

/* ════════ 子智能体 ════════ */

function setSubNote(msg, ok) {
  var node = $('#subagents-result');
  node.className = 'note' + (ok === true ? ' ok' : ok === false ? ' fail' : '');
  node.textContent = msg || '';
}
function stepsLabel(v, inheritVal) {
  return v ? (v + ' 步') : ('跟随主循环（' + inheritVal + ' 步）');
}
function syncSubLocks() {
  var enabled = $('#sub-enabled').checked;
  ['sub-max', 'sub-steps', 'sub-allow-write', 'sub-allow-delete', 'sub-allow-memory'].forEach(function (id) {
    $('#' + id).disabled = !enabled;
  });
  if (enabled && !$('#sub-allow-write').checked) $('#sub-allow-delete').disabled = true;
}
function applySubView(d) {
  var cap = d.max_allowed || 5;
  $('#sub-enabled').checked = d.enabled !== false;
  var range = $('#sub-max');
  range.max = cap;
  range.value = d.max_concurrent || cap;
  $('#sub-max-cap').textContent = cap;
  $('#sub-max-val').textContent = range.value + ' 个';
  var eff = Number(d.steps_effective) || 0;
  $('#sub-steps-inherit-val').textContent = eff;
  var steps = $('#sub-steps');
  steps.max = Number(d.max_steps_allowed) || 60;
  steps.value = Number(d.max_steps) || 0;
  $('#sub-steps-val').textContent = stepsLabel(steps.value, eff);
  $('#sub-allow-write').checked = d.allow_write !== false;
  $('#sub-allow-delete').checked = d.allow_delete !== false;
  $('#sub-allow-memory').checked = d.allow_memory !== false;
  syncSubLocks();
}
function postSub(patch, okMsg) {
  setSubNote('保存中…');
  return api('/api/subagents', { method: 'POST', body: patch }).then(function (res) {
    if (!res.ok) { setSubNote('保存失败：' + (res.data.error || '未知错误'), false); return false; }
    if (res.data.settings) applySubView(res.data.settings);
    setSubNote(okMsg || '已保存', true);
    return true;
  });
}
function loadSubagents() {
  setSubNote('加载中…');
  api('/api/subagents').then(function (res) {
    if (!res.ok) { setSubNote('加载失败，请确认服务正在运行', false); return; }
    applySubView(res.data);
    setSubNote('');
  });
}
pageLoaders.subagents = loadSubagents;
(function bindSubagents() {
  $('#sub-enabled').addEventListener('change', function () {
    var want = this.checked;
    this.disabled = true;
    postSub({ enabled: want }, want ? '已启用多智能体协作' : '已关闭多智能体协作')
      .then(function (ok) { if (!ok) $('#sub-enabled').checked = !want; syncSubLocks(); });
  });
  var range = $('#sub-max');
  range.addEventListener('input', function () { $('#sub-max-val').textContent = range.value + ' 个'; });
  range.addEventListener('change', function () {
    postSub({ max_concurrent: Number(range.value) }, '并发上限已设为 ' + range.value + ' 个');
  });
  var steps = $('#sub-steps');
  steps.addEventListener('input', function () {
    var eff = Number($('#sub-steps-inherit-val').textContent) || 0;
    $('#sub-steps-val').textContent = stepsLabel(Number(steps.value) || 0, eff);
  });
  steps.addEventListener('change', function () {
    var raw = Number(steps.value);
    var n = isFinite(raw) && raw > 0 ? Math.floor(raw) : 0;
    steps.value = n;
    postSub({ max_steps: n }, n ? '单任务步数上限已设为 ' + n + ' 步' : '已改为跟随主循环的步数上限')
      .then(function (ok) { if (!ok) loadSubagents(); });
  });
  [['sub-allow-write', 'allow_write', '写入文件'],
   ['sub-allow-delete', 'allow_delete', '删除文件'],
   ['sub-allow-memory', 'allow_memory', '写入记忆']].forEach(function (pair) {
    $('#' + pair[0]).addEventListener('change', function () {
      var patch = {}; patch[pair[1]] = this.checked;
      postSub(patch, (this.checked ? '已允许子智能体' : '已禁止子智能体') + pair[2])
        .then(function (ok) { if (!ok) loadSubagents(); syncSubLocks(); });
    });
  });
})();

/* ════════ MCP 服务 ════════ */

function renderMcpList() {
  var box = $('#mcp-items');
  box.innerHTML = '';
  box.appendChild(el('div', 'mem-empty', '加载中…'));
  api('/api/plugins').then(function (res) {
    var items = (res.data || {}).items || [];
    box.innerHTML = '';
    if (!items.length) {
      box.appendChild(el('div', 'mem-empty', '还没有 MCP 服务，切到「添加服务」新建一个'));
      return;
    }
    items.forEach(function (p) {
      var row = el('div', 'mcp-row');
      var main = el('div', 'mcp-main');
      var nm = el('div', 'mcp-name', (p.configured ? '⚡ ' : '⏸ ') + (p.display_name || p.name));
      nm.appendChild(el('span', 'badge', p.type));
      if (p.display_name && p.display_name !== p.name) nm.appendChild(el('span', 'badge', p.name));
      main.appendChild(nm);
      var detail = p.command ? (p.command + ' ' + (p.args || []).join(' ')) : (p.endpoint || p.path || '');
      var meta = el('div', 'mcp-meta', detail + (p.description ? ' — ' + p.description : ''));
      if (p.configured && !p.enabled) {
        meta.textContent += '  ⚠ 已启用但进程未运行：检查启动命令/参数或依赖';
      }
      main.appendChild(meta);
      row.appendChild(main);
      row.appendChild(el('span', 'badge ' + (p.enabled ? 'ok' : (p.configured ? 'err' : '')),
        p.enabled ? '运行中' : (p.configured ? '加载失败' : '已停用')));
      var acts = el('div', 'mcp-actions');
      acts.appendChild(button(p.configured ? '停用' : '启用', 'btn small', function () {
        api('/api/plugins', { method: 'PATCH', body: { name: p.name, enabled: !p.configured } })
          .then(function (r) {
            if (r.data.warning) showBanner(r.data.warning, 'warn');
            renderMcpList();
          });
      }));
      var del = button('删除', 'btn small', function () {
        var confirmBtn = button('确认删除', 'btn small confirming', function () {
          confirmBtn.disabled = true;
          api('/api/plugins?name=' + encodeURIComponent(p.name), { method: 'DELETE' }).then(function (r) {
            if (r.data && r.data.ok === false) { showBanner('删除失败：' + (r.data.error || '未知错误'), 'error'); return; }
            renderMcpList();
          });
        });
        var cancelBtn = button('取消', 'btn small', function () {
          acts.replaceChild(del, confirmBtn);
          acts.removeChild(cancelBtn);
        });
        acts.replaceChild(confirmBtn, del);
        acts.appendChild(cancelBtn);
      });
      acts.appendChild(del);
      row.appendChild(acts);
      box.appendChild(row);
    });
  });
}
function setMcpPane(paneId) {
  $$('#page-mcp .tab').forEach(function (t) { t.classList.toggle('active', t.dataset.pane === paneId); });
  $$('#page-mcp .pane').forEach(function (p) { p.classList.toggle('active', p.id === paneId); });
  if (paneId === 'mcp-pane-list') renderMcpList();
}
pageLoaders.mcp = renderMcpList;
$$('#page-mcp .tab').forEach(function (t) {
  t.addEventListener('click', function () { setMcpPane(t.dataset.pane); });
});
(function bindMcpForm() {
  var typeSel = $('#mcp-f-type');
  function syncType() {
    var remote = typeSel.value === 'mcp-http';
    ['mcp-f-cmd-field', 'mcp-f-args-field', 'mcp-f-env-field'].forEach(function (id) {
      $('#' + id).classList.toggle('hidden', remote);
    });
    $('#mcp-f-endpoint-field').classList.toggle('hidden', !remote);
  }
  typeSel.addEventListener('change', syncType);
  syncType();
  $('#mcp-f-add').addEventListener('click', function () {
    var note = $('#mcp-f-result');
    function fail(msg) { setNote(note, false, msg); }
    var name = $('#mcp-f-name').value.trim();
    var displayName = $('#mcp-f-display').value.trim();
    var remote = typeSel.value === 'mcp-http';
    var cmd = $('#mcp-f-cmd').value.trim();
    var endpoint = $('#mcp-f-endpoint').value.trim();
    var args = $('#mcp-f-args').value.trim();
    var envRaw = $('#mcp-f-env').value.trim();
    var desc = $('#mcp-f-desc').value.trim();
    if (!name) { fail('名称不能为空'); return; }
    if (remote) {
      if (!/^https?:\/\/\S+$/i.test(endpoint)) { fail('端点必须是以 http:// 或 https:// 开头的完整 URL'); return; }
    } else if (!cmd) { fail('本地（stdio）类型必须填写启动命令'); return; }
    var env = {};
    envRaw.split(/\s+/).forEach(function (kv) {
      var i = kv.indexOf('=');
      if (i > 0) env[kv.slice(0, i)] = kv.slice(i + 1);
    });
    setNote(note, null, '添加中…');
    api('/api/plugins', {
      method: 'POST',
      body: {
        name: name, display_name: displayName, type: typeSel.value,
        command: cmd, endpoint: endpoint,
        args: args ? args.split(/\s+/) : [],
        description: desc || (remote ? '远程 MCP 服务' : 'MCP 服务'),
        env: env
      }
    }).then(function (res) {
      if (res.data.ok) {
        ['mcp-f-name', 'mcp-f-display', 'mcp-f-cmd', 'mcp-f-endpoint', 'mcp-f-args', 'mcp-f-env', 'mcp-f-desc']
          .forEach(function (id) { $('#' + id).value = ''; });
        setNote(note, true, res.data.warning || '已添加并热加载');
        setMcpPane('mcp-pane-list');
      } else fail(res.data.error || '添加失败');
    });
  });
})();

/* ════════ 归档 ════════ */

function fmtArchDays(at) {
  if (!at) return '';
  var days = Math.floor((Date.now() / 1000 - at) / 86400);
  return days <= 0 ? '今天归档' : '已归档 ' + days + ' 天（满 10 天自动删除）';
}
function wsDisplayName(ws, name) {
  if (name) return name;
  if (!ws) return '新项目';
  var parts = String(ws).split(/[\\/]/).filter(Boolean);
  return parts[parts.length - 1] || ws;
}
function loadArchived() {
  var box = $('#archived-items');
  box.innerHTML = '';
  api('/api/sessions?archived=1').then(function (res) {
    var items = (res.data || {}).items || [];
    if (!items.length) { box.appendChild(el('div', 'arch-empty', '没有已归档的会话。')); return; }
    var groups = new Map();
    items.forEach(function (s) {
      var k = s.workspace || '';
      if (!groups.has(k)) groups.set(k, []);
      groups.get(k).push(s);
    });
    groups.forEach(function (list, ws) {
      var group = el('div', 'arch-group');
      var head = el('div', 'arch-group-head');
      var nameEl = el('span', 'arch-group-name', wsDisplayName(ws, list[0] && list[0].workspace_name));
      nameEl.title = ws || '新项目';
      head.appendChild(nameEl);
      head.appendChild(el('span', 'arch-group-count', list.length + ' 条会话'));
      head.appendChild(button('恢复整个项目', 'btn small', function () {
        api('/api/workspaces', { method: 'PATCH', body: { workspace: ws, archive: false } }).then(function (r) {
          if (!r.ok) { showBanner('恢复项目失败，请重试', 'error'); return; }
          showBanner('已把 ' + list.length + ' 条会话恢复到侧栏');
          loadArchived();
        });
      }));
      group.appendChild(head);
      list.forEach(function (s) {
        var row = el('div', 'arch-item');
        var main = el('div', 'arch-main');
        main.appendChild(el('div', 'arch-title', s.title || '未命名会话'));
        main.appendChild(el('div', 'arch-meta', (s.message_count || 0) + ' 条消息'));
        row.appendChild(main);
        row.appendChild(el('span', 'arch-days', fmtArchDays(s.archived_at)));
        var acts = el('div', 'mcp-actions');
        acts.appendChild(button('恢复', 'btn small', function () {
          api('/api/sessions', { method: 'PATCH', body: { id: s.id, archived: false } })
            .then(function () { loadArchived(); });
        }));
        var del = button('永久删除', 'btn small', function () {
          var confirmBtn = button('确认删除', 'btn small confirming', function () {
            confirmBtn.disabled = true;
            api('/api/sessions?id=' + encodeURIComponent(s.id), { method: 'DELETE' })
              .then(function () { loadArchived(); });
          });
          var cancelBtn = button('取消', 'btn small', function () {
            acts.replaceChild(del, confirmBtn);
            acts.removeChild(cancelBtn);
          });
          acts.replaceChild(confirmBtn, del);
          acts.appendChild(cancelBtn);
        });
        acts.appendChild(del);
        row.appendChild(acts);
        group.appendChild(row);
      });
      box.appendChild(group);
    });
  });
}
pageLoaders.archived = loadArchived;

/* ══════════════════════════════════════════════════════════════════
   模型管理（重构）
   ──────────────────────────────────────────────────────────────────
   旧版：三个子 Tab（模型列表 / 供应商管理 / 添加模型）各自成页，
   同一份数据在三处重复渲染，编辑就地展开、行内按钮多，越用越乱。
   新版：单页三栏 ——
     栏 1 供应商（含「全部模型」与「未归属」伪分组）
     栏 2 所选分组的模型列表（行只留「设为当前」，编辑在右栏）
     栏 3 选中对象详情：模型表单 / 供应商连接信息 / 新增表单
   ══════════════════════════════════════════════════════════════════ */

var ALL = '__all__';       // 「全部模型」伪分组
var NEW_PROV = '__new__';  // 新建供应商哨兵（不能用空串：空串 = 未归属分组）

var modelsCache = [];
var providersCache = [];
var activeModelId = '';
var provSel = ALL;          // 当前选中的分组：ALL | ''（未归属）| provider id
var provNameFilter = '';
var modelNameFilter = '';
var modelSel = '';          // 详情栏当前打开的模型 id
var detailMode = '';        // '' | 'model' | 'newmodel' | 'provider'

var CTX_PRESETS_IN = [1048576, 524288, 262144, 131072]; // 1M / 512K / 256K / 128K
var CTX_PRESETS_OUT = [393216, 262144, 131072];         // 384K / 256K / 128K

function loadLibrary() {
  return api('/api/models/list').then(function (res) {
    var d = res.data || {};
    modelsCache = d.models || [];
    providersCache = d.providers || [];
    activeModelId = d.active || '';
    return d;
  });
}
function findProvider(id) {
  for (var i = 0; i < providersCache.length; i++) {
    if (providersCache[i].id === id) return providersCache[i];
  }
  return null;
}
function modelsOfProvider(pid) {
  return modelsCache.filter(function (m) { return (m.provider_id || '') === pid; });
}
function providerKeyLabel(p) {
  if (!p) return '未设置';
  if (p.key_source === 'env') return p.key_name ? ('环境变量 ' + p.key_name) : '未设置环境变量';
  if (p.key_plain) return p.key_plain;
  return p.key_set ? '已保存（留空保持不变）' : '未设置';
}
function m3Show(pane) { // 'prov' | 'models' | 'detail' —— 仅窄屏生效
  var m3 = $('#m3');
  m3.classList.toggle('show-prov', pane === 'prov');
  m3.classList.toggle('show-models', pane === 'models');
  m3.classList.toggle('show-detail', pane === 'detail');
}

/* ---------- 上下文档位选择器（与原设置页同款） ---------- */
function buildCtxPicker(current, presets, fallback) {
  var wrap = el('span', 'ctx-pick');
  var num = input('number');
  num.className = 'ctx-num';
  var chips = el('span', 'ctx-chips');
  var btns = [];
  function sync() {
    var v = Number(num.value);
    btns.forEach(function (b) { b.classList.toggle('on', Number(b.dataset.v) === v); });
  }
  presets.forEach(function (v) {
    var b = el('button', 'ctx-chip', fmtCtxSlot(v));
    b.type = 'button';
    b.dataset.v = String(v);
    b.title = fmtCtxSlot(v) + ' = ' + v + ' tokens';
    b.addEventListener('click', function () { num.value = String(v); sync(); });
    btns.push(b);
    chips.appendChild(b);
  });
  num.addEventListener('input', sync);
  num.value = String(Number(current) || fallback);
  sync();
  wrap.appendChild(num);
  wrap.appendChild(chips);
  return {
    node: wrap,
    value: function () { return Number(num.value) || fallback; },
    set: function (v) { num.value = String(Number(v) || fallback); sync(); }
  };
}

/* ---------- 多模态三态分段 ---------- */
function buildCapSeg(which, value) {
  var wrap = el('div', 'key-seg cap');
  var state = value === true ? 'yes' : value === false ? 'no' : 'unset';
  [['unset', '不声明'], ['yes', '支持'], ['no', '不支持']].forEach(function (pair) {
    var b = el('button', null, pair[1]);
    b.type = 'button';
    b.dataset.cap = pair[0];
    if (pair[0] === state) b.classList.add('active');
    b.addEventListener('click', function () {
      state = pair[0];
      $$('button', wrap).forEach(function (x) { x.classList.toggle('active', x.dataset.cap === state); });
    });
    wrap.appendChild(b);
  });
  return {
    node: wrap,
    get: function () {
      if (state === 'yes') return true;
      if (state === 'no') return false;
      return null; // 不声明
    }
  };
}

/* ---------- 密钥控件（供应商表单） ---------- */
function buildKeyControl(p) {
  var wrap = el('div', 'key-row');
  var seg = el('div', 'key-seg');
  var envBtn = el('button', 'active', '环境变量');
  var plainBtn = el('button', null, '明文密钥');
  envBtn.type = 'button'; plainBtn.type = 'button';
  seg.appendChild(envBtn); seg.appendChild(plainBtn);
  var envIn = input('text', p ? p.key_name : '', '变量名，如 CODEFORGE_API_KEY');
  var eyeWrap = el('span', 'key-eye-wrap');
  var plainIn = input('password', (p && p.key_plain) ? p.key_plain : '',
    (p && p.key_set && !p.key_plain) ? '已保存（留空保持不变）' : 'sk-... 或 ${VAR}');
  var eye = el('button', 'key-eye', '👁');
  eye.type = 'button';
  eyeWrap.appendChild(plainIn); eyeWrap.appendChild(eye);
  wrap.appendChild(seg); wrap.appendChild(envIn); wrap.appendChild(eyeWrap);

  var src = (p && p.key_source === 'plain') ? 'plain' : 'env';
  var touched = false;
  function apply() {
    envBtn.classList.toggle('active', src === 'env');
    plainBtn.classList.toggle('active', src === 'plain');
    envIn.style.display = src === 'env' ? '' : 'none';
    plainIn.style.display = src === 'plain' ? '' : 'none';
    eye.style.display = (src === 'plain' && plainIn.value) ? '' : 'none';
  }
  envBtn.addEventListener('click', function () { if (src !== 'env') touched = true; src = 'env'; apply(); });
  plainBtn.addEventListener('click', function () { if (src !== 'plain') touched = true; src = 'plain'; apply(); });
  envIn.addEventListener('input', function () { touched = true; });
  plainIn.addEventListener('input', function () { touched = true; apply(); });
  eye.addEventListener('click', function () {
    var show = plainIn.type === 'password';
    plainIn.type = show ? 'text' : 'password';
    eye.textContent = show ? '🙈' : '👁';
  });
  apply();
  return {
    node: wrap,
    source: function () { return src; },
    name: function () { return envIn.value.trim(); },
    value: function () { return plainIn.value; },
    touched: function () { return touched; }
  };
}

/* ---------- 栏 1：供应商列表 ---------- */
function renderProvSide() {
  var box = $('#m-prov-list');
  box.innerHTML = '';
  var q = provNameFilter.trim().toLowerCase();

  function provItem(id, label, count, opts) {
    opts = opts || {};
    if (q && label.toLowerCase().indexOf(q) < 0 && (opts.sub || '').toLowerCase().indexOf(q) < 0) return;
    var b = el('button', 'prov-item' + (provSel === id ? ' active' : ''));
    b.type = 'button';
    b.appendChild(el('span', 'pv-dot' + (opts.disabled ? ' off' : '')));
    var nm = el('span', 'pv-name', label);
    if (opts.sub) nm.title = opts.sub;
    b.appendChild(nm);
    b.appendChild(el('span', 'pv-cnt', String(count)));
    if (opts.gear) {
      var g = el('button', 'pv-gear', '⚙');
      g.type = 'button';
      g.title = '编辑供应商连接信息';
      g.addEventListener('click', function (e) {
        e.stopPropagation();
        openProviderDetail(id);
      });
      b.appendChild(g);
    }
    b.addEventListener('click', function () {
      provSel = id;
      modelSel = '';
      renderProvSide();
      renderModelList();
      renderDetailEmpty();
      m3Show('models');
    });
    box.appendChild(b);
  }

  provItem(ALL, '全部模型', modelsCache.length);
  var unattached = modelsOfProvider('');
  if (unattached.length) provItem('', '未归属供应商', unattached.length, { sub: '各条目自带连接信息', disabled: true });
  providersCache.forEach(function (p) {
    provItem(p.id, p.name || p.id, modelsOfProvider(p.id).length, {
      disabled: p.disabled,
      sub: p.base_url,
      gear: true
    });
  });

  if (!box.children.length) {
    box.appendChild(el('div', 'list-empty', q ? '没有匹配的供应商' : '还没有供应商，点下方新建'));
  }
}

/* ---------- 栏 2：模型列表 ---------- */
function currentGroupModels() {
  if (provSel === ALL) return modelsCache.slice();
  return modelsCache.filter(function (m) { return (m.provider_id || '') === provSel; });
}

function renderModelList() {
  var box = $('#m-model-list');
  box.innerHTML = '';

  // 头部：当前分组标题 + 计数
  var title = '全部模型', sub = '';
  if (provSel === ALL) {
    var provCount = providersCache.length;
    sub = modelsCache.length + ' 个模型 · 来自 ' + provCount + ' 个供应商与 ' +
      modelsOfProvider('').length + ' 条未归属条目';
  } else if (provSel === '') {
    title = '未归属供应商';
    sub = '连接信息写在各条目上';
  } else {
    var p = findProvider(provSel);
    title = p ? (p.name || p.id) : '供应商';
    if (p) sub = p.base_url + (p.disabled ? ' · 已停用' : '');
  }
  $('#m-main-title').textContent = title;
  $('#m-main-sub').textContent = sub;
  var discBtn = $('#m-disc');
  discBtn.disabled = !(provSel && provSel !== ALL && findProvider(provSel));
  discBtn.title = discBtn.disabled ? '先选中一个供应商，再获取其上游模型列表' : '向该供应商上游请求模型列表';

  var list = currentGroupModels();
  var q = modelNameFilter.trim().toLowerCase();
  if (q) {
    list = list.filter(function (m) {
      return (m.id || '').toLowerCase().indexOf(q) >= 0 ||
        (m.name || '').toLowerCase().indexOf(q) >= 0;
    });
  }
  // 当前生效模型排最前
  list.sort(function (a, b) {
    var aa = a.id === activeModelId ? 0 : 1;
    var bb = b.id === activeModelId ? 0 : 1;
    return aa - bb;
  });

  lastListIds = list.map(function (m) { return m.id; });

  if (!list.length) {
    box.appendChild(el('div', 'list-empty', q ? '没有匹配的模型' :
      (modelsCache.length ? '该分组下没有模型' : '还没有模型：点右上角「＋ 添加模型」新建')));
    batchSyncBar();
    return;
  }

  list.forEach(function (m) {
    var row = el('div', 'mrow' +
      (batchMode ? ' batch' : (m.id === modelSel ? ' active' : '')) +
      (batchMode && batchSel[m.id] ? ' sel' : ''));
    if (batchMode) {
      var cb = input('checkbox');
      cb.className = 'row-check';
      cb.checked = !!batchSel[m.id];
      cb.addEventListener('click', function (e) { e.stopPropagation(); });
      cb.addEventListener('change', function () {
        if (cb.checked) batchSel[m.id] = true; else delete batchSel[m.id];
        row.classList.toggle('sel', cb.checked);
        batchSyncBar();
      });
      row.appendChild(cb);
    }
    var main = el('div', 'mrow-main');
    var name = el('div', 'mrow-name', m.name || m.id);
    if (m.id === activeModelId) name.appendChild(el('span', 'badge accent', '当前'));
    if (m.vision === true) name.appendChild(el('span', 'badge ok', '图'));
    if (m.video === true) name.appendChild(el('span', 'badge ok', '视频'));
    main.appendChild(name);

    var meta = m.id + ' · 上下文 ' + fmtCtx(m.ctx_in) + ' / ' + fmtCtx(m.ctx_out);
    if (m.rpm > 0) meta += ' · 限 ' + m.rpm + ' RPM';
    if (provSel === ALL) meta += ' · ' + ((m.provider_id && findProvider(m.provider_id))
      ? (findProvider(m.provider_id).name || m.provider_id) : '未归属');
    main.appendChild(el('div', 'mrow-id', meta));
    row.appendChild(main);

    var side = el('div', 'mrow-side');
    side.appendChild(el('span', 'badge ' + (m.key_set ? 'ok' : 'err'), m.key_set ? '密钥✓' : '无密钥'));
    if (m.id !== activeModelId) {
      side.appendChild(button('设为当前', 'btn small', function (e) {
        e.stopPropagation();
        applyModel(m.id);
      }));
    }
    row.appendChild(side);

    row.addEventListener('click', function () {
      if (batchMode) {
        if (batchSel[m.id]) delete batchSel[m.id]; else batchSel[m.id] = true;
        row.classList.toggle('sel', !!batchSel[m.id]);
        var cb = $('input.row-check', row);
        if (cb) cb.checked = !!batchSel[m.id];
        batchSyncBar();
        return;
      }
      openModelDetail(m.id);
    });
    box.appendChild(row);
  });
  batchSyncBar();
}

/* ---------- 批量管理（中间栏） ---------- */
var batchMode = false;
var batchSel = {};
var batchPanelKind = '';
var lastListIds = [];

function batchSelectedIds() {
  return Object.keys(batchSel).filter(function (id) {
    return modelsCache.some(function (m) { return m.id === id; });
  });
}
function batchSyncBar() {
  var ids = batchSelectedIds();
  var cnt = $('#m-batch-count');
  if (cnt) cnt.textContent = '已选 ' + ids.length + ' 项';
  var all = $('#m-batch-all');
  if (all) all.checked = lastListIds.length > 0 && lastListIds.every(function (id) { return !!batchSel[id]; });
}
function setBatchMode(on) {
  batchMode = on;
  batchSel = {};
  batchPanelKind = '';
  var btn = $('#m-batch');
  btn.classList.toggle('primary', on);
  btn.textContent = on ? '退出批量' : '批量管理';
  $('#m-batch-bar').classList.toggle('hidden', !on);
  $('#m-batch-panel').classList.add('hidden');
  $('#m-batch-panel').innerHTML = '';
  renderModelList();
  batchSyncBar();
}

/* 批量 patch → 单条保存载荷。未指明的字段沿用条目现值：
   vision/video 只有 true/false 才下发（null/未改 = 不声明），
   与编辑表单的省略语义一致。 */
function batchBodyFor(m, patch) {
  var body = {
    name: m.name || m.id,
    id: m.id,
    model: m.id,
    ctx_in: patch.ctx_in !== undefined ? patch.ctx_in : (m.ctx_in || 0),
    ctx_out: patch.ctx_out !== undefined ? patch.ctx_out : (m.ctx_out || 0),
    rpm: patch.rpm !== undefined ? patch.rpm : Math.max(0, m.rpm || 0)
  };
  var vis = patch.vision !== undefined ? patch.vision : m.vision;
  var vid = patch.video !== undefined ? patch.video : m.video;
  if (vis === true || vis === false) body.vision = vis;
  if (vid === true || vid === false) body.video = vid;
  if (m.provider_id) {
    body.provider_id = m.provider_id;
    body.protocol = '';
    body.inherit_key = true;
  } else {
    body.provider_id = '';
    body.base_url = m.base_url || '';
    body.protocol = m.protocol || 'openai';
    body.key_source = m.key_source || 'env';
    body.key_name = m.key_name || '';
  }
  return body;
}

function batchApply(patch, noteEl) {
  var ids = batchSelectedIds();
  if (!ids.length) { setNote(noteEl, false, '先勾选要修改的模型'); return Promise.resolve(); }
  var done = 0, fail = 0;
  setNote(noteEl, null, '应用中 0/' + ids.length + '…');
  var chain = Promise.resolve();
  ids.forEach(function (id) {
    chain = chain.then(function () {
      var m = modelsCache.filter(function (x) { return x.id === id; })[0];
      if (!m) return null;
      return api('/api/models/save', { method: 'POST', body: batchBodyFor(m, patch) }).then(function (res) {
        done++;
        if (!res.data.ok) fail++;
        setNote(noteEl, null, '应用中 ' + done + '/' + ids.length + '…');
      });
    });
  });
  return chain.then(function () {
    var ok = ids.length - fail;
    setNote(noteEl, fail ? false : true, fail
      ? ('完成：成功 ' + ok + ' 个，失败 ' + fail + ' 个')
      : ('已更新 ' + ok + ' 个模型'));
    showBanner(fail ? ('批量保存：成功 ' + ok + ' 个，失败 ' + fail + ' 个')
      : ('已更新 ' + ok + ' 个模型'), fail ? 'error' : undefined);
    return refreshModelViews();
  });
}

function batchDelete(noteEl) {
  var ids = batchSelectedIds();
  if (!ids.length) { setNote(noteEl, false, '先勾选要删除的模型'); return; }
  var done = 0, fail = 0;
  setNote(noteEl, null, '删除中 0/' + ids.length + '…');
  var chain = Promise.resolve();
  ids.forEach(function (id) {
    chain = chain.then(function () {
      return api('/api/models/delete', { method: 'POST', body: { model: id } }).then(function (res) {
        done++;
        if (!res.data.ok) fail++;
        setNote(noteEl, null, '删除中 ' + done + '/' + ids.length + '…');
      });
    });
  });
  chain.then(function () {
    var ok = ids.length - fail;
    setNote(noteEl, fail ? false : true, fail
      ? ('完成：成功 ' + ok + ' 个，失败 ' + fail + ' 个')
      : ('已删除 ' + ok + ' 个模型'));
    showBanner(fail ? ('批量删除：成功 ' + ok + ' 个，失败 ' + fail + ' 个')
      : ('已删除 ' + ok + ' 个模型'), fail ? 'error' : undefined);
    ids.forEach(function (id) { delete batchSel[id]; });
    if (ids.indexOf(modelSel) >= 0) { detailMode = ''; renderDetailEmpty(); }
    refreshModelViews();
  });
}

/* 四态能力分段（批量用：默认「不修改」） */
function buildCapSeg4() {
  var wrap = el('div', 'key-seg cap');
  var state = 'keep';
  [['keep', '不修改'], ['unset', '不声明'], ['yes', '支持'], ['no', '不支持']].forEach(function (pair) {
    var b = el('button', null, pair[1]);
    b.type = 'button';
    b.dataset.cap = pair[0];
    if (pair[0] === state) b.classList.add('active');
    b.addEventListener('click', function () {
      state = pair[0];
      $$('button', wrap).forEach(function (x) { x.classList.toggle('active', x.dataset.cap === state); });
    });
    wrap.appendChild(b);
  });
  return {
    node: wrap,
    get: function () {
      if (state === 'keep') return undefined;
      if (state === 'unset') return null;
      return state === 'yes';
    }
  };
}

function batchChips(presets, target) {
  var box = el('span', 'bp-chips');
  presets.forEach(function (v) {
    var b = el('button', 'ctx-chip', fmtCtxSlot(v));
    b.type = 'button';
    b.title = v + ' tokens';
    b.addEventListener('click', function () { target.value = String(v); });
    box.appendChild(b);
  });
  return box;
}

function toggleBatchPanel(kind) {
  var panel = $('#m-batch-panel');
  if (batchPanelKind === kind) {
    batchPanelKind = '';
    panel.classList.add('hidden');
    panel.innerHTML = '';
    return;
  }
  batchPanelKind = kind;
  panel.classList.remove('hidden');
  panel.innerHTML = '';
  var note = el('span', 'note');

  if (kind === 'ctx') {
    var inIn = input('number', '', '留空 = 不改');
    var outIn = input('number', '', '留空 = 不改');
    var r1 = el('div', 'bp-row');
    r1.appendChild(el('span', 'bp-label', '输入上下文'));
    r1.appendChild(inIn);
    r1.appendChild(batchChips(CTX_PRESETS_IN, inIn));
    var r2 = el('div', 'bp-row');
    r2.appendChild(el('span', 'bp-label', '输出上下文'));
    r2.appendChild(outIn);
    r2.appendChild(batchChips(CTX_PRESETS_OUT, outIn));
    var acts = el('div', 'bp-actions');
    acts.appendChild(button('应用到选中', 'btn primary small', function () {
      var patch = {};
      if (inIn.value.trim() !== '') patch.ctx_in = Math.max(1, parseInt(inIn.value, 10) || 0);
      if (outIn.value.trim() !== '') patch.ctx_out = Math.max(1, parseInt(outIn.value, 10) || 0);
      if (patch.ctx_in === undefined && patch.ctx_out === undefined) {
        setNote(note, false, '两项都为空 = 没有要改的内容');
        return;
      }
      batchApply(patch, note);
    }));
    acts.appendChild(note);
    panel.appendChild(r1);
    panel.appendChild(r2);
    panel.appendChild(acts);
    return;
  }

  if (kind === 'rpm') {
    var rpmIn = input('number', '', '留空 = 不改；0 = 取消限制');
    var rr = el('div', 'bp-row');
    rr.appendChild(el('span', 'bp-label', '每分钟上限'));
    rr.appendChild(rpmIn);
    var acts2 = el('div', 'bp-actions');
    acts2.appendChild(button('应用到选中', 'btn primary small', function () {
      if (rpmIn.value.trim() === '') { setNote(note, false, '填一个数值再应用（0 = 取消限制）'); return; }
      batchApply({ rpm: Math.max(0, parseInt(rpmIn.value, 10) || 0) }, note);
    }));
    acts2.appendChild(note);
    panel.appendChild(rr);
    panel.appendChild(acts2);
    return;
  }

  if (kind === 'del') {
    var ids = batchSelectedIds();
    var hasActive = ids.indexOf(activeModelId) >= 0;
    var tip = el('div', 'bp-row');
    tip.textContent = ids.length
      ? ('将删除选中的 ' + ids.length + ' 个模型，不可撤销。' +
        (hasActive ? '其中包含当前生效模型：只从列表移除，当前对话不受影响。' : ''))
      : '还没有勾选任何模型。';
    var actsD = el('div', 'bp-actions');
    var delConfirm = button('确认删除 ' + ids.length + ' 个', 'btn danger-ghost small', function () {
      delConfirm.disabled = true;
      batchDelete(note);
    });
    delConfirm.disabled = !ids.length;
    actsD.appendChild(delConfirm);
    actsD.appendChild(note);
    panel.appendChild(tip);
    panel.appendChild(actsD);
    return;
  }

  if (kind === 'cap') {
    var vis = buildCapSeg4();
    var vid = buildCapSeg4();
    var rv = el('div', 'bp-row');
    rv.appendChild(el('span', 'bp-label', '图片能力'));
    rv.appendChild(vis.node);
    var rvd = el('div', 'bp-row');
    rvd.appendChild(el('span', 'bp-label', '视频能力'));
    rvd.appendChild(vid.node);
    var acts3 = el('div', 'bp-actions');
    acts3.appendChild(button('应用到选中', 'btn primary small', function () {
      var patch = {};
      var v = vis.get();
      var d = vid.get();
      if (v !== undefined) patch.vision = v;
      if (d !== undefined) patch.video = d;
      if (patch.vision === undefined && patch.video === undefined) {
        setNote(note, false, '两项都是「不修改」= 没有要改的内容');
        return;
      }
      batchApply(patch, note);
    }));
    acts3.appendChild(note);
    panel.appendChild(rv);
    panel.appendChild(rvd);
    panel.appendChild(acts3);
    return;
  }
}

/* ---------- 应用的统一出口：模型库变更后重拉 + 重绘 ---------- */
function refreshModelViews() {
  return loadLibrary().then(function () {
    // 选中的分组可能已不存在（供应商被删）→ 回到「全部」
    if (provSel !== ALL && provSel !== '' && !findProvider(provSel)) provSel = ALL;
    renderProvSide();
    renderModelList();
    ensureDetailStillValid();
  });
}
function ensureDetailStillValid() {
  if (detailMode === 'model' && modelSel) {
    var hit = modelsCache.filter(function (m) { return m.id === modelSel; })[0];
    if (!hit) { renderDetailEmpty(); return; }
    if ($('#m-detail .d-form')) return; // 正在编辑时不重绘，避免打断输入
    openModelDetail(modelSel);
  }
}
pageLoaders.models = function () {
  loadLibrary().then(function () {
    renderProvSide();
    renderModelList();
    if (detailMode) {
      // 刷新时留在原来的详情上；正在编辑的表单不重绘，避免打断输入
      ensureDetailStillValid();
    } else {
      renderDetailEmpty();
      m3Show('prov');
    }
  });
};

function applyModel(id) {
  api('/api/models/apply', { method: 'POST', body: { model: id } }).then(function (res) {
    if (!res.data.ok) { showBanner('应用失败：' + (res.data.error || '未知错误'), 'error'); return; }
    showBanner('已切换为「' + id + '」');
    refreshModelViews();
  });
}

/* ---------- 栏 3：模型详情 / 编辑器 ---------- */
function renderDetailEmpty() {
  detailMode = '';
  modelSel = '';
  var box = $('#m-detail');
  box.innerHTML = '';
  var d = el('div', 'm3-empty');
  d.innerHTML = '从中间列表选择一个模型查看详情，或点「＋ 添加模型」新建。<br><br>选中供应商后点 ⚙ 可编辑其连接信息（Base URL / 密钥）。';
  box.appendChild(d);
}

function detailHead(box, title, sub, kind) {
  var head = el('div', 'd-head');
  head.appendChild(button('‹', 'btn icon d-back', function () { m3Show('models'); }));
  head.appendChild(el('div', 'd-head-title', title));
  box.appendChild(head);
  if (sub) box.appendChild(el('div', 'd-head-sub', sub));
  return head;
}

function openModelDetail(id) {
  var m = modelsCache.filter(function (x) { return x.id === id; })[0];
  if (!m) return;
  detailMode = 'model';
  modelSel = id;
  $$('.mrow').forEach(function () { /* 选中高亮由 renderModelList 负责 */ });
  renderModelList();
  m3Show('detail');

  var box = $('#m-detail');
  box.innerHTML = '';
  var prov = m.provider_id ? findProvider(m.provider_id) : null;
  detailHead(box, m.name || m.id, m.id + (prov ? ' · 供应商：' + (prov.name || prov.id) : ' · 未归属供应商'), 'model');

  var form = el('div', 'd-form');
  var nameIn = input('text', m.name || '', '界面显示的名称');
  var idIn = input('text', m.id || '', '如 gpt-4o');
  form.appendChild(field('显示名称', nameIn));
  form.appendChild(field('模型 id', idIn));

  var ctxIn = buildCtxPicker(m.ctx_in, CTX_PRESETS_IN, 262144);
  var ctxOut = buildCtxPicker(m.ctx_out, CTX_PRESETS_OUT, 131072);
  var pair = el('div', 'ctx-pair');
  pair.appendChild(field('上下文 · 输入', ctxIn.node));
  pair.appendChild(field('上下文 · 输出', ctxOut.node));
  form.appendChild(pair);

  var vision = buildCapSeg('vision', m.vision);
  var video = buildCapSeg('video', m.video);
  form.appendChild(field('图片能力', vision.node));
  form.appendChild(field('视频能力（原生透传）', video.node));

  var rpmIn = input('number', m.rpm > 0 ? String(m.rpm) : '', '留空或 0 = 不限制');
  form.appendChild(field('每分钟请求上限（RPM）', rpmIn));

  // 密钥可改：挂供应商时改的是供应商共享密钥（作用于其下所有模型），
  // 未归属时改的是本条目自己的密钥。
  var keyCtl = buildKeyControl(prov || m);
  form.appendChild(field(
    prov ? ('API Key / 环境变量（供应商「' + (prov.name || prov.id) + '」共享，修改影响其下所有模型）')
         : 'API Key / 环境变量（写在本模型条目上）',
    keyCtl.node, true));

  var box2 = $('#m-detail');
  box2.appendChild(form);

  var inherit = el('div', 'me-inherit',
    prov ? ('连接信息继承自「' + (prov.name || prov.id) + '」\n' + prov.base_url)
      : ('连接信息写在本条目上\n' + (m.base_url || '—')));
  inherit.style.whiteSpace = 'pre-wrap';
  box2.appendChild(inherit);

  var acts = el('div', 'd-actions');
  var note = el('span', 'note');
  acts.appendChild(note);
  var testBtn = button('测试连接', 'btn ghost', function () {
    setNote(note, null, '测试中…（通过后逐档探测上下文，可能要几十秒）');
    var body = collectModelForm(m, nameIn, idIn, ctxIn, ctxOut, vision, video, rpmIn, keyCtl);
    // 挂供应商时也带上密钥三件套：测试用「所见即所得」的当前值，
    // 改过但还没保存的密钥可以直接先测（applyProviderDefaults 在给了密钥时不覆盖）。
    if (prov) {
      body.key_source = keyCtl.source();
      body.key_name = keyCtl.name();
      body.key_value = keyCtl.value();
    }
    body.probe_context = true;
    api('/api/models/test', { method: 'POST', body: body }).then(function (res) {
      var d = res.data || {};
      if (!d.ok) { setNote(note, false, '失败：' + (d.error || '未知错误')); return; }
      var probe = d.probe || {};
      var got = Number(probe.ctx_in) || 0;
      var extra = got > 0 ? ('；探测到上下文 ≈ ' + fmtCtx(got)) : ('；上下文未探到：' + (probe.note || '上游未给出长度信号'));
      setNote(note, true, '连接成功' + extra);
      if (got > 0) ctxIn.set(got);
    });
  });
  var delBtn = button('删除模型', 'btn danger-ghost', function () {
    var confirmBtn = button('确认删除', 'btn confirming', function () {
      confirmBtn.disabled = true;
      api('/api/models/delete', { method: 'POST', body: { model: m.id } }).then(function (res) {
        if (!res.data.ok) {
          confirmBtn.disabled = false;
          setNote(note, false, '删除失败：' + (res.data.error || '未知错误'));
          return;
        }
        showBanner('已删除模型「' + m.id + '」');
        detailMode = '';
        renderDetailEmpty();
        refreshModelViews();
      });
    });
    var cancelBtn = button('取消', 'btn ghost', function () {
      acts.replaceChild(delBtn, confirmBtn);
      acts.removeChild(cancelBtn);
    });
    acts.replaceChild(confirmBtn, delBtn);
    acts.appendChild(cancelBtn);
  });
  var saveBtn = button('保存修改', 'btn primary', function () {
    var body = collectModelForm(m, nameIn, idIn, ctxIn, ctxOut, vision, video, rpmIn, keyCtl);
    if (!body.id) { setNote(note, false, '模型 id 不能为空'); return; }
    saveBtn.disabled = true;
    setNote(note, null, '保存中…');
    var doSave = function () {
      api('/api/models/save', { method: 'POST', body: body }).then(function (res) {
        if (!res.data.ok) { setNote(note, false, res.data.error || '保存失败'); saveBtn.disabled = false; return; }
        setNote(note, true, '已保存');
        refreshModelViews();
        saveBtn.disabled = false;
      });
    };
    // 供应商共享密钥被改过 → 先落供应商（带 key_touched 明确提交），
    // 再存模型本身（模型仍走 inherit_key，不在条目上留覆盖）。
    if (prov && keyCtl.touched()) {
      api('/api/providers/save', { method: 'POST', body: {
        id: prov.id,
        name: prov.name || prov.id,
        base_url: prov.base_url || '',
        protocol: prov.protocol || 'openai',
        key_source: keyCtl.source(),
        key_name: keyCtl.name(),
        key_value: keyCtl.value(),
        key_touched: true,
        disabled: !!prov.disabled
      } }).then(function (res) {
        if (!res.data.ok) {
          setNote(note, false, '供应商密钥保存失败：' + (res.data.error || '未知错误'));
          saveBtn.disabled = false;
          return;
        }
        doSave();
      });
      return;
    }
    doSave();
  });
  acts.appendChild(testBtn);
  acts.appendChild(delBtn);
  acts.appendChild(saveBtn);
  box2.appendChild(acts);
}

/* 从详情表单收集模型字段（编辑与新增共用）。
   沿用原设置页的语义：
   - 归属供应商 → 连接三件套不提交，inherit_key 清掉条目上的历史遗留密钥
     （此时密钥改的是供应商本身，由调用方先走 /api/providers/save）；
   - 未归属旧条目 → 原样带回 base_url / protocol / key_source / key_name；
     密钥框动过才提交 key_value，没动则服务端按「保持已存」处理。 */
function collectModelForm(m, nameIn, idIn, ctxIn, ctxOut, vision, video, rpmIn, keyCtl) {
  var id = idIn.value.trim();
  var body = {
    name: nameIn.value.trim(),
    id: id,
    model: id,
    ctx_in: ctxIn.value(),
    ctx_out: ctxOut.value(),
    rpm: Math.max(0, parseInt(rpmIn.value, 10) || 0)
  };
  var v = vision.get();
  var vd = video.get();
  if (v !== null) body.vision = v;
  if (vd !== null) body.video = vd;
  if (m && m.provider_id) {
    body.provider_id = m.provider_id;
    body.protocol = '';
    body.inherit_key = true;
  } else if (m) {
    body.provider_id = '';
    body.base_url = m.base_url || '';
    body.protocol = m.protocol || 'openai';
    body.key_source = keyCtl ? keyCtl.source() : (m.key_source || 'env');
    body.key_name = keyCtl ? keyCtl.name() : (m.key_name || '');
    if (keyCtl && keyCtl.touched()) body.key_value = keyCtl.value();
  }
  return body;
}

/* ---------- 新增模型 ---------- */
function openNewModel(presetProviderId) {
  detailMode = 'newmodel';
  modelSel = '';
  renderModelList();
  m3Show('detail');

  var box = $('#m-detail');
  box.innerHTML = '';
  detailHead(box, '添加模型', '选定供应商后只需填模型 id —— 地址与密钥继承自供应商', 'newmodel');

  var form = el('div', 'd-form');
  var provSelEl = el('select');
  var optNew = el('option', null, '＋ 新建供应商（在本表单填地址与密钥）');
  optNew.value = '';
  provSelEl.appendChild(optNew);
  providersCache.forEach(function (p) {
    var o = el('option', null, (p.name || p.id) + (p.disabled ? '（已停用）' : ''));
    o.value = p.id;
    provSelEl.appendChild(o);
  });
  if (presetProviderId && findProvider(presetProviderId)) provSelEl.value = presetProviderId;
  form.appendChild(field('归属供应商', provSelEl));

  var inherit = el('div', 'me-inherit hidden');
  form.appendChild(inherit);

  var nameIn = input('text', '', '界面显示的名称');
  form.appendChild(field('显示名称', nameIn));

  var idRow = el('div', 'key-row');
  var idIn = input('text', '', '如 gpt-4o / claude-sonnet-4');
  idRow.appendChild(idIn);
  idRow.appendChild(button('获取模型列表', 'btn ghost', function () { discoverForNew(); }));
  form.appendChild(field('模型 id', idRow, true));

  var urlIn = input('text', '', 'https://api.openai.com/v1');
  var urlField = field('请求地址', urlIn, true);
  form.appendChild(urlField);
  var protoSel = el('select');
  ['openai', 'anthropic'].forEach(function (v) {
    var o = el('option', null, v === 'anthropic' ? 'Anthropic' : 'OpenAI');
    o.value = v;
    protoSel.appendChild(o);
  });
  var protoField = field('兼容协议', protoSel);
  form.appendChild(protoField);

  var keyCtl = buildKeyControl(null);
  var keyField = field('密钥', keyCtl.node, true);
  form.appendChild(keyField);

  var ctxIn = buildCtxPicker(262144, CTX_PRESETS_IN, 262144);
  var ctxOut = buildCtxPicker(131072, CTX_PRESETS_OUT, 131072);
  var pair = el('div', 'ctx-pair');
  pair.appendChild(field('上下文 · 输入', ctxIn.node));
  pair.appendChild(field('上下文 · 输出', ctxOut.node));
  form.appendChild(pair);

  var vision = buildCapSeg('vision', null);
  var video = buildCapSeg('video', null);
  form.appendChild(field('图片能力', vision.node));
  form.appendChild(field('视频能力（原生透传）', video.node));
  var rpmIn = input('number', '', '留空 = 不限制');
  form.appendChild(field('每分钟请求上限（RPM）', rpmIn));
  box.appendChild(form);

  // 上游模型勾选面板
  var discPanel = el('div', 'disc-panel hidden');
  var discHead = el('div', 'disc-head');
  discHead.appendChild(el('div', 'disc-title', '上游可用模型'));
  var discCount = el('span', 'disc-count');
  discHead.appendChild(discCount);
  var discTools = el('div', 'disc-tools');
  var discFilter = input('search', '', '筛选模型 id…');
  discTools.appendChild(discFilter);
  discTools.appendChild(button('收起', null, function () {
    discPanel.classList.add('hidden');
    discItems = []; discPick = {};
  }));
  discHead.appendChild(discTools);
  discPanel.appendChild(discHead);
  var discListEl = el('div', 'disc-list');
  discPanel.appendChild(discListEl);
  var discFoot = el('div', 'disc-foot');
  var discNote = el('span', 'note');
  var discAddBtn = button('添加选中', 'btn primary');
  discAddBtn.disabled = true;
  discFoot.appendChild(discNote);
  discFoot.appendChild(discAddBtn);
  discPanel.appendChild(discFoot);
  box.appendChild(discPanel);

  var discItems = [];
  var discPick = {};
  function discSync() {
    var n = Object.keys(discPick).length;
    discCount.textContent = discItems.length ? ('共 ' + discItems.length + ' 个' + (n ? '，已选 ' + n + ' 个' : '')) : '';
    discAddBtn.disabled = n === 0;
    discAddBtn.textContent = provSelEl.value ? ('批量添加选中（' + n + ' 个）') : '填入表单';
  }
  function discRender() {
    var q = discFilter.value.trim().toLowerCase();
    var shown = discItems.filter(function (m) {
      return !q || m.id.toLowerCase().indexOf(q) >= 0 || (m.name || '').toLowerCase().indexOf(q) >= 0;
    });
    discListEl.innerHTML = '';
    if (!shown.length) {
      discListEl.appendChild(el('div', 'disc-empty', discItems.length ? '没有匹配的模型' : '没有可添加的模型'));
      discSync();
      return;
    }
    shown.forEach(function (m) {
      var row = el('label', 'disc-item' + (m.in_library ? ' added' : ''));
      var cb = input('checkbox');
      cb.checked = !!discPick[m.id];
      cb.disabled = !!m.in_library;
      cb.addEventListener('change', function () {
        if (cb.checked) discPick[m.id] = true; else delete discPick[m.id];
        discSync();
      });
      row.appendChild(cb);
      row.appendChild(el('span', 'disc-id', m.id));
      if (m.name && m.name !== m.id) row.appendChild(el('span', 'disc-name', m.name));
      if (m.in_library) row.appendChild(el('span', 'disc-badge', '已在库'));
      discListEl.appendChild(row);
    });
    discSync();
  }
  discFilter.addEventListener('input', discRender);
  discAddBtn.addEventListener('click', function () {
    var ids = Object.keys(discPick);
    if (!ids.length) return;
    var pick = function (id) {
      return discItems.filter(function (m) { return m.id === id; })[0] || {};
    };
    var pid = provSelEl.value;
    if (!pid) {
      var id = ids[0], hit = pick(id);
      idIn.value = id;
      if (!nameIn.value.trim() && hit.name && hit.name !== id) nameIn.value = hit.name;
      setNote(note, true, '已填入模型 id：' + id + '，补好地址与密钥后点「保存」');
      discPanel.classList.add('hidden');
      return;
    }
    setNote(note, null, '正在添加 ' + ids.length + ' 个模型…');
    api('/api/models/save_batch', {
      method: 'POST',
      body: {
        provider_id: pid,
        ctx_in: ctxIn.value(),
        ctx_out: ctxOut.value(),
        models: ids.map(function (id) {
          var hit = pick(id);
          return { id: id, name: (hit.name && hit.name !== id) ? hit.name : '' };
        })
      }
    }).then(function (res) {
      if (!res.data.ok) { setNote(note, false, res.data.error || '批量添加失败'); return; }
      setNote(note, true, '已添加 ' + res.data.added + ' 个模型' +
        (res.data.skipped ? '，跳过已存在 ' + res.data.skipped + ' 个' : ''));
      discPanel.classList.add('hidden');
      discItems = []; discPick = {};
      refreshModelViews();
    });
  });

  function discoverForNew() {
    var ids = collectModelForm(null, nameIn, idIn, ctxIn, ctxOut, vision, video, rpmIn);
    if (!ids.id) { setNote(note, false, '请先填写模型 id 再获取模型列表'); return; }
    var body = { provider_id: provSelEl.value };
    if (!body.provider_id) {
      body.protocol = protoSel.value;
      body.base_url = urlIn.value.trim();
      body.key_source = keyCtl.source();
      body.key_name = keyCtl.name();
      body.key_value = keyCtl.value();
      body.model = ids.id;
    }
    discPanel.classList.remove('hidden');
    setNote(discNote, null, '正在获取模型列表…');
    api('/api/models/discover', { method: 'POST', body: body }).then(function (res) {
      var d = res.data || {};
      if (!d.ok) {
        discItems = []; discPick = {};
        discListEl.innerHTML = '';
        discSync();
        setNote(discNote, false, '获取失败：' + (d.error || '未知错误'));
        return;
      }
      discItems = d.models || [];
      discPick = {};
      discFilter.value = '';
      discRender();
      setNote(discNote, true, '已获取 ' + discItems.length + ' 个模型' +
        (d.key_from_active ? '（密钥取自当前生效模型）' : ''));
    });
  }

  function syncProvider() {
    var pid = provSelEl.value;
    var p = pid ? findProvider(pid) : null;
    inherit.classList.toggle('hidden', !p);
    urlField.classList.toggle('hidden', !!p);
    protoField.classList.toggle('hidden', !!p);
    keyField.classList.toggle('hidden', !!p);
    if (!p) return;
    inherit.textContent = '继承自供应商「' + (p.name || p.id) + '」\n' +
      'Base URL：' + p.base_url + '\n' +
      'API Key：' + providerKeyLabel(p);
  }
  provSelEl.addEventListener('change', syncProvider);
  syncProvider();

  var acts = el('div', 'd-actions');
  var note = el('span', 'note');
  acts.appendChild(note);
  acts.appendChild(button('测试连接', 'btn ghost', function () {
    var body = collectModelForm(null, nameIn, idIn, ctxIn, ctxOut, vision, video, rpmIn);
    if (!body.id) { setNote(note, false, '模型 id 不能为空'); return; }
    if (provSelEl.value) {
      body.provider_id = provSelEl.value;
      delete body.base_url; delete body.key_source; delete body.key_name;
    } else {
      body.provider_id = '';
      body.protocol = protoSel.value;
      body.base_url = urlIn.value.trim();
      body.key_source = keyCtl.source();
      body.key_name = keyCtl.name();
      body.key_value = keyCtl.value();
    }
    body.probe_context = true;
    setNote(note, null, '测试中…（可能要几十秒）');
    api('/api/models/test', { method: 'POST', body: body }).then(function (res) {
      var d = res.data || {};
      if (!d.ok) { setNote(note, false, '失败：' + (d.error || '未知错误')); return; }
      var got = Number((d.probe || {}).ctx_in) || 0;
      setNote(note, true, '连接成功' + (got > 0 ? ('；探测到上下文 ≈ ' + fmtCtx(got)) : ''));
      if (got > 0) ctxIn.set(got);
    });
  }));
  acts.appendChild(button('重置', 'btn ghost', function () { openNewModel(provSelEl.value || presetProviderId); }));
  var saveBtn = button('保存', 'btn primary', function () {
    var body = collectModelForm(null, nameIn, idIn, ctxIn, ctxOut, vision, video, rpmIn);
    if (!body.id) { setNote(note, false, '模型 id 不能为空'); return; }
    if (provSelEl.value) {
      body.provider_id = provSelEl.value;
      body.protocol = '';
      body.inherit_key = true;
    } else {
      body.provider_id = '';
      body.base_url = urlIn.value.trim();
      body.protocol = protoSel.value;
      body.key_source = keyCtl.source();
      body.key_name = keyCtl.name();
      if (keyCtl.touched()) body.key_value = keyCtl.value();
    }
    saveBtn.disabled = true;
    setNote(note, null, '保存中…');
    api('/api/models/save', { method: 'POST', body: body }).then(function (res) {
      if (!res.data.ok) { setNote(note, false, res.data.error || '保存失败'); saveBtn.disabled = false; return; }
      showBanner('已保存模型「' + body.id + '」');
      detailMode = '';
      modelSel = body.id;
      renderDetailEmpty();
      modelSel = body.id;
      refreshModelViews();
      m3Show('models');
    });
  });
  acts.appendChild(saveBtn);
  box.appendChild(acts);
}

/* ---------- 供应商详情 / 编辑器 ---------- */
function openProviderDetail(id) {
  var isNew = !id;
  var p = isNew ? null : findProvider(id);
  if (!isNew && !p) return;
  detailMode = 'provider';
  modelSel = '';
  renderModelList();
  m3Show('detail');

  var box = $('#m-detail');
  box.innerHTML = '';
  detailHead(box, isNew ? '新建供应商' : (p.name || p.id),
    isNew ? '连接信息只维护这一份，其下模型只填 id' : (p.base_url + (p.disabled ? ' · 已停用' : '')), 'provider');

  var form = el('div', 'd-form');
  var nameIn = input('text', p ? p.name : '', '如 上海模型实验室 / SiliconFlow');
  var urlIn = input('text', p ? p.base_url : '', 'https://api.example.com/v1');
  var protoSel = el('select');
  ['openai', 'anthropic'].forEach(function (v) {
    var o = el('option', null, v === 'anthropic' ? 'Anthropic' : 'OpenAI');
    o.value = v;
    protoSel.appendChild(o);
  });
  protoSel.value = (p && p.protocol === 'anthropic') ? 'anthropic' : 'openai';
  var keyCtl = buildKeyControl(p);

  form.appendChild(field('名称', nameIn));
  form.appendChild(field('兼容协议', protoSel));
  form.appendChild(field('Base URL', urlIn, true));
  form.appendChild(field('密钥', keyCtl.node, true));
  var disWrap = el('label', 'f-field wide');
  var dis = input('checkbox');
  dis.checked = !!(p && p.disabled);
  var disLabel = el('span');
  disLabel.style.cssText = 'display:flex;align-items:center;gap:8px;font-weight:400';
  disLabel.appendChild(dis);
  disLabel.appendChild(el('span', null, '停用该供应商（其下模型不可选用，配置保留）'));
  disWrap.appendChild(disLabel);
  form.appendChild(disWrap);
  box.appendChild(form);

  var discPanel = null, discItems = [], discPick = {};
  if (!isNew) {
    discPanel = el('div', 'disc-panel hidden');
    var discHead = el('div', 'disc-head');
    discHead.appendChild(el('div', 'disc-title', '上游可用模型'));
    var discCount = el('span', 'disc-count');
    discHead.appendChild(discCount);
    var discTools = el('div', 'disc-tools');
    var discFilter = input('search', '', '筛选模型 id…');
    discTools.appendChild(discFilter);
    discTools.appendChild(button('收起', null, function () {
      discPanel.classList.add('hidden'); discItems = []; discPick = {};
    }));
    discHead.appendChild(discTools);
    discPanel.appendChild(discHead);
    var discListEl = el('div', 'disc-list');
    discPanel.appendChild(discListEl);
    var discFoot = el('div', 'disc-foot');
    var discNote = el('span', 'note');
    var discAddBtn = button('批量添加选中', 'btn primary');
    discAddBtn.disabled = true;
    discFoot.appendChild(discNote);
    discFoot.appendChild(discAddBtn);
    discPanel.appendChild(discFoot);
    box.appendChild(discPanel);

    var sync = function () {
      var n = Object.keys(discPick).length;
      discCount.textContent = discItems.length ? ('共 ' + discItems.length + ' 个' + (n ? '，已选 ' + n + ' 个' : '')) : '';
      discAddBtn.disabled = n === 0;
      discAddBtn.textContent = n ? ('批量添加选中（' + n + ' 个）') : '批量添加选中';
    };
    var renderDisc = function () {
      var q = discFilter.value.trim().toLowerCase();
      var shown = discItems.filter(function (m) {
        return !q || m.id.toLowerCase().indexOf(q) >= 0 || (m.name || '').toLowerCase().indexOf(q) >= 0;
      });
      discListEl.innerHTML = '';
      if (!shown.length) {
        discListEl.appendChild(el('div', 'disc-empty', discItems.length ? '没有匹配的模型' : '没有可添加的模型'));
        sync(); return;
      }
      shown.forEach(function (m) {
        var row = el('label', 'disc-item' + (m.in_library ? ' added' : ''));
        var cb = input('checkbox');
        cb.checked = !!discPick[m.id];
        cb.disabled = !!m.in_library;
        cb.addEventListener('change', function () {
          if (cb.checked) discPick[m.id] = true; else delete discPick[m.id];
          sync();
        });
        row.appendChild(cb);
        row.appendChild(el('span', 'disc-id', m.id));
        if (m.name && m.name !== m.id) row.appendChild(el('span', 'disc-name', m.name));
        if (m.in_library) row.appendChild(el('span', 'disc-badge', '已在库'));
        discListEl.appendChild(row);
      });
      sync();
    };
    discFilter.addEventListener('input', renderDisc);
    discAddBtn.addEventListener('click', function () {
      var ids = Object.keys(discPick);
      if (!ids.length) return;
      var pick = function (id) { return discItems.filter(function (m) { return m.id === id; })[0] || {}; };
      setNote(discNote, null, '正在添加 ' + ids.length + ' 个模型…');
      api('/api/models/save_batch', {
        method: 'POST',
        body: {
          provider_id: p.id,
          ctx_in: 262144,
          ctx_out: 131072,
          models: ids.map(function (id) {
            var hit = pick(id);
            return { id: id, name: (hit.name && hit.name !== id) ? hit.name : '' };
          })
        }
      }).then(function (res) {
        if (!res.data.ok) { setNote(discNote, false, res.data.error || '批量添加失败'); return; }
        discPanel.classList.add('hidden');
        discItems = []; discPick = {};
        showBanner('已添加 ' + res.data.added + ' 个模型（共用该供应商的地址与密钥）' +
          (res.data.skipped ? '，跳过已存在 ' + res.data.skipped + ' 个' : ''));
        refreshModelViews();
      });
    });
    // 「获取上游模型列表」按钮
    var fetchUpstream = function () {
      discPanel.classList.remove('hidden');
      setNote(discNote, null, '正在获取模型列表…');
      api('/api/models/discover', { method: 'POST', body: { provider_id: p.id } }).then(function (res) {
        var d = res.data || {};
        if (!d.ok) {
          discItems = []; discPick = {};
          discListEl.innerHTML = '';
          sync();
          setNote(discNote, false, '获取失败：' + (d.error || '未知错误'));
          return;
        }
        discItems = d.models || [];
        discPick = {};
        discFilter.value = '';
        renderDisc();
        setNote(discNote, true, '已获取 ' + discItems.length + ' 个模型' +
          (d.key_from_active ? '（密钥取自当前生效模型）' : ''));
      });
    };
    var acts2 = el('div', 'd-actions');
    acts2.appendChild(button('获取模型列表', 'btn ghost', fetchUpstream));
    acts2.appendChild(button('＋ 添加模型到此供应商', 'btn ghost', function () {
      openNewModel(p.id);
    }));
    box.appendChild(acts2);
  }

  var acts = el('div', 'd-actions');
  var note = el('span', 'note');
  acts.appendChild(note);
  acts.appendChild(button('测试连接', 'btn ghost', function () {
    if (isNew) { setNote(note, false, '先保存供应商再测试'); return; }
    setNote(note, null, '测试中…');
    api('/api/models/discover', { method: 'POST', body: { provider_id: p.id } }).then(function (res) {
      var d = res.data || {};
      if (d.ok) setNote(note, true, '连接正常，上游 ' + (d.count || 0) + ' 个模型可用');
      else setNote(note, false, '失败：' + (d.error || '未知错误'));
    });
  }));
  if (!isNew) {
    var delBtn = button('删除供应商', 'btn danger-ghost', function () {
      var n = modelsOfProvider(p.id).length;
      if (n && !confirm('其下 ' + n + ' 个模型不会被删除，连接信息会保留到各自条目上。\n\n确认删除供应商「' + (p.name || p.id) + '」？')) return;
      var confirmBtn = button('确认删除', 'btn confirming', function () {
        confirmBtn.disabled = true;
        api('/api/providers/delete', { method: 'POST', body: { provider: p.id } }).then(function (res) {
          if (!res.data.ok) {
            confirmBtn.disabled = false;
            setNote(note, false, res.data.error || '删除失败');
            return;
          }
          showBanner('已删除供应商「' + (p.name || p.id) + '」');
          provSel = ALL;
          detailMode = '';
          renderDetailEmpty();
          refreshModelViews();
          m3Show('prov');
        });
      });
      var cancelBtn = button('取消', 'btn ghost', function () {
        acts.replaceChild(delBtn, confirmBtn);
        acts.removeChild(cancelBtn);
      });
      acts.replaceChild(confirmBtn, delBtn);
      acts.appendChild(cancelBtn);
    });
    acts.appendChild(delBtn);
  }
  var saveBtn = button('保存供应商', 'btn primary', function () {
    var name = nameIn.value.trim();
    var url = urlIn.value.trim();
    if (!name) { setNote(note, false, '名称不能为空'); return; }
    if (!url) { setNote(note, false, 'Base URL 不能为空'); return; }
    var body = {
      id: isNew ? newProviderId(name) : p.id,
      name: name,
      base_url: url,
      protocol: protoSel.value,
      key_source: keyCtl.source(),
      key_name: keyCtl.name(),
      key_value: keyCtl.value(),
      key_touched: keyCtl.touched(),
      disabled: dis.checked
    };
    saveBtn.disabled = true;
    setNote(note, null, '保存中…');
    api('/api/providers/save', { method: 'POST', body: body }).then(function (res) {
      if (!res.data.ok) { setNote(note, false, res.data.error || '保存失败'); saveBtn.disabled = false; return; }
      showBanner(res.data.hot ? '已保存，当前生效模型已改用新配置' : '已保存供应商「' + name + '」');
      provSel = body.id;
      refreshModelViews();
      openProviderDetail(body.id);
    });
  });
  acts.appendChild(saveBtn);
  box.appendChild(acts);
}
function newProviderId(name) {
  var slug = String(name).toLowerCase().replace(/[^a-z0-9]+/g, '-').replace(/^-+|-+$/g, '');
  if (!slug) slug = 'provider';
  var id = 'p-' + slug;
  for (var n = 2; findProvider(id); n++) id = 'p-' + slug + '-' + n;
  return id;
}

/* ---------- 模型页事件绑定 ---------- */
$('#m-prov-new').addEventListener('click', function () { openProviderDetail(null); });
$('#m-add').addEventListener('click', function () {
  var preset = (provSel && provSel !== ALL) ? provSel : '';
  openNewModel(preset);
});
$('#m-batch').addEventListener('click', function () { setBatchMode(!batchMode); });
$('#m-batch-all').addEventListener('change', function () {
  var on = this.checked;
  lastListIds.forEach(function (id) { if (on) batchSel[id] = true; else delete batchSel[id]; });
  renderModelList();
  batchSyncBar();
});
$('#m-batch-ctx').addEventListener('click', function () { toggleBatchPanel('ctx'); });
$('#m-batch-rpm').addEventListener('click', function () { toggleBatchPanel('rpm'); });
$('#m-batch-cap').addEventListener('click', function () { toggleBatchPanel('cap'); });
$('#m-batch-del').addEventListener('click', function () { toggleBatchPanel('del'); });
$('#m-back-prov').addEventListener('click', function () { m3Show('prov'); });
$('#m-prov-filter').addEventListener('input', function () { provNameFilter = this.value; renderProvSide(); });
$('#m-model-filter').addEventListener('input', function () { modelNameFilter = this.value; renderModelList(); });
$('#m-disc').addEventListener('click', function () {
  if (provSel && provSel !== ALL && findProvider(provSel)) openProviderDetail(provSel);
});

/* ════════ 初始化 ════════ */
function boot() {
  api('/api/health').then(function (res) {
    if (res.ok) setConn(true, '已连接');
    else setConn(false, '服务异常');
  });
  showPage('general');
}
boot();

})();
