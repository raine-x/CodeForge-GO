// 输入区交互：权限控制弹层 / 模型选择弹层 / 发送按钮两态
// 权限：只读 / 请求 / 自主；点击空白处关闭弹层
(function () {
  // ---------- 工具 ----------
  function $(sel) { return document.querySelector(sel); }

  // hideWithAnim 统一退场：先播 .leaving 动画（约 .1s），结束后再真正 hidden。
  // 动画被 prefers-reduced-motion 关闭时 animationend 不触发，兜底 setTimeout 照常隐藏；
  // 退场途中被重新打开（leaving 被移除）时，pending 的定时器/事件落地即中止，不误藏。
  function hideWithAnim(el, done) {
    if (!el || el.classList.contains('hidden')) { if (done) done(); return; }
    var timer = el._hideTimer;
    if (timer) { clearTimeout(timer); el._hideTimer = null; }
    el.classList.add('leaving');
    var finished = false;
    function finish() {
      if (finished) return;
      finished = true;
      el._hideTimer = null;
      if (!el.classList.contains('leaving')) { if (done) done(); return; } // 已重新打开，中止
      el.classList.remove('leaving');
      el.classList.add('hidden');
      if (done) done();
    }
    el._hideTimer = setTimeout(finish, 160); // 兜底：动画事件缺失/被禁时也保证隐藏
    el.addEventListener('animationend', function onEnd(e) {
      if (e.target !== el || e.animationName !== 'uiPopOut' && e.animationName !== 'uiFadeOut') return;
      el.removeEventListener('animationend', onEnd);
      if (el._hideTimer) { clearTimeout(el._hideTimer); }
      finish();
    });
  }

  // 关闭指定弹层外的所有弹层
  function closeAllPops(except) {
    document.querySelectorAll('.popup').forEach(function (p) {
      if (p !== except) hideWithAnim(p);
    });
  }

  // 点击空白处关闭全部弹层
  document.addEventListener('mousedown', function (e) {
    if (!e.target.closest('.pop-anchor')) closeAllPops(null);
  });

  // ---------- 权限控制（动态）----------
  // 状态的唯一权威在服务端（/api/perm）：每次展开弹层都实时拉取，
  // 切换时 POST 后按服务端返回值渲染，本地不缓存最终状态。
  const permBtn = $('#perm-btn');
  const permPop = $('#perm-pop');
  const permLabel = permBtn.querySelector('.perm-label');
  let perm = 'ask';
  const permNames = { readonly: '只读', ask: '请求', auto: '自主' };

  function renderPerm(animate) {
    if (!animate) {
      permLabel.textContent = permNames[perm] || perm;
    } else {
      // 切换动画：旧状态淡出 → 新状态淡入
      permLabel.classList.remove('switch-in');
      permLabel.classList.add('switch-out');
      setTimeout(function () {
        permLabel.textContent = permNames[perm] || perm;
        permLabel.classList.remove('switch-out');
        void permLabel.offsetWidth; // 重启动画
        permLabel.classList.add('switch-in');
      }, 180);
    }
    permBtn.classList.toggle('perm-auto', perm === 'auto'); // 自主模式红字
    permBtn.title = '权限控制：' + (permNames[perm] || perm);
    permPop.querySelectorAll('.pop-opt').forEach(function (b) {
      b.classList.toggle('selected', b.dataset.perm === perm);
    });
  }

  // 每次展开都向服务端实时查询当前权限状态
  async function refreshPerm() {
    try {
      const res = await fetch('/api/perm');
      if (res.ok) {
        const d = await res.json();
        const m = (d.mode || 'ask').trim();
        if (m !== perm) { perm = m; renderPerm(false); }
        else renderPerm(false);
      }
    } catch (_) { /* 拉取失败保持本地展示 */ }
  }

  permBtn.addEventListener('click', function (e) {
    e.stopPropagation();
    const willOpen = permPop.classList.contains('hidden') && !permPop.classList.contains('leaving');
    closeAllPops(willOpen ? permPop : null);
    if (willOpen) {
      permPop.classList.remove('leaving', 'hidden');
      refreshPerm();
    } else {
      hideWithAnim(permPop);
    }
  });
  // 页面加载即同步服务端真实权限状态，避免「点击才拉取」导致按钮状态跳变
  refreshPerm();
  permPop.querySelectorAll('.pop-opt').forEach(function (b) {
    b.addEventListener('click', function () {
      const target = b.dataset.perm;
      hideWithAnim(permPop); // 选中即收起（带退场动画）
      if (target === perm) return;
      fetch('/api/perm', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ mode: target })
      }).then(function (r) { return r.json(); }).then(function (d) {
        if (d.ok) {
          perm = d.mode || target;
          renderPerm(true); // 带切换动画
        } else {
          alert('切换权限失败：' + (d.error || '未知错误'));
          refreshPerm(); // 失败时回查真实状态
        }
      }).catch(function () { refreshPerm(); });
    });
  });
  renderPerm(false);
  // 供发送消息等外部时机实时校验权限状态
  window.CodeForgePerm = { refresh: refreshPerm };

  // ---------- 皮肤（最高思考强度特效）----------
  let skin = localStorage.getItem('cf_skin') === 'forge' ? 'forge' : 'meteor';

  // 强度颜色：随皮肤取不同色相带（meteor 蓝→粉紫；forge 金→橙红，升温感）
  function intensityColor(ratio) {
    const hue = skin === 'forge' ? 45 - ratio * 33 : 207 + ratio * (330 - 207);
    return 'hsl(' + hue.toFixed(0) + ' 85% 60%)';
  }
  // 填充渐变：左端深、右端（滑块处）亮，像视频里发热的一端
  function fillGradient(ratio) {
    const c = intensityColor(ratio);
    return skin === 'forge'
      ? 'linear-gradient(90deg, hsl(12 78% 46%), ' + c + ')'
      : 'linear-gradient(90deg, hsl(207 88% 58%), ' + c + ')';
  }

  // ---------- 模型选择 ----------
  let model = '';          // 当前配置的模型（由 /api/config 拉取）
  let modelName = '';      // 模型显示名（后端 display_name 参数，缺省用 id）
  let modelInLib = true;   // 生效模型是否仍在模型库中（库被删空 → 界面提示重新配置）
  let thinkingSpec = null; // 上游思考分级规格（steps/range/none)
  let thinkingVal = '';    // 当前思考参数原值（枚举或 budget 数字）
  let modelsLoaded = false;
  let modelChoices = [];   // 模型库快照（/api/models/list 脱敏列表）：对话框弹层据此列出全部可选模型

  const modelBtn = $('#model-btn');
  const modelPop = $('#model-pop');
  const modelList = $('#model-list');
  const levelList = $('#level-list');

  // 思考档位按协议记忆（steps / range 各存一份）：刷新页面、重启、切模型后自动恢复。
  // 旧值在新规格里不合法（例如从 OpenAI 换成 Anthropic）时回落到规格默认档。
  function thinkingStorageKey(spec) { return 'cf_thinking_' + ((spec && spec.mode) || 'none'); }
  function pickThinking(spec) {
    if (!spec || spec.mode === 'none') return '';
    let saved = '';
    try { saved = localStorage.getItem(thinkingStorageKey(spec)) || ''; } catch (_) { saved = ''; }
    if (spec.mode === 'steps') {
      const ok = (spec.steps || []).some(function (st) { return st.value === saved; });
      return ok ? saved : (spec.default || '');
    }
    if (saved === 'none') return 'none';
    if (/^\d+$/.test(saved)) {
      const n = Number(saved);
      if (n <= 0) return 'none';
      if (n >= (spec.min || 0) && n <= (spec.max || 0)) return saved;
    }
    return spec.default || '';
  }
  function saveThinking() {
    if (!thinkingSpec || thinkingSpec.mode === 'none' || !thinkingVal) return;
    try { localStorage.setItem(thinkingStorageKey(thinkingSpec), thinkingVal); } catch (_) { /* 隐私模式等：忽略 */ }
  }

  // 拉取模型库快照：失败时保留上一次结果，不打断当前生效配置的加载。
  // 新增 / 删除 / 切换模型后由调用方置 modelsLoaded=false 再走 loadModels 刷新。
  async function fetchModelChoices() {
    try {
      const res = await fetch('/api/models/list');
      if (res.ok) {
        const d = await res.json();
        modelChoices = d.models || [];
      }
    } catch (_) { /* 服务不可达：保留旧快照 */ }
  }

  // 从后端拉取配置：模型 + 模型库 + 思考分级规格（首次展开时执行一次）
  async function loadModels() {
    if (modelsLoaded) return;
    modelsLoaded = true;
    try {
      const res = await fetch('/api/config');
      if (res.ok) {
        const cfg = await res.json();
        model = (cfg.model || '').trim();
        modelName = (cfg.display_name || cfg.model_display_name || '').trim(); // 预留显示名字段
        modelInLib = cfg.model_in_library !== false; // 缺省 true 兼容旧后端
        thinkingSpec = cfg.thinking || { mode: 'none' };
        thinkingVal = pickThinking(thinkingSpec);
      }
    } catch (_) { /* 拉取失败按无模型处理 */ }
    await fetchModelChoices();
    buildSlider();
    renderModelBtn();
    renderModelPop();
  }

  // 思考档位显示名：枚举档直接用原值（minimal/low/…），range 为 token 数，关闭档显示「关闭」
  function thinkingLabel() {
    if (!thinkingSpec) return 'medium';
    if (thinkingSpec.mode === 'steps') {
      if (thinkingVal === 'none') return '关闭';
      const hit = (thinkingSpec.steps || []).find(function (s) { return s.value === thinkingVal; });
      return hit ? hit.value : thinkingVal;
    }
    if (thinkingSpec.mode === 'range') {
      if (!thinkingVal || thinkingVal === 'none' || thinkingVal === '0') return '关闭';
      return thinkingVal + ' tokens';
    }
    return 'medium';
  }

  function renderModelBtn() {
    // 未配置模型、或生效模型已被从模型库删除（库空）：统一显示提示，不展示思考档位
    if (!model || !modelInLib) {
      modelBtn.classList.add('no-model');
      $('#model-level').textContent = '';
      $('#model-name').textContent = '请先配置模型';
      $('#model-name').title = '模型库为空，请在 设置 → 模型 中添加';
      return;
    }
    modelBtn.classList.remove('no-model');
    $('#model-level').textContent = thinkingLabel() + ' ›';
    $('#model-name').textContent = modelName || model; // 优先显示显示名，缺省回退模型 id
    $('#model-name').title = model;
  }
  // 模型弹层：列出模型库全部条目，点击即切换生效模型（POST /api/models/apply）；
  // 当前生效项带选中小圆点。库为空 / 生效项被删出库时补一行提示，避免弹层空无一物。
  function renderModelPop() {
    modelList.innerHTML = '';
    const activeInLib = !!model && modelInLib;
    modelChoices.forEach(function (m) {
      const b = document.createElement('button');
      b.type = 'button';
      const active = activeInLib && m.id === model;
      b.className = 'pop-opt' + (active ? ' selected' : '');
      b.dataset.model = m.id;
      b.textContent = m.name || m.id; // 显示名优先，缺省回退模型 id
      b.title = m.id;
      if (!active) b.addEventListener('click', function () { switchActiveModel(m.id); });
      modelList.appendChild(b);
    });
    if (!modelChoices.length) {
      const tip = document.createElement('div');
      tip.className = 'pop-tip';
      tip.textContent = '模型库为空，请在 设置 → 模型 → 添加模型 中添加';
      modelList.appendChild(tip);
    } else if (!activeInLib) {
      const tip = document.createElement('div');
      tip.className = 'pop-tip';
      tip.textContent = model ? '当前模型已不在模型库中，请重新选择' : '请选择模型';
      modelList.appendChild(tip);
    }
    syncLevelUI(); // 同步思考强度滑条
  }

  // 切换生效模型：服务端热切换后回填配置（含思考分级与显示名），刷新弹层/按钮/设置页。
  let switchingModel = false;
  function switchActiveModel(id) {
    if (switchingModel || !id || id === model) return;
    switchingModel = true;
    fetch('/api/models/apply', {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ model: id })
    }).then(function (r) { return r.json(); }).then(function (d) {
      if (!d.ok) { alert('切换模型失败：' + (d.error || '未知错误')); return; }
      const cfg = d.config || {};
      model = (cfg.model || id).trim();
      modelName = (cfg.display_name || cfg.model_display_name || '').trim();
      modelInLib = cfg.model_in_library !== false;
      thinkingSpec = cfg.thinking || { mode: 'none' };
      thinkingVal = pickThinking(thinkingSpec); // 按协议恢复上次选择的档位（含「关闭」）
      modelsLoaded = false; // 强制重拉，保证弹层选中态与思考分级是服务端的最新值
      loadModels();
      const mEl = document.getElementById('settings-model');
      if (mEl) { mEl.textContent = modelName || model || '--'; mEl.title = model || ''; }
      // 设置页开着时同步列表里的「当前」徽标
      if (!settingsOverlay.classList.contains('hidden')) renderModelItems();
    }).catch(function (e) {
      alert('切换模型失败：' + e);
    }).then(function () { switchingModel = false; });
  }

  // ---------- 思考强度滑条（按上游分级动态构建） ----------
  let slider = null;
  function buildSlider() {
    const spec = thinkingSpec || { mode: 'none' };
    if (spec.mode === 'none') {
      levelList.innerHTML = '<div class="pop-tip">当前模型不支持思考强度</div>';
      slider = null;
      return;
    }
    const n = spec.mode === 'steps' ? (spec.steps || []).length : 9; // range 用 9 个刻度点示意
    let dots = '';
    for (let i = 0; i < n; i++) dots += '<div class="fs-dot" data-i="' + i + '"></div>';
    // 星尘粒子群（复刻 effort 滑杆填充内的流动光尘，持续向左漂移）：
    // dot=光点缓慢漂移 / streak=流星拖尾快速掠过，位置与节奏全部随机错开
    let dust = '';
    for (let i = 0; i < 16; i++) {
      dust += '<span class="fs-p dot" style="--x:' + (4 + Math.random() * 92).toFixed(1) +
        '%;--y:' + (3 + Math.random() * 12).toFixed(1) +
        'px;--s:' + (2 + Math.random() * 2).toFixed(1) +
        'px;--d:' + (1.6 + Math.random() * 2.2).toFixed(2) +
        's;--dl:' + (Math.random() * 2.5).toFixed(2) +
        's;--tv:' + (40 + Math.random() * 70).toFixed(0) + 'px"></span>';
    }
    for (let i = 0; i < 5; i++) {
      dust += '<span class="fs-p streak" style="--x:' + (10 + Math.random() * 88).toFixed(1) +
        '%;--y:' + (4 + Math.random() * 11).toFixed(1) +
        'px;--len:' + (18 + Math.random() * 20).toFixed(0) +
        'px;--d:' + (0.9 + Math.random() * 1.1).toFixed(2) +
        's;--dl:' + (Math.random() * 1.8).toFixed(2) +
        's;--tv:' + (70 + Math.random() * 60).toFixed(0) + 'px"></span>';
    }
    levelList.innerHTML =
      '<div class="level-slider-wrap">' +
      '<div class="fancy-slider' + (skin === 'forge' ? ' skin-forge' : '') + '" id="level-slider">' +
      '<div class="fs-track"><div class="fs-fill">' + dust + '</div>' + dots + '</div>' +
      '<div class="fs-thumb"><span class="fs-tip"></span></div>' +
      '</div></div>';
    slider = levelList.querySelector('#level-slider');
    const fill = slider.querySelector('.fs-fill');
    const thumb = slider.querySelector('.fs-thumb');
    const tip = slider.querySelector('.fs-tip');
    const dotsEls = slider.querySelectorAll('.fs-dot');
    // 刻度点均匀分布在轨道上
    dotsEls.forEach(function (d, i) {
      d.style.left = (i / Math.max(1, dotsEls.length - 1) * 100) + '%';
    });

    function valToPct(v) {
      if (spec.mode === 'steps') {
        const idx = spec.steps.findIndex(function (s) { return s.value === v; });
        return (idx < 0 ? 0 : idx) / (spec.steps.length - 1) * 100;
      }
      const num = Number(v) || spec.min;
      return (num - spec.min) / (spec.max - spec.min) * 100;
    }
    syncLevelUI = function () {
      const pct = valToPct(thinkingVal);
      const ratio = pct / 100;
      const color = intensityColor(ratio);
      // 小球保持在槽内：中心活动范围 = [半径, 100%-半径]（thumb 24px → 半径 12px）
      const r = 12;
      thumb.style.left = 'calc(' + pct + '% + ' + (r - ratio * 2 * r).toFixed(2) + 'px)';
      fill.style.width = 'calc(' + pct + '% + ' + (r - ratio * 2 * r).toFixed(2) + 'px)';
      slider.style.setProperty('--glow', color);
      slider.style.setProperty('--fxo', (0.45 + ratio * 0.55).toFixed(2)); // 强度越高星尘越亮
      thumb.style.boxShadow = '0 2px 8px rgba(0,0,0,.35), 0 0 ' + (6 + ratio * 14).toFixed(0) + 'px ' + (2 + ratio * 3).toFixed(0) + 'px ' + color;
      fill.style.background = fillGradient(ratio);
      tip.textContent = thinkingLabel();
      dotsEls.forEach(function (d, i) {
        const dp = spec.mode === 'steps'
          ? i / Math.max(1, spec.steps.length - 1) * 100
          : i / (dotsEls.length - 1) * 100;
        d.classList.toggle('active', Math.abs(dp - pct) < 1.5);
      });
      // 最高强度：粒子加速 + 滑块呼吸光晕
      slider.classList.toggle('max', ratio >= 0.995);
    };
    // 拖动中预览档位名（关闭档统一显示「关闭」，range 的 0 就是关闭）
    function previewLabel(v) {
      if (spec.mode === 'steps') return v === 'none' ? '关闭' : v;
      if (!v || v === '0' || v === 'none') return '关闭';
      return v + ' tokens';
    }
    // 指针位置 → 最近档位原值（steps 取最近档；range 对齐步进）
    function nearestValue(clientX) {
      const rect = slider.getBoundingClientRect();
      const ratio = Math.min(1, Math.max(0, (clientX - rect.left) / rect.width));
      let v;
      if (spec.mode === 'steps') {
        v = spec.steps[Math.round(ratio * (spec.steps.length - 1))].value;
      } else {
        v = String(Math.round((spec.min + ratio * (spec.max - spec.min)) / spec.step) * spec.step);
      }
      return { v: v, ratio: ratio };
    }
    let dragRatio = null; // 拖动中的连续位置（线性跟随）
    // 拖动预览：滑块线性跟随 + 实时变色（保持槽内）
    function previewAt(ratio) {
      const color = intensityColor(ratio);
      const r = 12;
      thumb.style.left = 'calc(' + ratio * 100 + '% + ' + (r - ratio * 2 * r).toFixed(2) + 'px)';
      fill.style.width = 'calc(' + ratio * 100 + '% + ' + (r - ratio * 2 * r).toFixed(2) + 'px)';
      slider.style.setProperty('--glow', color);
      slider.style.setProperty('--fxo', (0.45 + ratio * 0.55).toFixed(2));
      thumb.style.boxShadow = '0 2px 8px rgba(0,0,0,.35), 0 0 ' + (6 + ratio * 14).toFixed(0) + 'px ' + (2 + ratio * 3).toFixed(0) + 'px ' + color;
      fill.style.background = fillGradient(ratio);
    }
    slider.addEventListener('pointerdown', function (e) {
      e.preventDefault();
      // 命中刻度点：直接跳档（preventDefault 会拦截 click，须在此处理）
      const dot = e.target.closest ? e.target.closest('.fs-dot') : null;
      if (dot) {
        const i = Number(dot.dataset.i);
        const dp = i / Math.max(1, dotsEls.length - 1);
        if (spec.mode === 'steps') {
          thinkingVal = spec.steps[Math.round(dp * (spec.steps.length - 1))].value;
        } else {
          thinkingVal = String(Math.round((spec.min + dp * (spec.max - spec.min)) / spec.step) * spec.step);
        }
        saveThinking();
        syncLevelUI();
        renderModelBtn();
        return;
      }
      try { slider.setPointerCapture(e.pointerId); } catch (_) { /* 合成事件/指针已释放时忽略 */ }
      slider.classList.add('dragging');
      const hit = nearestValue(e.clientX);
      dragRatio = hit.ratio;
      previewAt(dragRatio); // 线性跟随，不吸附
      tip.textContent = hit.v === thinkingVal ? thinkingLabel() : previewLabel(hit.v);
    });
    slider.addEventListener('pointermove', function (e) {
      if (!slider.classList.contains('dragging')) return;
      const hit = nearestValue(e.clientX);
      dragRatio = hit.ratio;
      previewAt(dragRatio);
      tip.textContent = previewLabel(hit.v);
    });
    slider.addEventListener('pointerup', function (e) {
      if (!slider.classList.contains('dragging')) return;
      slider.classList.remove('dragging');
      dragRatio = null;
      // 松手：吸附最近档位（动画过渡），不关闭弹层
      const hit = nearestValue(e.clientX);
      thinkingVal = hit.v;
      saveThinking();
      syncLevelUI();
      renderModelBtn();
    });
    slider.addEventListener('pointercancel', function () {
      slider.classList.remove('dragging');
      dragRatio = null;
      syncLevelUI();
    });
  }
  function syncLevelUI() {} // spec 加载后由 buildSlider 覆盖
  modelBtn.addEventListener('click', function (e) {
    e.stopPropagation();
    const willOpen = modelPop.classList.contains('hidden') && !modelPop.classList.contains('leaving');
    closeAllPops(willOpen ? modelPop : null);
    if (willOpen) {
      modelPop.classList.remove('leaving', 'hidden');
      loadModels();          // 拉出时加载当前配置的模型与思考分级
      renderModelPop();
    } else {
      hideWithAnim(modelPop);
    }
  });
  renderModelBtn();

  // ---------- 轻量 Markdown 渲染（零依赖） ----------
  function escapeHtml(s) {
    return s.replace(/[&<>"']/g, function (c) {
      return { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c];
    });
  }
  function renderMD(src) {
    if (!src) return '';
    const blocks = [];
    // 先抽取 fenced 代码块，避免内部被行内规则处理
    let t = src.replace(/```(\w*)\n?([\s\S]*?)(```|$)/g, function (_, lang, code) {
      blocks.push('<pre><code>' + escapeHtml(code) + '</code></pre>');
      return '\u0000B' + (blocks.length - 1) + '\u0000';
    });
    // 数学公式：必须同样先于行内规则抽取，否则 \ * ` _ 会被当成 Markdown 处理
    // （例如 $a*b*c$ 会被拆成 <em>、$x_1$ 的下标会被吞）。
    // 零依赖处理：不做排版，仅原样保留源码并加专用样式区分。
    // 支持 $$…$$ / \[…\]（块级）与 $…$ / \(…\)（行内）四种写法。
    // 用 <span> 承载块级样式（CSS display:block），避免 <div> 落进 <p> 造成非法嵌套。
    function holdMath(tex, cls) {
      blocks.push('<span class="' + cls + '">' + escapeHtml(tex.trim()) + '</span>');
      return '\u0000B' + (blocks.length - 1) + '\u0000';
    }
    t = t.replace(/\$\$([\s\S]+?)\$\$/g, function (_, tex) { return holdMath(tex, 'math-block'); });
    t = t.replace(/\\\[([\s\S]+?)\\\]/g, function (_, tex) { return holdMath(tex, 'math-block'); });
    // 行内 $…$：三重约束避免误吞普通文本 ——
    //   ① $ 后非空白；② 结尾前一位非空白；③ 闭合 $ 后不能紧跟数字（挡「价格 $5 到 $10」）
    t = t.replace(/\$(?!\s)([^\n$]*[^\s$])\$(?!\d)/g, function (_, tex) { return holdMath(tex, 'math-inline'); });
    t = t.replace(/\\\(([\s\S]+?)\\\)/g, function (_, tex) { return holdMath(tex, 'math-inline'); });
    t = escapeHtml(t);
    // 行内代码 / 粗斜体 / 链接
    t = t.replace(/`([^`\n]+)`/g, '<code>$1</code>');
    t = t.replace(/\*\*([^*]+)\*\*/g, '<strong>$1</strong>');
    t = t.replace(/(^|[^*])\*([^*\n]+)\*/g, '$1<em>$2</em>');
    t = t.replace(/\[([^\]]+)\]\((https?:[^)\s]+)\)/g, '<a href="$2" target="_blank" rel="noopener">$1</a>');
    // 标题（h1-h4）
    t = t.replace(/^#### (.*)$/gm, '<h4>$1</h4>');
    t = t.replace(/^### (.*)$/gm, '<h3>$1</h3>');
    t = t.replace(/^## (.*)$/gm, '<h2>$1</h2>');
    t = t.replace(/^# (.*)$/gm, '<h1>$1</h1>');
    // 引用 / 分隔线
    t = t.replace(/^&gt; ?(.*)$/gm, '<blockquote>$1</blockquote>');
    t = t.replace(/^ {0,3}(---+|\*\*\*+)\s*$/gm, '<hr>');
    // 表格：表头行 + 分隔行（|---|:--:|）+ 数据行；支持 :--- / :--: / ---: 对齐。
    // 前后补空行，使其成为独立段落块，避免与相邻文本被并进同一个 <p>。
    t = t.replace(
      /(?:^|\n)(\|[^\n]*\|[ \t]*\n[ \t]*\|[ \t:|-]*-[ \t:|-]*\|[ \t]*(?:\n[ \t]*\|[^\n]*\|[ \t]*)*)/g,
      function (m, block) {
        const rows = block.trim().split('\n').map(function (r) {
          return r.trim().replace(/^\|/, '').replace(/\|$/, '')
            .split('|').map(function (c) { return c.trim(); });
        });
        if (rows.length < 2) return m;
        const aligns = rows[1].map(function (c) {
          const l = c.charAt(0) === ':';
          const r = c.charAt(c.length - 1) === ':';
          return l && r ? 'center' : r ? 'right' : l ? 'left' : '';
        });
        function cell(tag, txt, i) {
          return '<' + tag + (aligns[i] ? ' style="text-align:' + aligns[i] + '"' : '') + '>' + txt + '</' + tag + '>';
        }
        let html = '<table><thead><tr>' +
          rows[0].map(function (c, i) { return cell('th', c, i); }).join('') + '</tr></thead>';
        const body = rows.slice(2);
        if (body.length) {
          html += '<tbody>' + body.map(function (r) {
            return '<tr>' + r.map(function (c, i) { return cell('td', c, i); }).join('') + '</tr>';
          }).join('') + '</tbody>';
        }
        return '\n\n' + html + '</table>\n\n';
      }
    );
    // 无序 / 有序列表（连续行合并）
    t = t.replace(/(?:^|\n)((?:[ \t]*[-*] .+(?:\n|$))+)/g, function (_, list) {
      const items = list.trim().split(/\n/).map(function (l) {
        return '<li>' + l.replace(/^[ \t]*[-*] /, '') + '</li>';
      }).join('');
      return '\n<ul>' + items + '</ul>';
    });
    t = t.replace(/(?:^|\n)((?:[ \t]*\d+\. .+(?:\n|$))+)/g, function (_, list) {
      const items = list.trim().split(/\n/).map(function (l) {
        return '<li>' + l.replace(/^[ \t]*\d+\. /, '') + '</li>';
      }).join('');
      return '\n<ol>' + items + '</ol>';
    });
    // 段落：双换行分段，段内单换行转 <br>；已是块级标签的段不包裹
    t = t.split(/\n{2,}/).map(function (p) {
      const s = p.trim();
      if (!s) return '';
      if (/^<(h\d|ul|ol|pre|blockquote|hr|table|\u0000B)/.test(s) || /^\u0000B\d+\u0000$/.test(s)) return s;
      return '<p>' + s.replace(/\n/g, '<br>') + '</p>';
    }).join('\n');
    // 恢复代码块
    return t.replace(/\u0000B(\d+)\u0000/g, function (_, i) { return blocks[+i]; });
  }

  // ---------- 对话面板渲染 ----------
  const messagesEl = $('#messages');
  let msgCol = null;

  function ensureCol() {
    if (!msgCol) {
      msgCol = document.createElement('div');
      msgCol.className = 'msg-col';
      messagesEl.appendChild(msgCol);
    }
    return msgCol;
  }
  function scrollBottom() {
    messagesEl.scrollTop = messagesEl.scrollHeight;
  }
  function addAssistant(text) {
    const d = document.createElement('div');
    d.className = 'msg-assistant';
    d.textContent = text;
    ensureCol().appendChild(d);
    scrollBottom();
  }
  // 提及 token 的统一切分规则：`@名字`（不含空白）或 `@"名字"`（名字含空白时用引号）。
  // ⚠️ 两条例外规矩：
  //   ① 只能有**一个**捕获组，且内部不许再嵌套分组 —— String.split 会把**所有**捕获组
  //      都塞进结果数组，多一个组就会把内容多切一份（引号内的名字曾被重复吐出来）。
  //   ② **不加 `g` 标志**：这三处都只用 split（split 本来就会切所有匹配，不需要 g），
  //      而带 g 的正则一旦被谁拿去做 test()/exec() 就会残留 lastIndex、结果飘忽。
  // 输入框镜像高亮、发送前展开别名、抽取待 stage 的文件，三处必须共用这一套。
  const MENTION_SPLIT = /(@"(?:[^"]*)"|@[^\s@]+)/;
  // ＋添加文件插入的是「@文件名」而不是完整路径，真实路径记在这张表里（发送前展开）。
  // ⚠️ 声明必须早于 syncInputMirror 的**首次调用**（那里会 prune），否则会撞 TDZ。
  const fileAlias = new Map(); // 提及 token（含 @ 与可能的引号）→ 完整路径
  // 取出 token 里的真正内容（去掉 @ 与可能的引号）
  function mentionBody(tok) {
    const s = String(tok).slice(1);
    return (s.length >= 2 && s.charAt(0) === '"' && s.charAt(s.length - 1) === '"') ? s.slice(1, -1) : s;
  }
  // 渲染 @提及（@技能名 / @文件路径）为蓝色 <span class="at-mention">。两处复用：
  //   ① 消息气泡（addUser）：文件提及**只显示文件名**（title 悬浮看完整路径），
  //      否则一长串路径会把气泡撑爆；
  //   ② 输入框的镜像高亮层（syncInputMirror）：必须**原样显示**——那里就是用户正在编辑的
  //      文字，缩成文件名会与 textarea 的真实内容错位（所以传 { shortenPath: false }）。
  function renderUserText(container, text, opts) {
    const shorten = !opts || opts.shortenPath !== false;
    const parts = String(text).split(MENTION_SPLIT);
    parts.forEach(function (p) {
      if (!p) return;
      if (p.charAt(0) === '@' && p.length > 1) {
        const s = document.createElement('span');
        s.className = 'at-mention';
        const body = mentionBody(p);
        // 含路径分隔符 = 文件提及：气泡里只显示文件名（title 存完整路径）
        if (shorten && /[\\/]/.test(body)) {
          const name = body.replace(/[\\/]+$/, '').split(/[\\/]/).pop() || body;
          s.textContent = '@' + name;
          s.title = body; // 悬浮看完整路径
        } else {
          // 气泡（shorten=true）顺手去掉可能的引号：`@"a b.txt"` → `@a b.txt`。
          // ⚠️ 输入框镜像层必须**逐字原样**（textarea 里就是带引号的），否则会与真实文字错位。
          s.textContent = shorten ? '@' + body : p;
        }
        container.appendChild(s);
      } else {
        container.appendChild(document.createTextNode(p));
      }
    });
  }
  function addUser(text) {
    const row = document.createElement('div');
    row.className = 'msg-user';
    const b = document.createElement('div');
    b.className = 'bubble';
    renderUserText(b, text);
    row.appendChild(b);
    ensureCol().appendChild(row);
    scrollBottom();
    syncComposerMode(); // 用户发出第一句话：输入卡片下放回底部
  }
  function addInfo(text) {
    const d = document.createElement('div');
    d.className = 'msg-info';
    d.textContent = text;
    ensureCol().appendChild(d);
    scrollBottom();
    syncComposerMode();
  }
  // 工具中文名映射（内置工具 + 内置插件工具）：审批条等只显示短语、不暴露参数的场景使用。
  // 未识别的工具（MCP 插件等）回退显示原始工具名。
  const toolPhrases = {
    read_file:    '读取文件',
    list_dir:     '浏览目录',
    search_files: '搜索文件',
    run_command:  '执行命令',
    write_file:   '写入文件',
    edit_file:    '编辑文件',
    delete_file:  '删除文件',
    save_memory:  '保存记忆',
    create_skill: 'Skill Creator 插件', // 内置插件工具：审批显示「需要审批：Skill Creator 插件」
    delegate_subagents: 'Multi-Agent 插件',
  };
  function toolPhrase(name) {
    return toolPhrases[name] || ('调用 ' + name);
  }

  // 工具卡片文案：中文短语 + 目标（文件名 / 命令）。
  // 只取路径最后一段，避免长路径把卡片撑宽；完整参数仍在 title 悬浮提示里。
  // 未识别的工具回退成「调用了 <name>」，新增内置工具或插件工具时不会显示空白。
  function toolLabel(name, input) {
    let arg = '';
    if (input && typeof input === 'object') {
      arg = input.path || input.command || '';
    } else if (typeof input === 'string') {
      try {
        const o = JSON.parse(input);
        if (o && typeof o === 'object') arg = o.path || o.command || '';
      } catch (_) { /* 非 JSON 字符串：忽略 */ }
    }
    arg = String(arg == null ? '' : arg).trim();
    const base = arg.replace(/[\\/]+$/, '').split(/[\\/]/).pop() || arg;
    const named = base.length > 60 ? base.slice(0, 60) + '…' : base;
    const shown = arg.length > 80 ? arg.slice(0, 80) + '…' : arg;

    switch (name) {
      case 'read_file':    return named ? '读取了文件 ' + named : '读取了文件';
      case 'list_dir':     return '查看项目';
      case 'search_files': return '搜索项目';
      case 'write_file':   return named ? '创建 ' + named : '创建了文件';
      case 'edit_file':    return named ? '编辑 ' + named : '编辑了文件';
      case 'delete_file':  return named ? '删除了文件 ' + named : '删除了文件';
      case 'run_command':  return shown ? '运行 ' + shown : '运行了命令';
      // 内置插件工具：工作流中显示「使用插件 XXX」提醒
      case 'create_skill': {
        const skName = (input && typeof input === 'object' && input.name) ? input.name : '';
        return '使用插件 Skill Creator' + (skName ? '：创建技能 ' + skName : '');
      }
      case 'delegate_subagents': {
        let n = 0;
        if (input && typeof input === 'object' && Array.isArray(input.tasks)) n = input.tasks.length;
        return '使用插件 Multi-Agent：并行委派 ' + (n > 0 ? n + ' 个子智能体' : '子智能体');
      }
      case 'save_memory': return '保存了记忆';
      default:             return '调用了 ' + name;
    }
  }

  // activeToolEl：最近一条工具条目（运行中带 spinner；收到结果或开始下一段
  // 思考/正文时移除 spinner，让用户知道工具正在执行而不是卡死）。
  let activeToolEl = null;
  function settleActiveTool() {
    if (activeToolEl) {
      activeToolEl.classList.remove('running');
      const sp = activeToolEl.querySelector('.tool-spinner');
      if (sp) sp.remove();
      activeToolEl = null;
    }
  }
  function addTool(text, detail, diffStats) {
    settleActiveTool();
    const d = document.createElement('div');
    d.className = 'msg-tool running';
    const sp = document.createElement('span');
    sp.className = 'tool-spinner';
    d.appendChild(sp);
    d.appendChild(document.createTextNode(text));
    if (detail) d.title = detail;
    // 编辑类工具：追加 +新增 / -删除 行数（绿增红减，等宽数字）
    if (diffStats && (diffStats.added > 0 || diffStats.removed > 0)) {
      d.appendChild(document.createTextNode(' '));
      const add = document.createElement('span');
      add.className = 'diffstat add';
      add.textContent = '+' + diffStats.added;
      const del = document.createElement('span');
      del.className = 'diffstat del';
      del.textContent = '-' + diffStats.removed;
      d.appendChild(add);
      d.appendChild(del);
    }
    ensureCol().appendChild(d);
    activeToolEl = d;
    scrollBottom();
  }
  function addError(text) {
    const d = document.createElement('div');
    d.className = 'msg-error';
    d.textContent = text;
    ensureCol().appendChild(d);
    scrollBottom();
    syncComposerMode();
  }

  // ---------- 子智能体进度卡片 ----------
  // 每个子任务一张卡片（按 id 复用），status: started → running → completed/failed。
  // 失败红、完成绿、运行中 spinner；detail 单行展示工具活动，summary 悬浮可见。
  const subagentCards = new Map();
  function addSubagentCard(sa) {
    const id = sa.id || 'sub';
    const status = sa.status || 'running';
    let card = subagentCards.get(id);
    if (!card) {
      card = document.createElement('div');
      card.className = 'msg-subagent';
      card.dataset.subagent = id;
      const badge = document.createElement('span');
      badge.className = 'sub-badge';
      const label = document.createElement('span');
      label.className = 'sub-label';
      const detail = document.createElement('span');
      detail.className = 'sub-detail';
      card.appendChild(badge);
      card.appendChild(label);
      card.appendChild(detail);
      card.title = '';
      ensureCol().appendChild(card);
      subagentCards.set(id, card);
    }
    const badge = card.querySelector('.sub-badge');
    const label = card.querySelector('.sub-label');
    const detail = card.querySelector('.sub-detail');
    card.dataset.status = status;
    badge.textContent = (sa.mode === 'implement' ? '实现' : '探索') + ' · ' + id;
    if (sa.detail) detail.textContent = sa.detail;
    if (sa.summary) card.title = sa.summary; // 悬浮查看摘要全文
    // 终态标头：完成/失败替换 detail 行文案
    if (status === 'completed' && sa.summary) {
      detail.textContent = sa.summary.length > 120 ? sa.summary.slice(0, 120) + '…' : sa.summary;
    }
    if (status === 'failed' && sa.summary) {
      detail.textContent = '失败：' + (sa.summary.length > 100 ? sa.summary.slice(0, 100) + '…' : sa.summary);
    }
    scrollBottom();
  }
  // 新一轮对话开始（busy）时清空子智能体卡片缓存，避免跨轮复用旧 DOM
  function resetSubagentCards() {
    subagentCards.clear();
  }

  // 上游瞬时故障自动重试提示：样式同「编辑文件」类文字条目，点击展开/收起错误原因。
  let retryEl = null;
  function addRetry(reason) {
    if (!retryEl) {
      retryEl = document.createElement('div');
      retryEl.className = 'msg-tool retry';
      const label = document.createElement('span');
      label.textContent = '请求失败，正在重试…';
      const detail = document.createElement('span');
      detail.className = 'retry-reason';
      retryEl.appendChild(label);
      retryEl.appendChild(detail);
      // 点击切换显示/隐藏错误原因
      retryEl.addEventListener('click', function () {
        detail.classList.toggle('show');
      });
      ensureCol().appendChild(retryEl);
      scrollBottom();
    }
    retryEl.querySelector('.retry-reason').textContent = '原因：' + (reason || '未知错误');
  }
  function removeRetry() {
    if (retryEl) { retryEl.remove(); retryEl = null; }
  }

  // 回复结束后的操作栏：复制 / 模型名 / 重新生成
  function addActions(replyText) {
    const bar = document.createElement('div');
    bar.className = 'msg-actions';

    // 复制
    const copy = document.createElement('button');
    copy.type = 'button';
    copy.className = 'act-btn';
    copy.title = '复制';
    copy.innerHTML = '<svg viewBox="0 0 24 24" width="14" height="14" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"><rect x="9" y="9" width="12" height="12" rx="2"/><path d="M5 15H4a2 2 0 0 1-2-2V4a2 2 0 0 1 2-2h9a2 2 0 0 1 2 2v1"/></svg>';
    copy.addEventListener('click', function () {
      navigator.clipboard.writeText(replyText).then(function () {
        copy.classList.add('done');
        setTimeout(function () { copy.classList.remove('done'); }, 1200);
      });
    });

    // 模型名（后端将提供 display_name，缺省用模型 id）
    const chip = document.createElement('span');
    chip.className = 'act-model';
    chip.textContent = modelName || model || '--';
    chip.title = model || '';

    // 重新生成
    const regen = document.createElement('button');
    regen.type = 'button';
    regen.className = 'act-btn';
    regen.title = '重新生成';
    regen.innerHTML = '<svg viewBox="0 0 24 24" width="14" height="14" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"><path d="M21 12a9 9 0 1 1-2.64-6.36"/><polyline points="21 3 21 9 15 9"/></svg>';
    regen.addEventListener('click', function () {
      if (running || !lastUserText || !wsReady) return;
      // 删除本列中用户消息之后的旧回复（思考/正文/工具/操作栏），原位等待新回复
      const col = bar.closest('.msg-col');
      if (col) {
        let node = col.lastChild;
        while (node) {
          const prev = node.previousSibling;
          if (node.classList && node.classList.contains('msg-user')) break;
          node.remove();
          node = prev;
        }
      }
      // 重置流式状态：新一轮内容继续写入这一列
      currentTextEl = null; textBuffer = '';
      reasonEl = null; reasonBuffer = '';
      lastReply = '';
      msgCol = col || null;
      wsSend({
        type: 'regenerate',
        session_id: sessionID,
        thinking: thinkingVal
      });
    });

    bar.appendChild(copy);
    bar.appendChild(chip);
    bar.appendChild(regen);
    ensureCol().appendChild(bar);
    scrollBottom();
  }
  // HITL 审批：内联批准 / 拒绝
  // 审批条只显示工具中文短语（如「写入文件」），不暴露命令 / 路径等参数明文；
  // 判定理由与动作目标放在悬浮提示里，需要时悬停即可查看。
  function addApproval(req) {
    const wrap = document.createElement('div');
    wrap.className = 'msg-approval';
    const label = document.createElement('div');
    label.className = 'approval-text';
    // 审批可能来自后台运行的另一个会话：显式标注来源，避免被误认为当前会话的操作。
    const fromOther = !!req.session_id && req.session_id !== sessionID;
    if (fromOther) {
      const meta = sessionsCache.find(function (s) { return s.id === req.session_id; });
      label.textContent = '需要审批（来自会话「' + ((meta && meta.title) || req.session_id) + '」）：' + toolPhrase(req.tool);
      wrap.classList.add('from-other');
    } else {
      label.textContent = '需要审批：' + toolPhrase(req.tool);
    }
    const tip = [req.reason, req.action ? '目标：' + req.action : ''].filter(Boolean).join('\n');
    if (tip) label.title = tip;
    const btns = document.createElement('div');
    btns.className = 'approval-btns';
    const ok = document.createElement('button');
    ok.type = 'button'; ok.className = 'approval-btn approve'; ok.textContent = '批准';
    const no = document.createElement('button');
    no.type = 'button'; no.className = 'approval-btn reject'; no.textContent = '拒绝';
    function decide(approved) {
      wsSend({ type: 'hitl_decision', approval_id: req.approval_id, approved: approved });
      label.textContent += approved ? ' —— 已批准' : ' —— 已拒绝';
      ok.remove(); no.remove();
    }
    ok.addEventListener('click', function () { decide(true); });
    no.addEventListener('click', function () { decide(false); });
    btns.appendChild(ok); btns.appendChild(no);
    wrap.appendChild(label); wrap.appendChild(btns);
    ensureCol().appendChild(wrap);
    scrollBottom();
  }

  // ---------- WebSocket：接入真实模型 ----------
  let ws = null;
  let wsReady = false;
  let sessionID = '';
  let currentTextEl = null;   // 当前流式输出的助手段落
  let textBuffer = '';        // 当前段落的原始 markdown
  let thinkingEl = null;      // 「等待模型响应」提示
  let reasonEl = null;        // 当前思考过程折叠块
  let reasonBuffer = '';
  let reasonPinned = false;   // 用户是否手动上翻了思考过程（上翻时不自动贴底）
  let lastReply = '';         // 本轮回复全文（供复制/操作栏）
  let lastUserText = '';      // 最近一次用户消息（供重新生成）
  // 后台运行：一个连接同时只跑一个任务（服务端 c.run 会先停旧任务），
  // runSessionID 记录这一轮属于哪个会话；用户切走后事件继续到达，但不能画进当前视图。
  let runSessionID = '';
  // 后台运行会话「已流出但尚未落盘」的思考/正文片段：不随视图切换重置，
  // 切回该会话时补渲染。落盘点与正常流程一致（正文开始弃思考、工具调用清正文）。
  let runReason = '';
  let runText = '';

  // 本轮任务不在当前视图（用户切到别的会话去了）
  function runAway() {
    return running && runSessionID && runSessionID !== sessionID;
  }

  function wsSend(obj) {
    if (ws && ws.readyState === WebSocket.OPEN) ws.send(JSON.stringify(obj));
  }
  // 上报页面可见性：窗口在前台可见（用户正看着）时，任务完成不弹系统通知。
  // 连接建立时上报一次，之后每次切换标签页/最小化/回到前台都上报。
  function sendVisibility() {
    wsSend({ type: 'visibility', hidden: !!document.hidden });
  }
  document.addEventListener('visibilitychange', sendVisibility);

  // ---------- 上下文占用进度条（底部栏右侧）----------
  // 服务端在「切换会话 / 新建会话 / 每轮 idle」时下发 {type:'context', ...}；
  // 前端打开明细时再主动拉一次，保证数字是当前会话的。
  const ctxMeter = $('#ctx-meter');
  const ctxFill = $('#ctx-fill');
  const ctxPct = $('#ctx-pct');
  const ctxPop = $('#ctx-pop');
  let ctxUsage = null;   // 最近一次下发的上下文占用数据

  // 把 token 数缩写成 12.3k / 1.2M（明细与提示里空间有限）。
  function fmtTokens(n) {
    n = Number(n) || 0;
    if (n >= 1000000) return (n / 1000000).toFixed(1).replace(/\.0$/, '') + 'M';
    if (n >= 1000) return (n / 1000).toFixed(1).replace(/\.0$/, '') + 'k';
    return String(n);
  }

  function renderCtxUsage(d) {
    ctxUsage = d || null;
    const pct = ctxUsage ? Math.max(0, Number(ctxUsage.percent) || 0) : 0;
    ctxFill.style.width = Math.min(100, pct) + '%'; // 条宽封顶 100%，数字照实显示
    ctxPct.textContent = ctxUsage ? pct.toFixed(1) + '%' : '—';
    ctxMeter.classList.toggle('warn', pct >= 70 && pct < 90);
    ctxMeter.classList.toggle('danger', pct >= 90);
    ctxMeter.classList.toggle('compressed', !!(ctxUsage && ctxUsage.compressed));
    ctxMeter.title = '上下文使用：' + fmtTokens(ctxUsage && ctxUsage.used) + ' / ' +
      fmtTokens(ctxUsage && ctxUsage.budget) + ' tokens（' + pct.toFixed(1) + '%，达 100% 自动摘要压缩）';
    if (!ctxPop.classList.contains('hidden')) renderCtxPop();
  }

  function ctxRow(k, v) {
    const row = document.createElement('div');
    row.className = 'ctx-row';
    const kEl = document.createElement('span');
    kEl.className = 'k'; kEl.textContent = k;
    const vEl = document.createElement('span');
    vEl.className = 'v'; vEl.textContent = v; vEl.title = v;
    row.appendChild(kEl); row.appendChild(vEl);
    return row;
  }

  function renderCtxPop() {
    ctxPop.innerHTML = '';
    const title = document.createElement('div');
    title.className = 'ctx-title';
    title.textContent = '上下文使用情况';
    ctxPop.appendChild(title);

    if (!ctxUsage) {
      const tip = document.createElement('div');
      tip.className = 'ctx-tip';
      tip.textContent = '暂无数据（当前没有活动会话）。';
      ctxPop.appendChild(tip);
      return;
    }
    const d = ctxUsage;
    const pct = Math.max(0, Number(d.percent) || 0);
    const summarized = Number(d.summarized) || 0;
    const rows = [
      ['模型名称', d.model || d.model_id || '—'],
      ['上下文长度', d.window ? fmtTokens(d.window) + ' tokens' : '—'],
      ['已使用总 tokens', fmtTokens(d.total_tokens) + ' tokens'],
      ['缓存命中', fmtTokens(d.cache_hit) + ' tokens'],
      ['缓存未命中', fmtTokens(d.cache_miss) + ' tokens'],
    ];
    rows.forEach(function (r) { ctxPop.appendChild(ctxRow(r[0], r[1])); });

    const tip = document.createElement('div');
    const level = pct >= 90 ? ' danger' : (pct >= 70 ? ' warn' : '');
    tip.className = 'ctx-tip' + level;
    if (d.over_budget && !d.compressed) {
      tip.textContent = '已越过压缩线，正在压缩上下文…';
    } else if (d.compressed) {
      tip.textContent = '已自动压缩：前 ' + summarized +
        ' 条历史被压成摘要送入模型，完整历史仍保留在会话里（可正常回看）。' +
        '占比达 100% 时会继续增量压缩。';
    } else {
      tip.textContent = '占比 = 送模占用 / 压缩线（模型窗口扣除输出预留后的 95%）。' +
        '达 100% 时自动调用模型把较早历史压成详细摘要。';
    }
    tip.textContent += '用量与缓存统计自服务启动后累计（重启重新计数）。';
    ctxPop.appendChild(tip);
  }

  ctxMeter.addEventListener('click', function (e) {
    e.stopPropagation(); // 别让下面的 document 收起逻辑立刻又关掉
    if (ctxPop.classList.contains('hidden')) {
      wsSend({ type: 'context', session_id: sessionID }); // 打开即拉最新占用
      renderCtxPop();
      ctxPop.classList.remove('hidden');
    } else {
      ctxPop.classList.add('hidden');
    }
  });
  document.addEventListener('click', function (e) {
    if (ctxPop.classList.contains('hidden')) return;
    if (ctxPop.contains(e.target) || ctxMeter.contains(e.target)) return;
    ctxPop.classList.add('hidden'); // 点击别处收起
  });
  renderCtxUsage(null);

  // 模型尚未开始输出时的等待提示（首字用户消息、工具返回后再次等待模型时都会显示）。
  function showThinking() {
    if (thinkingEl) return;
    thinkingEl = document.createElement('div');
    thinkingEl.className = 'msg-thinking';
    thinkingEl.innerHTML = '等待模型响应<span class="dots"><i>.</i><i>.</i><i>.</i></span>';
    ensureCol().appendChild(thinkingEl);
    scrollBottom();
  }
  function removeThinking() {
    if (thinkingEl) { thinkingEl.remove(); thinkingEl = null; }
  }
  // 思考过程折叠块（浅色、缩进、可展开；思考期间保持展开，正文出现后才折叠）
  function ensureReason() {
    if (!reasonEl) {
      reasonEl = document.createElement('details');
      reasonEl.className = 'msg-reasoning';
      reasonEl.open = true; // 思考期间始终展开，不按行数中途折叠
      const summary = document.createElement('summary');
      summary.textContent = '思考过程';
      const body = document.createElement('div');
      body.className = 'reason-body';
      // 贴底跟随：只有「用户主动滚动」才停（wheel/触摸/按住），滚回底部自动恢复。
      // 不能用 scroll 事件置位 —— 限高生效、内容增长等布局变化也会触发 scroll，
      // 那样一超过 10 行就会误判成「用户上翻」，从此不再自动滚动（历史 BUG）。
      body.addEventListener('wheel', function () { reasonPinned = true; }, { passive: true });
      body.addEventListener('touchmove', function () { reasonPinned = true; }, { passive: true });
      body.addEventListener('mousedown', function () { reasonPinned = true; });
      body.addEventListener('scroll', function () {
        if (body.scrollHeight - body.scrollTop - body.clientHeight <= 24) reasonPinned = false;
      }, { passive: true });
      reasonEl.appendChild(summary);
      reasonEl.appendChild(body);
      ensureCol().appendChild(reasonEl);
      reasonPinned = false;
    }
    return reasonEl.querySelector('.reason-body');
  }
  function appendReason(delta) {
    reasonBuffer += delta;
    const body = ensureReason();
    body.textContent = reasonBuffer;
    // 思考过程过长（>10 行）时收进「页内页」：限高 + 内部滚动，不撑长聊天列。
    body.classList.toggle('scroll', countLines(reasonBuffer) > 10);
    // 限高后必须让「页内页」自己贴底，否则新内容都在视口下方，看起来像卡住不动。
    if (!reasonPinned) body.scrollTop = body.scrollHeight;
    scrollBottom();
  }
  // 统计字符串行数（按 \n；最后一行无换行也算 1 行）。
  function countLines(s) {
    if (!s) return 0;
    let n = 1;
    for (let i = 0; i < s.length; i++) if (s[i] === '\n') n++;
    return n;
  }
  // 思考阶段结束（正文或工具调用开始输出）后折叠思考过程。
  // 只折叠一次：折叠后 reasonEl 置空，后续调用为 no-op，因此不会覆盖用户手动展开的结果。
  function foldReason() {
    if (reasonEl) {
      reasonEl.open = false;
      reasonEl = null;
    }
    reasonBuffer = '';
    reasonPinned = false;
  }
  function closeText() {
    currentTextEl = null;
    textBuffer = '';
  }
  function appendText(delta) {
    textBuffer += delta;
    lastReply += delta;
    if (!currentTextEl) {
      currentTextEl = document.createElement('div');
      currentTextEl.className = 'msg-assistant md';
      ensureCol().appendChild(currentTextEl);
    }
    currentTextEl.innerHTML = renderMD(textBuffer); // markdown 实时渲染
    scrollBottom();
  }

  // ---------- 项目 / 会话侧栏（SQLite 持久化）----------
  // 术语统一：左栏分组叫「项目」（键 = 会话的 workspace），分组内的条目叫「会话」。
  const sessionListEl = document.getElementById('session-list');
  let sessionsCache = []; // 最近一次 sessions 事件/列表（全部项目）

  // 项目名显示：优先用**项目自定义显示名**（workspace_names 表，随 /api/sessions 的
  // workspace_name 下发）；没设过则取路径最后一段；空串（未选择项目）显示「新项目」。
  // 注意：显示名只是标签，真实工作区键（ws）永远不变。
  function wsDisplayName(ws, name) {
    if (name) return name;
    if (!ws) return '新项目';
    const parts = String(ws).replace(/[\\/]+$/, '').split(/[\\/]/);
    return parts[parts.length - 1] || ws;
  }
  // 从最近一次会话列表里取某项目的显示名（没取到返回空串，交给 wsDisplayName 回落）
  function wsNameOf(ws) {
    const k = ws || '';
    const hit = sessionsCache.find(function (s) { return (s.workspace || '') === k; });
    return (hit && hit.workspace_name) || '';
  }

  // 通用内联菜单（三点按钮旁弹出，无浏览器弹窗）；点击外部自动关闭
  let openMenu = null;
  function closeInlineMenu() {
    if (openMenu) { openMenu.remove(); openMenu = null; }
  }
  document.addEventListener('mousedown', function (e) {
    if (openMenu && !openMenu.contains(e.target)) closeInlineMenu();
  });
  // anchor: 触发元素（按钮/行）；items: [{label, danger, fn}]
  function showInlineMenu(anchor, items) {
    closeInlineMenu();
    const menu = document.createElement('div');
    menu.className = 'inline-menu';
    items.forEach(function (it) {
      const b = document.createElement('button');
      b.type = 'button';
      b.textContent = it.label;
      if (it.danger) b.className = 'danger';
      b.addEventListener('click', function () {
        closeInlineMenu();
        it.fn();
      });
      menu.appendChild(b);
    });
    anchor.closest('#app, body').appendChild(menu);
    // 定位到锚点右上角
    const r = anchor.getBoundingClientRect();
    menu.style.left = Math.max(6, r.right - menu.offsetWidth) + 'px';
    menu.style.top = (r.bottom + 4) + 'px';
    openMenu = menu;
  }

  // SVG 图标工厂（三点菜单等，替代字符 ⋯ 避免字体渲染不一致）
  function svgIcon(d, size) {
    const ns = 'http://www.w3.org/2000/svg';
    const svg = document.createElementNS(ns, 'svg');
    svg.setAttribute('viewBox', '0 0 24 24');
    svg.setAttribute('width', size || 14);
    svg.setAttribute('height', size || 14);
    svg.setAttribute('fill', 'currentColor');
    const path = document.createElementNS(ns, 'path');
    path.setAttribute('d', d);
    svg.appendChild(path);
    return svg;
  }
  const ICON_DOTS = 'M5 12h.01M12 12h.01M19 12h.01'; // 三点（stroke 版本见调用处）

  // 渲染侧栏：项目 = 按工作区分组；项目行 = 折叠箭头 + 项目名 + ＋（新建会话）+ ⋯（项目操作）
  const wsFolded = {}; // workspace → 是否折叠（内存态，刷新重置）
  function renderSessions(items) {
    sessionsCache = items || [];
    sessionListEl.innerHTML = '';
    if (!sessionsCache.length) {
      sessionListEl.innerHTML = '<li class="session-empty" style="cursor:default">暂无项目</li>';
      return;
    }
    // 分组（保持列表本身的更新时间倒序 → 组间按组内最新排序）
    const groups = new Map();
    sessionsCache.forEach(function (s) {
      const k = s.workspace || '';
      if (!groups.has(k)) groups.set(k, []);
      groups.get(k).push(s);
    });
    groups.forEach(function (list, ws) {
      const group = document.createElement('li');
      group.className = 'ws-group';
      const head = document.createElement('div');
      head.className = 'ws-group-head';
      // 显示名取组内任一条会话带下来的 workspace_name（同组必然一致）
      const groupName = wsDisplayName(ws, list[0] && list[0].workspace_name);

      // 折叠箭头（点击整行名称也可折叠）
      const fold = document.createElement('button');
      fold.type = 'button';
      fold.className = 'label-btn fold-btn';
      fold.title = '折叠/展开';
      const arrow = svgIcon('M6 9l6 6 6-6', 12);
      arrow.setAttribute('stroke', 'currentColor');
      arrow.setAttribute('fill', 'none');
      arrow.setAttribute('stroke-width', '2');
      arrow.setAttribute('stroke-linecap', 'round');
      arrow.setAttribute('stroke-linejoin', 'round');
      fold.appendChild(arrow);
      const folded = !!wsFolded[ws];
      if (folded) fold.classList.add('folded');

      const name = document.createElement('span');
      name.className = 'ws-name';
      name.textContent = groupName;
      name.title = ws || '新项目'; // 悬浮看真实工作区路径

      function toggleFold() {
        wsFolded[ws] = !wsFolded[ws];
        renderSessions(sessionsCache); // 重渲染保持状态
      }
      fold.addEventListener('click', function (e) { e.stopPropagation(); toggleFold(); });
      name.addEventListener('click', toggleFold);

      // ＋：给该项目新建会话（切换项目上下文后开新会话）
      const add = document.createElement('button');
      add.type = 'button';
      add.className = 'label-btn';
      add.title = '在此项目新建会话';
      add.textContent = '＋';
      add.addEventListener('click', function (e) {
        e.stopPropagation();
        newSessionInWorkspace(ws);
      });

      // 三点：重命名 / 归档 / 删除整个项目
      const dots = document.createElement('button');
      dots.type = 'button';
      dots.className = 'dots-btn';
      dots.title = '项目操作';
      const dotsvg = svgIcon(ICON_DOTS, 14);
      dotsvg.setAttribute('stroke', 'currentColor');
      dotsvg.setAttribute('fill', 'none');
      dotsvg.setAttribute('stroke-width', '2.4');
      dotsvg.setAttribute('stroke-linecap', 'round');
      dots.appendChild(dotsvg);
      dots.addEventListener('click', function (e) {
        e.stopPropagation();
        const items = [
          { label: '重命名', fn: function () { renameWorkspaceInline(dots, ws, name, groupName); } },
        ];
        // 只有设过自定义显示名才给「恢复默认名」：清掉 workspace_names 里的记录，
        // 回落按工作区路径末段显示（后端 new_name 传空串即清除）。
        if (list[0] && list[0].workspace_name) {
          items.push({ label: '恢复默认名', fn: function () { clearWorkspaceName(ws); } });
        }
        items.push({ label: '归档', fn: function () { archiveWorkspace(ws); } });
        items.push({ label: '删除项目', danger: true, fn: function () {
          if (confirm('删除项目「' + groupName + '」及其全部会话？此操作不可恢复。')) {
            deleteWorkspace(ws);
          }
        } });
        showInlineMenu(dots, items);
      });

      head.appendChild(fold);
      head.appendChild(name);
      head.appendChild(add);    // ＋：在此项目新建会话
      head.appendChild(dots);   // ⋯：项目操作（重命名 / 归档 / 删除），收在最右
      group.appendChild(head);

      // 会话条目（组折叠时整组隐藏）
      if (!folded) {
        const ul = document.createElement('ul');
        ul.className = 'session-list';
        list.forEach(function (s) {
          const li = document.createElement('li');
          li.className = 'session-item'; // 三点定位/悬停规则只认这个类，别用 #session-list li
          li.dataset.id = s.id;
          if (s.id === sessionID) li.classList.add('active');
          li.textContent = s.title || '未命名会话';
          li.title = (s.title || '未命名会话') + '（' + (s.message_count || 0) + ' 条消息）';
          // 三点菜单（悬停浮现）：重命名 / 归档 / 删除
          const sdots = document.createElement('button');
          sdots.type = 'button';
          sdots.className = 'dots-btn';
          sdots.title = '会话操作';
          const sdsvg = svgIcon(ICON_DOTS, 13);
          sdsvg.setAttribute('stroke', 'currentColor');
          sdsvg.setAttribute('fill', 'none');
          sdsvg.setAttribute('stroke-width', '2.4');
          sdsvg.setAttribute('stroke-linecap', 'round');
          sdots.appendChild(sdsvg);
          sdots.addEventListener('click', function (e) {
            e.stopPropagation();
            showInlineMenu(sdots, [
              { label: '重命名', fn: function () { renameSessionInline(sdots, s); } },
              { label: '归档', fn: function () { archiveSession(s); } },
              { label: '永久删除', danger: true, fn: function () { deleteSession(s); } },
            ]);
          });
          li.appendChild(sdots);
          if (running && s.id === runSessionID) addRunBadge(li); // 正在运行的会话：转圈提示
          li.addEventListener('click', function () {
            if (s.id === sessionID) return;
            loadSession(s.id); // 运行中也允许切换：后台继续跑，只切视图
          });
          ul.appendChild(li);
        });
        group.appendChild(ul);
      }
      sessionListEl.appendChild(group);
    });
  }

  // 会话列表运行标记：正在运行的会话左侧加转圈；随 busy/idle 与列表重绘同步。
  function addRunBadge(li) {
    if (!li || li.querySelector('.session-spin')) return;
    const sp = document.createElement('span');
    sp.className = 'session-spin';
    sp.title = '正在运行';
    li.insertBefore(sp, li.firstChild);
    li.classList.add('running');
  }
  function syncRunBadges() {
    sessionListEl.querySelectorAll('.session-item').forEach(function (li) {
      const on = running && !!runSessionID && li.dataset.id === runSessionID;
      if (on) {
        addRunBadge(li);
      } else {
        li.classList.remove('running');
        const sp = li.querySelector('.session-spin');
        if (sp) sp.remove();
      }
    });
  }

  // 在指定工作区新建会话：该工作区成为当前工作区（发消息时挂在它下面）
  function newSessionInWorkspace(ws) {
    if (running) return;
    if (ws && ws !== workspaceRoot) {
      // 先切换工作区，再新建会话（服务端 Create 挂当前工作区）。
      // ⚠️ 必须看 res.ok：目录已被删除时服务端会拒绝，若照样往下走，
      // 新会话会带着**没切换成功**的旧工作区落进上一个项目下（静默错位）。
      setWorkspace(ws).then(function (res) {
        if (!res.ok) {
          addError(res.error || ('无法切换到该项目的目录：' + ws));
          loadSessionList2(); // workspaceRoot 回滚成服务端真实状态
          return;
        }
        doNewSession();
      });
    } else {
      doNewSession();
    }
  }
  function doNewSession() {
    wsSend({ type: 'new_session', title: '' });
    messagesEl.innerHTML = '';
    msgCol = null; currentTextEl = null; textBuffer = '';
    reasonEl = null; reasonBuffer = ''; lastReply = ''; lastUserText = '';
    sessionID = ''; // 等服务端 session 事件回填
    syncComposerMode(); // 新会话为空：输入卡片回到居中
  }

  // 侧栏「新建项目」：先弹窗选好 工作区 / 项目名 / 默认权限，确认后才真正创建。
  // （不再一键直建 —— 直接点一下就建好会跳过所有项目级设置。）
  document.getElementById('new-chat-btn').addEventListener('click', function () {
    if (running) return;
    openNewProjectDialog();
  });

  // ---------- 新建项目弹窗 ----------
  let npOverlay = null;   // 弹窗 DOM（只建一次，复用）
  let npWs = '';          // 已选工作区（'' = 不挂目录的临时项目）
  let npPerm = 'ask';     // 弹窗里选中的默认权限
  let npPermInit = 'ask'; // 打开弹窗时服务端的权限值（没改就不提交）
  let npPermTouched = false; // 用户是否已手动选过权限（回显晚到时不覆盖）
  function npEl(id) { return npOverlay.querySelector('#' + id); }

  function openNewProjectDialog() {
    if (!npOverlay) buildNewProjectDialog();
    // 每次打开都复位：不挂目录 + 空项目名；权限回显服务端当前值（打开时拉取）
    npWs = '';
    npPermTouched = false;
    npEl('np-name').value = '';
    npEl('np-create').disabled = false;
    setNpError('');
    renderNpWs();
    fetch('/api/perm').then(function (r) { return r.json(); })
      .then(function (d) { npPermInit = d.mode || 'ask'; })
      .catch(function () { npPermInit = 'ask'; })
      .then(function () {
        if (!npPermTouched) { npPerm = npPermInit; renderNpPerm(); }
      });
    npOverlay.classList.remove('leaving', 'hidden');
    setTimeout(function () { npEl('np-name').focus(); }, 60);
  }

  function buildNewProjectDialog() {
    npOverlay = document.createElement('div');
    npOverlay.id = 'newproj-overlay';
    npOverlay.className = 'overlay hidden';
    npOverlay.innerHTML =
      '<div class="modal newproj-modal">' +
      '<h2>新建项目</h2>' +
      '<label class="np-field"><span class="np-label">项目名</span>' +
      '<input id="np-name" type="text" placeholder="默认取文件夹名，未选目录则为「新项目」" maxlength="60"></label>' +
      '<div class="np-field"><span class="np-label">工作区</span>' +
      '<div class="np-ws-row">' +
      '<span id="np-ws-path"></span>' +
      '<button type="button" id="np-ws-pick" class="np-btn">选择文件夹…</button>' +
      '<button type="button" id="np-ws-clear" class="np-btn" hidden>清除</button>' +
      '</div></div>' +
      '<div class="np-field"><span class="np-label">默认权限</span>' +
      '<div class="np-seg" id="np-perm-seg">' +
      '<button type="button" data-perm="readonly">只读</button>' +
      '<button type="button" data-perm="ask">请求</button>' +
      '<button type="button" data-perm="auto">自主</button>' +
      '</div></div>' +
      '<div class="modal-actions">' +
      '<span id="np-error" class="np-error"></span>' +
      '<button type="button" id="np-cancel" class="np-btn">取消</button>' +
      '<button type="button" id="np-create" class="np-btn primary">创建</button>' +
      '</div></div>';
    document.body.appendChild(npOverlay);

    // 选工作区：与左上角工作区标签同一套入口（Windows 系统对话框 / 其他平台内置选择器），
    // 区别是这里只把路径记在弹窗里，确认创建时才真正切换。
    npEl('np-ws-pick').addEventListener('click', function () {
      fetch('/api/pick_folder', { method: 'POST' })
        .then(function (r) { return r.json().then(function (d) { return { status: r.status, data: d }; }); })
        .then(function (res) {
          if (res.status !== 200) { openBuiltinPicker('', 'dir', setNpWs); return; }
          if (res.data.builtin) {
            openBuiltinPicker(res.data.start_path || '', 'dir', setNpWs);
          } else if (res.data.ok && res.data.path) {
            setNpWs(res.data.path); // Windows 资源管理器对话框选中
          }
          // res.data.ok === false：用户在系统对话框点了取消，无需任何动作
        })
        .catch(function () { openBuiltinPicker('', 'dir', setNpWs); });
    });
    npEl('np-ws-clear').addEventListener('click', function () { setNpWs(''); });
    npEl('np-perm-seg').querySelectorAll('button').forEach(function (b) {
      b.addEventListener('click', function () { npPermTouched = true; npPerm = b.dataset.perm; renderNpPerm(); });
    });
    npEl('np-cancel').addEventListener('click', function () { hideWithAnim(npOverlay); });
    npEl('np-create').addEventListener('click', confirmNewProject);
    // 点遮罩关闭 / 弹窗内 Esc 关闭 / 项目名里 Enter 直接创建
    npOverlay.addEventListener('mousedown', function (e) { if (e.target === npOverlay) hideWithAnim(npOverlay); });
    npOverlay.addEventListener('keydown', function (e) {
      if (e.key === 'Escape') hideWithAnim(npOverlay);
    });
    npEl('np-name').addEventListener('keydown', function (e) {
      if (e.key === 'Enter') { e.preventDefault(); confirmNewProject(); }
    });
  }

  function setNpWs(p) {
    npWs = p || '';
    setNpError('');
    renderNpWs();
  }
  function renderNpWs() {
    const el = npEl('np-ws-path');
    if (npWs) {
      el.textContent = npWs;
      el.title = npWs;
      el.classList.remove('empty');
    } else {
      el.textContent = '未选择（不挂目录的临时项目）';
      el.title = '';
      el.classList.add('empty');
    }
    npEl('np-ws-clear').hidden = !npWs;
  }
  function renderNpPerm() {
    npEl('np-perm-seg').querySelectorAll('button').forEach(function (b) {
      b.classList.toggle('active', b.dataset.perm === npPerm);
    });
  }
  function setNpError(msg) { npEl('np-error').textContent = msg || ''; }

  function confirmNewProject() {
    if (npEl('np-create').disabled) return;
    const name = npEl('np-name').value.trim();
    npEl('np-create').disabled = true;
    setNpError('');
    // ⚠️ 先清 sessionID 再切工作区：setWorkspace 会把「无工作区的当前会话」挂到新工作区下
    //（见其内部逻辑），旧会话会被错误吸进新项目组。失败时再回滚，会话区保持原样。
    const prevSession = sessionID;
    sessionID = '';
    setWorkspace(npWs).then(function (res) {
      if (!res.ok) {
        sessionID = prevSession; // 切换失败：只在弹窗里报错，不破坏当前对话
        npEl('np-create').disabled = false;
        setNpError(res.error || '切换工作区失败');
        loadSessionList2(); // workspaceRoot 回滚成服务端真实状态
        return;
      }
      const jobs = [];
      // 项目显示名（可选）：写入 workspace_names 表，侧栏分组即显示该名
      if (name) {
        jobs.push(fetch('/api/workspaces', {
          method: 'PATCH', headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ workspace: npWs, new_name: name })
        }).catch(function () {}));
      }
      // 默认权限：只在用户改了打开时的值才提交（服务端权威，改完同步输入区的权限按钮）
      if (npPerm !== npPermInit) {
        jobs.push(fetch('/api/perm', {
          method: 'POST', headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ mode: npPerm })
        }).then(function () {
          if (window.CodeForgePerm) window.CodeForgePerm.refresh();
        }).catch(function () {}));
      }
      Promise.all(jobs).then(function () {
        doNewSession(); // 清空对话区 + WS new_session（挂在刚切好的工作区下；sessions 事件会带出新分组名）
        hideWithAnim(npOverlay);
      });
    }).catch(function () { // 网络断开等：fetch 本身失败
      sessionID = prevSession;
      npEl('np-create').disabled = false;
      setNpError('无法连接服务端，请重试');
      loadSessionList2();
    });
  }

  // ---------- Termux 工具安装建议弹窗（安卓平台） ----------
  // 服务端在 GET /api/termux/tools 报告「Termux 且未安装 termux-tools」时弹出：
  // 点「安装」→ 后台 pkg install → 按钮变「安装中…」→ 轮询到装好变「已安装 ✓」。
  // 左上角「跳过」只关本次；右上角「不再提示」写 localStorage 永久关闭。
  let thOverlay = null;       // 弹窗 DOM（只建一次）
  let thTimer = null;         // 安装中轮询定时器（关弹窗时必须清掉）
  let thInstalling = false;   // 是否处于安装中（重开弹窗时恢复轮询）
  const TH_NEVER_KEY = 'cf_termux_tools_hint'; // localStorage：不再提示

  function maybeShowTermuxHint() {
    fetch('/api/termux/tools').then(function (r) { return r.json(); })
      .then(function (d) {
        if (!d || !d.termux || d.installed || d.installing) return;
        if (localStorage.getItem(TH_NEVER_KEY) === 'never') return;
        openTermuxHint();
      })
      .catch(function () {}); // 探测失败静默：弹窗只是建议，不该打扰
  }

  function thEl(id) { return thOverlay.querySelector('#' + id); }

  function openTermuxHint() {
    if (!thOverlay) buildTermuxHint();
    thEl('th-error').textContent = '';
    renderThInstall('安装');
    thOverlay.classList.remove('leaving', 'hidden');
    if (thInstalling) startThPolling(); // 关弹窗期间装完了/装挂了：重开时立即同步
  }

  function buildTermuxHint() {
    thOverlay = document.createElement('div');
    thOverlay.id = 'termux-overlay';
    thOverlay.className = 'overlay hidden';
    thOverlay.innerHTML =
      '<div class="modal termux-modal">' +
      '<div class="th-top">' +
      '<button type="button" id="th-skip" class="th-link">跳过</button>' +
      '<button type="button" id="th-never" class="th-link">不再提示</button>' +
      '</div>' +
      '<h2>建议安装工具</h2>' +
      '<div class="th-body">为了更好地使用 CodeForge，建议您安装以下工具：<b>termux-tools</b>' +
      '<div class="th-desc">提供 termux-open-url（自动打开浏览器）与 termux-setup-storage（授权访问手机存储）。' +
      '安装完成后会自动运行一次存储授权，请在弹出的系统对话框中允许；安装过程在后台进行，不影响当前使用。</div></div>' +
      '<div class="modal-actions">' +
      '<span id="th-error" class="np-error"></span>' +
      '<button type="button" id="th-install" class="np-btn primary">安装</button>' +
      '</div></div>';
    document.body.appendChild(thOverlay);

    thEl('th-skip').addEventListener('click', function () { closeTermuxHint(); });
    thEl('th-never').addEventListener('click', function () {
      localStorage.setItem(TH_NEVER_KEY, 'never');
      closeTermuxHint();
    });
    thEl('th-install').addEventListener('click', requestTermuxInstall);
    // 点遮罩 = 跳过（本次不再打扰），Esc 同效
    thOverlay.addEventListener('mousedown', function (e) { if (e.target === thOverlay) closeTermuxHint(); });
    thOverlay.addEventListener('keydown', function (e) { if (e.key === 'Escape') closeTermuxHint(); });
  }

  function renderThInstall(text, disabled) {
    const btn = thEl('th-install');
    btn.textContent = text;
    btn.disabled = !!disabled;
  }

  function requestTermuxInstall() {
    thEl('th-error').textContent = '';
    renderThInstall('安装中…', true);
    fetch('/api/termux/tools', { method: 'POST' })
      .then(function (r) { return r.json().catch(function () { return {}; }).then(function (d) { return { ok: r.ok, data: d }; }); })
      .then(function (res) {
        if (!res.ok) {
          thFail(res.data.error || '无法启动安装');
          return;
        }
        if (res.data.installed) { thDone(); return; }
        thInstalling = true;
        startThPolling();
      })
      .catch(function () { thFail('无法连接服务端，请重试'); });
  }

  // 轮询安装状态：装好 →「已安装 ✓」并自动关窗；失败 → 显示错误并允许重试。
  // 服务端兜底超时 10 分钟，这里 15 分钟不再继续（按失败收场）。
  function startThPolling() {
    if (thTimer) return;
    const started = Date.now();
    thTimer = setInterval(function () {
      fetch('/api/termux/tools').then(function (r) { return r.json(); })
        .then(function (d) {
          if (d.installed) { thDone(); return; }
          if (!d.installing || Date.now() - started > 15 * 60 * 1000) {
            thFail(d.error || '安装失败，请稍后重试');
          }
        })
        .catch(function () {}); // 单次轮询失败不打断，下个周期再试
    }, 2000);
  }

  function stopThPolling() {
    if (thTimer) { clearInterval(thTimer); thTimer = null; }
  }

  function thDone() {
    thInstalling = false;
    stopThPolling();
    renderThInstall('已安装 ✓', true);
    setTimeout(closeTermuxHint, 1200); // 让用户看到结果再自动收起
  }

  function thFail(msg) {
    thInstalling = false;
    stopThPolling();
    renderThInstall('重试安装');
    thEl('th-error').textContent = msg;
  }

  function closeTermuxHint() {
    stopThPolling();
    hideWithAnim(thOverlay);
  }

  // 页面就绪后稍作延迟再探测：避免与首屏渲染 / 会话回放抢资源
  setTimeout(maybeShowTermuxHint, 1200);

  // 内联重命名输入（无浏览器 prompt）：原位替换为输入框 + ✓
  function attachInlineRename(anchor, current, apply) {
    closeInlineMenu();
    const parent = anchor.parentElement;
    const input = document.createElement('input');
    input.type = 'text';
    input.value = current;
    input.style.cssText = 'flex:1;min-width:0;font-size:12.5px;font-family:inherit;' +
      'border:1px solid var(--accent);border-radius:6px;padding:3px 6px;background:var(--bg);color:var(--text)';
    const ok = document.createElement('button');
    ok.type = 'button';
    ok.className = 'label-btn';
    ok.textContent = '✓';
    ok.title = '确认';
    parent.textContent = '';
    parent.appendChild(input);
    parent.appendChild(ok);
    input.focus();
    input.select();
    function done() {
      const v = input.value.trim();
      apply(v || current);
    }
    ok.addEventListener('click', done);
    input.addEventListener('keydown', function (e) {
      if (e.key === 'Enter') done();
      if (e.key === 'Escape') renderSessions(sessionsCache);
    });
  }
  function renameSessionInline(anchor, s) {
    attachInlineRename(anchor, s.title || '', function (v) {
      fetch('/api/sessions', {
        method: 'PATCH', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ id: s.id, title: v })
      }).then(function () { loadSessionList(); });
    });
  }
  function renameWorkspaceInline(anchor, ws, nameEl, currentName) {
    closeInlineMenu();
    // 只把项目名替换为输入框，不清空组头（保留箭头/三点/加号）。
    // 预填当前**显示名**：重命名只改显示名，改回原样也不会有副作用。
    const input = document.createElement('input');
    input.type = 'text';
    input.value = currentName || wsDisplayName(ws);
    input.style.cssText = 'flex:1;min-width:0;font-size:12px;font-family:inherit;' +
      'border:1px solid var(--accent);border-radius:6px;padding:2px 5px;background:var(--bg);color:var(--text)';
    const done = function () {
      const v = input.value.trim();
      if (!v) { renderSessions(sessionsCache); return; }
      fetch('/api/workspaces', {
        method: 'PATCH', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ workspace: ws, new_name: v })
      }).then(function (r) { return r.json(); }).then(function () {
        loadSessionList();
        loadSessionList2(); // 刷新工作区标签（若当前工作区被改）
      });
    };
    nameEl.textContent = '';
    nameEl.appendChild(input);
    input.focus();
    input.select();
    input.addEventListener('keydown', function (e) {
      if (e.key === 'Enter') done();
      if (e.key === 'Escape') renderSessions(sessionsCache);
    });
    input.addEventListener('blur', done);
  }
  // 清除项目自定义显示名 → 回落按工作区路径末段显示（new_name 传空串 = 清除）
  function clearWorkspaceName(ws) {
    fetch('/api/workspaces', {
      method: 'PATCH', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ workspace: ws, new_name: '' })
    }).then(function () { loadSessionList(); });
  }
  function archiveSession(s) {
    fetch('/api/sessions', {
      method: 'PATCH', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ id: s.id, archived: true })
    }).then(function () {
      if (s.id === sessionID) { // 归档的是当前会话：回到空态
        sessionID = '';
        messagesEl.innerHTML = '';
        msgCol = null; currentTextEl = null; textBuffer = '';
        reasonEl = null; reasonBuffer = ''; lastReply = '';
        syncComposerMode(); // 回到空态：输入卡片回到居中
      }
      loadSessionList();
    });
  }
  function archiveWorkspace(ws) {
    fetch('/api/workspaces', {
      method: 'PATCH', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ workspace: ws, archive: true })
    }).then(function () {
      if ((ws || '') === (workspaceRoot || '')) { // 当前工作区整组归档：清空
        sessionID = '';
        messagesEl.innerHTML = '';
        msgCol = null; currentTextEl = null; textBuffer = '';
        reasonEl = null; reasonBuffer = ''; lastReply = '';
        syncComposerMode(); // 回到空态：输入卡片回到居中
      }
      loadSessionList();
    });
  }
  function deleteSession(s) {
    fetch('/api/sessions?id=' + encodeURIComponent(s.id), { method: 'DELETE' })
      .then(function () {
        if (s.id === sessionID) {
          sessionID = '';
          messagesEl.innerHTML = '';
          msgCol = null; currentTextEl = null; textBuffer = '';
          reasonEl = null; reasonBuffer = ''; lastReply = '';
          syncComposerMode(); // 回到空态：输入卡片回到居中
        }
        loadSessionList();
      });
  }
  // 删除整个项目（含其全部会话）；若删除的是当前运行项目，清空对话区回到未选择项目
  function deleteWorkspace(ws) {
    fetch('/api/workspaces?workspace=' + encodeURIComponent(ws || ''), { method: 'DELETE' })
      .then(function () {
        if ((ws || '') === (workspaceRoot || '')) {
          workspaceRoot = '';
          sessionID = '';
          messagesEl.innerHTML = '';
          msgCol = null; currentTextEl = null; textBuffer = '';
          reasonEl = null; reasonBuffer = ''; lastReply = '';
        }
        loadSessionList();
      });
  }
  // 当前工作区状态（侧栏分组 / 发消息挂靠都用它；入口只剩「新建项目」弹窗与项目组 ＋）
  let workspaceRoot = '';
  // 从服务端回读当前工作区（切换失败后把 workspaceRoot 回滚成真实状态）
  function loadSessionList2() {
    fetch('/api/workspace').then(function (r) { return r.json(); })
      .then(function (d) {
        workspaceRoot = d.root || '';
      });
  }

  // 主动拉一次会话列表（REST 与 WS sessions 事件同构）
  function loadSessionList() {
    fetch('/api/sessions').then(function (r) { return r.json(); })
      .then(function (d) { renderSessions(d.items || []); });
  }

  // 切换会话：请求服务端回放历史。
  // 运行中也允许切换：任务继续在后台跑，只换视图；未落盘片段由 runReason/runText
  // 跨视图保留，replayHistory 渲染完历史后自动补上。
  function loadSession(id) {
    if (!id || id === sessionID) return;
    if (running) sendBtn.title = '点击打断' + (id === runSessionID ? '' : '（正在运行的会话）');
    wsSend({ type: 'load_session', session_id: id });
  }

  // 回放历史消息：服务端 history 事件 → 用渲染原语重建聊天列。
  // 审批条与 spinner 不重建（历史是既成事实）；末条助手回复带操作栏。
  function replayHistory(ev) {
    messagesEl.innerHTML = '';
    msgCol = null; currentTextEl = null; textBuffer = '';
    reasonEl = null; reasonBuffer = ''; reasonPinned = false;
    // 视图重建 = 旧 DOM 全部作废：这些「当前元素」引用必须一起清空，
    // 否则后续事件会去找已经不在文档里的节点（切走→切回最容易触发）。
    thinkingEl = null; activeToolEl = null; retryEl = null;
    subagentCards.clear();
    lastReply = ''; lastUserText = '';
    sessionID = ev.session_id || '';

    (ev.messages || []).forEach(function (m) {
      const blocks = m.content || [];
      blocks.forEach(function (b) {
        if (b.type === 'text') {
          if (m.role === 'user') {
            closeText();
            addUser(b.text || '');
            lastUserText = b.text || '';
          } else {
            appendText(b.text || ''); // 助手段落：markdown 渲染
          }
        } else if (b.type === 'tool_use') {
          settleActiveTool();
          closeText();
          addTool(toolLabel(b.name, b.input),
            b.input ? JSON.stringify(b.input, null, 2) : '');
        } else if (b.type === 'tool_result') {
          settleActiveTool(); // 工具已完成：无 spinner
        }
      });
    });
    settleActiveTool();
    closeText();
    // 切回「正在后台运行」的会话：补上已流出但尚未落盘的思考/正文片段。
    // 运行中不挂操作栏（与正常流式输出期间一致，idle 时再出现）。
    const runningHere = running && !!runSessionID && ev.session_id === runSessionID;
    if (runningHere) {
      if (runReason) appendReason(runReason);
      if (runText) appendText(runText);
    }
    if (lastReply && !runningHere) addActions(lastReply);
    scrollBottom();
    syncComposerMode(); // 空会话回放 → 居中；有历史 → 下放底部
    renderSessions(sessionsCache); // 高亮切换后的 active
  }

  // ---------- MCP 入口（仅查看；新增/启停/删除在 设置 → MCP 服务） ----------
  // 只展示 type=mcp 且已启用的插件（plugins.yaml 里的示例模板 enabled=false 不显示）。
  function renderMcpList() {
    const list = document.getElementById('mcp-list');
    list.innerHTML = '';
    fetch('/api/plugins').then(function (r) { return r.json(); }).then(function (d) {
      const items = (d.items || []).filter(function (p) {
        return p.type === 'mcp' && p.configured;
      });
      if (!items.length) {
        list.innerHTML = '<li class="mcp-empty" style="cursor:default">尚未添加，见 设置 → MCP 服务</li>';
        return;
      }
      items.forEach(function (p) {
        const li = document.createElement('li');
        li.className = p.enabled ? 'on' : 'off';
        const dot = document.createElement('span');
        dot.className = 'dot';
        dot.title = p.enabled ? '运行中'
          : '已启用，但进程未运行（加载失败：请到 设置 → MCP 服务 检查启动命令）';
        const nm = document.createElement('span');
        nm.textContent = p.name + (p.enabled ? '' : '（加载失败）');
        nm.title = (p.description || p.name) + (p.command ? '：' + p.command + ' ' + (p.args || []).join(' ') : '') +
          (p.enabled ? '（运行中）' : '（已启用但加载失败）');
        li.appendChild(dot);
        li.appendChild(nm);
        list.appendChild(li);
      });
    }).catch(function () {
      list.innerHTML = '<li class="mcp-empty" style="cursor:default">MCP 服务加载失败</li>';
    });
  }
  function togglePlugin(name, enabled) {
    fetch('/api/plugins', {
      method: 'PATCH', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ name: name, enabled: enabled })
    }).then(function (r) { return r.json(); }).then(function (d) {
      if (d.warning) addInfo(d.warning);
      renderMcpList();
      renderMcpSettings();
    });
  }
  function deletePlugin(name) {
    fetch('/api/plugins?name=' + encodeURIComponent(name), { method: 'DELETE' })
      .then(function (r) { return r.json(); })
      .then(function () { renderMcpList(); renderMcpSettings(); });
  }
  renderMcpList();

  // ---------- 设置 → MCP 服务：列表管理 + 新增表单 ----------
  function renderMcpSettings() {
    const box = document.getElementById('mcp-items');
    if (!box) return;
    box.innerHTML = '<div class="mi-empty">加载中…</div>';
    fetch('/api/plugins').then(function (r) { return r.json(); }).then(function (d) {
      const items = d.items || [];
      box.innerHTML = '';
      if (!items.length) {
        box.innerHTML = '<div class="mi-empty">还没有插件，用下方表单添加一个 MCP 服务</div>';
        return;
      }
      items.forEach(function (p) {
        const row = document.createElement('div');
        row.className = 'model-item';
        // 三态：enabled=运行中（配置启用且进程已加载）；configured-only=启用但加载失败；
        // 都不是=已停用。按钮跟随配置态（configured），保证启用后一定出现「停用」。
        const icon = p.configured ? '⚡ ' : '⏸ ';
        const main = document.createElement('div');
        main.className = 'mi-main';
        const nm = document.createElement('div');
        nm.className = 'mi-name';
        nm.textContent = icon + p.name + '　' + p.type;
        const meta = document.createElement('div');
        meta.className = 'mi-id';
        const detail = p.command
          ? p.command + ' ' + (p.args || []).join(' ')
          : (p.endpoint || p.path || '');
        meta.textContent = detail + (p.description ? ' — ' + p.description : '');
        if (p.configured && !p.enabled) {
          meta.textContent += '　⚠ 已启用但进程未运行：常见原因为启动命令/参数错误或依赖缺失，可停用后修正再启用';
        }
        main.appendChild(nm);
        main.appendChild(meta);
        const badge = document.createElement('span');
        badge.className = 'mi-badge' + (p.enabled ? ' ok' : (p.configured ? ' err' : ''));
        badge.textContent = p.enabled ? '运行中' : (p.configured ? '加载失败' : '已停用');
        const acts = document.createElement('div');
        acts.className = 'mi-actions';
        function mkBtn(text, cls, fn) {
          const b = document.createElement('button');
          b.type = 'button'; b.textContent = text;
          if (cls) b.className = cls;
          b.addEventListener('click', fn);
          return b;
        }
        acts.appendChild(mkBtn(p.configured ? '停用' : '启用', 'apply', function () {
          togglePlugin(p.name, !p.configured);
        }));
        acts.appendChild(mkBtn('删除', '', function () {
          deletePlugin(p.name);
        }));
        row.appendChild(main); row.appendChild(badge); row.appendChild(acts);
        box.appendChild(row);
      });
    }).catch(function () {
      box.innerHTML = '<div class="mi-empty">插件列表加载失败，请确认服务正在运行</div>';
    });
  }
  // 新增表单
  document.getElementById('mcp-f-add').addEventListener('click', function () {
    const result = document.getElementById('mcp-f-result');
    const name = document.getElementById('mcp-f-name').value.trim();
    const cmd = document.getElementById('mcp-f-cmd').value.trim();
    const args = document.getElementById('mcp-f-args').value.trim();
    const envRaw = document.getElementById('mcp-f-env').value.trim();
    const desc = document.getElementById('mcp-f-desc').value.trim();
    if (!name || !cmd) { result.className = 'mf-test-result fail'; result.textContent = '名称和启动命令不能为空'; return; }
    const env = {};
    envRaw.split(/\s+/).forEach(function (kv) {
      const i = kv.indexOf('=');
      if (i > 0) env[kv.slice(0, i)] = kv.slice(i + 1);
    });
    result.className = 'mf-test-result';
    result.textContent = '添加中…';
    fetch('/api/plugins', {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ name: name, command: cmd, args: args ? args.split(/\s+/) : [], description: desc || 'MCP 服务', env: env })
    }).then(function (r) { return r.json(); }).then(function (d) {
      if (d.ok) {
        document.getElementById('mcp-f-name').value = '';
        document.getElementById('mcp-f-cmd').value = '';
        document.getElementById('mcp-f-args').value = '';
        document.getElementById('mcp-f-env').value = '';
        document.getElementById('mcp-f-desc').value = '';
        result.className = 'mf-test-result ok';
        result.textContent = d.warning || '已添加并热加载';
        renderMcpSettings();
        renderMcpList();
      } else {
        result.className = 'mf-test-result fail';
        result.textContent = d.error || '添加失败';
      }
    });
  });
  // 进入设置 MCP 页时刷新（绑定放在 settingsOverlay 声明之后，见下方设置区）

  // ---------- 左侧对话导航条（贴分栏拖动条，以它为中心竖向扩展） ----------
  // 每轮对话一条：悬停（桌面）/按住（触屏）拉长 + 右侧浮出预览卡
  // （用户问题加粗 + AI 回复前 3 行 + …）；整体在主对话框上下居中。
  const mmRail = document.createElement('div');
  mmRail.id = 'mm-rail';
  messagesEl.parentElement.appendChild(mmRail); // 挂 #main（position:relative）
  let mmRounds = [];        // [{el(条), userEl, replyText}]
  let mmRebuildTimer = null;
  let mmAnim = null;

  // 从消息列收集「一轮对话」：一条用户消息 + 其后所有助手文本（拼接）
  function mmCollectRounds() {
    const rounds = [];
    let cur = null;
    messagesEl.querySelectorAll('.msg-col > *').forEach(function (el) {
      if (el.classList.contains('msg-user')) {
        cur = { userEl: el, replyText: '' };
        rounds.push(cur);
      } else if (cur && (el.classList.contains('msg-assistant') || el.classList.contains('md'))) {
        cur.replyText += (cur.replyText ? '\n' : '') + (el.textContent || '').trim();
      }
    });
    return rounds;
  }

  // 重建导航条（内容变化后防抖调用）
  function mmRebuild() {
    mmRail.innerHTML = '';
    mmRounds = [];
    const rounds = mmCollectRounds();
    if (!rounds.length) { mmRail.style.display = 'none'; return; }
    mmRail.style.display = '';

    // 紧凑排列：所有横条以轨道垂直中点为基准、均匀间距堆在一起（不按滚动
    // 位置分散），从中心向上/向下扩展；点击仍跳到对应问题。
    const railH = mmRail.clientHeight || 1;
    const n = rounds.length;
    const step = 10; // 条间垂直间距
    const blockH = n * step;
    const top0 = Math.max(0, (railH - blockH) / 2); // 整组上下居中
    rounds.forEach(function (r, i) {
      const bar = document.createElement('div');
      bar.className = 'mm-bar';
      bar.style.top = (top0 + i * step) + 'px';

      // 预览卡：悬停/按住时右侧展开
      const card = document.createElement('div');
      card.className = 'mm-card';
      const q = document.createElement('div');
      q.className = 'mm-q';
      q.textContent = (r.userEl.textContent || '（无文字）').slice(0, 120);
      const a = document.createElement('div');
      a.className = 'mm-a';
      const lines = r.replyText.split('\n').filter(Boolean).slice(0, 3);
      a.textContent = lines.join('\n') + (lines.length ? '\n…' : '');
      if (!lines.length) a.textContent = '（AI 暂未回复）';
      card.appendChild(q);
      card.appendChild(a);
      bar.appendChild(card);

      // 交互：桌面 hover、触屏按住 → 拉长 + 显示预览卡；点击 → 平滑滚动
      bar.addEventListener('pointerenter', function () { bar.classList.add('open'); });
      bar.addEventListener('pointerleave', function () { bar.classList.remove('open'); });
      bar.addEventListener('pointerdown', function (e) {
        bar.classList.add('open'); // 安卓按住
        bar.setPointerCapture(e.pointerId);
        function up() {
          bar.classList.remove('open');
          bar.removeEventListener('pointerup', up);
          bar.removeEventListener('pointercancel', up);
        }
        bar.addEventListener('pointerup', up);
        bar.addEventListener('pointercancel', up);
      });
      bar.addEventListener('click', function (e) {
        e.stopPropagation();
        mmScrollTo(r.userEl);
      });
      mmRail.appendChild(bar);
      mmRounds.push({ el: bar, userEl: r.userEl });
    });
  }

  // 轨道定位：贴分栏拖动条，整体垂直居中于对话面板
  function mmSyncSize() {
    const mRect = messagesEl.getBoundingClientRect();
    const mainRect = messagesEl.parentElement.getBoundingClientRect();
    // 以分栏拖动条为中心：轨道高度取消息区 60%（上下居中），定位在其右 8px
    mmRail.style.left = '2px';
    mmRail.style.top = (mRect.top - mainRect.top + mRect.height * 0.2) + 'px';
    mmRail.style.height = Math.max(120, mRect.height * 0.6) + 'px';
  }

  // 平滑滚动到目标（easeInOutCubic，时长随距离自适应）
  function mmScrollTo(el) {
    const mTop = messagesEl.getBoundingClientRect().top;
    const target = el.getBoundingClientRect().top - mTop + messagesEl.scrollTop - 14;
    mmAnimateScroll(Math.max(0, Math.min(target, messagesEl.scrollHeight - messagesEl.clientHeight)));
  }
  function mmAnimateScroll(to) {
    if (mmAnim) cancelAnimationFrame(mmAnim);
    const from = messagesEl.scrollTop;
    const delta = to - from;
    if (Math.abs(delta) < 2) return;
    const dur = Math.min(650, 260 + Math.abs(delta) / 5);
    const t0 = performance.now();
    function ease(t) { return t < .5 ? 4 * t * t * t : 1 - Math.pow(-2 * t + 2, 3) / 2; }
    function step(now) {
      const p = Math.min(1, (now - t0) / dur);
      messagesEl.scrollTop = from + delta * ease(p);
      if (p < 1) mmAnim = requestAnimationFrame(step); else mmAnim = null;
    }
    mmAnim = requestAnimationFrame(step);
  }

  // 滚动高亮当前视口内的条
  function mmUpdateView() {
    if (!mmRounds.length) return;
    const mTop = messagesEl.getBoundingClientRect().top;
    const ch = messagesEl.clientHeight;
    let activeSet = false;
    mmRounds.forEach(function (r) {
      const top = r.userEl.getBoundingClientRect().top;
      const vis = !activeSet && top >= mTop - 4 && top < mTop + ch * 0.5;
      r.el.classList.toggle('active', vis);
      if (vis) activeSet = true;
    });
  }

  messagesEl.addEventListener('scroll', mmUpdateView, { passive: true });
  const mmScheduleRebuild = function () {
    if (mmRebuildTimer) return;
    mmRebuildTimer = setTimeout(function () {
      mmRebuildTimer = null;
      mmSyncSize();
      mmRebuild();
      mmUpdateView();
    }, 250);
  };
  new MutationObserver(mmScheduleRebuild).observe(messagesEl, {
    childList: true, subtree: true, characterData: true
  });
  new ResizeObserver(mmScheduleRebuild).observe(messagesEl);
  mmSyncSize();
  mmRebuild();
  mmUpdateView();

  function connectWS() {
    const proto = location.protocol === 'https:' ? 'wss://' : 'ws://';
    ws = new WebSocket(proto + location.host + '/ws');
    ws.addEventListener('open', function () {
      wsReady = true;
      sendVisibility(); // 告知服务端当前页面是否可见（决定任务完成是否弹系统通知）
      loadModels(); // 连接后预取模型名与思考分级（供操作栏显示模型名称）
    });
    ws.addEventListener('close', function () {
      wsReady = false;
      setTimeout(connectWS, 2000); // 断线重连
    });
    ws.addEventListener('message', function (e) {
      let ev;
      try { ev = JSON.parse(e.data); } catch (_) { return; }
      switch (ev.type) {
        case 'ready':
          // 断线重连：旧任务已随连接关闭被服务端取消，复位运行态避免按钮卡在 ▶
          running = false; runSessionID = ''; runReason = ''; runText = '';
          sendBtn.classList.remove('running');
          sendBtn.textContent = '↑';
          sendBtn.title = '发送';
          renderSessions(ev.sessions || []);
          // 刷新/重启自动恢复：回放该工作区最近一次会话
          if (!sessionID && sessionsCache.length) {
            composerSnap = true; // 自动恢复的那次位置修正直接落位（避免「中间 → 底部」滑一下）
            loadSession(sessionsCache[0].id);
          }
          break;
        case 'sessions':
          renderSessions(ev.items || []);
          break;
        case 'history':
          replayHistory(ev);
          break;
        case 'session':
          // 任务运行中不接受 session 事件改视图（那是别的会话的启动回报）
          if (ev.session_id && (!running || !runSessionID || ev.session_id === runSessionID)) {
            sessionID = ev.session_id;
          }
          break;
        case 'context':
          // 上下文占用只画当前视图会话的（后台会话结束也会下发它自己的 context）
          if (!ev.session_id || ev.session_id === sessionID) renderCtxUsage(ev);
          break;
        case 'busy':
          running = true;
          runSessionID = sessionID; // 本轮属于当前视图的会话；之后用户切走也能凭它识别
          runReason = ''; runText = ''; // 新一轮：未落盘片段从零开始
          lastReply = '';        // 新一轮开始：清空回复累积
          removeRetry();
          resetSubagentCards(); // 新一轮：重置子智能体卡片缓存
          sendBtn.classList.add('running');
          sendBtn.textContent = '▶';
          sendBtn.title = '点击打断';
          showThinking();
          syncRunBadges();
          break;
        case 'retry':
          if (runAway()) break; // 后台会话的重试提示不画进当前视图
          removeThinking();
          addRetry(ev.error || '');
          break;
        case 'compress':
          // 上下文越过压缩线，服务端已自动压缩。这一段是「无声发生」的关键动作，
          // 必须告诉用户：否则他会以为历史丢了（实际完整保留，只是送模内容变了）。
          if (runAway()) break;
          (function () {
            const ci = ev.compress || {};
            if (ci.degraded) {
              addInfo('上下文超出阈值，已临时压缩历史（' + fmtTokens(ci.before) + ' → ' +
                fmtTokens(ci.after) + ' tokens）。' + (ci.reason || ''));
              return;
            }
            const added = Number(ci.added) || 0;
            const total = Number(ci.summarized) || 0;
            addInfo('上下文已自动摘要压缩：' + added + ' 条历史并入摘要' +
              (total ? '（累计 ' + total + ' 条）' : '') + '，' +
              fmtTokens(ci.before) + ' → ' + fmtTokens(ci.after) + ' tokens。完整历史仍保留在会话中。');
          })();
          break;
        case 'reasoning':
          runReason += ev.text || '';
          if (runAway()) break; // 只累积（runReason），切回时补渲染
          removeThinking();
          removeRetry(); // 重试成功：撤掉提示
          settleActiveTool(); // 工具执行完进入下一段思考：撤掉 spinner
          appendReason(ev.text || '');
          break;
        case 'text':
          runReason = ''; // 正文开始，思考段结束（与 foldReason 同步）
          runText += ev.text || '';
          if (runAway()) { foldReason(); break; }
          removeThinking();
          removeRetry(); // 重试成功：撤掉提示
          foldReason();
          settleActiveTool(); // 进入正文输出：撤掉 spinner
          appendText(ev.text || '');
          break;
        case 'tool_call': {
          // 该段内容此刻已写入会话消息：思考丢弃、正文交给历史回放，不再算未落盘
          runReason = ''; runText = '';
          if (runAway()) { foldReason(); closeText(); break; }
          removeThinking();
          removeRetry();
          foldReason();
          closeText();
          const name = ev.tool_name || '工具';
          addTool(toolLabel(name, ev.tool_input),
            ev.tool_input ? JSON.stringify(ev.tool_input, null, 2) : '',
            ev.diff_stats);
          break;
        }
        case 'subagent': {
          // 子智能体实时进度：每个子任务一张卡片，status 驱动样式
          runReason = ''; runText = '';
          if (runAway()) { foldReason(); closeText(); break; }
          removeThinking();
          foldReason();
          closeText();
          addSubagentCard(ev.subagent || {});
          break;
        }
        case 'tool_result':
          if (runAway()) break;
          removeThinking();
          settleActiveTool(); // 工具已返回：撤掉 spinner
          closeText();
          // 工具执行结束后，模型通常会再次被调用；在下一段 reasoning/text 到来前明确提示等待。
          if (running) showThinking();
          break;
        case 'hitl_request':
          removeThinking();
          removeRetry();
          settleActiveTool(); // 等待审批不算运行：撤掉 spinner
          foldReason();
          closeText();
          addApproval(ev); // 后台会话的审批会带 session_id，卡片上标注来源
          break;
        case 'error': {
          if (runAway()) {
            // 后台会话出错：在当前视图标注来源，不能静默吞掉
            const emeta = sessionsCache.find(function (s) { return s.id === runSessionID; });
            addError('后台会话「' + ((emeta && emeta.title) || runSessionID) + '」出错：' + (ev.error || '未知错误'));
            break;
          }
          removeThinking();
          removeRetry();
          settleActiveTool();
          foldReason();
          closeText();
          addError('出错了：' + (ev.error || '未知错误'));
          break;
        }
        case 'idle': {
          const backHome = !runAway();
          removeThinking();
          removeRetry();
          settleActiveTool();
          foldReason();
          closeText();
          if (backHome && lastReply) addActions(lastReply); // 回复结束：显示复制/模型/重新生成
          running = false;
          runSessionID = '';
          runReason = ''; runText = '';
          sendBtn.classList.remove('running');
          sendBtn.textContent = '↑';
          sendBtn.title = '发送';
          syncRunBadges();
          break;
        }
      }
    });
  }
  connectWS();

  // ---------- 工作区状态 ----------
  // 初始化：回读后端当前工作区（发消息、侧栏归属都以它为准）
  fetch('/api/workspace').then(function (r) { return r.json(); }).then(function (d) {
    if (d.root) { workspaceRoot = d.root; }
  }).catch(function () {});

  function setWorkspace(path) {
    workspaceRoot = path || '';
    // 返回 Promise：需要「先切工作区、再新建会话」的调用方必须等它，否则 new_session 会
    // 抢在清空请求前面到达服务端，新会话被挂到上一个项目下（见「新建项目」按钮）。
    // ⚠️ resolve 成 {ok, error}：fetch 对 400 也是 resolve，**调用方必须看 ok** ——
    // 服务端会拒绝不存在的目录，忽略状态码会把「切换失败」当成成功。
    const done = fetch('/api/workspace', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ path: path })
    }).then(function (r) {
      return r.json().catch(function () { return {}; }).then(function (d) {
        return { ok: r.ok, error: (d && d.error) || '' };
      });
    }).then(function (res) {
      // 新建的空工作区会话选中工作区后，把该会话归属到新工作区
      // （否则它会一直留在「未选择工作区」分组，名字不更新）。
      // ⚠️ 只在**切换成功**时做：失败时这个路径是坏的，写进会话键又是一次损坏。
      if (res.ok && path && sessionID) {
        const cur = sessionsCache.find(function (s) { return s.id === sessionID; });
        if (cur && !cur.workspace) {
          fetch('/api/sessions', {
            method: 'PATCH',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({ id: sessionID, workspace: path })
          }).then(function () { loadSessionList(); });
        }
      }
      return res;
    });
    return done;
  }
  // 内置目录浏览选择器（Linux/macOS 等）；startPath 为服务端给出的默认起始目录
  // 内置选择器（Linux/macOS/Termux 等没有原生对话框的平台）。
  //   mode='dir'（默认）：只列子目录，确认键「选择当前目录」→ onPick(当前路径)；
  //   mode='file'：目录可进入、文件可点选（高亮），确认键「选择此文件」→ onPick(绝对路径)。
  // 面板 DOM 只建一次并复用，所以 mode / 回调 / 当前选中项放在外层变量里。
  let pickerMode = 'dir', pickerOnPick = null, pickerSel = '';
  function openBuiltinPicker(startPath, mode, onPick) {
    pickerMode = mode === 'file' ? 'file' : 'dir';
    pickerOnPick = onPick || null;
    pickerSel = '';
    let picker = document.getElementById('picker-overlay');
    if (!picker) {
      picker = document.createElement('div');
      picker.id = 'picker-overlay';
      picker.innerHTML =
        '<div class="picker-modal">' +
        '<div class="picker-head"><span id="picker-path"></span></div>' +
        '<div id="picker-list" class="picker-list"></div>' +
        '<div class="picker-actions">' +
        '<button type="button" id="picker-cancel">取消</button>' +
        '<button type="button" id="picker-ok" class="primary">选择当前目录</button>' +
        '</div></div>';
      document.body.appendChild(picker);
      picker.querySelector('#picker-cancel').addEventListener('click', function () {
        pickerSel = '';
        hideWithAnim(picker);
      });
      picker.querySelector('#picker-ok').addEventListener('click', function () {
        if (pickerMode === 'file') {
          if (!pickerSel) return;           // 没选文件时按钮是禁用的，这里兜底
          const picked = pickerSel;
          pickerSel = '';
          hideWithAnim(picker);
          if (pickerOnPick) pickerOnPick(picked);
          return;
        }
        const cur = picker.dataset.current || '';
        hideWithAnim(picker);
        // 目录模式同样走 onPick：把选中路径交回调用方（「新建项目」弹窗），
        // 切不切、什么时候切由调用方决定。
        if (pickerOnPick) pickerOnPick(cur);
      });
    }
    const okBtn = picker.querySelector('#picker-ok');
    okBtn.textContent = pickerMode === 'file' ? '选择此文件' : '选择当前目录';
    okBtn.disabled = pickerMode === 'file'; // 文件模式：选中文件后才可确认
    function browse(rel) {
      // picker=1：内置选择器要浏览工作区外的目录（如 Termux 的 ~/storage/shared），
      // 服务端只在这个模式下放行绝对路径 —— 不带它会被「路径越出工作区范围」403，
      // 表现正是「选择目录时列表永远为空」。
      fetch('/api/tree?depth=1&picker=1' + (rel ? '&path=' + encodeURIComponent(rel) : ''))
        .then(function (r) { return r.json(); })
        .then(function (data) {
          const list = picker.querySelector('#picker-list');
          if (data.error) { // 出错明确显示，不再静默渲染成「无子目录」
            list.innerHTML = '<div class="picker-empty">' + (data.error || '浏览失败') + '</div>';
            return;
          }
          picker.dataset.current = data.path || '';
          picker.querySelector('#picker-path').textContent = data.path || '';
          list.innerHTML = '';
          const items = data.items || [];
          items.filter(function (it) { return it.is_dir; }).forEach(function (it) {
            const d = document.createElement('div');
            d.className = 'picker-item';
            d.textContent = '📁 ' + it.name;
            d.addEventListener('click', function () { browse(it.path); });
            list.appendChild(d);
          });
          if (pickerMode === 'file') {
            items.filter(function (it) { return !it.is_dir; }).forEach(function (it) {
              const d = document.createElement('div');
              d.className = 'picker-item';
              d.textContent = '📄 ' + it.name;
              d.title = it.path;
              d.addEventListener('click', function () {
                pickerSel = it.path;
                list.querySelectorAll('.picker-item.sel').forEach(function (x) { x.classList.remove('sel'); });
                d.classList.add('sel');
                okBtn.disabled = false;
              });
              list.appendChild(d);
            });
          }
          if (!list.children.length) {
            list.innerHTML = '<div class="picker-empty">' + (pickerMode === 'file' ? '此目录为空' : '无子目录') + '</div>';
          }
        });
    }
    picker.classList.remove('leaving', 'hidden');
    browse(startPath || '');
  }

  // ---------- 发送按钮两态：↑ 空闲 / ▶ 运行中 ----------
  const form = $('#composer');
  const sendBtn = $('#send-btn');
  const input = $('#input');
  let running = false;

  // 输入框 @提及 蓝色高亮：textarea 自身无法局部着色，靠 .input-mirror 这层
  // 「镜像文字」画字（textarea 文字是 transparent，只留光标）。见 index.html 注释
  // 与 app.css 的「共享排版」块。内容或滚动位置一变就要同步，否则会和真实文字错位。
  const inputMirror = $('#input-mirror');
  function syncInputMirror() {
    // 先按当前内容清理「@文件名」别名表（放在最前面：即使镜像层缺失也要保持别名与输入一致）
    pruneFileAliases(input.value);
    if (!inputMirror) return;
    inputMirror.innerHTML = '';
    // 原样渲染（不把路径缩成文件名）：这里就是用户正在编辑的文字
    renderUserText(inputMirror, input.value, { shortenPath: false });
    // 末尾补零宽字符：value 以换行结尾时，保证镜像层也保留那个空行
    inputMirror.appendChild(document.createTextNode('\u200b'));
    inputMirror.scrollTop = input.scrollTop;
  }
  input.addEventListener('scroll', syncInputMirror);
  syncInputMirror();

  // 提取文本里的 @文件路径 提及（含分隔符/盘符的才算文件；@技能名 忽略）。
  // 与渲染共用 MENTION_SPLIT，因此 `@"C:\a b\c.txt"` 这种带空白的写法也能取到。
  function fileMentions(text) {
    const out = [];
    String(text).split(MENTION_SPLIT).forEach(function (p) {
      if (!p || p.charAt(0) !== '@' || p.length < 2) return;
      const body = mentionBody(p);
      if (/[\\/]/.test(body)) out.push(body);
    });
    return out;
  }

  // 发送前的处理链：把 @区外文件 stage 成工作区副本。
  //   text    —— 真正发出去、给模型看的文本（别名已展开成真实路径）
  //   display —— 消息气泡里展示的文本（= 用户原本输入的样子，@文件名 保持蓝色）
  // 两者刻意分开：模型要能读到路径，用户要看自己写的东西。
  // staging 失败的文件保持原样（模型看不到，但至少消息可发；会附带提示）。
  async function prepareMentions(text, display) {
    const notes = [];
    for (const f of fileMentions(text)) {
      if (insideWorkspace(f)) continue;
      try {
        const res = await fetch('/api/stage_file', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ path: f })
        });
        const d = await res.json();
        if (res.ok && d.ok && d.staged_path) {
          // 按 token 粒度整体替换（含 @、含可能的引号），换成模型可直接用的路径
          text = text.split(MENTION_SPLIT).map(function (part) {
            return (part && part.charAt(0) === '@' && mentionBody(part) === f) ? d.staged_path : part;
          }).join('');
          // ⚠️ inside=true 表示它本来就在工作区内、**没有复制** —— 别说「已复制到 attachments/」。
          // （前端自己的 insideWorkspace 是词法判断，手打的相对路径会被它误判成区外，
          //   于是走到这里；以服务端的 inside 为准才不会弹假提示。）
          if (!d.inside) {
            notes.push('已把 ' + d.name + ' 复制到工作区 attachments/，模型将通过副本访问');
          }
        } else {
          notes.push('文件 ' + f + ' 在工作区外且副本创建失败：' + (d.error || '未知错误'));
        }
      } catch (_) {
        notes.push('文件 ' + f + ' 暂存失败（服务不可达）');
      }
    }
    return { text: text, display: display === undefined ? text : display, notes: notes };
  }

  form.addEventListener('submit', function (e) {
    e.preventDefault();
    if (running) {
      // 运行中按发送 = 打断；若正在看别的会话，说明打断的是后台任务
      if (runAway()) addInfo('已请求打断正在后台运行的任务');
      wsSend({ type: 'cancel' });
      return;
    }
    const raw = input.value.trim();
    if (!raw) return;
    if (!wsReady) { addError('未连接到服务，请稍候重试'); return; }
    // ⚠️ 顺序要紧：别名展开必须在清空输入框**之前**。syncInputMirror() 会按当前内容
    // 清理「@文件名」别名表，先清空就等于把别名全删了，expandFileAliases 只能原样返回。
    const outgoing = expandFileAliases(raw);
    input.value = '';
    syncInputMirror();
    // 再把区外文件 stage 成工作区副本（attachments/），最后发出去。
    // 第二个参数 = 气泡要显示的原文（raw），与发出去的 outgoing 分开。
    prepareMentions(outgoing, raw).then(function (p) {
      lastUserText = p.text; // 记录供「重新生成」（用发送文本，可直接重放）
      composerSnap = false; // 用户主动发出第一句：位置切换要有下放动画
      // 气泡显示 p.display（= 用户原本输入的样子，@文件名 保持蓝色），
      // 模型拿到的仍是 p.text（真实路径 / attachments 暂存路径）——显示与发送分离。
      addUser(p.display);
      p.notes.forEach(function (n) { addInfo(n); });
      // 不等服务端 busy 往返：用户消息发出后，模型尚未回复的空窗立即显示提示。
      showThinking();
      wsSend({
        type: 'user_message',
        session_id: sessionID,
        text: p.text,
        thinking: thinkingVal // 上游参数原值：枚举（minimal/low/…）或 budget 数字
      });
    });
  });

  // 快捷键：Enter 换行（textarea 默认行为），Shift+Enter 发送
  input.addEventListener('keydown', function (e) {
    if (e.key === 'Enter' && e.shiftKey) {
      e.preventDefault();
      form.requestSubmit();
    }
  });

  // ---------- 输入框 @ 提及：技能 + 插件 ----------
  // 输入末尾输入「@」弹出选择面板（输入框上方内联面板，非浏览器弹窗），分两组：
  //   · 技能：@技能名 → 服务端把该 SKILL.md 正文注入当轮（pkg/agent/skills.go 的 skillMatches）
  //   · 插件：@插件名 → 提示模型调用该插件注册的工具（MCP 工具以「插件名.工具名」注册）
  // 数据在每次「新打开」@ 面板时拉一次并缓存（/api/skills + /api/plugins + /api/config 的
  // 工具表），随后按输入内容本地过滤 —— 不再每敲一个字符就打一轮接口。
  // 方向键选择、Enter/点击 选中，把 @名称 插入输入框；Esc 关闭。
  let atPop = null, atItems = [], atIdx = 0, atStart = -1;
  let atData = null, atLoading = false, atLoadedFor = -1;
  function closeAtPop() {
    if (atPop) { atPop.remove(); atPop = null; }
    atItems = []; atIdx = 0; atStart = -1;
  }
  // 光标前最近的 @（@后只跟过滤字符）→ {start, filter}；不在 @ 语境返回 null
  function atContext() {
    const pos = input.selectionStart;
    const m = input.value.slice(0, pos).match(/@([A-Za-z0-9_\-\u4e00-\u9fa5]*)$/);
    return m ? { start: pos - m[0].length, filter: m[1] || '' } : null;
  }
  // 拉技能 + 插件 + 工具表；插件工具按「插件名.」前缀归属到插件
  function loadAtData() {
    if (atData || atLoading) return;
    atLoading = true;
    Promise.all([
      fetch('/api/skills').then(function (r) { return r.json(); }).catch(function () { return {}; }),
      fetch('/api/plugins').then(function (r) { return r.json(); }).catch(function () { return {}; }),
      fetch('/api/config').then(function (r) { return r.json(); }).catch(function () { return {}; })
    ]).then(function (res) {
      atLoading = false;
      const tools = (res[2] && res[2].tools) || [];
      atData = {
        skills: ((res[0] || {}).items || []).filter(function (sk) { return sk.enabled; }),
        plugins: ((res[1] || {}).items || []).map(function (p) {
          p.toolNames = tools.filter(function (t) { return t.indexOf(p.name + '.') === 0; });
          return p;
        }).sort(function (a, b) {
          if (!!b.enabled !== !!a.enabled) return b.enabled ? 1 : -1; // 已启用的排前面
          return a.name < b.name ? -1 : 1;
        })
      };
      // 数据回来时用户可能已经改了输入：以当前光标处的 @ 语境为准
      const cur = atContext();
      if (cur) { atStart = cur.start; drawAtPop(cur.filter); }
    });
  }
  function drawAtPop(filter) {
    if (!atData) return;
    const q = (filter || '').toLowerCase();
    const hit = function (name, desc) {
      return !q || (name + ' ' + (desc || '')).toLowerCase().indexOf(q) >= 0;
    };
    const skills = atData.skills.filter(function (sk) { return hit(sk.name, sk.description); });
    const plugins = atData.plugins.filter(function (p) { return hit(p.name, p.description); });
    if (!skills.length && !plugins.length) {
      closeAtPop();
      if (q) return; // 过滤没命中：直接收起
      // 一条都没有：给提示（否则用户会以为 @ 坏了）
      atPop = document.createElement('div');
      atPop.className = 'at-skill-pop';
      const hint = document.createElement('div');
      hint.className = 'at-empty';
      hint.textContent = '还没有可提及的技能或插件。插件可在 设置 → MCP 服务 里添加。';
      atPop.appendChild(hint);
      atAnchor().appendChild(atPop);
      return;
    }
    atItems = [];
    atIdx = 0;
    if (atPop) atPop.remove();
    atPop = document.createElement('div');
    atPop.className = 'at-skill-pop';
    const title = document.createElement('div');
    title.className = 'at-title';
    title.textContent = '提及技能 / 插件（↑↓ 选择，Enter 确认，Esc 取消）';
    atPop.appendChild(title);
    function addGroup(text) {
      const g = document.createElement('div');
      g.className = 'at-group';
      g.textContent = text;
      atPop.appendChild(g);
    }
    // 一条可选行；dataset.idx 指向 atItems 下标，供点击与高亮对齐（分组标题不参与选择）
    function addRow(name, desc, note, disabled) {
      const b = document.createElement('button');
      b.type = 'button';
      const idx = atItems.length;
      atItems.push({ name: name });
      b.dataset.idx = String(idx);
      b.textContent = name;
      if (desc) {
        const d = document.createElement('span');
        d.className = 'sk-desc-inline';
        d.textContent = desc;
        b.appendChild(d);
      }
      if (note) {
        const t = document.createElement('span');
        t.className = 'at-tools';
        t.textContent = note;
        b.appendChild(t);
      }
      if (disabled) b.classList.add('at-disabled');
      b.addEventListener('click', function () { pickAtItem(idx); });
      atPop.appendChild(b);
    }
    if (skills.length) {
      addGroup('技能');
      skills.forEach(function (sk) { addRow(sk.name, sk.description, '', false); });
    }
    if (plugins.length) {
      addGroup('插件');
      plugins.forEach(function (p) {
        const note = (p.toolNames && p.toolNames.length)
          ? '工具：' + p.toolNames.join('、')
          : (p.enabled ? '已启用' : '未启用（见 设置 → MCP 服务）');
        addRow(p.name, p.description, note, !p.enabled);
      });
    }
    atAnchor().appendChild(atPop);
    highlightAt();
  }
  // 面板锚定到输入框上方（composer 内）
  function atAnchor() {
    const a = input.closest('#composer') || input.parentElement;
    a.style.position = a.style.position || 'relative';
    return a;
  }
  function highlightAt() {
    if (!atPop) return;
    atPop.querySelectorAll('button').forEach(function (b) {
      b.classList.toggle('sel', Number(b.dataset.idx) === atIdx);
    });
    const sel = atPop.querySelector('button.sel');
    if (sel) sel.scrollIntoView({ block: 'nearest' });
  }
  function pickAtItem(i) {
    const it = atItems[i];
    if (!it) { closeAtPop(); return; }
    // 用「@名称 」替换刚输入的 @起始段
    const before = input.value.slice(0, atStart);
    const after = input.value.slice(input.selectionStart);
    input.value = before + '@' + it.name + ' ' + after;
    syncInputMirror();
    closeAtPop();
    input.focus();
    const pos = (before + '@' + it.name + ' ').length;
    input.setSelectionRange(pos, pos);
  }
  input.addEventListener('input', function () {
    syncInputMirror(); // 内容一变就重画 @提及 高亮
    const cur = atContext();
    if (!cur) { closeAtPop(); atLoadedFor = -1; return; }
    atStart = cur.start;
    // 换了一处新的 @ → 重新拉一次（刚配好的插件能立刻出现）；同一处 @ 内只本地过滤
    if (atLoadedFor !== cur.start) { atLoadedFor = cur.start; atData = null; }
    if (!atData) { loadAtData(); return; }
    drawAtPop(cur.filter);
  });
  input.addEventListener('keydown', function (e) {
    if (!atPop) return;
    if (!atItems.length) return; // 只有提示行，不参与上下键
    if (e.key === 'ArrowDown') { e.preventDefault(); atIdx = (atIdx + 1) % atItems.length; highlightAt(); }
    else if (e.key === 'ArrowUp') { e.preventDefault(); atIdx = (atIdx - 1 + atItems.length) % atItems.length; highlightAt(); }
    else if (e.key === 'Enter' && !e.shiftKey) { e.preventDefault(); pickAtItem(atIdx); }
    else if (e.key === 'Escape') { e.preventDefault(); closeAtPop(); }
  });
  input.addEventListener('blur', function () {
    // 延迟关闭：给点击面板项留时间
    setTimeout(closeAtPop, 150);
  });

  // ---------- ＋ 更多菜单：添加文件 / Skills（右展） ----------
  // 点 ＋ → 图标变 ✕ 并向上弹出菜单；再点一次、点别处或按 Esc 收起并复位图标。
  //   · 添加文件：Windows 走资源管理器「打开文件」对话框；Linux/Termux 走内置选择器（文件模式）。
  //     选中后把绝对路径插到输入框光标处。
  //   · Skills：向右展开当前工作区已加载的技能，点选即插入 @技能名（与 @ 提及同款，
  //     服务端据此把该 SKILL.md 正文注入当轮）。
  const moreBtn = $('#more-btn');
  const morePop = $('#more-pop');
  const moreSkillsBtn = $('#more-skills');
  const moreSkillsPop = $('#more-skills-pop');
  let moreOpen = false;
  // 图标不换字符：加号靠 CSS 旋转 45° 变成叉号（见 #more-btn.open svg），这样才有过渡
  function syncMoreIcon(visible) {
    moreOpen = visible;
    moreBtn.classList.toggle('open', visible);
    moreBtn.title = visible ? '收起' : '更多';
    if (!visible) moreSkillsPop.classList.add('hidden');
  }
  function setMoreOpen(open) {
    if (open) {
      closeAllPops(morePop); // 打开时顺手收起权限/模型弹层（沿用全局机制）
      morePop.classList.remove('leaving', 'hidden');
    } else {
      hideWithAnim(morePop);
      moreSkillsPop.classList.add('hidden');
    }
    syncMoreIcon(open);
  }
  // 图标状态跟着弹层的**实际**显隐走：别的路径关掉它时（点空白处的 closeAllPops、
  // 打开权限/模型弹层）也能同步，否则图标会停在 ✕ 而菜单已经没了，下次点还要按两下。
  new MutationObserver(function () {
    const visible = !morePop.classList.contains('hidden') && !morePop.classList.contains('leaving');
    if (visible !== moreOpen) syncMoreIcon(visible);
  }).observe(morePop, { attributes: true, attributeFilter: ['class'] });
  moreBtn.addEventListener('click', function (e) {
    e.stopPropagation();
    setMoreOpen(morePop.classList.contains('hidden') && !morePop.classList.contains('leaving'));
  });
  document.addEventListener('keydown', function (e) {
    if (e.key === 'Escape' && moreOpen) setMoreOpen(false);
  });

  // 把文本插到输入框光标处（前面缺空格就补一个，末尾统一补空格便于继续输入）
  function insertIntoInput(text) {
    const pos = input.selectionStart;
    const before = input.value.slice(0, pos);
    const after = input.value.slice(pos);
    const sep = (before && !/\s$/.test(before)) ? ' ' : '';
    const ins = text + ' ';
    input.value = before + sep + ins + after;
    syncInputMirror();
    const np = (before + sep + ins).length;
    input.focus();
    input.setSelectionRange(np, np);
  }
  // 路径是否落在工作区内（代理默认只能读工作区内的文件）
  function insideWorkspace(p) {
    const norm = function (s) { return String(s || '').replace(/\\/g, '/').replace(/\/+$/, '').toLowerCase(); };
    const root = norm(workspaceRoot);
    const f = norm(p);
    return !!root && (f === root || f.indexOf(root + '/') === 0);
  }
  // ＋添加文件 /「当前目录的文件」：输入框里只放「@文件名」（蓝色高亮），
  // 完整路径记进别名表，发送前由 expandFileAliases() 展开 —— 输入区保持干净，
  // 模型那边拿到的仍是可读的完整路径。文件名含空白时用 `@"名字"` 形式
  //（提及语法以空白分隔，不加引号会被切成两段）。
  // 输入框里已经不存在的 token → 别名一并清掉。否则删掉提及后又手打一个同名 token 时，
  // 会被上一次的旧路径悄悄劫持（输入框显示 A、实际发出去 B）。
  function pruneFileAliases(text) {
    if (!fileAlias.size) return;
    const alive = new Set(String(text).split(MENTION_SPLIT));
    fileAlias.forEach(function (_, tok) { if (!alive.has(tok)) fileAlias.delete(tok); });
  }
  function mentionToken(name) {
    return /[\s"]/.test(name) ? '@"' + name + '"' : '@' + name;
  }
  // 把输入框里的「@文件名」别名展开成真实路径；没有别名的 token 原样保留
  //（例如用户手打的 @技能名 / @完整路径）。
  // ⚠️ 展开后必须重新按 mentionToken 的规则加引号：真实路径可能含空白
  //（`C:\...\新建 文本文档.txt`），裸着写回去会被 MENTION_SPLIT / fileMentions
  // 在空格处切断，暂存出一个不存在的路径。
  function expandFileAliases(text) {
    if (!fileAlias.size) return text;
    return String(text).split(MENTION_SPLIT).map(function (part) {
      const real = fileAlias.get(part);
      return real ? mentionToken(real) : part;
    }).join('');
  }
  // 为某个文件挑一个不冲突的提及 token：优先只用文件名（满足「只显示文件名」），
  // 只有当该 token 已被**另一个**文件占用时，才逐级多带父目录（如 @b/hello.html）。
  // 否则两个同名文件会互相顶掉 —— 输入框看着是两份，实际都指向后选的那个。
  function mentionTokenFor(fullPath) {
    const segs = String(fullPath).replace(/[\\/]+$/, '').split(/[\\/]/);
    for (let take = 1; take <= segs.length; take++) {
      const tok = mentionToken(segs.slice(-take).join('/'));
      const exist = fileAlias.get(tok);
      if (!exist || exist === fullPath) return tok;
    }
    return mentionToken(segs.join('/'));
  }
  function insertPickedFile(p) {
    const name = String(p).replace(/[\\/]+$/, '').split(/[\\/]/).pop() || p;
    const token = mentionTokenFor(p);
    fileAlias.set(token, p);
    insertIntoInput(token);
    if (workspaceRoot && !insideWorkspace(p)) {
      addInfo('已插入工作区外的文件 ' + name + '（代理默认只能读工作区内的文件，需要时可在设置里放开工作区边界）');
    }
  }
  document.getElementById('more-add-file').addEventListener('click', function () {
    setMoreOpen(false);
    fetch('/api/pick_file', { method: 'POST' })
      .then(function (r) { return r.json().then(function (d) { return { status: r.status, data: d }; }); })
      .then(function (res) {
        if (res.status !== 200) { addError(res.data.error || '打开文件选择器失败'); return; }
        if (res.data.builtin) {
          // Linux/Termux 等：内置选择器（文件模式），起始目录由服务端按平台决定
          openBuiltinPicker(res.data.start_path || '', 'file', insertPickedFile);
        } else if (res.data.ok && res.data.path) {
          insertPickedFile(res.data.path); // Windows 资源管理器对话框选中
        }
        // res.data.ok === false：用户在系统对话框点了取消，无需任何动作
      })
      .catch(function () { openBuiltinPicker('', 'file', insertPickedFile); });
  });

  // 当前目录的文件：内置选择器（文件模式），起点 = 工作区根。
  // 与「添加文件」的区别：不弹系统对话框，直接浏览工作区树（所有平台一致），
  // 选中后同样以 @文件 路径插入输入框。
  document.getElementById('more-browse-workspace').addEventListener('click', function () {
    setMoreOpen(false);
    if (!workspaceRoot) {
      addInfo('还没有选择工作区：请先在左上角选择工作区，再浏览当前目录的文件');
      return;
    }
    openBuiltinPicker(workspaceRoot, 'file', insertPickedFile);
  });

  // Skills 二级菜单：每次展开都重新拉一次（刚建的技能能立刻出现）
  function renderMoreSkills() {
    fetch('/api/skills').then(function (r) { return r.json(); }).then(function (d) {
      const items = (d.items || []).filter(function (sk) { return sk.enabled; });
      moreSkillsPop.innerHTML = '';
      if (!items.length) {
        const e = document.createElement('div');
        e.className = 'more-sub-empty';
        e.textContent = '还没有已加载的技能';
        moreSkillsPop.appendChild(e);
        return;
      }
      items.forEach(function (sk) {
        const b = document.createElement('button');
        b.type = 'button';
        b.className = 'pop-opt more-sub-item';
        b.textContent = sk.name;
        if (sk.description) {
          const s = document.createElement('span');
          s.className = 'sk-desc-inline';
          s.textContent = sk.description;
          b.appendChild(s);
        }
        b.addEventListener('click', function (e) {
          e.stopPropagation();
          setMoreOpen(false);
          insertIntoInput('@' + sk.name);
        });
        moreSkillsPop.appendChild(b);
      });
    }).catch(function () {});
  }
  moreSkillsBtn.addEventListener('click', function (e) {
    e.stopPropagation();
    const willShow = moreSkillsPop.classList.contains('hidden');
    if (willShow) renderMoreSkills();
    moreSkillsPop.classList.toggle('hidden', !willShow);
  });

  // ---------- 滚动条向上翻历史 → 输入卡片折叠；向下回底部 → 弹回 ----------
  const composerWrap = $('#composer-wrap');
  let composerHide = 0;          // 当前下移像素（0 = 完全显示）
  let lastMsgScroll = messagesEl.scrollTop;

  messagesEl.addEventListener('scroll', function () {
    if (composerCentered) return; // 居中态（无消息，无可滚动内容）不参与折叠
    const st = messagesEl.scrollTop;
    const delta = st - lastMsgScroll;
    lastMsgScroll = st;
    // 最大位移 = 卡片自身高度 + 底部间隙（7vh），滑过即完全不可见
    const max = composerWrap.offsetHeight + window.innerHeight * 0.07 + 10;
    if (delta > 0 || messagesEl.scrollHeight - st - messagesEl.clientHeight < 80) {
      // 滚动条向下（或已到底部）：弹回显示
      if (composerHide !== 0) {
        composerHide = 0;
        composerWrap.style.transform = 'translateX(-50%)';
      }
    } else if (delta < 0) {
      // 滚动条向上翻历史：按滚动量渐进折叠
      composerHide = Math.min(max, composerHide - delta);
      composerWrap.style.transform = 'translateX(-50%) translateY(' + Math.round(composerHide) + 'px)';
    }
  });
  // 聚焦输入框时恢复显示
  input.addEventListener('focus', function () {
    if (composerHide !== 0) {
      composerHide = 0;
      composerWrap.style.transform = 'translateX(-50%)';
    }
  });

  // ---------- 空对话居中 / 有对话下放 ----------
  // 消息区一条记录都没有（新建项目、新建会话、归档/删除后回到空态、空会话回放）时，
  // 输入卡片在主对话栏内上下左右居中；用户发出第一句话（或载入有历史的会话）后，
  // 摘掉 .composer-centered 让卡片下放回底部常态。位置全部由 CSS 决定，这里只切类。
  let composerCentered = false;
  let composerSnap = true;   // 本次修正直接落位（无过渡）：页面加载后自动回放历史时用
  function syncComposerMode() {
    const empty = messagesEl.childElementCount === 0;
    if (empty === composerCentered) return;
    composerCentered = empty;
    const snap = composerSnap;
    composerSnap = false;
    if (snap) composerWrap.classList.add('composer-no-anim');
    composerWrap.classList.toggle('composer-centered', empty);
    if (empty) {
      // 居中态：清掉内联 transform，让样式类的平移生效；同时退出滚动折叠
      composerHide = 0;
      composerWrap.style.transform = '';
    } else {
      composerWrap.style.transform = 'translateX(-50%)';
    }
    if (snap) {
      // 落位完成即摘掉免动画类，后续切换（用户发第一句话）恢复平滑过渡
      requestAnimationFrame(function () { composerWrap.classList.remove('composer-no-anim'); });
    }
  }
  syncComposerMode(); // 首次进入（尚未选会话）即居中

  // ---------- 设置：左导航 + 右内容（皮肤即时应用） ----------
  const settingsOverlay = $('#settings-overlay');
  const openSettingsBtn = $('#open-settings');

  function refreshSkinSeg() {
    document.querySelectorAll('#skin-seg button').forEach(function (b) {
      b.classList.toggle('active', b.dataset.skin === skin);
    });
  }
  // 「任务完成通知」开关：读服务端当前偏好并回填；切换时写回并落盘 config/local.yaml。
  function refreshNotifySwitch() {
    var el = document.getElementById('settings-notify');
    if (!el) return;
    fetch('/api/notify').then(function (r) { return r.json(); }).then(function (d) {
      el.checked = d.enabled !== false;
    }).catch(function () { /* 读取失败保持原状 */ });
  }
  (function bindNotifySwitch() {
    var el = document.getElementById('settings-notify');
    if (!el) return;
    el.addEventListener('change', function () {
      var want = el.checked;
      el.disabled = true;
      fetch('/api/notify', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ enabled: want })
      }).then(function (r) { return r.json().then(function (d) { return { ok: r.ok, d: d }; }); })
        .then(function (res) {
          if (!res.ok) { el.checked = !want; addError('通知设置保存失败：' + (res.d.error || '未知错误')); }
        })
        .catch(function () { el.checked = !want; addError('通知设置保存失败（服务不可达）'); })
        .then(function () { el.disabled = false; });
    });
  })();
  openSettingsBtn.addEventListener('click', function () {
    refreshSkinSeg();
    // 常规页信息回填
    fetch('/api/workspace').then(function (r) { return r.json(); }).then(function (d) {
      var el = document.getElementById('settings-workdir');
      if (el) { el.textContent = d.root || '--'; el.title = d.root || ''; }
    });
    var mEl = document.getElementById('settings-model');
    if (mEl) { mEl.textContent = modelName || model || '--'; mEl.title = model || ''; }
    refreshNotifySwitch();
    settingsOverlay.classList.remove('leaving', 'hidden');
  });
  settingsOverlay.addEventListener('click', function (e) {
    if (e.target === settingsOverlay) hideWithAnim(settingsOverlay); // 点空白关闭（窗口化时）
  });
  // 「返回工作区」：左上角首项，退出设置（全屏下点空白不可达，这里是主出口）
  document.getElementById('nav-back').addEventListener('click', function () {
    hideWithAnim(settingsOverlay);
  });
  // Esc 退出设置
  document.addEventListener('keydown', function (e) {
    if (e.key === 'Escape' && !settingsOverlay.classList.contains('hidden')) {
      hideWithAnim(settingsOverlay);
    }
  });
  // 左侧分类导航（nav-back / nav-models 无 data-page，各自单独绑定）
  settingsOverlay.querySelectorAll('.nav-item[data-page]').forEach(function (item) {
    item.addEventListener('click', function () {
      settingsOverlay.querySelectorAll('.nav-item').forEach(function (n) { n.classList.remove('active'); });
      item.classList.add('active');
      settingsOverlay.querySelectorAll('.settings-page').forEach(function (p) {
        p.classList.toggle('active', p.id === 'page-' + item.dataset.page);
      });
    });
  });
  // 皮肤分段选择：点击即时生效
  document.querySelectorAll('#skin-seg button').forEach(function (b) {
    b.addEventListener('click', function () {
      if (b.dataset.skin === skin) return;
      skin = b.dataset.skin;
      localStorage.setItem('cf_skin', skin);
      refreshSkinSeg();
      buildSlider();      // 用新皮肤重建滑条
      renderModelPop();
    });
  });

  // ---------- 外观：对话框宽度/高度滑条（CSS 变量即时生效，localStorage 记忆） ----------
  function applyComposerSize(w, h) {
    document.documentElement.style.setProperty('--composer-max-w', w + 'px');
    document.documentElement.style.setProperty('--composer-min-h', h + 'px');
  }
  (function initComposerSize() {
    const wEl = document.getElementById('opt-comp-w');
    const hEl = document.getElementById('opt-comp-h');
    if (!wEl || !hEl) return;
    const wVal = document.getElementById('opt-comp-w-val');
    const hVal = document.getElementById('opt-comp-h-val');
    // 恢复记忆值（缺省 760 / 60）
    let w = parseInt(localStorage.getItem('cf_composer_w'), 10) || 760;
    let h = parseInt(localStorage.getItem('cf_composer_h'), 10) || 60;
    wEl.value = w; hEl.value = h;
    wVal.textContent = w + 'px'; hVal.textContent = h + 'px';
    applyComposerSize(w, h);
    wEl.addEventListener('input', function () {
      w = +wEl.value;
      wVal.textContent = w + 'px';
      applyComposerSize(w, h);
      localStorage.setItem('cf_composer_w', w);
    });
    hEl.addEventListener('input', function () {
      h = +hEl.value;
      hVal.textContent = h + 'px';
      applyComposerSize(w, h);
      localStorage.setItem('cf_composer_h', h);
    });
  })();

  // ---------- 记忆管理页（当前工作区） ----------
  function renderMemories(items) {
    const box = document.getElementById('mem-items');
    box.innerHTML = '';
    if (!items || !items.length) {
      box.innerHTML = '<div class="mem-empty">还没有记忆。对话中让 AI「记住…」，或用上方输入框手动添加。</div>';
      return;
    }
    items.forEach(function (m) {
      const row = document.createElement('div');
      row.className = 'mem-item';
      const text = document.createElement('span');
      text.className = 'mem-text';
      text.textContent = m.content;
      const time = document.createElement('span');
      time.className = 'mem-time';
      time.textContent = new Date(m.updated_at || m.created_at).toLocaleDateString();
      const edit = document.createElement('button');
      edit.type = 'button'; edit.className = 'label-btn'; edit.textContent = '✎'; edit.title = '编辑';
      edit.addEventListener('click', function () {
        const v = prompt('编辑记忆：', m.content);
        if (v == null || !v.trim() || v.trim() === m.content) return;
        fetch('/api/memory', {
          method: 'PATCH', headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ id: m.id, content: v.trim() })
        }).then(function (r) { return r.json(); }).then(function (d) { if (d.ok) renderMemories(d.items); });
      });
      const del = document.createElement('button');
      del.type = 'button'; del.className = 'label-btn'; del.textContent = '✕'; del.title = '删除';
      del.addEventListener('click', function () {
        if (!confirm('删除这条记忆？')) return;
        fetch('/api/memory?id=' + m.id, { method: 'DELETE' })
          .then(function (r) { return r.json(); })
          .then(function () { loadMemories(); });
      });
      row.appendChild(text); row.appendChild(time); row.appendChild(edit); row.appendChild(del);
      box.appendChild(row);
    });
  }
  function loadMemories() {
    fetch('/api/memory').then(function (r) { return r.json(); })
      .then(function (d) { renderMemories(d.items || []); });
  }
  document.getElementById('mem-add').addEventListener('click', function () {
    const inputEl = document.getElementById('mem-new');
    const v = inputEl.value.trim();
    if (!v) return;
    fetch('/api/memory', {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ content: v })
    }).then(function (r) { return r.json(); }).then(function (d) {
      if (d.ok) { inputEl.value = ''; renderMemories(d.items); }
    });
  });
  document.getElementById('mem-new').addEventListener('keydown', function (e) {
    if (e.key === 'Enter') document.getElementById('mem-add').click();
  });
  // 进入记忆页时刷新
  settingsOverlay.querySelectorAll('.nav-item[data-page="memory"]').forEach(function (n) {
    n.addEventListener('click', loadMemories);
  });

  // ---------- 技能管理页（只读列表，SKILL.md 文件为准） ----------
  function renderSkills(items) {
    const box = document.getElementById('skill-items');
    box.innerHTML = '';
    if (!items || !items.length) {
      box.innerHTML = '<div class="mem-empty">当前项目还没有技能。在 <工作区>/.codeforge/skills/<名称>/SKILL.md 创建即可被识别。</div>';
      return;
    }
    items.forEach(function (sk) {
      const row = document.createElement('div');
      row.className = 'skill-item';
      const head = document.createElement('div');
      head.className = 'sk-name';
      head.textContent = (sk.enabled ? '⚡ ' : '⏸ ') + sk.name; // ⏸ = enabled:false
      row.appendChild(head);
      if (sk.description) {
        const d = document.createElement('div');
        d.className = 'sk-desc';
        d.textContent = sk.description;
        row.appendChild(d);
      }
      if (sk.triggers && sk.triggers.length) {
        const tg = document.createElement('div');
        tg.className = 'sk-triggers';
        tg.textContent = '触发词：' + sk.triggers.join('、');
        row.appendChild(tg);
      }
      if (sk.path) {
        const p = document.createElement('div');
        p.className = 'sk-path';
        p.textContent = sk.path;
        row.appendChild(p);
      }
      box.appendChild(row);
    });
  }
  function loadSkillsPage() {
    fetch('/api/skills').then(function (r) { return r.json(); })
      .then(function (d) { renderSkills(d.items || []); });
  }
  settingsOverlay.querySelectorAll('.nav-item[data-page="skills"]').forEach(function (n) {
    n.addEventListener('click', loadSkillsPage);
  });
  // 进入设置 MCP 页时刷新列表
  settingsOverlay.querySelectorAll('.nav-item[data-page="mcp"]').forEach(function (n) {
    n.addEventListener('click', renderMcpSettings);
  });

  // ---------- 内置插件页：列表 + 滑块开关 ----------
  // 只列一个列表：名称 / 用途 / 调用时机 + 滑块；拨动立即生效（注册/注销工具
  // + 同步提示词注入）并持久化到 local.yaml。
  function renderBuiltinPlugins() {
    const box = document.getElementById('builtin-items');
    if (!box) return;
    box.innerHTML = '<div class="mi-empty">加载中…</div>';
    fetch('/api/builtin-plugins').then(function (r) { return r.json(); }).then(function (d) {
      const items = d.items || [];
      box.innerHTML = '';
      if (!items.length) {
        box.innerHTML = '<div class="mi-empty">没有可用的内置插件</div>';
        return;
      }
      items.forEach(function (p) {
        const row = document.createElement('div');
        row.className = 'bp-item';
        const main = document.createElement('div');
        main.className = 'bp-main';
        const nm = document.createElement('div');
        nm.className = 'bp-name';
        nm.textContent = p.name;
        const purpose = document.createElement('div');
        purpose.className = 'bp-purpose';
        purpose.textContent = p.purpose || '';
        main.appendChild(nm);
        if (p.purpose) main.appendChild(purpose);
        if (p.when_to_use) {
          const when = document.createElement('div');
          when.className = 'bp-when';
          when.textContent = '调用时机：' + p.when_to_use;
          main.appendChild(when);
        }
        // 滑块开关
        const sw = document.createElement('label');
        sw.className = 'bp-switch';
        const cb = document.createElement('input');
        cb.type = 'checkbox';
        cb.checked = !!p.enabled;
        cb.addEventListener('change', function () {
          fetch('/api/builtin-plugins', {
            method: 'PATCH', headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({ id: p.id, enabled: cb.checked })
          }).then(function (r) { return r.json(); }).then(function (d2) {
            if (!d2.ok) {
              cb.checked = !cb.checked; // 失败回弹
              addError('切换插件失败：' + (d2.error || '未知错误'));
            }
          }).catch(function () {
            cb.checked = !cb.checked;
          });
        });
        const track = document.createElement('span');
        track.className = 'track';
        track.title = (p.enabled ? '已启用' : '已停用');
        sw.appendChild(cb);
        sw.appendChild(track);
        row.appendChild(main);
        row.appendChild(sw);
        box.appendChild(row);
      });
    }).catch(function () {
      box.innerHTML = '<div class="mi-empty">插件列表加载失败，请确认服务正在运行</div>';
    });
  }
  settingsOverlay.querySelectorAll('.nav-item[data-page="builtin"]').forEach(function (n) {
    n.addEventListener('click', renderBuiltinPlugins);
  });

  // ---------- 子智能体页：总开关 / 并发上限 / 能力限制 ----------
  // 后端 /api/subagents 按指针语义做「部分更新」：这里永远只提交被改动的那一项，
  // 避免用界面上的旧值覆盖用户刚在别处（如插件页）改过的开关。
  // 总开关与「插件」页的内置插件开关是同一字段，两边切换都会立即互相体现。
  function setSubagentNote(msg, ok) {
    const el = document.getElementById('subagents-result');
    if (!el) return;
    el.className = 'settings-note' + (ok === true ? ' ok' : ok === false ? ' fail' : '');
    el.textContent = msg || '';
  }

  // syncSubagentLocks 让界面反映真实的生效关系：
  // 总开关关闭 → 其余项都无意义，置灰；总开关开启但禁写 → 删除开关无意义，置灰。
  function syncSubagentLocks() {
    const on = document.getElementById('sub-enabled');
    if (!on) return;
    const enabled = on.checked;
    ['sub-max', 'sub-allow-write', 'sub-allow-delete', 'sub-allow-memory'].forEach(function (id) {
      document.getElementById(id).disabled = !enabled;
    });
    if (enabled && !document.getElementById('sub-allow-write').checked) {
      document.getElementById('sub-allow-delete').disabled = true;
    }
  }

  function applySubagentView(d) {
    const cap = d.max_allowed || 5;
    const max = d.max_concurrent || cap;
    document.getElementById('sub-enabled').checked = d.enabled !== false;
    const range = document.getElementById('sub-max');
    range.max = cap;
    range.value = max;
    document.getElementById('sub-max-cap').textContent = cap;
    document.getElementById('sub-max-val').textContent = max + ' 个';
    document.getElementById('sub-allow-write').checked = d.allow_write !== false;
    document.getElementById('sub-allow-delete').checked = d.allow_delete !== false;
    document.getElementById('sub-allow-memory').checked = d.allow_memory !== false;
    syncSubagentLocks();
  }

  function loadSubagentSettings() {
    if (!document.getElementById('sub-enabled')) return;
    setSubagentNote('加载中…');
    fetch('/api/subagents').then(function (r) { return r.json(); }).then(function (d) {
      applySubagentView(d);
      setSubagentNote('');
    }).catch(function () {
      setSubagentNote('子智能体设置加载失败，请确认服务正在运行', false);
    });
  }

  // postSubagentSetting 提交一项改动；成功后用服务端回传的设置整体重绘（含越界收敛后的值）。
  function postSubagentSetting(patch, okMsg) {
    setSubagentNote('保存中…');
    return fetch('/api/subagents', {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(patch)
    }).then(function (r) {
      return r.json().then(function (d) { return { ok: r.ok, d: d }; });
    }).then(function (res) {
      if (!res.ok) {
        setSubagentNote('保存失败：' + (res.d.error || '未知错误'), false);
        return false;
      }
      if (res.d.settings) applySubagentView(res.d.settings);
      setSubagentNote(okMsg || '已保存', true);
      return true;
    }).catch(function () {
      setSubagentNote('保存失败（服务不可达）', false);
      return false;
    });
  }

  (function bindSubagentSettings() {
    const on = document.getElementById('sub-enabled');
    if (!on) return;

    on.addEventListener('change', function () {
      const want = on.checked;
      on.disabled = true;
      postSubagentSetting({ enabled: want }, want ? '已启用多智能体协作' : '已关闭多智能体协作')
        .then(function (ok) { if (!ok) on.checked = !want; syncSubagentLocks(); });
    });

    const range = document.getElementById('sub-max');
    const val = document.getElementById('sub-max-val');
    range.addEventListener('input', function () { val.textContent = range.value + ' 个'; });
    range.addEventListener('change', function () {
      postSubagentSetting({ max_concurrent: Number(range.value) }, '并发上限已设为 ' + range.value + ' 个')
        .then(function (ok) { if (!ok) loadSubagentSettings(); });
    });

    [['sub-allow-write', 'allow_write', '写入文件'],
     ['sub-allow-delete', 'allow_delete', '删除文件'],
     ['sub-allow-memory', 'allow_memory', '写入记忆']].forEach(function (pair) {
      const el = document.getElementById(pair[0]);
      el.addEventListener('change', function () {
        const patch = {};
        patch[pair[1]] = el.checked;
        postSubagentSetting(patch, (el.checked ? '已允许子智能体' : '已禁止子智能体') + pair[2])
          .then(function (ok) { if (!ok) el.checked = !el.checked; syncSubagentLocks(); });
      });
    });
  })();
  settingsOverlay.querySelectorAll('.nav-item[data-page="subagents"]').forEach(function (n) {
    n.addEventListener('click', loadSubagentSettings);
  });

  // ---------- 归档管理页：查看 / 恢复 / 永久删除 ----------
  function fmtArchDays(at) {
    if (!at) return '';
    const days = Math.floor((Date.now() / 1000 - at) / 86400);
    return days <= 0 ? '今天归档' : '已归档 ' + days + ' 天（满 10 天自动删除）';
  }
  // 单条已归档会话的行（三点：恢复到侧栏 / 永久删除）
  function archSessionRow(s) {
    const row = document.createElement('div');
    row.className = 'arch-item';
    const main = document.createElement('div');
    main.className = 'arch-main';
    const t = document.createElement('div');
    t.className = 'arch-title';
    t.textContent = s.title || '未命名会话';
    const meta = document.createElement('div');
    meta.className = 'arch-meta';
    meta.textContent = (s.message_count || 0) + ' 条消息'; // 项目名在组头上，这里不重复
    main.appendChild(t);
    main.appendChild(meta);
    const days = document.createElement('span');
    days.className = 'arch-days';
    days.textContent = fmtArchDays(s.archived_at);
    // 三点：恢复 / 永久删除（内联菜单，无浏览器弹窗）
    const dots = document.createElement('button');
    dots.type = 'button';
    dots.className = 'dots-btn';
    dots.textContent = '⋯';
    dots.title = '操作';
    dots.addEventListener('click', function (e) {
      e.stopPropagation();
      showInlineMenu(dots, [
        { label: '恢复到侧栏', fn: function () {
          fetch('/api/sessions', {
            method: 'PATCH', headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({ id: s.id, archived: false })
          }).then(function () { loadArchived(); loadSessionList(); });
        } },
        { label: '永久删除', danger: true, fn: function () {
          fetch('/api/sessions?id=' + encodeURIComponent(s.id), { method: 'DELETE' })
            .then(function () { loadArchived(); loadSessionList(); });
        } },
      ]);
    });
    row.appendChild(main);
    row.appendChild(days);
    row.appendChild(dots);
    return row;
  }
  // 恢复整个项目：把该项目下全部已归档会话一次性恢复。
  // 与侧栏 ⋯ 的「归档」（一次点掉整组）对称 —— 否则归档是一步、恢复是 N 步。
  function restoreWorkspace(ws, n) {
    fetch('/api/workspaces', {
      method: 'PATCH', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ workspace: ws, archive: false })
    }).then(function (r) {
      if (!r.ok) { addError('恢复项目失败，请重试'); return; }
      addInfo('已把 ' + (n ? n + ' 条' : '') + '会话恢复到侧栏');
      loadArchived();
      loadSessionList();
    }).catch(function () { addError('恢复项目失败（服务不可达）'); });
  }
  // 归档页：按项目分组渲染，组头带「恢复整个项目」
  function renderArchived(items) {
    const box = document.getElementById('archived-items');
    box.innerHTML = '';
    if (!items || !items.length) {
      box.innerHTML = '<div class="arch-empty">没有已归档的会话。</div>';
      return;
    }
    const groups = new Map();
    items.forEach(function (s) {
      const k = s.workspace || '';
      if (!groups.has(k)) groups.set(k, []);
      groups.get(k).push(s);
    });
    groups.forEach(function (list, ws) {
      const group = document.createElement('div');
      group.className = 'arch-group';
      const head = document.createElement('div');
      head.className = 'arch-group-head';
      const nameEl = document.createElement('span');
      nameEl.className = 'arch-group-name';
      nameEl.textContent = wsDisplayName(ws, list[0] && list[0].workspace_name);
      nameEl.title = ws || '新项目';
      const countEl = document.createElement('span');
      countEl.className = 'arch-group-count';
      countEl.textContent = list.length + ' 条会话';
      const restore = document.createElement('button');
      restore.type = 'button';
      restore.className = 'arch-group-restore';
      restore.textContent = '恢复整个项目';
      restore.title = '把该项目下全部已归档会话一次性恢复到侧栏';
      restore.addEventListener('click', function () { restoreWorkspace(ws, list.length); });
      head.appendChild(nameEl);
      head.appendChild(countEl);
      head.appendChild(restore);
      group.appendChild(head);
      list.forEach(function (s) { group.appendChild(archSessionRow(s)); });
      box.appendChild(group);
    });
  }
  function loadArchived() {
    fetch('/api/sessions?archived=1').then(function (r) { return r.json(); })
      .then(function (d) { renderArchived(d.items || []); });
  }
  settingsOverlay.querySelectorAll('.nav-item[data-page="archived"]').forEach(function (n) {
    n.addEventListener('click', loadArchived);
  });

  // ---------- 模型管理：＋模型 / 列表 / 配置 ----------
  const settingsContent = document.querySelector('.settings-content');

  function showPage(id) {
    settingsOverlay.querySelectorAll('.nav-item').forEach(function (n) {
      var match = n.dataset.page === id || (id === 'models' && n.id === 'nav-models');
      n.classList.toggle('active', match);
    });
    settingsOverlay.querySelectorAll('.settings-page').forEach(function (p) {
      p.classList.toggle('active', p.id === 'page-' + id);
    });
  }
  function showMTab(name) {
    document.querySelectorAll('.models-tab').forEach(function (t) {
      t.classList.toggle('active', t.dataset.mtab === name);
    });
    document.getElementById('mtab-list').classList.toggle('active', name === 'list');
    document.getElementById('mtab-config').classList.toggle('active', name === 'config');
  }

  // ---------- 模型管理（服务端模型库）----------
  //
  // 模型列表的唯一数据源是服务端 config/models.yaml（/api/models/list|save|delete）。
  // 旧的 localStorage 方案已废弃 —— 它把明文 API Key 存在浏览器里，且与服务端配置互不同步。
  // 「应用」只发 {model}，密钥由服务端从库里解析，前端永远接触不到明文 key。

  let editingIndex = -1; // -1 = 新增；>=0 编辑现有项（仅用于表单标题态，列表本身以 id 判重）
  let modelsCache = [];  // 最近一次 /api/models/list 的脱敏列表

  function renderModelItems() {
    const box = document.getElementById('model-items');
    box.innerHTML = '<div class="mi-empty">加载中…</div>';
    fetch('/api/models/list').then(function (r) { return r.json(); }).then(function (d) {
      modelsCache = d.models || [];
      box.innerHTML = '';
      if (!modelsCache.length) {
        box.innerHTML = '<div class="mi-empty">还没有模型，点击右边「＋ 模型」添加</div>';
        return;
      }
      modelsCache.forEach(function (m, i) {
        const row = document.createElement('div');
        row.className = 'model-item';
        const main = document.createElement('div');
        main.className = 'mi-main';
        const nm = document.createElement('div');
        nm.className = 'mi-name';
        nm.textContent = m.name || m.id;
        if (m.id === d.active) {
          const tag = document.createElement('span');
          tag.className = 'mi-badge';
          tag.style.marginLeft = '8px';
          tag.textContent = '当前';
          nm.appendChild(tag);
        }
        const idv = document.createElement('div');
        idv.className = 'mi-id';
        idv.textContent = m.id + (m.base_url ? ' · ' + m.base_url : '');
        main.appendChild(nm); main.appendChild(idv);
        const badge = document.createElement('span');
        badge.className = 'mi-badge';
        badge.textContent = (m.protocol || 'openai') + (m.key_set ? ' · 密钥✓' : ' · 无密钥');
        const acts = document.createElement('div');
        acts.className = 'mi-actions';
        function mkBtn(text, cls, fn) {
          const b = document.createElement('button');
          b.type = 'button'; b.textContent = text;
          if (cls) b.className = cls;
          b.addEventListener('click', fn);
          return b;
        }
        acts.appendChild(mkBtn('应用', 'apply', function () {
          fetch('/api/models/apply', {
            method: 'POST', headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({ model: m.id })
          }).then(function (r) { return r.json(); }).then(function (d2) {
            if (d2.ok) {
              modelsLoaded = false; // 让对话框下次拉取刷新
              loadModels().then(function () { renderModelBtn(); });
              showPage('general');
            } else if (d2.error) {
              alert('应用失败：' + d2.error);
            }
          });
        }));
        acts.appendChild(mkBtn('编辑', '', function () {
          editingIndex = i;
          fillForm(m);
          showPage('models');
          showMTab('config');
        }));
        acts.appendChild(mkBtn('删除', '', function () {
          if (!confirm('从模型库删除「' + (m.name || m.id) + '」？')) return;
          fetch('/api/models/delete', {
            method: 'POST', headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({ model: m.id })
          }).then(function (r) { return r.json(); }).then(function () {
            renderModelItems();
            modelsLoaded = false; // 删除的可能是当前模型，刷新主界面模型显示
            loadModels().then(function () { renderModelBtn(); });
          });
        }));
        row.appendChild(main); row.appendChild(badge); row.appendChild(acts);
        box.appendChild(row);
      });
    }).catch(function () {
      box.innerHTML = '<div class="mi-empty">模型列表加载失败，请确认服务正在运行</div>';
    });
  }

  // fillForm 接收列表条目：
  //  - 普通条目是脱敏视图（无明文，只有 key_set），明文框只显示占位；
  //  - 「当前生效」模型会带 key_plain 明文，回填到掩码框并可用眼睛切换查看。
  function fillForm(m) {
    // 编辑与新增复用同一表单，仅标题区分；editingIndex 由调用方先行设置。
    document.getElementById('mf-title').textContent = editingIndex >= 0 ? '编辑模型' : '添加模型';
    keyTouched = false; // 刚回填的表单没有改过密钥（脱敏明文框为空 ≠ 删除）
    document.getElementById('mf-name').value = m.name || '';
    document.getElementById('mf-id').value = m.id || '';
    document.getElementById('mf-url').value = m.base_url || '';
    setKeySrc(m.key_source || 'env');
    document.getElementById('mf-key-env').value = m.key_name || '';
    const kp = document.getElementById('mf-key-plain');
    kp.value = m.key_plain || '';
    kp.placeholder = (m.key_set && !m.key_plain) ? '已保存（留空保持不变）' : 'sk-...';
    updateKeyEye();
    const proto = (m.protocol || 'openai').toLowerCase();
    document.getElementById('mf-proto').value = proto === 'custom' ? 'openai' : proto; // 旧数据 custom ≡ openai
    document.getElementById('mf-ctx-in').value = m.ctx_in || 262144;
    document.getElementById('mf-ctx-out').value = m.ctx_out || 131072;
    setTestResult('', '');
    collapseDiscover(); // 换了模型就收起上一次的上游列表，避免勾选状态串味
  }
  function resetForm() {
    editingIndex = -1;
    fillForm({ protocol: 'openai', ctx_in: 262144, ctx_out: 131072 });
    document.getElementById('mf-key-plain').placeholder = 'sk-...';
    updateKeyEye();
  }
  function collectForm() {
    return {
      name: document.getElementById('mf-name').value.trim(),
      id: document.getElementById('mf-id').value.trim(),
      model: document.getElementById('mf-id').value.trim(), // 测试接口字段名（后端 modelTestReq.model）
      base_url: document.getElementById('mf-url').value.trim(),
      key_source: keySrc,
      key_name: document.getElementById('mf-key-env').value.trim(),
      key_value: document.getElementById('mf-key-plain').value,
      protocol: document.getElementById('mf-proto').value,
      ctx_in: Number(document.getElementById('mf-ctx-in').value) || 262144,
      ctx_out: Number(document.getElementById('mf-ctx-out').value) || 131072
    };
  }
  function setTestResult(ok, msg) {
    const el = document.getElementById('mf-test-result');
    el.className = 'mf-test-result' + (ok === true ? ' ok' : ok === false ? ' fail' : '');
    el.textContent = msg || '';
  }

  // 密钥来源分段；keyTouched 记录本次编辑是否动过密钥（决定保存时是否提交 key_value）。
  let keySrc = 'env';
  let keyTouched = false;
  function setKeySrc(ks) {
    keySrc = ks === 'plain' ? 'plain' : 'env';
    document.querySelectorAll('#mf-key-seg button').forEach(function (b) {
      b.classList.toggle('active', b.dataset.ks === keySrc);
    });
    document.getElementById('mf-key-env').style.display = keySrc === 'env' ? '' : 'none';
    document.getElementById('mf-key-plain').style.display = keySrc === 'plain' ? '' : 'none';
    updateKeyEye();
  }
  // 眼睛按钮：仅当「明文密钥」tab 里有实际值时才显示，点击切换掩码/明文。
  function updateKeyEye() {
    const kp = document.getElementById('mf-key-plain');
    const eye = document.getElementById('mf-key-eye');
    const visible = keySrc === 'plain' && kp.value.length > 0;
    eye.style.display = visible ? '' : 'none';
    if (!visible) {
      kp.type = 'password';
      eye.textContent = '👁';
    }
  }
  document.getElementById('mf-key-eye').addEventListener('click', function () {
    const kp = document.getElementById('mf-key-plain');
    const show = kp.type === 'password';
    kp.type = show ? 'text' : 'password';
    this.textContent = show ? '🙈' : '👁';
  });
  document.getElementById('mf-key-plain').addEventListener('input', function () {
    keyTouched = true;
    updateKeyEye();
  });
  document.getElementById('mf-key-env').addEventListener('input', function () { keyTouched = true; });
  document.querySelectorAll('#mf-key-seg button').forEach(function (b) {
    b.addEventListener('click', function () {
      if (b.dataset.ks !== keySrc) keyTouched = true; // 主动切换来源也算动过密钥
      setKeySrc(b.dataset.ks);
    });
  });

  // ＋ 模型：左侧导航项 → 模型管理页（列表视图）
  document.getElementById('nav-models').addEventListener('click', function () {
    settingsOverlay.querySelectorAll('.nav-item').forEach(function (n) { n.classList.remove('active'); });
    this.classList.add('active');
    showPage('models');
    showMTab('list');
    renderModelItems();
  });
  // 子 Tab：模型列表 / 添加模型（点「添加模型」即回到空白新建态，编辑态复用同一表单）
  document.querySelectorAll('.models-tab').forEach(function (t) {
    t.addEventListener('click', function () {
      if (t.dataset.mtab === 'config') resetForm();
      showMTab(t.dataset.mtab);
    });
  });

  // 测试连接
  document.getElementById('mf-test').addEventListener('click', function () {
    const m = collectForm();
    setTestResult(null, '测试中…');
    fetch('/api/models/test', {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(m)
    }).then(function (r) { return r.json(); }).then(function (d) {
      if (d.ok) {
        setTestResult(true, '连接成功');
      } else {
        setTestResult(false, '失败：' + (d.error || '未知错误'));
      }
    }).catch(function (e) {
      setTestResult(false, '失败：' + e);
    });
  });
  // 重置
  document.getElementById('mf-reset').addEventListener('click', resetForm);
  // 保存（写入服务端模型库 config/models.yaml；编辑当前生效模型时服务端
  // 会实时热切换，无需再点「应用」）
  document.getElementById('mf-save').addEventListener('click', function () {
    const m = collectForm();
    if (!m.id) { setTestResult(false, '模型 id 不能为空'); return; }
    // 编辑已有模型：本次没动过密钥 → 不提交 key_value，服务端按「保持库里已存
    // 密钥」处理。前端拿到的列表是脱敏视图，明文框为空只是回显受限，绝不能被
    // 当成「用户清空了密钥」而覆盖掉。
    if (editingIndex >= 0 && !keyTouched) {
      delete m.key_value;
    } else if (m.key_source === 'plain' && !m.key_value.trim()) {
      delete m.key_value;
    }
    setTestResult(null, '保存中…');
    fetch('/api/models/save', {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(m)
    }).then(function (r) { return r.json(); }).then(function (d) {
      if (d.ok) {
        editingIndex = -1;
        collapseDiscover();
        showMTab('list');
        renderModelItems();   // 新增/编辑立即出现在模型列表
        modelsLoaded = false; // 置假后 loadModels 会重拉模型库快照，对话框的模型选择同步出现新条目
        loadModels();
      } else {
        setTestResult(false, d.error || '保存失败');
      }
    }).catch(function (e) {
      setTestResult(false, '保存失败：' + e);
    });
  });

  // ---------- 添加模型：从上游 /models 拉取列表 → 选中 → 填入表单 ----------
  // 与「测试连接」的区别：测试是「所见即所测」，这里允许在表单没填地址/密钥时
  // 回退到当前生效模型（用户常在同一个网关上添加模型），服务端会在响应里
  // 用 key_from_active 如实告知，界面据此提示。
  //
  // 用 var 而非 let 声明状态：collapseDiscover 会被 fillForm/resetForm 早期调用，
  // var 提升到函数顶部（值为 undefined）不会踩 let 的暂时性死区。
  var discModels = [];
  var discSelected = {};

  function discPanel() { return document.getElementById('mf-discover-panel'); }

  function setDiscResult(ok, msg) {
    const el = document.getElementById('disc-result');
    if (!el) return;
    el.className = 'mf-test-result' + (ok === true ? ' ok' : ok === false ? ' fail' : '');
    el.textContent = msg || '';
  }

  // collapseDiscover 收起面板并清空状态（切换模型 / 保存成功后调用，避免上次的结果串味）。
  function collapseDiscover() {
    discModels = [];
    discSelected = {};
    const panel = discPanel();
    if (panel) panel.classList.add('hidden');
    const list = document.getElementById('disc-list');
    if (list) list.innerHTML = '';
    const flt = document.getElementById('disc-filter');
    if (flt) flt.value = '';
    const cnt = document.getElementById('disc-count');
    if (cnt) cnt.textContent = '';
    setDiscResult(null, '');
  }

  function updateDiscCount() {
    const n = Object.keys(discSelected).length;
    const cnt = document.getElementById('disc-count');
    if (cnt) cnt.textContent = '共 ' + (discModels || []).length + ' 个' + (n ? '，已选 ' + n + ' 个' : '');
    const btn = document.getElementById('disc-add');
    if (btn) btn.disabled = n === 0;
  }

  function renderDiscList() {
    const box = document.getElementById('disc-list');
    if (!box) return;
    const q = (document.getElementById('disc-filter').value || '').trim().toLowerCase();
    const shown = (discModels || []).filter(function (m) {
      if (!q) return true;
      return m.id.toLowerCase().indexOf(q) >= 0 ||
        (m.name || '').toLowerCase().indexOf(q) >= 0;
    });
    box.innerHTML = '';
    if (!shown.length) {
      box.innerHTML = '<div class="disc-empty">' +
        ((discModels || []).length ? '没有匹配的模型' : '没有可添加的模型') + '</div>';
      updateDiscCount();
      return;
    }
    shown.forEach(function (m) {
      const row = document.createElement('label');
      row.className = 'disc-item' + (m.in_library ? ' added' : '');
      const cb = document.createElement('input');
      // 单选：模型 id 只能一个一个填进表单，不做批量添加。
      cb.type = 'radio';
      cb.name = 'disc-model';
      cb.checked = !!discSelected[m.id];
      cb.disabled = !!m.in_library; // 已在库中：不可选，也不覆盖
      cb.addEventListener('change', function () {
        if (cb.checked) { discSelected = {}; discSelected[m.id] = true; }
        updateDiscCount();
      });
      const idEl = document.createElement('span');
      idEl.className = 'disc-id';
      idEl.textContent = m.id;
      row.appendChild(cb);
      row.appendChild(idEl);
      if (m.name && m.name !== m.id) {
        const nm = document.createElement('span');
        nm.className = 'disc-name';
        nm.textContent = m.name;
        row.appendChild(nm);
      }
      if (m.in_library) {
        const badge = document.createElement('span');
        badge.className = 'disc-badge';
        badge.textContent = '已添加';
        row.appendChild(badge);
      }
      box.appendChild(row);
    });
    updateDiscCount();
  }

  function discoverModels() {
    const btn = document.getElementById('mf-discover');
    btn.disabled = true;
    setDiscResult(null, '正在获取模型列表…');
    discPanel().classList.remove('hidden');
    fetch('/api/models/discover', {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(collectForm())
    }).then(function (r) { return r.json(); }).then(function (d) {
      if (!d.ok) {
        discModels = [];
        discSelected = {};
        document.getElementById('disc-list').innerHTML = '';
        updateDiscCount();
        setDiscResult(false, '获取失败：' + (d.error || '未知错误'));
        return;
      }
      discModels = d.models || [];
      discSelected = {};
      document.getElementById('disc-filter').value = '';
      renderDiscList();
      setDiscResult(true, '已获取 ' + discModels.length + ' 个模型' +
        (d.key_from_active ? '（密钥取自当前生效模型）' : ''));
    }).catch(function (e) {
      setDiscResult(false, '获取失败：' + e);
    }).then(function () { btn.disabled = false; });
  }

  // 「添加选中」：只把选中的模型 id（及上游显示名）填进上方表单，
  // 由用户确认连接信息、补好密钥后点「保存」正式入库 —— 所见即所得，
  // 不会把表单里还没确认的内容偷偷写进模型库。
  function fillSelectedDiscModel() {
    const ids = Object.keys(discSelected);
    if (!ids.length) { setDiscResult(false, '请先选择一个模型'); return; }
    const id = ids[0];
    const hit = (discModels || []).filter(function (m) { return m.id === id; })[0] || {};
    document.getElementById('mf-id').value = id;
    const nameEl = document.getElementById('mf-name');
    if (!nameEl.value.trim() && hit.name && hit.name !== id) nameEl.value = hit.name;
    setTestResult(true, '已填入模型 id：' + id + '，填好密钥后点「保存」');
    collapseDiscover();
  }

  (function bindModelDiscover() {
    const btn = document.getElementById('mf-discover');
    if (!btn) return;
    btn.addEventListener('click', discoverModels);
    document.getElementById('disc-close').addEventListener('click', collapseDiscover);
    document.getElementById('disc-filter').addEventListener('input', renderDiscList);
    document.getElementById('disc-add').addEventListener('click', fillSelectedDiscModel);
  })();
})();