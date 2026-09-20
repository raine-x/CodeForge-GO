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
    // 关闭弹层时若正处于滑块拖动中（Esc / 点击外部），复位拖动状态，
    // 否则下次 pointerup 会误吸附一次档位。
    if (slider && slider.classList.contains('dragging')) {
      slider.classList.remove('dragging');
      dragRatio = null;
    }
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
// 模型弹层：**两段式** —— 供应商单独拎一段在最上、点击可切换；
  // 下方只列出当前供应商下的模型。选模型时会把 filter 同步到该模型的供应商，
  // 上下两段始终一致 —— 用户看到的「供应商 ↓ 模型」是一体的两段，不是耦合的一团。
  //
  // 早先一弹层列所有模型的写法有两个问题：
  //   ① 模型库多了就一长串，找特定供应商的模型得肉眼过滤；
  //   ② 选模型隐式带上了供应商，看起来像「模型和供应商被一起选中」，但实际上
  //      两者并不真的耦合 ——「供应商」这一维度被埋没了。
  let modelProvFilter = '';   // 用户在弹层里挑的供应商过滤；'' = 跟随当前生效模型
  let provListOpen = false;   // 供应商列表是否展开

  function renderModelPop() {
    modelList.innerHTML = '';
    const activeInLib = !!model && modelInLib;
    const activeEntry = activeInLib ? modelChoices.find(function (m) { return m.id === model; }) : null;
    const activeProvId = activeEntry ? activeEntry.provider_id : '';

    // 供应商列表**从 modelChoices 推导**，不依赖 providersCache。
    // ⚠️ providersCache 只在设置页 loadLibrary() 时填充，主界面从没加载过它 ——
    // 早先直接用 providersCache，结果顶段供应商行在主界面**根本不渲染**（实测为 null）。
    // 从模型条目推导还有个好处：只列出真正挂了模型的供应商，没有空条目。
    const provList = [];
    const seenProv = {};
    modelChoices.forEach(function (m) {
      const id = m.provider_id || '';
      const key = id || ('\u0000orphan\u0000' + (m.id || ''));
      if (seenProv[key]) return;
      seenProv[key] = 1;
      provList.push({ id: id, name: m.provider_name || '' });
    });

    // 当前显示的供应商：用户在弹层里挑的 → 失效则退回当前生效模型的供应商 → 再退回第一个
    const curProvId = modelProvFilter || activeProvId || (modelChoices[0] && modelChoices[0].provider_id) || '';
    const curProv = provList.find(function (p) { return p.id === curProvId; }) || null;

    // ---- 顶部：供应商行（独立的一段，可点击切换）----
    if (provList.length) {
      const provRow = document.createElement('button');
      provRow.type = 'button';
      provRow.className = 'pop-prov-row';
      const lbl = document.createElement('span');
      lbl.className = 'pop-prov-label';
      lbl.textContent = '供应商';
      const nm = document.createElement('span');
      nm.className = 'pop-prov-name';
      nm.textContent = (curProv && (curProv.name || curProv.id)) || '未选择';
      const arrow = document.createElement('span');
      arrow.className = 'pop-prov-arrow';
      arrow.textContent = provListOpen ? '▴' : '▾';
      provRow.appendChild(lbl);
      provRow.appendChild(nm);
      provRow.appendChild(arrow);
      provRow.addEventListener('click', function (e) {
        e.stopPropagation();
        provListOpen = !provListOpen;
        renderModelPop();
      });
      modelList.appendChild(provRow);

      // 展开时列出所有供应商，选中的高亮 —— 点一个就切 filter 并收起
      if (provListOpen) {
        const list = document.createElement('div');
        list.className = 'pop-prov-list';
        provList.forEach(function (p) {
          const item = document.createElement('button');
          item.type = 'button';
          item.className = 'pop-prov-item' + (p.id === curProvId ? ' selected' : '');
          item.textContent = p.name || p.id;
          item.addEventListener('click', function (e) {
            e.stopPropagation();
            modelProvFilter = p.id;
            provListOpen = false;
            renderModelPop();
          });
          list.appendChild(item);
        });
        modelList.appendChild(list);
      }
    }

    // ---- 模型列表：只显示当前供应商下的 ----
    const filtered = modelChoices.filter(function (m) {
      return !curProvId || m.provider_id === curProvId;
    });
    // 当前列表已经全在同一供应商下，组内不必重复供应商名 —— 只在首条显示
    let lastProvName = '';
    filtered.forEach(function (m) {
      const b = document.createElement('button');
      b.type = 'button';
      const active = activeInLib && m.id === model;
      b.className = 'pop-opt' + (active ? ' selected' : '');
      b.dataset.model = m.id;
      // 顶段已经显示供应商了，这里只在首条（也就是列表第一条、紧挨着供应商行）再提示一次，
      // 视觉上是「一个供应商标题 + 它下面的模型」，列表内的模型不再每行重复供应商名。
      if (m.provider_name && m.provider_name !== lastProvName) {
        const pv = document.createElement('span');
        pv.className = 'pop-opt-prov';
        pv.textContent = m.provider_name;
        b.appendChild(pv);
      }
      lastProvName = m.provider_name || lastProvName;
      const nm = document.createElement('span');
      nm.className = 'pop-opt-name';
      nm.textContent = m.name || m.id;
      b.appendChild(nm);
      b.title = (m.provider_name ? m.provider_name + ' ↓ ' : '') + (m.name || m.id) + '（' + m.id + '）';
      if (!active) b.addEventListener('click', function () { switchActiveModel(m.id); });
      modelList.appendChild(b);
    });

    // 提示行：优先级 库为空 > 当前供应商下没模型 > 当前生效模型不在库
    if (!modelChoices.length) {
      const tip = document.createElement('div');
      tip.className = 'pop-tip';
      tip.textContent = '模型库为空，请在 设置 → 模型 → 添加模型 中添加';
      modelList.appendChild(tip);
    } else if (curProvId && !filtered.length) {
      const tip = document.createElement('div');
      tip.className = 'pop-tip';
      tip.textContent = '该供应商下还没有模型：点上方「供应商」换一个，或去 设置 → 模型 添加';
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
    // 选模型时把供应商过滤切到该模型的供应商 —— 这样弹层顶段的「供应商」
    // 跟实际生效的模型始终是同一家，不会出现「上面 A、下面选了 B」的不一致。
    const m = (modelChoices || []).find(function (x) { return x.id === id; });
    if (m && m.provider_id) {
      modelProvFilter = m.provider_id;
      provListOpen = false;
    }
    fetch('/api/models/apply', {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ model: id, session_id: sessionID })
    }).then(function (r) { return r.json(); }).then(function (d) {
      if (!d.ok) {
        // 上下文护栏命中（context_overflow）：用顶部横幅阻断并给出引导，
        // 不弹浏览器 alert —— 横幅可复用、且不阻塞页面。
        if (d.code === 'context_overflow') {
          showBanner(d.error || '当前会话上下文已超过该模型的窗口，无法切换', 'error');
          return;
        }
        showBanner('切换模型失败：' + (d.error || '未知错误'), 'error');
        return;
      }
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
      showBanner('已切换到 ' + (modelName || model), 'info');
    }).catch(function (e) {
      showBanner('切换模型失败：' + e, 'error');
    }).then(function () { switchingModel = false; });
  }

  // ---------- 思考强度滑条（按上游分级动态构建） ----------
  let slider = null;
  let dragRatio = null; // 拖动中的连续位置（线性跟随）；模块级供 closeAllPops 复位
  // document 级拖动监听的清理句柄：buildSlider 每次重建时先移除旧监听，
  // 避免模型切换等重建场景下重复挂载、旧闭包误触发。
  let detachSliderDrag = null;
  function buildSlider() {
    if (detachSliderDrag) { detachSliderDrag(); detachSliderDrag = null; }
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
    dragRatio = null; // 拖动中的连续位置（线性跟随）
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
      // 命中刻度点：先跳档，再进入拖动（不再 return —— 否则按在刻度点上会拖不动：
      // 既没 setPointerCapture 也没 dragging，按下后移动鼠标无响应，须移走重按才恢复）
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
      }
      // 无论命中刻度点与否都进入拖动：指针捕获失败时（指针移出弹层/元素被替换），
      // 全局 pointermove/pointerup 仍能收到事件，拖动不中断。
      try { slider.setPointerCapture(e.pointerId); } catch (_) { /* 合成事件/指针已释放时忽略 */ }
      slider.classList.add('dragging');
      const hit = nearestValue(e.clientX);
      dragRatio = hit.ratio;
      previewAt(dragRatio); // 线性跟随，不吸附
      tip.textContent = hit.v === thinkingVal ? thinkingLabel() : previewLabel(hit.v);
    });
    // 拖动中的 pointermove / pointerup / pointercancel 挂在 document：
    // slider 的 setPointerCapture 可能失败（指针移出弹层、元素重建等），此时事件
    // 按指针所在元素派发，绑在 slider 上会收不到 → 表现为「拖一下卡住，移走重按才好」。
    // document 级监听无论指针在哪都能收到（拖动不要求指针始终在滑块内）。
    document.addEventListener('pointermove', onSliderMove);
    document.addEventListener('pointerup', onSliderUp);
    document.addEventListener('pointercancel', onSliderCancel);
    detachSliderDrag = function () {
      document.removeEventListener('pointermove', onSliderMove);
      document.removeEventListener('pointerup', onSliderUp);
      document.removeEventListener('pointercancel', onSliderCancel);
    };
    function onSliderMove(e) {
      if (!slider.classList.contains('dragging')) return;
      const hit = nearestValue(e.clientX);
      dragRatio = hit.ratio;
      previewAt(dragRatio);
      tip.textContent = previewLabel(hit.v);
    }
    function onSliderUp(e) {
      if (!slider.classList.contains('dragging')) return;
      slider.classList.remove('dragging');
      dragRatio = null;
      // 松手：吸附最近档位（动画过渡），不关闭弹层
      const hit = nearestValue(e.clientX);
      thinkingVal = hit.v;
      saveThinking();
      syncLevelUI();
      renderModelBtn();
    }
    function onSliderCancel() {
      slider.classList.remove('dragging');
      dragRatio = null;
      syncLevelUI();
    }
  }
  function syncLevelUI() {} // spec 加载后由 buildSlider 覆盖
  modelBtn.addEventListener('click', function (e) {
    e.stopPropagation();
    const willOpen = modelPop.classList.contains('hidden') && !modelPop.classList.contains('leaving');
    closeAllPops(willOpen ? modelPop : null);
    if (willOpen) {
      modelPop.classList.remove('leaving', 'hidden');
      // 每次打开都把供应商过滤复位：**默认跟随当前生效模型**。
      // 否则上一次「切去别家浏览」的选择会一直留着，而生效模型可能已经
      // 从设置页换成别家的了 —— 顶上显示 A、实际生效的是 B，自相矛盾。
      // 只在本次展开期间保留浏览选择；关掉再开就重新跟随生效模型。
      modelProvFilter = '';
      provListOpen = false;
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
  // ---------- 任务清单面板（输入卡片上方，可折叠） ----------
  // todo 事件驱动：○待办 / ◐进行中 / ✓完成 / ✕取消；点标题折叠/展开。
  //
  // ⚠️ 位置很要紧：面板在 #composer-wrap 内、#composer **外**（见 index.html），
  //    所以它跟着输入卡片一起浮动，却不受输入卡片的折叠/居中 transform 影响。
  //    早先的做法是 messagesEl.prepend()，把面板塞进滚动容器的最顶部 ——
  //    一旦会话变长、用户往上翻了几屏，面板就跟着滚出可视区，
  //    表现成「模型报『已更新任务列表』但界面什么都看不见」。
  //    放进输入卡片上方后，它任何时候都在视野里，与滚动位置无关。
  //
  // 默认关闭：面板只在**清单非空**时出现。
  // 清单非空 ⟺ AI 真的调用过任务清单 —— 服务端推来的 todos 只有两种情况：
  // ① AI 调了 todo_write；② 载入的会话里存着上次的清单。两种都是「有内容」，
  // 都该显示；而空数组意味着这轮压根没有清单，留一个空壳面板长期占着输入框
  // 上方的一块地方只会碍事，所以直接整个隐藏（见 renderTodoBar 的 empty 分支）。
  let todoBar = null;         // 运行时绑定到 #todo-bar（HTML 里已写好骨架）
  const todoBarEl = $('#todo-bar');
  const TODO_COLLAPSE_KEY = 'cf_todo_collapsed';
  // 折叠状态跨会话保留：用户收起过一次，就不该在每轮工具调用后被强行展开。
  let todoCollapsed = false;
  try { todoCollapsed = localStorage.getItem(TODO_COLLAPSE_KEY) === '1'; } catch (e) {}

  function applyTodoCollapsed() {
    todoBarEl.classList.toggle('collapsed', todoCollapsed);
    const head = todoBarEl.querySelector('.todo-head');
    if (head) head.setAttribute('aria-expanded', todoCollapsed ? 'false' : 'true');
    // 折叠/展开改变了输入卡片的整体高度 → 消息区的底部留白要跟着重算，
    // 否则折叠后最后几条消息会被多出来的空白顶离底部。下一帧量，等样式落定。
    requestAnimationFrame(function () { syncComposerPadding(); });
  }
  function toggleTodoBar() {
    todoCollapsed = !todoCollapsed;
    try { localStorage.setItem(TODO_COLLAPSE_KEY, todoCollapsed ? '1' : '0'); } catch (e) {}
    applyTodoCollapsed();
  }
  // 折叠只改类，不重建内容 —— 早先的写法先 toggle 再整表重建，
  // 纯粹是为了刷新标题里的 ▾/▸，现在箭头交给 CSS 的 ::before，不需要重建。
  todoBarEl.querySelector('.todo-head').addEventListener('click', toggleTodoBar);
  todoBarEl.querySelector('.todo-head').addEventListener('keydown', function (e) {
    if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); toggleTodoBar(); }
  });

  function renderTodoBar(todos) {
    todoBar = todoBarEl;
    const list = todos || [];
    const body = todoBar.querySelector('.todo-body');
    body.innerHTML = '';
    // 空清单 → 整个面板隐藏（默认关闭）。先隐藏再返回，
    // 顺便把 body 清空，避免下次出现时闪过上一轮的旧条目。
    if (!list.length) {
      todoBar.querySelector('.todo-count').textContent = '';
      todoBar.classList.add('hidden');
      requestAnimationFrame(function () { syncComposerPadding(); }); // 面板消失后重算底部留白
      return;
    }
    todoBar.classList.remove('hidden');
    const done = list.filter(function (t) { return t.status === 'completed'; }).length;
    todoBar.querySelector('.todo-count').textContent = done + '/' + list.length;
    applyTodoCollapsed();      // 折叠类要在填内容后落好（内部会重算底部留白）
    const icon = { pending: '○', in_progress: '◐', completed: '✓', cancelled: '✕' };
    list.forEach(function (t) {
      const row = document.createElement('div');
      row.className = 'todo-row';
      const i = document.createElement('span');
      i.className = 'todo-icon st-' + (icon[t.status] ? t.status : 'pending');
      i.textContent = icon[t.status] || '○';
      const txt = document.createElement('span');
      txt.className = 'todo-text' + (t.status === 'completed' || t.status === 'cancelled' ? ' done' : '');
      txt.textContent = t.content;
      row.appendChild(i);
      row.appendChild(txt);
      body.appendChild(row);
    });
  }
  // ---------- 右下角「回到底部」浮动箭头 ----------
  // 会话变长、用户往上翻历史时出现；点击平滑回到底部并恢复自动跟随。
  // 面板本体写在 index.html（#to-bottom），显隐只切 .show 类。
  const toBottomBtn = $('#to-bottom');
  const FOLLOW_NEAR_BOTTOM = 80;  // 距底多少像素内算「在底部」，与输入卡片折叠的阈值同量级
  let followTail = true;          // 是否跟随最新内容（用户主动上滚即置 false）
  function isNearBottom() {
    return messagesEl.scrollHeight - messagesEl.scrollTop - messagesEl.clientHeight <= FOLLOW_NEAR_BOTTOM;
  }
  function syncToBottomBtn() {
    if (!toBottomBtn) return;
    toBottomBtn.classList.toggle('show', !isNearBottom());
  }
  // 内容追加时的滚底：只在「跟随态」执行。
  // 早先 scrollBottom() 无条件滚底，是「翻不上去」的主要元凶之一 ——
  // 模型流式吐字时每来一小段就把视口拽回底部，用户刚滚上去就被拉回。
  function scrollBottom(force) {
    if (!force && !followTail) {
      syncToBottomBtn();
      return;
    }
    messagesEl.scrollTop = messagesEl.scrollHeight;
    followTail = true;
    syncToBottomBtn();
  }
  if (toBottomBtn) {
    toBottomBtn.addEventListener('click', function () {
      followTail = true;
      messagesEl.scrollTo({ top: messagesEl.scrollHeight, behavior: 'smooth' });
      syncToBottomBtn();
    });
  }
  function addAssistant(text) {
    const d = document.createElement('div');
    d.className = 'msg-assistant';
    d.textContent = text;
    ensureCol().appendChild(d);
    scrollBottom();
  }
  // ---------- 消息区底部留白 ≈ 浮动输入卡片的实际高度 ----------
  // #messages 是滚动容器，输入卡片是**绝对定位浮在它上面**的，不占布局空间。
  // 于是必须在滚动内容里留出等高的空白，否则最后几条消息会被卡片永久盖住 ——
  // 滚到底也看不见，和「滚不下去」是同一个症状。
  //
  // 早先写死 padding-bottom:200px，而卡片高度是变数（任务清单展开/折叠、
  // 输入框随打字长高、底栏换行、任务清单列表变长），常量必然对不上。
  // 这里实测高度写进 CSS 变量，用 ResizeObserver 跟着卡片一起变。
  //
  // ⚠️ 这里自己取一次元素，不复用下面那个 `composerWrap`：那个 const 在文件
  //    更靠下的位置声明，在它之前访问会撞 TDZ（const 没有变量提升）。
  const composerPadEl = $('#composer-wrap');
  let lastComposerPad = -1;
  function syncComposerPadding() {
    const h = composerPadEl.offsetHeight;
    if (h <= 0) return;
    // 卡片离底部还留了 5vh 的空档，多补一点让最后一条能滚到舒服的位置
    const pad = Math.round(h + window.innerHeight * 0.07);
    if (pad === lastComposerPad) return;   // 值没变就不写 DOM，避免 ResizeObserver 抖动
    lastComposerPad = pad;
    document.documentElement.style.setProperty('--composer-pad-bottom', pad + 'px');
  }
  syncComposerPadding();
  if (typeof ResizeObserver === 'function') {
    new ResizeObserver(syncComposerPadding).observe(composerPadEl);
  }
  window.addEventListener('resize', syncComposerPadding);

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
    // 原文存在 dataset 上：编辑按钮要靠它与服务端下发的白名单（按文本匹配）对上号。
    // 用 textContent 的渲染结果反推是不行的 —— 提及高亮会把 @路径 缩成文件名。
    b.dataset.rawText = text;
    renderUserText(b, text);
    row.appendChild(b);
    ensureCol().appendChild(row);
    scrollBottom();
    syncComposerMode(); // 用户发出第一句话：输入卡片下放回底部
    syncUserEditButtons();
  }

  // 可编辑用户消息白名单：由服务端下发的 checkpoints 事件给出（back = 距最后一条的距离）。
  // 前端不自己推断「哪几条能编辑」—— 白名单规则（最近 N 条、且必须是纯文本发言）
  // 只有服务端知道，两边各算一份必然会漂。
  let editableByText = new Map(); // 文本 → back（同一文本重复出现时取最近的一条）
  function setEditables(list) {
    editableByText = new Map();
    (list || []).forEach(function (it) {
      if (it && typeof it.text === 'string' && typeof it.back === 'number') {
        editableByText.set(it.text, it.back);
      }
    });
    syncUserEditButtons();
  }

  // 就地刷新每条用户消息上的编辑按钮显隐（历史回放、新消息、白名单更新都会走到这里）。
  function syncUserEditButtons() {
    // 只给当前正在查看的会话挂按钮：后台会话的历史不在视图里，也不该出现可点的编辑。
    const rows = messagesEl.querySelectorAll('.msg-user');
    rows.forEach(function (row) {
      const bubble = row.querySelector('.bubble');
      if (!bubble) return;
      const btn = row.querySelector('.edit-btn');
      const raw = bubble.dataset.rawText || '';
      const back = editableByText.get(raw);
      // 运行中不给编辑入口：这一轮的上下文正在被模型消费，就地改写会让事件与历史错位。
      if (typeof back === 'number' && !running) {
        if (btn) {
          btn.dataset.back = String(back);
        } else {
          // 编辑按钮放在气泡**前面**（HTML 顺序），靠 CSS 的 order 决定视觉位置
          row.insertBefore(makeEditButton(raw, back), bubble);
        }
      } else if (btn) {
        btn.remove();
      }
    });
  }

  // 单条用户消息的「编辑」按钮（气泡左侧，hover 才显形）。
  function makeEditButton(rawText, back) {
    const btn = document.createElement('button');
    btn.type = 'button';
    btn.className = 'edit-btn';
    btn.dataset.back = String(back);
    btn.title = '编辑这条消息并重新发送';
    btn.innerHTML = '<svg viewBox="0 0 24 24" width="14" height="14" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"><path d="M12 20h9"/><path d="M16.5 3.5a2.1 2.1 0 0 1 3 3L7 19l-4 1 1-4Z"/></svg>';
    btn.addEventListener('click', function () {
      startEditMessage(rawText, Number(btn.dataset.back) || 0);
    });
    return btn;
  }
  function addInfo(text) {
    const d = document.createElement('div');
    d.className = 'msg-info';
    d.textContent = text;
    ensureCol().appendChild(d);
    scrollBottom();
    syncComposerMode();
    return d;
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
    todo_write:   '更新任务清单',
    web_fetch:    '读取网页',
    web_search:   '搜索网页',
    create_skill: 'Skill Creator 插件', // 内置插件工具：审批显示「需要审批：Skill Creator 插件」
    delegate_subagents: 'Multi-Agent 插件',
  };
  function toolPhrase(name) {
    // 与 toolLabel 同一套规则：插件工具（插件名.工具名）按去前缀的本名查表，
    // 兜底也只给中文描述 —— 任何情况下都不把工具 ID 显示给用户。
    //
    // ⚠️ name 可能缺失：历史里确实存在 name 为 undefined 的 tool_use 块
    // （实测某个会话 65 条消息里有 4 个）。裸用 name.indexOf 会抛 TypeError，
    // 而调用点在 replayHistory 里 —— 异常会**中断整次回放**，
    // 导致末尾的 renderSessions 跑不到、侧栏高亮停在上一个会话（2026-09-19 实际故障）。
    name = typeof name === 'string' ? name : '';
    const local = name.indexOf('.') > 0 ? name.slice(name.indexOf('.') + 1) : name;
    return toolPhrases[name] || toolPhrases[local] || '外部能力';
  }

  // 工具卡片文案：中文短语 + 目标（文件名 / 命令）。
  // 只取路径最后一段，避免长路径把卡片撑宽；完整参数仍在 title 悬浮提示里。
  // ⚠️ 未识别的工具**绝不显示工具 ID**（那是系统内部标识），只给中文兜底文案。
  function toolLabel(name, input) {
    // ⚠️ 同 toolPhrase：name 可能为 undefined，必须先归一化再做 indexOf。
    // 缺失时走 default 分支，返回「调用了外部能力」—— 既不崩，也不泄露工具 ID。
    name = typeof name === 'string' ? name : '';
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

    // 插件工具名形如「插件名.工具名」（如 parallel_search.web_search）：
    // 按去前缀后的本名匹配文案，否则默认分支会把生硬的限定名直接摊给用户。
    let local = name.indexOf('.') > 0 ? name.slice(name.indexOf('.') + 1) : name;
    // 同族工具用前缀匹配：Exa 给的是 web_search_exa / web_fetch_exa，
    // 精确匹配会落到默认分支、把工具 ID 显示给用户。
    if (local.indexOf('web_search') === 0) local = 'web_search';
    else if (local.indexOf('web_fetch') === 0) local = 'web_fetch';
    switch (local) {
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
      case 'todo_write':  return '更新了任务清单';
      case 'web_fetch': {
        // 内置 web_fetch 用 {url}；远程 MCP（Parallel）用 {urls:[…]}，两者都取首个地址
        let shown = '';
        if (input && typeof input === 'object') {
          if (input.url) shown = String(input.url);
          else if (Array.isArray(input.urls) && input.urls.length) shown = String(input.urls[0]);
        }
        const sliced = shown.length > 80 ? shown.slice(0, 80) + '…' : shown;
        return sliced ? '读取了网页 ' + sliced : '读取了网页';
      }
      case 'web_search': {
        // 内置 web_search 用 {query}；远程 MCP（Parallel）用 {objective, search_queries}
        let shown = '';
        if (input && typeof input === 'object') {
          if (input.query) shown = String(input.query);
          else if (input.objective) shown = String(input.objective);
          else if (Array.isArray(input.search_queries) && input.search_queries.length) shown = String(input.search_queries[0]);
        }
        const sliced = shown.length > 60 ? shown.slice(0, 60) + '…' : shown;
        return sliced ? '搜索了 ' + sliced : '搜索了网页';
      }
      // 兜底也不能吐工具 ID：工具名是系统内部标识，界面上只给中文能力描述
      default:             return '调用了外部能力';
    }
  }

  // activeToolEl：最近一条工具条目（运行中带 spinner；收到结果或开始下一段
  // 思考/正文时移除 spinner，让用户知道工具正在执行而不是卡死）。
  let activeToolEl = null;
  let pendingToolEl = null; // 「正在生成工具调用参数…」占位（tool_call 到达后移除）
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

  // 把后端抛出的 LLM 错误串分类成一句人话，供最终错误显示（「出错了」）使用。
  // 与 retryReasonBrief 的区别：这里不只给「上游限流」这种标签，而是直接给可理解的原因，
  // 优先识别配额耗尽这类永久性错误（重试无用，必须明确告诉用户去充值/换密钥）。
  function describeLLMError(raw) {
    const s = String(raw == null ? '' : raw).trim();
    if (!s) return '未知错误';
    const low = s.toLowerCase();
    if (/quota_exceeded|quota exceeded|insufficient_quota|insufficient quota|balance|欠费|余额不足/.test(low)) {
      return '模型配额已用尽（余额不足或额度用尽）。请在「设置 > 模型」中更换模型、充值或更换 API Key 后重试。';
    }
    // 模型不支持图片输入
    if (/cannot read.*image|does not support image|image.*not support|unsupported image|image input.*not supported/i.test(s)) {
      return '当前模型不支持图片输入。请在「设置 > 模型」中切换到支持多模态的模型（如 GPT-4o、Claude 3.5 Sonnet 等），或移除图片后重试。';
    }
    // 上游错误体里的 message 常是现成的人话（如「该模型当前访问量过大，请您稍后再试」），
    // 优先带出来；没有才按 HTTP 状态码分类给通用提示。
    const msg = s.match(/"message"\s*:\s*"([^"\\]{1,120})"/);
    if (msg) return msg[1];
    const m = s.match(/\((\d{3})\)/);
    const code = m ? Number(m[1]) : 0;
    if (code === 401) return 'API 密钥无效或未授权（401）。请在「设置 > 模型」中检查密钥。';
    if (code === 403) return '当前密钥无权限访问该模型（403）。请在「设置 > 模型」中更换模型或密钥。';
    if (code === 429) return '请求过于频繁（429），请稍后再试。';
    if (code >= 500) return '上游服务异常（' + code + '），请稍后再试。';
    return s;
  }

  // 把后端原始错误串压成一句人话：用户第一眼要的是「为什么失败」，不是 HTTP 报文。
  // 例：'LLM 请求失败 (429): {"error":{"code":"1305","message":"该模型当前访问量过大，请您稍后再试"}}'
  //     → '上游限流：该模型当前访问量过大，请您稍后再试'
  function retryReasonBrief(reason) {
    const s = String(reason == null ? '' : reason).trim();
    if (!s) return '未知原因';
    let brief;
    const m = s.match(/\((\d{3})\)/);
    if (m) {
      const code = Number(m[1]);
      if (/quota_exceeded|quota exceeded|insufficient_quota|insufficient quota/i.test(s)) {
        brief = '模型配额已用尽';
      } else if (code === 429) brief = '上游限流';
      else if (code === 503) brief = '上游过载';
      else if (code === 502 || code === 504) brief = '上游网关异常';
      else if (code >= 500) brief = '上游服务异常';
      else if (code >= 400) brief = '请求被上游拒绝';
      else brief = '上游返回 ' + code;
    } else if (/timeout|timed out|超时/i.test(s)) {
      brief = '请求超时';
    } else if (/EOF|connection reset|refused|no such host|Bad Gateway|dial tcp/i.test(s)) {
      brief = '网络连接异常';
    } else {
      brief = '网络异常';
    }
    // 上游错误体里常带一句现成的人话（如「该模型当前访问量过大，请您稍后再试」），尽量带出来
    const msg = s.match(/"message"\s*:\s*"([^"\\]{1,80})"/);
    return msg ? brief + '：' + msg[1] : brief;
  }

  // 上游瞬时故障自动重试提示：样式同「编辑文件」类文字条目，点击展开/收起完整错误原因。
  let retryEl = null;
  // 未回复打断的重试圆环：用户在模型未回复前点击打断，显示圆环，点击重新发送
  let retryRingEl = null;
  let hasModelReplied = false; // 标记模型是否已开始回复（收到 text/reasoning/tool_call）
  function addRetry(reason, attempt, maxAttempts) {
    const brief = retryReasonBrief(reason);
    // 次数信息直接写进主文案：只给一句笼统的「正在重试」，用户分不清偶发抖动与持续故障
    const times = attempt > 0
      ? '（第 ' + attempt + (maxAttempts > 0 ? '/' + maxAttempts : '') + ' 次 · ' + brief + '）'
      : '（' + brief + '）';
    if (!retryEl) {
      retryEl = document.createElement('div');
      retryEl.className = 'msg-tool retry';
      retryEl.title = '点击展开/收起完整错误信息';
      const label = document.createElement('span');
      label.className = 'retry-label';
      const more = document.createElement('span');
      more.className = 'retry-more';
      more.textContent = '详情';
      const detail = document.createElement('span');
      detail.className = 'retry-reason';
      retryEl.appendChild(label);
      retryEl.appendChild(more);
      retryEl.appendChild(detail);
      // 点击切换显示/隐藏完整错误原因
      retryEl.addEventListener('click', function () {
        detail.classList.toggle('show');
        more.textContent = detail.classList.contains('show') ? '收起' : '详情';
      });
      ensureCol().appendChild(retryEl);
      scrollBottom();
    }
    // 主文案每轮重试都刷新（次数与原因都会变），完整原因留给点击展开
    retryEl.querySelector('.retry-label').textContent = '请求失败，正在重试…' + times;
    retryEl.querySelector('.retry-reason').textContent = '原因：' + (reason || '未知错误');
  }
  function removeRetry() {
    if (retryEl) { retryEl.remove(); retryEl = null; }
  }
  // 未回复打断的重试圆环
  function showRetryRing() {
    if (retryRingEl) return;
    retryRingEl = document.createElement('button');
    retryRingEl.type = 'button';
    retryRingEl.className = 'retry-ring';
    retryRingEl.title = '重新发送（保留上下文）';
    retryRingEl.innerHTML = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5" stroke-linecap="round" stroke-linejoin="round"><path d="M1 4v6h6M23 20v-6h-6"/></svg>';
    retryRingEl.addEventListener('click', function () {
      removeRetryRing();
      // 重新发送：使用 lastUserText（保留上下文），走正常发送流程
      form.requestSubmit();
    });
    ensureCol().appendChild(retryRingEl);
    scrollBottom();
  }
  function removeRetryRing() {
    if (retryRingEl) { retryRingEl.remove(); retryRingEl = null; }
  }

  // ---------- 可复用操作选择面板（ActionPanel）：输入框上方多选 / 用户输入 / 多页 ----------
  // 配置（spec）：
  //   title     面板标题（如「计划书已生成」）
  //   pages     [{ title?, options: [{label, primary?, send?, value?}] }]
  //               option.send    点击后要发送的文本（可含 {input} 占位，替换为用户输入）
  //               option.value   有值时不发送，而是回调 onPick(value)（自定义动作）
  //               option.primary 主按钮（强调色）
  //   placeholder 底部输入框占位（提供 = 显示用户输入行；发送即调用 options 中 send 含 {input} 的项）
  //   onPick    自定义动作回调（value 型 option 触发）
  // 关闭动作：任何 option / 关闭按钮 / 输入发送后都会收起面板。
  let actionPanelShown = false; // 本轮回复是否已触发过（避免 idle 反复弹）
  function showActionPanel(spec) {
    const host = $('#action-panel');
    if (!host) return;
    host.innerHTML = '';
    host.classList.add('visible');
    host.classList.toggle('multi-page', (spec.pages || []).length > 1);
    if ((spec.pages || []).length === 1 && spec.pages[0].options.length <= 1) {
      host.classList.remove('multi-page');
    }

    let pageIdx = 0;
    const pages = spec.pages || [{ options: spec.options || [] }];

    const header = document.createElement('div');
    header.className = 'ap-header';
    const title = document.createElement('span');
    title.className = 'ap-title';
    title.textContent = spec.title || '选择操作';
    const close = document.createElement('button');
    close.type = 'button';
    close.className = 'ap-close';
    close.textContent = '×';
    close.title = '关闭';
    close.addEventListener('click', function () { hideActionPanel(); });
    header.appendChild(title);
    header.appendChild(close);

    const body = document.createElement('div');
    body.className = 'ap-body';

    const foot = document.createElement('div');
    foot.className = 'ap-foot';
    const input = document.createElement('input');
    input.type = 'text';
    input.className = 'ap-input';
    input.placeholder = spec.placeholder || '';
    input.autocomplete = 'off';
    const go = document.createElement('button');
    go.type = 'button';
    go.className = 'ap-go';
    go.textContent = '发送';
    foot.appendChild(input);
    foot.appendChild(go);

    const pagesRow = document.createElement('div');
    pagesRow.className = 'ap-pages';

    function draw() {
      body.innerHTML = '';
      pagesRow.innerHTML = '';
      const page = pages[pageIdx] || { options: [] };
      (page.options || []).forEach(function (opt) {
        const b = document.createElement('button');
        b.type = 'button';
        b.className = 'ap-opt' + (opt.primary ? ' primary' : '');
        b.textContent = opt.label;
        b.addEventListener('click', function () { handleOption(opt); });
        body.appendChild(b);
      });
      if (pages.length > 1) {
        host.classList.add('multi-page');
        pages.forEach(function (p, i) {
          const dot = document.createElement('span');
          dot.className = 'ap-dot' + (i === pageIdx ? ' on' : '');
          dot.addEventListener('click', function () { pageIdx = i; draw(); });
          pagesRow.appendChild(dot);
        });
      }
      if (page.title) title.textContent = page.title; else if (spec.title) title.textContent = spec.title;
    }
    function handleOption(opt) {
      if (opt.value !== undefined) {
        hideActionPanel();
        if (opt.onPick) opt.onPick(opt.value);
        return;
      }
      // send 型：把 {input} 替换为用户输入框内容（无输入则原样发送）
      const extra = input.value.trim();
      const text = (opt.send || '').replace(/\{input\}/g, extra || '');
      if (!text) { input.focus(); return; }
      hideActionPanel();
      sendAsUserText(text);
    }
    // 底部输入「发送」：触发当前页所有 send 型 option（用{input}替换的发送完整文案）
    go.addEventListener('click', function () {
      const cur = pages[pageIdx] || {};
      const opt = (cur.options || []).filter(function (o) { return o.send !== undefined; })[0];
      if (!opt) { hideActionPanel(); return; }
      handleOption(opt);
    });
    input.addEventListener('keydown', function (e) {
      if (e.key === 'Enter') { e.preventDefault(); go.click(); }
    });

    draw();
    host.appendChild(header);
    host.appendChild(body);
    foot.classList.toggle('has-input', true);
    host.classList.toggle('has-input', typeof spec.placeholder === 'string' && spec.placeholder !== '');
    if (host.classList.contains('has-input')) host.appendChild(foot);
    if (pages.length > 1) host.appendChild(pagesRow);
    input.focus();
  }
  function hideActionPanel() {
    const host = $('#action-panel');
    if (host) host.classList.remove('visible');
  }
  // 发送一条用户消息（复用输入框提交链路：别名展开 / 气泡 / 发送）。运行中不允许。
  function sendAsUserText(text) {
    if (running || !text || !wsReady) { if (text) addError('正在处理中，请稍后再试'); return; }
    // 直接走 form.requestSubmit：会把 input 的内容清空并显示气泡 —— 但我们要发送的是
    // 面板触发的文本，不是输入框内容。为了复用展开链路，先临时借用 input。
    if (!input) return;
    const keep = input.value;
    sending = false;
    input.value = text;
    form.requestSubmit();
    input.value = keep;
    syncInputMirror();
  }
  // @plan 计划书回复完成后：输入框上方弹出「直接发送 / 取消」（可复用 ActionPanel）。
  function maybeShowPlanActions() {
    if (actionPanelShown || running) return;
    // 只认用户主动 @plan（允许 @ plan / 大小写），别被回复里的「计划书」字样误触发
    if (!lastUserText || !/@\s*plan/i.test(lastUserText)) return;
    actionPanelShown = true;
    showActionPanel({
      title: '计划书已生成，接下来？',
      pages: [{
        title: '计划书已生成，接下来？',
        options: [
          { label: '直接发送（按计划执行）', primary: true,
            send: '请按照上面的计划书开始执行{input}' },
          { label: '取消', value: 'cancel' }
        ]
      }],
      placeholder: '补充指令（可选，随发送带上）'
    });
  }
  // 发送新消息时重置 ActionPanel 触发状态（下一条 @plan 仍能弹）
  function resetPlanActions() { actionPanelShown = false; }

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
  let composerEpoch = 0;
  let sessionChanging = false;
  let workspaceChanging = false;
  let pendingUploads = 0;
  let uploadQueue = Promise.resolve();
  let sending = false;
  const uploadAliasWorkspace = new Map();
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

  // 首帧该开哪个会话：
  //   1. 地址栏带 ?s=<会话ID> —— codeforge -continue / -resume 的深链，优先照办；
  //   2. 否则回到本工作区最近更新的那条（刷新/重启自动恢复）。
  // 深链会话属于别的工作区时先切工作区再载入：在一个项目里回放另一个项目的
  // 对话，模型下一步改的就是错项目的文件。
  function restoreStartSession() {
    // 开场自动恢复（不论深链还是最近会话）都属于「回放历史」：
    // 位置修正要直接落位，不该让用户看见输入卡片从中间滑到底部。
    composerSnap = true;
    const id = consumeSessionDeepLink();
    if (!id) {
      if (sessionsCache.length) loadSession(sessionsCache[0].id);
      return;
    }
    const meta = sessionsCache.find(function (s) { return s.id === id; });
    const target = meta ? (meta.workspace || '') : '';
    if (target && target !== workspaceRoot) {
      addInfo('该会话属于 ' + target + '，正在切换工作区');
      Promise.resolve(setWorkspace(target)).then(function (r) {
        if (r && r.ok === false) addError('切换工作区失败：' + (r.error || '') + '，已在原工作区打开该会话');
        loadSession(id);
      });
      return;
    }
    loadSession(id);
  }

  function consumeSessionDeepLink() {
    try {
      const u = new URL(location.href);
      const id = u.searchParams.get('s') || '';
      if (!id) return '';
      // 用完即摘：留在地址栏里会让下一次刷新强行跳回这条会话，盖掉用户后来的选择
      u.searchParams.delete('s');
      history.replaceState(null, '', u.toString());
      return id;
    } catch (e) {
      return '';
    }
  }

  function wsSend(obj) {
    if (!ws || ws.readyState !== WebSocket.OPEN) return false;
    ws.send(JSON.stringify(obj));
    return true;
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

  // ---------- 网页内顶部通知横幅（可复用，**不用浏览器 Notification**）----------
  const topBanner = document.getElementById('top-banner');
  let bannerTimer = 0;
  function showBanner(msg, level, sticky) {
    if (!topBanner) return;
    topBanner.textContent = String(msg == null ? '' : msg);
    topBanner.className = 'top-banner ' + (level || 'info');
    void topBanner.offsetWidth; // 强制重排，连续两次也能重播进入动画
    if (bannerTimer) { clearTimeout(bannerTimer); bannerTimer = 0; }
    if (!sticky) {
      bannerTimer = setTimeout(hideBanner, level === 'error' ? 6000 : 4000);
    }
  }
  function hideBanner() {
    if (!topBanner) return;
    topBanner.classList.add('hidden');
    if (bannerTimer) { clearTimeout(bannerTimer); bannerTimer = 0; }
  }

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
    const hit = Number(d.cache_hit) || 0;
    const miss = Number(d.cache_miss) || 0;
    const hitRate = (hit + miss) > 0 ? (hit / (hit + miss) * 100).toFixed(1) + '%' : '—';
    const rows = [
      ['模型名称', d.model || d.model_id || '—'],
      ['上下文长度', d.window ? fmtTokens(d.window) + ' tokens' : '—'],
      ['已使用总 tokens', fmtTokens(d.total_tokens) + ' tokens'],
      ['缓存命中', fmtTokens(hit) + ' tokens'],
      ['缓存未命中', fmtTokens(miss) + ' tokens'],
      ['平均缓存命中率', hitRate],
    ];
    rows.forEach(function (r) { ctxPop.appendChild(ctxRow(r[0], r[1])); });

    const level = pct >= 90 ? ' danger' : (pct >= 70 ? ' warn' : '');
    if (d.over_budget && !d.compressed) {
      const tip = document.createElement('div');
      tip.className = 'ctx-tip danger';
      tip.textContent = '已越过压缩线，正在压缩上下文…';
      ctxPop.appendChild(tip);
      return; // 自动压缩正在进行，别再点一次手动压缩
    }
    if (d.compressed) {
      const tip = document.createElement('div');
      tip.className = 'ctx-tip' + level;
      tip.textContent = '已压缩：前 ' + summarized +
        ' 条历史被压成摘要送入模型，完整历史仍保留在会话里（可正常回看）。';
      ctxPop.appendChild(tip);
    }
    ctxPop.appendChild(ctxCompressBox());
  }

  // 「立即压缩上下文」：不等占比涨到 100% 被动手压，用户可现在就让出窗口。
  // 走的是与自动压缩完全相同的一条摘要路径（同保留段预算、同样不动完整历史），
  // 差别只在触发时机；压缩后「本会话读过哪些文件」的登记会一并作废。
  // 上一次手动压缩的结果。必须存成状态而不是只写进 DOM：
  // 压缩成功后会主动拉一次占用，回包会重绘整个面板，只写进节点的话那句结果
  // 会在几十毫秒内被默认文案冲掉 —— 用户点了按钮却看不到国果。
  // 记住它属于哪条会话：切到别的会话时不该把上一条的压缩结果带过去。
  let ctxManualNote = '', ctxManualNoteFor = '';

  function ctxCompressBox() {
    const box = document.createElement('div');
    box.className = 'ctx-act';
    const btn = document.createElement('button');
    btn.type = 'button';
    btn.className = 'ctx-compress';
    btn.textContent = '立即压缩上下文';
    btn.title = '把较早的历史并入摘要，最近几轮保留原文；会调用一次模型生成摘要';
    const hint = document.createElement('div');
    hint.className = 'ctx-act-hint';
    hint.textContent = (ctxManualNoteFor === sessionID && ctxManualNote) ? ctxManualNote :
      '占比到 100% 时会自动压缩；也可以现在手动腾出窗口。';
    btn.addEventListener('click', function () { compressContextNow(btn, hint); });
    box.appendChild(btn);
    box.appendChild(hint);
    return box;
  }

  async function compressContextNow(btn, hint) {
    btn.disabled = true;
    btn.textContent = '压缩中…';
    const owner = sessionID;
    try {
      const r = await fetch('/api/context/compress', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ session_id: sessionID })
      });
      const d = await r.json().catch(function () { return {}; });
      if (r.ok && d.ok) {
        const saved = Math.max(0, (Number(d.before) || 0) - (Number(d.after) || 0));
        ctxManualNote = '已并入 ' + (Number(d.added) || 0) + ' 条历史：' +
          fmtTokens(d.before) + ' → ' + fmtTokens(d.after) + ' tokens（省 ' + fmtTokens(saved) + '）。';
        ctxManualNoteFor = owner;
        showBanner('上下文已压缩：' + ctxManualNote, 'info');
        wsSend({ type: 'context', session_id: sessionID }); // 拉最新占用，进度条要跟着降下来
      } else {
        // 409（任务在跑 / 没有较早历史）与 502（摘要失败）都照实说明原因
        ctxManualNote = (d && d.error) || ('压缩未完成（HTTP ' + r.status + '）');
        ctxManualNoteFor = owner;
        hint.textContent = ctxManualNote;
      }
    } catch (e) {
      ctxManualNote = '压缩请求失败：' + (e.message || '服务不可达');
      ctxManualNoteFor = owner;
      hint.textContent = ctxManualNote;
    } finally {
      btn.disabled = false;
      btn.textContent = '立即压缩上下文';
    }
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
      let confirmed = false;
      b.addEventListener('click', function () {
        if (it.confirmDelete && !confirmed) {
          confirmed = true;
          b.textContent = '确认删除';
          return;
        }
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

      // 📂 文件夹图标（SVG 描边式，颜色随文字，黑白灰不抢眼）
      const folder = document.createElement('span');
      folder.className = 'ws-folder';
      const fsvg = svgIcon('M22 19a2 2 0 0 1-2 2H4a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h5l2 3h9a2 2 0 0 1 2 2z', 14);
      fsvg.setAttribute('stroke', 'currentColor');
      fsvg.setAttribute('fill', 'none');
      fsvg.setAttribute('stroke-width', '2');
      fsvg.setAttribute('stroke-linecap', 'round');
      fsvg.setAttribute('stroke-linejoin', 'round');
      folder.appendChild(fsvg);

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
        items.push({ label: '删除项目', danger: true, confirmDelete: true, fn: function () {
          deleteWorkspace(ws);
        } });
        showInlineMenu(dots, items);
      });

      head.appendChild(fold);
      head.appendChild(folder); // 📂 项目文件夹图标
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
              { label: '永久删除', danger: true, confirmDelete: true, fn: function () { deleteSession(s); } },
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
    if (sending || workspaceChanging || sessionChanging) { addInfo('正在发送或切换，请稍候'); return; }
    if (!wsSend({ type: 'new_session', title: '' })) return;
    composerEpoch++;
    sessionChanging = true;
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
    if (pendingUploads || sending || workspaceChanging || sessionChanging) {
      setNpError('文件上传、发送或切换尚未完成，请稍候');
      return;
    }
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
    if (pendingUploads || sending || workspaceChanging || sessionChanging) {
      addInfo('文件上传、发送或切换尚未完成，请稍候再删除项目');
      return;
    }
    workspaceChanging = true;
    composerEpoch++;
    return fetch('/api/workspaces?workspace=' + encodeURIComponent(ws || ''), { method: 'DELETE' })
      .then(function () {
        if ((ws || '') === (workspaceRoot || '')) {
          workspaceRoot = '';
          sessionID = '';
          messagesEl.innerHTML = '';
          msgCol = null; currentTextEl = null; textBuffer = '';
          reasonEl = null; reasonBuffer = ''; lastReply = '';
        }
        loadSessionList();
      }).catch(function (err) { addError('删除项目失败：' + err.message); })
      .finally(function () { workspaceChanging = false; });
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
    if (sending || workspaceChanging || sessionChanging) { addInfo('正在发送或切换，请稍候'); return; }
    if (!wsReady) return;
    composerEpoch++;
    sessionChanging = true;
    if (running) sendBtn.title = '点击打断' + (id === runSessionID ? '' : '（正在运行的会话）');
    wsSend({ type: 'load_session', session_id: id });
  }

  // 回放历史消息：服务端 history 事件 → 用渲染原语重建聊天列。
  // 审批条与 spinner 不重建（历史是既成事实）；末条助手回复带操作栏。
  function replayHistory(ev) {
    composerEpoch++;
    sessionChanging = false;
    messagesEl.innerHTML = '';
    msgCol = null; currentTextEl = null; textBuffer = '';
    reasonEl = null; reasonBuffer = ''; reasonPinned = false;
    // 视图重建 = 旧 DOM 全部作废：这些「当前元素」引用必须一起清空，
    // 否则后续事件会去找已经不在文档里的节点（切走→切回最容易触发）。
    thinkingEl = null; activeToolEl = null; retryEl = null; pendingToolEl = null;
    subagentCards.clear();
    lastReply = ''; lastUserText = '';
    sessionID = ev.session_id || '';

    (ev.messages || []).forEach(function (m) {
      const blocks = m.content || [];
      blocks.forEach(function (b) {
        // ⚠️ 单块渲染失败**绝不能**让整次回放中断：本函数末尾还有 renderSessions
        // （重打侧栏高亮）、scrollBottom、syncComposerMode 等状态同步。
        // 一旦中途抛出，会话切了但侧栏高亮还停在上一个会话 —— 实测就是这么发生的
        // （一个 name 缺失的 tool_use 块把 65 条消息的回放整个打断）。
        // 这里吞掉异常并打日志：回放是「尽力重建视图」，坏一块不该毁掉整屏。
        try {
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
        } catch (err) {
          console.warn('[replay] 跳过无法渲染的历史块', b && b.type, err);
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
    // 切会话/回放历史后一律回到「跟随最新」状态：用户对新会话的默认预期是看最新内容，
    // 而不是继承上一个会话「当时翻到了中间」的跟随状态。
    followTail = true;
    scrollBottom(true);
    syncComposerMode(); // 空会话回放 → 居中；有历史 → 下放底部
    // 切会话后旧白名单属于上一个会话：先清掉，等 checkpoints 事件到达再挂按钮。
    // 各条消息的 dataset.rawText 也一并作废（气泡已被重建）。
    editableByText = new Map();
    syncUserEditButtons();
    renderSessions(sessionsCache); // 高亮切换后的 active
  }

  // ---------- MCP 入口（仅查看；新增/启停/删除在 设置 → MCP 服务） ----------
  // 展示 type=mcp（stdio）与 type=mcp-http（远程 Streamable HTTP）且已启用的插件。
  function renderMcpList() {
    const list = document.getElementById('mcp-list');
    list.innerHTML = '';
    fetch('/api/plugins').then(function (r) { return r.json(); }).then(function (d) {
      const items = (d.items || []).filter(function (p) {
        return (p.type === 'mcp' || p.type === 'mcp-http') && p.configured;
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
          : '已启用，但连接未建立（加载失败：请到 设置 → MCP 服务 检查端点/启动命令）';
        const nm = document.createElement('span');
        nm.textContent = p.name + (p.enabled ? '' : '（加载失败）');
        nm.title = (p.description || p.name) +
          (p.endpoint ? '：' + p.endpoint : (p.command ? '：' + p.command + ' ' + (p.args || []).join(' ') : '')) +
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
        box.innerHTML = '<div class="mi-empty">还没有 MCP 服务，切到「添加服务」新建一个</div>';
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
  // 类型切换：stdio 要「启动命令 + 参数 + 环境变量」，远程只要「端点 URL」。
  // 用 .hidden 类而非 hidden 属性——.f-field 是 display:flex，属性会被样式覆盖。
  const mcpTypeSel = document.getElementById('mcp-f-type');
  function syncMcpFormByType() {
    const remote = mcpTypeSel && mcpTypeSel.value === 'mcp-http';
    function toggle(id, show) {
      const el = document.getElementById(id);
      if (el) el.classList.toggle('hidden', !show);
    }
    toggle('mcp-f-cmd-field', !remote);
    toggle('mcp-f-args-field', !remote);
    toggle('mcp-f-env-field', !remote);
    toggle('mcp-f-endpoint-field', remote);
  }
  if (mcpTypeSel) {
    mcpTypeSel.addEventListener('change', syncMcpFormByType);
    syncMcpFormByType();
  }
  document.getElementById('mcp-f-add').addEventListener('click', function () {
    const result = document.getElementById('mcp-f-result');
    function fail(msg) { result.className = 'mf-test-result fail'; result.textContent = msg; }
    const name = document.getElementById('mcp-f-name').value.trim();
    const type = mcpTypeSel ? mcpTypeSel.value : 'mcp';
    const remote = type === 'mcp-http';
    const cmd = document.getElementById('mcp-f-cmd').value.trim();
    const endpoint = document.getElementById('mcp-f-endpoint').value.trim();
    const args = document.getElementById('mcp-f-args').value.trim();
    const envRaw = document.getElementById('mcp-f-env').value.trim();
    const desc = document.getElementById('mcp-f-desc').value.trim();
    if (!name) { fail('名称不能为空'); return; }
    // 校验与服务端同源（handlePlugins POST），前端先拦一遍给出即时反馈
    if (remote) {
      if (!/^https?:\/\/\S+$/i.test(endpoint)) { fail('端点必须是以 http:// 或 https:// 开头的完整 URL'); return; }
    } else if (!cmd) {
      fail('本地（stdio）类型必须填写启动命令'); return;
    }
    const env = {};
    envRaw.split(/\s+/).forEach(function (kv) {
      const i = kv.indexOf('=');
      if (i > 0) env[kv.slice(0, i)] = kv.slice(i + 1);
    });
    result.className = 'mf-test-result';
    result.textContent = '添加中…';
    fetch('/api/plugins', {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({
        name: name, type: type,
        command: cmd, endpoint: endpoint,
        args: args ? args.split(/\s+/) : [],
        description: desc || (remote ? '远程 MCP 服务' : 'MCP 服务'),
        env: env
      })
    }).then(function (r) { return r.json(); }).then(function (d) {
      if (d.ok) {
        document.getElementById('mcp-f-name').value = '';
        document.getElementById('mcp-f-cmd').value = '';
        document.getElementById('mcp-f-endpoint').value = '';
        document.getElementById('mcp-f-args').value = '';
        document.getElementById('mcp-f-env').value = '';
        document.getElementById('mcp-f-desc').value = '';
        result.className = 'mf-test-result ok';
        result.textContent = d.warning || '已添加并热加载';
        renderMcpSettings();
        renderMcpList();
        // 添加成功后跳回「已注册服务」，新条目就在列表顶部
        setMcpPane('mcp-pane-list');
      } else {
        fail(d.error || '添加失败');
      }
    });
  });
  // 分栏切换：已注册服务 / 添加服务。切到列表时刷新一次（停用/删除后状态可能已过期）。
  function setMcpPane(paneId) {
    const page = document.getElementById('page-mcp');
    if (!page) return;
    page.querySelectorAll('.mcp-tab').forEach(function (t) {
      t.classList.toggle('active', t.dataset.pane === paneId);
    });
    page.querySelectorAll('.mcp-pane').forEach(function (p) {
      p.classList.toggle('active', p.id === paneId);
    });
    if (paneId === 'mcp-pane-list') renderMcpSettings();
  }
  document.querySelectorAll('#page-mcp .mcp-tab').forEach(function (t) {
    t.addEventListener('click', function () { setMcpPane(t.dataset.pane); });
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
      sending = false;
      sessionChanging = false;
      composerEpoch++;
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
          sendBtn.title = '发送';
          renderSessions(ev.sessions || []);
          if (!sessionID) restoreStartSession();
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
            if (sessionID !== ev.session_id) composerEpoch++;
            sessionID = ev.session_id;
            sessionChanging = false;
          }
          break;
        case 'todo':
          // 任务清单只画当前视图会话的（load_session/new_session/工具写入都会带 session_id）
          // 兼容 sessionID 尚未就绪（新建会话时）：允许空 sessionID 直接渲染
          if (ev.session_id && (sessionID === '' || ev.session_id === sessionID)) renderTodoBar(ev.todos || []);
          break;
        case 'checkpoints':
          // 服务端下发的可编辑白名单 + 回滚点（切会话、每轮结束、编辑后都会来）。
          // 只认当前视图会话的：后台会话的白名单不该影响这一屏的按钮。
          if (!ev.session_id || ev.session_id === sessionID) {
            setEditables(ev.editables || []);
            checkpointSteps = ev.steps || [];
          }
          break;
        case 'edit':
          // 历史已被截断到这条用户消息：把视图清到该点，重新流式渲染新回复。
          // 服务端紧接着会发 busy + 新一轮事件，这里只需把「下方旧内容」清干净。
          if (!ev.session_id || ev.session_id === sessionID) {
            beginEditedView(ev.text || '');
          }
          break;
        case 'rewind':
          // 文件已按检查点回滚：给一条可见反馈（含还原/删除/失败计数）。
          if (ev.result) addInfo(describeRewind(ev.result));
          break;
        case 'context':
          // 上下文占用只画当前视图会话的（后台会话结束也会下发它自己的 context）
          if (!ev.session_id || ev.session_id === sessionID) renderCtxUsage(ev);
          break;
case 'busy':
          sending = false;
          running = true;
          runSessionID = sessionID;
          runReason = ''; runText = '';
          lastReply = '';
          hasModelReplied = false; // 新一轮开始：重置回复标记
          removeRetry();
          removeRetryRing();
          resetSubagentCards();
          sendBtn.classList.add('running');
          sendBtn.title = '点击打断';
          showThinking();
          syncRunBadges();
          // 运行中不给编辑入口（上下文正在被消费）：按钮在 syncUserEditButtons 里统一摘掉。
          syncUserEditButtons();
          break;
        case 'retry':
          if (runAway()) break; // 后台会话的重试提示不画进当前视图
          removeThinking();
          addRetry(ev.error || '', Number(ev.attempt) || 0, Number(ev.max_attempts) || 0);
          break;
        case 'steer':
          // 服务端已把转向指令并入上下文：气泡标注从「等待并入」变成「已并入」，
          // 让用户看见话确实被接住了，而不是石沉大海。
          if (runAway()) break;
          markSteerMerged();
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
          hasModelReplied = true;
          removeRetryRing();
          runReason += ev.text || '';
          if (runAway()) break;
          removeThinking();
          removeRetry();
          settleActiveTool();
          appendReason(ev.text || '');
          break;
        case 'text':
          hasModelReplied = true;
          removeRetryRing();
          runReason = '';
          runText += ev.text || '';
          if (runAway()) { foldReason(); break; }
          removeThinking();
          removeRetry();
          foldReason();
          settleActiveTool();
          appendText(ev.text || '');
          break;
        case 'tool_pending': {
          // 工具调用参数正在流式生成（大参数要生成几十 KB）：立即给出反馈，
          // 不然这几分钟界面看起来像卡死。真正的 tool_call 卡片到达后替换。
          runReason = ''; runText = '';
          if (runAway()) { foldReason(); closeText(); break; }
          removeThinking();
          removeRetry();
          foldReason();
          closeText();
          if (pendingToolEl) pendingToolEl.remove();
          pendingToolEl = addInfo('正在生成工具调用参数…');
          break;
        }
        case 'tool_call': {
          hasModelReplied = true;
          if (pendingToolEl) { pendingToolEl.remove(); pendingToolEl = null; }
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
          if (pendingToolEl) { pendingToolEl.remove(); pendingToolEl = null; }
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
          sending = false;
          sessionChanging = false;
          if (runAway()) {
            // 后台会话出错：在当前视图标注来源，不能静默吞掉
            const emeta = sessionsCache.find(function (s) { return s.id === runSessionID; });
            addError('后台会话「' + ((emeta && emeta.title) || runSessionID) + '」出错：' + describeLLMError(ev.error));
            break;
          }
          removeThinking();
          removeRetry();
          removeRetryRing();
          settleActiveTool();
          if (pendingToolEl) { pendingToolEl.remove(); pendingToolEl = null; }
          foldReason();
          closeText();
          addError(describeLLMError(ev.error || '未知错误'));
          break;
        }
case 'idle': {
          sending = false;
          const backHome = !runAway();
          removeThinking();
          removeRetry();
          removeRetryRing();
          settleActiveTool();
          if (pendingToolEl) { pendingToolEl.remove(); pendingToolEl = null; }
          foldReason();
          closeText();
          if (backHome && lastReply) addActions(lastReply);
          running = false;
          runSessionID = '';
          runReason = ''; runText = '';
          hasModelReplied = false;
          sendBtn.classList.remove('running');
          sendBtn.title = '发送';
          syncRunBadges();
          // 一轮结束：重新按白名单挂上编辑按钮（最近 N 条用户消息）。
          syncUserEditButtons();
          maybeShowPlanActions();
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
    if (pendingUploads || sending || workspaceChanging || sessionChanging) {
      return Promise.resolve({ ok: false, error: '文件上传、发送或切换尚未完成，请稍候再切换工作区' });
    }
    const previous = workspaceRoot;
    workspaceChanging = true;
    composerEpoch++;
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
      if (!res.ok) workspaceRoot = previous;
      return res;
    }).catch(function (err) {
      workspaceRoot = previous;
      return { ok: false, error: err.message || '无法切换工作区' };
    }).finally(function () { workspaceChanging = false; });
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

  // ---------- 编辑重发 ----------
  // 进入编辑态：把该条用户消息的文字放回输入框，上方露出「发送 / 取消」。
  // 发送时走 edit_user_message（而非普通 user_message），服务端会截断到那条消息、
  // 回退压缩态并（默认）回滚其后的文件改动，然后重跑 —— 新回复覆盖下方旧内容。
  const editBar = $('#edit-bar');
  let editing = null; // { back:number, original:string } —— 非 null 即处于编辑态

  function isEditing() { return !!editing; }

  function startEditMessage(text, back) {
    if (running) { addInfo('正在处理中，请稍后再试'); return; }
    if (!wsReady) { addError('未连接到服务，请稍候重试'); return; }
    if (editing) exitEditMode(false); // 已在编辑另一条：先复位再切过去
    editing = { back: back, original: text };
    input.value = text;
    syncInputMirror();
    if (editBar) editBar.classList.remove('hidden');
    input.focus();
    // 光标放到末尾，方便直接在原话上修改
    try { input.setSelectionRange(text.length, text.length); } catch (_) {}
  }

  // 退出编辑态。restore=true 时把输入框恢复成进入编辑前的样子（取消按钮用）。
  function exitEditMode(restore) {
    if (!editing) return;
    if (restore) {
      input.value = '';
      syncInputMirror();
    }
    editing = null;
    if (editBar) editBar.classList.add('hidden');
  }

  // 编辑态下点「发送」：校验后走 edit_user_message。
  function submitEdit() {
    if (!editing) return;
    const text = String(input.value).trim();
    if (!text) { addError('编辑后的内容不能为空'); return; }
    if (running) { addInfo('正在处理中，请稍后再试'); return; }
    if (!wsReady) { addError('未连接到服务，请稍候重试'); return; }

    const back = editing.back;
    const snap = composerSnapshot();
    sendEditRequest(back, text, snap);
  }

  function sendEditRequest(back, text, snap) {
    sending = true;
    // 先在前端把「被编辑消息之后」的内容删掉：既立刻给出反馈，也避免等
    // 服务端事件回来之前旧内容仍在屏幕上与新回复混排。服务端随后还会重放历史。
    truncateAfterEditableMessage(back);
    if (!wsSend({
      type: 'edit_user_message',
      session_id: snap.session,
      text: text,
      back: back,
      rollback_files: true,
      thinking: thinkingVal
    })) {
      sending = false;
      addError('未连接到服务，请稍候重试');
      return;
    }
    lastUserText = text;
    exitEditMode(true);
    composerSnap = false;
    showThinking();
  }

  // 删除界面中「被编辑那条用户消息之后」的全部节点（含那条消息自身的旧气泡，
  // 因为重跑后服务端会把改过的消息重新推出来）。
  // 从后往前扫，遇到第 (back+1) 条用户消息即停 —— 与 back 的语义一致。
  function truncateAfterEditableMessage(back) {
    const col = ensureCol();
    let remaining = back + 1;
    let node = col.lastChild;
    while (node) {
      const prev = node.previousSibling;
      const isUserRow = node.classList && node.classList.contains('msg-user');
      node.remove();
      if (isUserRow) {
        remaining--;
        if (remaining <= 0) break;
      }
      node = prev;
    }
    // 清掉流式状态引用（它们指向已被删除的节点）
    currentTextEl = null; textBuffer = '';
    reasonEl = null; reasonBuffer = '';
    lastReply = '';
    scrollBottom();
  }

  if (editBar) {
    const sendEditBtn = editBar.querySelector('#edit-send');
    const cancelEditBtn = editBar.querySelector('#edit-cancel');
    if (sendEditBtn) sendEditBtn.addEventListener('click', submitEdit);
    if (cancelEditBtn) cancelEditBtn.addEventListener('click', function () { exitEditMode(true); });
  }

  // 最近一次收到的检查点列表（回滚菜单的数据源；空数组 = 没有可回滚的步骤）。
  let checkpointSteps = [];

  // 服务端确认历史已截断到被编辑的那条消息：把聊天列清空，只留这条消息，
  // 后续的流式事件会把新回复画在它下面 —— 视觉上就是「覆盖掉下面的内容」。
  function beginEditedView(text) {
    composerEpoch++;
    messagesEl.innerHTML = '';
    msgCol = null; currentTextEl = null; textBuffer = '';
    reasonEl = null; reasonBuffer = ''; reasonPinned = false;
    thinkingEl = null; activeToolEl = null; retryEl = null; pendingToolEl = null;
    subagentCards.clear();
    lastReply = '';
    if (text) addUser(text);
    lastUserText = text;
    followTail = true;   // 视图整体重建 = 从零开始看，回到跟随态
    syncComposerMode();
  }

  // 把回滚结果转成一句人话（文件为空时说明「没有需要回退的改动」）。
  function describeRewind(res) {
    if (!res) return '已回滚';
    const n = (res.paths || []).length;
    if (n === 0) return '没有需要回退的文件改动';
    let msg = '已回退 ' + n + ' 个文件的改动';
    if (res.restored) msg += '（还原 ' + res.restored + '）';
    if (res.deleted) msg += '（删除 ' + res.deleted + '）';
    if (res.failed) msg += '，' + res.failed + ' 个失败';
    return msg;
  }

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

  function pasteFiles(e) {
    const clipboard = e.clipboardData;
    if (!clipboard) return;
    let files = Array.from(clipboard.files || []);
    if (!files.length) {
      files = Array.from(clipboard.items || []).map(function (item) {
        return item.kind === 'file' && typeof item.getAsFile === 'function' ? item.getAsFile() : null;
      }).filter(Boolean);
    }
    if (!files.length) return;
    e.preventDefault();
    if (!workspaceRoot) { addError('还没有选择工作区，请先选择工作区再粘贴文件'); return; }
    if (workspaceChanging || sessionChanging || sending) { addInfo('正在发送或切换，请稍候再粘贴文件'); return; }
    const snap = composerSnapshot();
    const text = clipboard.getData ? clipboard.getData('text/plain') : '';
    if (text) insertIntoInput(text);
    pendingUploads += files.length;
    uploadQueue = uploadQueue.then(async function () {
      for (const file of files) {
        try {
          if (!sameComposer(snap)) continue;
          if (file.size > 20 * 1024 * 1024) throw new Error('单文件不能超过20MiB');
          const body = new FormData();
          body.append('file', file);
          const res = await fetch('/api/upload_file', { method: 'POST', body: body });
          const d = await res.json();
          if (!sameComposer(snap)) continue;
          if (!res.ok || !d.ok || typeof d.staged_path !== 'string' || !d.staged_path) {
            throw new Error(d.error || '上传未返回有效附件路径');
          }
          insertUploadedFile(file.name || d.name || 'image.png', d.staged_path, snap.workspace);
        } catch (err) {
          if (sameComposer(snap)) addError('文件 ' + file.name + ' 上传失败：' + (err.message || '服务不可达'));
        } finally {
          pendingUploads--;
        }
      }
    });
    return uploadQueue;
  }
  input.addEventListener('paste', pasteFiles);

  function insertUploadedFile(name, path, workspace) {
    const base = String(name).replace(/["\r\n]/g, '_');
    const dot = base.lastIndexOf('.');
    const stem = dot > 0 ? base.slice(0, dot) : base;
    const ext = dot > 0 ? base.slice(dot) : '';
    const occupied = new Set(String(input.value).split(MENTION_SPLIT));
    let token = mentionToken(base), n = 2;
    while (fileAlias.has(token) || occupied.has(token)) {
      token = mentionToken(stem + ' (' + n++ + ')' + ext);
    }
    fileAlias.set(token, path);
    uploadAliasWorkspace.set(token, workspace);
    insertIntoInput(token);
  }

  function composerSnapshot() {
    return { epoch: composerEpoch, session: sessionID, workspace: workspaceRoot };
  }

  function sameComposer(snap) {
    return snap.epoch === composerEpoch && snap.session === sessionID &&
      snap.workspace === workspaceRoot && !workspaceChanging && !sessionChanging;
  }

  function fileMentions(text) {
    const out = [];
    String(text).split(MENTION_SPLIT).forEach(function (p, i) {
      if (i % 2 !== 1 || !p || p.length < 2) return;
      const body = mentionBody(p);
      if (/[\\/]/.test(body) || /[^.\s]\.[A-Za-z0-9]{1,16}$/.test(body)) out.push(body);
    });
    return out;
  }

  function attachmentKey(path) {
    let p = String(path).replace(/\\/g, '/');
    if (/^[A-Za-z]:/.test(workspaceRoot)) p = p.toLowerCase();
    let root = workspaceRoot.replace(/\\/g, '/').replace(/\/+$/, '');
    if (/^[A-Za-z]:/.test(root)) root = root.toLowerCase();
    if (p.indexOf(root + '/') === 0) p = p.slice(root.length + 1);
    return p.replace(/\/+/g, '/').replace(/(^|\/)\.\//g, '$1');
  }

  async function prepareMentions(text, display) {
    const snap = composerSnapshot();
    const notes = [], attachments = [], seen = new Set(), staged = new Map();
    const files = fileMentions(text);
    if (files.length && !workspaceRoot) throw new Error('还没有选择工作区，请先选择工作区');
    for (const f of files) {
      if (!sameComposer(snap)) throw new Error('会话或工作区已切换，请重新发送');
      const key = attachmentKey(f);
      let path = staged.get(key) || f;
      const relative = !/^(?:[\\/]|[A-Za-z]:)/.test(f) && !f.split(/[\\/]/).includes('..');
      if (!staged.has(key) && !insideWorkspace(f) && !relative) {
        const res = await fetch('/api/stage_file', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ path: f })
        });
        const d = await res.json();
        if (!sameComposer(snap)) throw new Error('会话或工作区已切换，请重新发送');
        if (!res.ok || !d.ok || typeof d.staged_path !== 'string' || !d.staged_path) {
          throw new Error('文件暂存失败：' + (d.error || '无有效附件路径'));
        }
        path = d.staged_path;
        staged.set(key, path);
        if (!d.inside) {
          notes.push('已把 ' + d.name + ' 复制到工作区 attachments/，模型将通过副本访问');
        }
      }
      if (path !== f) {
        text = text.split(MENTION_SPLIT).map(function (part, i) {
          return (i % 2 === 1 && mentionBody(part) === f) ? mentionToken(path) : part;
        }).join('');
      }
      const finalKey = attachmentKey(path);
      if (!seen.has(finalKey)) { seen.add(finalKey); attachments.push(path); }
    }
    return { text: text, display: display === undefined ? text : display, notes: notes, attachments: attachments };
  }

  // ---------- 运行中转向（steering）与打断（cancel）----------
  // 转向：任务不换、方向换。指令交给正在跑的循环，在下一个步骤边界并入上下文，
  // 已经产生的工具结果全部保留 —— 不打断正在飞行中的那次请求，也不清空历史。
  // 打断：输入框为空时的 Esc / 发送，让循环在边界处收摊（工具结果仍然留存）。
  function steerNow() {
    const raw = String(input.value || '').trim();
    if (!raw) return false;
    // 转向的对象是「正在跑的那一轮」：用户可能已经把视图切到别的会话。
    const target = runSessionID || sessionID;
    if (!target) { addError('还没有会话可转向，先发送一条消息'); return true; }
    if (!wsSend({ type: 'steer', session_id: target, text: raw })) {
      addError('未连接到服务，这条转向没发出去');
      return true;
    }
    input.value = '';
    syncInputMirror();
    if (target === sessionID) addSteer(raw);
    else addInfo('已把这条指令转向到后台正在运行的任务');
    return true;
  }

  function markSteerMerged() {
    const tags = document.querySelectorAll('.msg-steer .steer-tag');
    const last = tags[tags.length - 1];
    if (last) last.textContent = '转向 · 已并入上下文，本步起生效';
  }

  // 转向气泡：与普通用户消息同一角色（模型看到的就是用户插话），
  // 但标注「转向」并说明何时生效，避免用户以为任务被打断了。
  function addSteer(text) {
    const row = document.createElement('div');
    row.className = 'msg-user msg-steer';
    const b = document.createElement('div');
    b.className = 'bubble';
    b.dataset.rawText = text;
    const tag = document.createElement('span');
    tag.className = 'steer-tag';
    tag.textContent = '转向 · 下一步生效，不打断当前任务';
    b.appendChild(tag);
    const body = document.createElement('div');
    renderUserText(body, text);
    b.appendChild(body);
    row.appendChild(b);
    ensureCol().appendChild(row);
    scrollBottom();
  }

  async function submitMessage(e, override) {
    e.preventDefault();
    // 编辑态下按发送（含 Shift+Enter / 发送按钮）走编辑重发，而不是普通新消息。
    if (isEditing() && override === undefined) { submitEdit(); return; }
    if (pendingUploads) { addInfo('文件正在上传，请等待上传完成后发送'); return; }
    if (sending) { addInfo('消息正在发送，请勿重复提交'); return; }
    if (workspaceChanging || sessionChanging) { addInfo('正在切换会话或工作区，请稍候'); return; }
    if (running) {
      // 运行中按发送 = 转向：任务继续跑，只是中途换个方向（已产生的工具结果保留）。
      // 输入框是空的才算「打断」。
      if (String(input.value).trim()) { steerNow(); return; }
      if (runAway()) addInfo('已请求打断正在后台运行的任务');
      wsSend({ type: 'cancel' });
      // 未回复的打断：显示重试圆环，点击可从用户输入重新开始（保留上下文）
      if (!hasModelReplied) showRetryRing();
      return;
    }
    const raw = String(override === undefined ? input.value : override).trim();
    if (!raw) return;
    if (!wsReady) { addError('未连接到服务，请稍候重试'); return; }
    const snap = composerSnapshot();
    const original = input.value;
    const socket = ws;
    sending = true;
    try {
      const outgoing = expandFileAliases(raw);
      const p = await prepareMentions(outgoing, raw);
      if (!sameComposer(snap)) throw new Error('会话或工作区已切换，请重新发送');
      if (override === undefined && input.value !== original) throw new Error('输入已更改，请确认后重新发送');
      if (!wsReady || socket !== ws) throw new Error('连接已更改，请重新发送');
      if (!wsSend({
        type: 'user_message',
        session_id: snap.session,
        text: p.text,
        attachments: p.attachments,
        thinking: thinkingVal
      })) throw new Error('未连接到服务，请稍候重试');
      if (override === undefined) {
        input.value = '';
        syncInputMirror();
      }
      lastUserText = p.text; // 记录供「重新生成」（用发送文本，可直接重放）
      resetPlanActions(); // 新用户消息发出：下一条 @plan 回复完成后可再次弹出选择面板
      composerSnap = false; // 用户主动发出第一句：位置切换要有下放动画
      // 刚发出提问 = 明确想看接下来的回答：无论此前翻到哪，都回到跟随态。
      // 否则用户上翻看完历史后发问，视口会停在原地、看不到任何回复，像是「没反应」。
      followTail = true;
      // 气泡显示 p.display（= 用户原本输入的样子，@文件名 保持蓝色），
      // 模型拿到的仍是 p.text（真实路径 / attachments 暂存路径）——显示与发送分离。
      addUser(p.display);
      p.notes.forEach(function (n) { addInfo(n); });
      // 不等服务端 busy 往返：用户消息发出后，模型尚未回复的空窗立即显示提示。
      showThinking();
    } catch (err) {
      sending = false;
      addError('发送失败：' + (err.message || '服务不可达'));
    }
  }
  form.addEventListener('submit', submitMessage);

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
  // 拉技能 + 内置插件（不含 MCP 服务），供 @ 提及面板展示（显示名 + 简要介绍）。
  function loadAtData() {
    if (atData || atLoading) return;
    atLoading = true;
    Promise.all([
      fetch('/api/skills').then(function (r) { return r.json(); }).catch(function () { return {}; }),
      fetch('/api/builtin-plugins').then(function (r) { return r.json(); }).catch(function () { return {}; })
    ]).then(function (res) {
      atLoading = false;
      const skills = ((res[0] || {}).items || []).filter(function (sk) { return sk.enabled; });
      const builtins = ((res[1] || {}).items || []).filter(function (p) { return p.enabled; });
      atData = {
        // 技能：显示名 display_name（缺省回退 name），简介 description
        skills: skills.map(function (sk) {
          return { name: sk.name, display_name: sk.display_name || sk.name, description: sk.description || '', builtin: false };
        }),
        // 内置插件（Plan 等）：显示名 name，简介 purpose + when_to_use 首句
        plugins: builtins.map(function (p) {
          return { builtin: true, id: p.id, name: p.name, description: p.purpose || p.when_to_use || '', enabled: true };
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
    const hit = function (d) { return !q || (d + '').toLowerCase().indexOf(q) >= 0; };
    const skills = atData.skills.filter(function (sk) {
      return hit(sk.display_name + ' ' + sk.name + ' ' + sk.description);
    });
    const plugins = atData.plugins.filter(function (p) {
      return hit(p.name + ' ' + p.description);
    });
    if (!skills.length && !plugins.length) {
      closeAtPop();
      if (q) return; // 过滤没命中：直接收起
      // 一条都没有：给提示（否则用户会以为 @ 坏了）
      atPop = document.createElement('div');
      atPop.className = 'at-skill-pop';
      const hint = document.createElement('div');
      hint.className = 'at-empty';
      hint.textContent = '还没有可提及的技能或内置插件。';
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
    title.textContent = '提及技能 / 内置插件（↑↓ 选择，Enter 确认，Esc 取消）';
    atPop.appendChild(title);
    function addGroup(text) {
      const g = document.createElement('div');
      g.className = 'at-group';
      g.textContent = text;
      atPop.appendChild(g);
    }
    // 一条可选行；dataset.idx 指向 atItems 下标，供点击与高亮对齐（分组标题不参与选择）
    function addRow(display, desc, note, meta) {
      const b = document.createElement('button');
      b.type = 'button';
      const idx = atItems.length;
      const item = { name: display || '' };
      // 插入 @ 提及用的 token：内置插件用 id（@plan），技能用 slug（@codeforge-build）
      if (meta) {
        if (meta.kind === 'builtin') { item.builtin = true; item.id = meta.id; }
        else { item.token = meta.token || display || ''; }
      }
      atItems.push(item);
      b.dataset.idx = String(idx);
      b.textContent = display;
      if (desc) {
        const d = document.createElement('span');
        d.className = 'sk-desc-inline';
        d.textContent = desc;
        b.appendChild(d);
      }
      if (meta && meta.note) {
        const t = document.createElement('span');
        t.className = 'at-tools';
        t.textContent = meta.note;
        b.appendChild(t);
      }
      if (meta && meta.disabled) b.classList.add('at-disabled');
      b.addEventListener('click', function () { pickAtItem(idx); });
      atPop.appendChild(b);
    }
    if (skills.length) {
      addGroup('技能');
      skills.forEach(function (sk) {
        addRow(sk.display_name, sk.description, { kind: 'skill', token: sk.name, display: sk.display_name });
      });
    }
    if (plugins.length) {
      addGroup('内置插件');
      plugins.forEach(function (p) {
        addRow(p.name, p.description, { kind: 'builtin', id: p.id || '', note: '@提及即触发', display: p.name });
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
    // 内置插件插入 @id（@plan），技能插入 @slug（@codeforge-build），其余回退显示名
    const token = it.builtin ? it.id : (it.token || it.name);
    const before = input.value.slice(0, atStart);
    const after = input.value.slice(input.selectionStart);
    input.value = before + '@' + token + ' ' + after;
    syncInputMirror();
    closeAtPop();
    input.focus();
    const pos = (before + '@' + token + ' ').length;
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
    fileAlias.forEach(function (_, tok) {
      if (!alive.has(tok)) { fileAlias.delete(tok); uploadAliasWorkspace.delete(tok); }
    });
  }
  function mentionToken(name) {
    return /[\s"@]/.test(name) ? '@"' + name + '"' : '@' + name;
  }
  // 把输入框里的「@文件名」别名展开成真实路径；没有别名的 token 原样保留
  //（例如用户手打的 @技能名 / @完整路径）。
  // ⚠️ 展开后必须重新按 mentionToken 的规则加引号：真实路径可能含空白
  //（`C:\...\新建 文本文档.txt`），裸着写回去会被 MENTION_SPLIT / fileMentions
  // 在空格处切断，暂存出一个不存在的路径。
  function expandFileAliases(text) {
    if (!fileAlias.size) return text;
    return String(text).split(MENTION_SPLIT).map(function (part) {
      if (uploadAliasWorkspace.has(part) && uploadAliasWorkspace.get(part) !== workspaceRoot) {
        throw new Error('附件属于其他工作区，请重新粘贴文件');
      }
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
    Promise.all([
      fetch('/api/skills').then(function (r) { return r.json(); }).catch(function () { return {}; }),
      fetch('/api/builtin-plugins').then(function (r) { return r.json(); }).catch(function () { return {}; })
    ]).then(function (res) {
      const skills = ((res[0] || {}).items || []).filter(function (sk) { return sk.enabled; });
      // 内置插件（Plan 等）也在此列出：点击插入 @id（触发词）
      const builtins = ((res[1] || {}).items || []).filter(function (p) { return p.enabled; })
        .map(function (p) { return { name: p.name, desc: p.purpose || p.when_to_use || '', at: '@' + p.id }; });
      moreSkillsPop.innerHTML = '';
      if (!skills.length && !builtins.length) {
        const e = document.createElement('div');
        e.className = 'more-sub-empty';
        e.textContent = '还没有已加载的技能或内置插件';
        moreSkillsPop.appendChild(e);
        return;
      }
      skills.forEach(function (sk) {
        const b = document.createElement('button');
        b.type = 'button';
        b.className = 'pop-opt more-sub-item';
        b.textContent = sk.display_name || sk.name; // 显示名优先，缺省回退 slug
        if (sk.description) {
          const s = document.createElement('span');
          s.className = 'sk-desc-inline';
          s.textContent = sk.description;
          b.appendChild(s);
        }
        b.addEventListener('click', function (e) {
          e.stopPropagation();
          setMoreOpen(false);
          insertIntoInput('@' + sk.name); // 插入用 slug，确保后端命中
        });
        moreSkillsPop.appendChild(b);
      });
      builtins.forEach(function (bp) {
        const b = document.createElement('button');
        b.type = 'button';
        b.className = 'pop-opt more-sub-item';
        b.textContent = bp.name;
        if (bp.desc) {
          const s = document.createElement('span');
          s.className = 'sk-desc-inline';
          s.textContent = bp.desc;
          b.appendChild(s);
        }
        b.addEventListener('click', function (e) {
          e.stopPropagation();
          setMoreOpen(false);
          insertIntoInput(bp.at);
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
  // 同一个 scroll 回调里顺带维护两件与「当前位置」有关的事：
  //   ① followTail：用户主动上滚就停止自动跟随，滚回底部附近就恢复。
  //      没有这一条，「向上翻历史」会被不断追加的内容拽回底部 —— 翻不上去。
  //   ② 右下角「回到底部」箭头的显隐。
  const composerWrap = $('#composer-wrap');
  let composerHide = 0;          // 当前下移像素（0 = 完全显示）
  let lastMsgScroll = messagesEl.scrollTop;

  messagesEl.addEventListener('scroll', function () {
    const st = messagesEl.scrollTop;
    const delta = st - lastMsgScroll;
    lastMsgScroll = st;
    // 是否仍在跟随最新内容：用「是否贴近底部」判定，而不是靠滚动方向。
    // 方向判定会被「内容增长把 scrollTop 顶大」误认成用户在向下滚。
    followTail = isNearBottom();
    syncToBottomBtn();
    if (composerCentered) return; // 居中态（无消息，无可滚动内容）不参与折叠
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
    // 视图清空（新建会话 / 归档 / 删除 / 切回空会话）一律回到跟随态：
    // 这里是最集中的一处，上面每个清空 messagesEl 的分支都会走到，不必逐个补。
    if (empty) {
      followTail = true;
      syncToBottomBtn();
    }
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
  // 「工具调用轮数」卡片：回填服务端当前值，保存时 POST /api/config（热生效 + 落盘）。
  function refreshMaxStepsForm() {
    fetch('/api/config').then(function (r) { return r.json(); }).then(function (cfg) {
      var el = document.getElementById('max-steps');
      if (el) { el.value = cfg.max_steps > 0 ? cfg.max_steps : 500; }
    }).catch(function () { /* 读取失败保持默认 */ });
  }
  (function bindMaxStepsForm() {
    var btn = document.getElementById('max-steps-save');
    if (!btn) return;
    btn.addEventListener('click', function () {
      var v = Math.max(1, Number(document.getElementById('max-steps').value) || 500);
      var result = document.getElementById('max-steps-result');
      btn.disabled = true;
      if (result) { result.textContent = '保存中…'; result.style.color = ''; }
      fetch('/api/config', {
        method: 'POST', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ max_steps: v })
      }).then(function (r) { return r.json().then(function (d) { return { ok: r.ok, d: d }; }); })
        .then(function (res) {
          if (result) {
            if (res.ok) { result.textContent = '已保存'; result.style.color = 'var(--accent, #4c9aff)'; }
            else { result.textContent = res.d.error || '保存失败'; result.style.color = ''; }
          }
          if (!res.ok) addError('轮数设置保存失败：' + (res.d.error || '未知错误'));
        })
        .catch(function () {
          if (result) { result.textContent = '服务不可达'; result.style.color = ''; }
          addError('轮数设置保存失败（服务不可达）');
        })
        .then(function () { btn.disabled = false; });
    });
  })();
  // 「请求重试」卡片：回填服务端当前策略，保存时 POST /api/config（热生效 + 落盘）。
  function refreshRetryForm() {
    fetch('/api/config').then(function (r) { return r.json(); }).then(function (cfg) {
      var a = document.getElementById('retry-attempts');
      var m = document.getElementById('retry-mode');
      var i = document.getElementById('retry-interval');
      if (!a || !m || !i) return;
      a.value = cfg.retry_max_attempts || 5;
      m.value = cfg.retry_mode === 'fixed' ? 'fixed' : 'backoff';
      i.value = cfg.retry_interval_sec > 0 ? cfg.retry_interval_sec : 1;
    }).catch(function () { /* 读取失败保持默认 */ });
  }
  (function bindRetryForm() {
    var btn = document.getElementById('retry-save');
    if (!btn) return;
    btn.addEventListener('click', function () {
      var attempts = Math.max(1, Math.min(15, Number(document.getElementById('retry-attempts').value) || 5));
      var interval = Math.max(1, Math.min(60, Number(document.getElementById('retry-interval').value) || 1));
      var mode = document.getElementById('retry-mode').value;
      var result = document.getElementById('retry-result');
      btn.disabled = true;
      if (result) { result.textContent = '保存中…'; result.style.color = ''; }
      fetch('/api/config', {
        method: 'POST', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          retry_max_attempts: attempts,
          retry_mode: mode,
          retry_interval_sec: interval
        })
      }).then(function (r) { return r.json().then(function (d) { return { ok: r.ok, d: d }; }); })
        .then(function (res) {
          if (result) {
            if (res.ok) { result.textContent = '已保存'; result.style.color = 'var(--accent, #4c9aff)'; }
            else { result.textContent = res.d.error || '保存失败'; result.style.color = ''; }
          }
          if (!res.ok) addError('重试设置保存失败：' + (res.d.error || '未知错误'));
        })
        .catch(function () {
          if (result) { result.textContent = '服务不可达'; result.style.color = ''; }
          addError('重试设置保存失败（服务不可达）');
        })
        .then(function () { btn.disabled = false; });
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
    refreshRetryForm();
    refreshMaxStepsForm();
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
  // Esc 打断 / 转向：任务在跑时按 Esc —— 输入框里有话就先转向（换个方向继续跑），
  // 空着才是打断。任何弹层（设置、新建项目、终端选择、文件选择、@ 面板、更多菜单）
  // 开着时一律让位：那些界面的 Esc 优先，这里不能抢。
  document.addEventListener('keydown', function (e) {
    if (e.key !== 'Escape' || !running || atPop || moreOpen) return;
    if (['settings-overlay', 'newproj-overlay', 'termux-overlay', 'picker-overlay']
      .some(function (id) {
        const el = document.getElementById(id);
        return el && !el.classList.contains('hidden');
      })) return;
    e.preventDefault();
    if (String(input.value || '').trim()) { steerNow(); return; }
    if (runAway()) addInfo('已请求打断正在后台运行的任务');
    wsSend({ type: 'cancel' });
    if (!hasModelReplied) showRetryRing();
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

  // ---------- 液态玻璃（设置→外观开关；localStorage 记忆，默认关闭） ----------
  function applyLiquidGlass(on) {
    document.body.classList.toggle('liquid-glass', !!on);
    const el = document.getElementById('opt-liquid-glass');
    if (el) el.checked = !!on;
  }
  (function initLiquidGlass() {
    const el = document.getElementById('opt-liquid-glass');
    if (!el) return;
    applyLiquidGlass(localStorage.getItem('cf_liquid_glass') === '1'); // 默认关闭
    el.addEventListener('change', function () {
      localStorage.setItem('cf_liquid_glass', el.checked ? '1' : '0');
      applyLiquidGlass(el.checked);
    });
  })();

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

  // ---------- 外观：自定义背景图（最底层 fixed 层，blur/亮度即时生效） ----------
  const bgLayer = (function () {
    const el = document.createElement('div');
    el.id = 'bg-layer';
    document.body.insertBefore(el, document.body.firstChild);
    return el;
  })();
  let bgSaveTimer = null;
  let bgBright = 100;        // 当前亮度（%），供「深浅色自适应」判断
  let bgAvgLum = null;       // 背景图平均亮度 0..1（按图片缓存）
  let bgLumFor = '';         // bgAvgLum 对应的图片 URL

  // ---------- 深浅色自适应：保证文字永远清楚 ----------
  // 需求：亮度调低 → 整页变暗 → 文字必须转白，否则深色文字压在暗背景上完全看不见。
  //
  // 判据不是「滑条值」而是**图片实际有多亮**：一张亮图调到 60% 可能仍然偏亮，
  // 一张暗图即使 100% 也已经够暗。所以先量出图片的平均亮度，再乘上亮度系数。
  const BG_DARK_THRESHOLD = 0.4;   // 有效亮度低于此值 → 切浅色文字

  function syncBgTheme() {
    if (!bgLayer.classList.contains('on') || bgAvgLum === null) {
      // 没有背景图（或图还没量出来）→ 维持默认浅色主题
      document.body.classList.remove('bg-dark');
      return;
    }
    const effective = bgAvgLum * (bgBright / 100);
    document.body.classList.toggle('bg-dark', effective < BG_DARK_THRESHOLD);
  }

  // 量背景图的平均亮度：缩到 32×32 再读像素，代价可忽略。
  // 图片走同源接口（/api/appearance/background），canvas 不会被跨域污染。
  function loadBgLuminance(url, done) {
    if (bgLumFor === url && bgAvgLum !== null) { done(); return; }
    const img = new Image();
    img.onload = function () {
      try {
        const N = 32;
        const cv = document.createElement('canvas');
        cv.width = N; cv.height = N;
        const ctx = cv.getContext('2d', { willReadFrequently: true });
        ctx.drawImage(img, 0, 0, N, N);
        const d = ctx.getImageData(0, 0, N, N).data;
        let sum = 0;
        for (let i = 0; i < d.length; i += 4) {
          sum += 0.2126 * d[i] + 0.7152 * d[i + 1] + 0.0722 * d[i + 2];
        }
        bgAvgLum = sum / (d.length / 4) / 255;
        bgLumFor = url;
      } catch (e) {
        bgAvgLum = null;   // 量不出来就退回浅色主题，不让主题卡在半路
      }
      done();
    };
    img.onerror = function () { bgAvgLum = null; done(); };
    img.src = url;
  }

  function applyBgFilters(blur, bright) {
    bgBright = bright;
    bgLayer.style.filter = 'blur(' + blur + 'px) brightness(' + bright + '%)';
    syncBgTheme();   // 亮度一变就重判深浅色
  }
  function applyBgImage(on) {
    bgLayer.classList.toggle('on', !!on);
    document.body.classList.toggle('has-bg', !!on);
    if (on) {
      const url = '/api/appearance/background?t=' + Date.now();
      bgLayer.style.backgroundImage = 'url(' + url + ')';
      loadBgLuminance(url, syncBgTheme);
    } else {
      bgLayer.style.backgroundImage = '';
      bgAvgLum = null; bgLumFor = '';
      syncBgTheme();
    }
  }
  function saveBg(patch) {
    return fetch('/api/appearance', {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(patch)
    }).catch(function () {});
  }
  (function initBackground() {
    const pickBtn = document.getElementById('bg-pick');
    const clearBtn = document.getElementById('bg-clear');
    const blurEl = document.getElementById('opt-bg-blur');
    const brightEl = document.getElementById('opt-bg-bright');
    if (!pickBtn || !blurEl || !brightEl) return;
    const blurVal = document.getElementById('opt-bg-blur-val');
    const brightVal = document.getElementById('opt-bg-bright-val');

    // 回填：模糊/亮度优先 localStorage（即时），再以服务端为准
    let blur = parseInt(localStorage.getItem('cf_bg_blur'), 10); if (isNaN(blur)) blur = 0;
    let bright = parseInt(localStorage.getItem('cf_bg_bright'), 10); if (isNaN(bright)) bright = 100;
    blurEl.value = blur; blurVal.textContent = blur + 'px';
    brightEl.value = bright; brightVal.textContent = bright + '%';
    applyBgFilters(blur, bright);

    fetch('/api/appearance').then(function (r) { return r.json(); }).then(function (d) {
      if (typeof d.blur === 'number' && !localStorage.getItem('cf_bg_blur_touched')) {
        blur = d.blur; blurEl.value = blur; blurVal.textContent = blur + 'px'; applyBgFilters(blur, bright);
      }
      if (typeof d.brightness === 'number' && !localStorage.getItem('cf_bg_bright_touched')) {
        bright = d.brightness; brightEl.value = bright; brightVal.textContent = bright + '%'; applyBgFilters(blur, bright);
      }
      if (d.background_set) applyBgImage(true);
    }).catch(function () {});

    // 模糊与亮度**共用**一个防抖定时器，且每次都提交**完整状态**（blur + brightness）。
    //
    // ⚠️ 早先两个 handler 各自 setTimeout 且只提交自己的字段，后果是：
    // 拖完模糊紧接着拖亮度 → 后者的 clearTimeout 把前者**尚未发出**的保存取消掉，
    // 于是模糊值永远存不进服务端（刷新后被打回原值），而亮度正常。
    // 表现成「设置不生效」，且只在一部分操作顺序下复现，极难自查（2026-09-19 实测抓到）。
    // 合成一次保存后：既不会互相取消，最后一次请求也必然带着全部值。
    function scheduleBgSave() {
      clearTimeout(bgSaveTimer);
      bgSaveTimer = setTimeout(function () { saveBg({ blur: blur, brightness: bright }); }, 400);
    }

    blurEl.addEventListener('input', function () {
      blur = +blurEl.value;
      blurVal.textContent = blur + 'px';
      localStorage.setItem('cf_bg_blur', blur);
      localStorage.setItem('cf_bg_blur_touched', '1');
      applyBgFilters(blur, bright);
      scheduleBgSave();
    });
    brightEl.addEventListener('input', function () {
      bright = +brightEl.value;
      brightVal.textContent = bright + '%';
      localStorage.setItem('cf_bg_bright', bright);
      localStorage.setItem('cf_bg_bright_touched', '1');
      applyBgFilters(blur, bright);
      scheduleBgSave();
    });

    clearBtn.addEventListener('click', function () {
      applyBgImage(false);
      saveBg({ clear: true });
    });

    pickBtn.addEventListener('click', function () {
      pickBtn.disabled = true;
      fetch('/api/appearance/pick', { method: 'POST' })
        .then(function (r) { return r.json(); })
        .then(function (d) {
          if (d.error) { addError(d.error); return; }
          if (d.builtin) {
            // Linux 桌面：内置选择器选图 → 把路径 POST 回服务端
            openBuiltinPicker(d.start_path || '', 'file', function (path) {
              if (!path) return;
              saveBg({ background_path: path }).then(function () { applyBgImage(true); });
            });
            return;
          }
          if (d.ok && d.path) {
            // 服务端返回 background:true 表示**它已经把路径落库了**，直接生效即可。
            //
            // 否则（老版本服务端、或某个平台分支漏写 cfg）必须自己补一次显式保存：
            // 背景图由 /api/appearance/background 提供，那个接口读的是服务端配置里的路径，
            // 没落库就永远 404 —— 图层 .on 了却无图可画，界面上一点反应都没有。
            // 这正是 Windows 分支曾经的 bug（2026-09-19），前端这层兜底可以防复发。
            // ⚠️ 必须等保存完成再 applyBgImage，否则请求会赶在落库前发出、照样 404。
            if (d.background) { applyBgImage(true); return; }
            saveBg({ background_path: d.path }).then(function () { applyBgImage(true); });
            return;
          }
        })
        .catch(function (e) { addError('选择背景图失败：' + e); })
        .then(function () { pickBtn.disabled = false; });
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
  // ---------- 模型管理：模型列表 / 供应商管理 / 添加模型 ----------
  //
  // 自供应商机制引入后，连接信息（Base URL / 协议 / 密钥）只在「供应商」上维护
  // 一份，模型条目只留 id 与显示名。由此得到三条主路径：
  //   · 模型列表：按供应商分组，点「设为当前」即可在同供应商下换 id；
  //   · 供应商管理：改地址 / 轮换密钥，其下模型与当前生效配置一起跟着变；
  //   · 添加模型：选已有供应商 → 只填模型 id（地址与密钥继承），
  //              或「获取模型列表」勾选多个一次性批量入库。

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
    // 三个面板用字面量 id 逐个切换，而不是 'mtab-' + name 拼接 ——
    // test/audit_dom_refs.mjs 靠静态匹配核对「ui.js 找的元素是否真的存在」，
    // 拼接写法会让它无法解析，等于把这类静默失效漏出审计网。
    function setPane(id, on) {
      const pane = document.getElementById(id);
      if (pane) pane.classList.toggle('active', on);
    }
    setPane('mtab-list', name === 'list');
    setPane('mtab-prov', name === 'prov');
    setPane('mtab-config', name === 'config');
  }

  // ---------- 模型库（服务端唯一数据源）----------
  //
  // 列表与供应商都来自 /api/models/list。服务端下发的 base_url / protocol /
  // key_set 已是「补齐供应商后」的结果，前端因此不必自己再拼一遍继承逻辑。
  // 旧的 localStorage 方案已废弃 —— 它把明文 API Key 存在浏览器里，且与服务端不同步。
  let editingIndex = -1;   // -1 = 新增；>=0 编辑现有项（仅用于表单标题态，列表以 id 判重）
  let modelsCache = [];    // 最近一次 /api/models/list 的脱敏模型列表
  let providersCache = []; // 同上，供应商列表
  let activeModelId = '';  // 当前生效模型 id
  // 供应商管理页「新建」态的哨兵值。
  // ⚠️ 不能用空串表示新建态：renderProviders 开头有
  //     `if (!provSelected && providersCache.length) provSelected = providersCache[0].id;`
  //   空串是假值，点「添加供应商」置空后立刻被重置回第一个供应商 ——
  //   表现就是「点添加没反应」（2026-09-20 实际故障）。用真值哨兵就不会被吃掉。
  const PROV_NEW = '__new__';
  let provSelected = '';   // 供应商管理页选中的供应商 id；'' = 尚未选择（会自动选第一个）
  // 一次性回执：由批量添加等操作写入，renderProviderDetail 渲染时取走并清空。
  // 为什么要这么绕：详情区是整块重绘的，直接往旧 DOM 上写提示会被下一次重绘冲掉。
  let provFlash = '';

  function loadLibrary() {
    return fetch('/api/models/list').then(function (r) { return r.json(); }).then(function (d) {
      modelsCache = d.models || [];
      providersCache = d.providers || [];
      activeModelId = d.active || '';
      return d;
    });
  }
  function findProvider(id) {
    for (let i = 0; i < providersCache.length; i++) {
      if (providersCache[i].id === id) return providersCache[i];
    }
    return null;
  }
  function modelsOfProvider(pid) {
    return modelsCache.filter(function (m) { return (m.provider_id || '') === pid; });
  }
  // 供应商密钥的可读描述：明文只有「当前生效模型所属供应商」会下发。
  function providerKeyLabel(p) {
    if (!p) return '未设置';
    if (p.key_source === 'env') return p.key_name ? ('环境变量 ' + p.key_name) : '未设置环境变量';
    if (p.key_plain) return p.key_plain;
    return p.key_set ? '已保存（留空保持不变）' : '未设置';
  }
  function fmtCtx(n) {
    n = Number(n) || 0;
    if (n >= 1000000) return (n / 1000000).toFixed(2).replace(/\.?0+$/, '') + 'M';
    if (n >= 1000) return Math.round(n / 1000) + 'K';
    return String(n);
  }

  // ---------- 上下文长度：数字输入框 + 右侧**直接点击**的预置档位 ----------
  // 档位按**二进制 K（1024）**算，与各家模型的标注口径一致：256K = 262144。
  // 输出单独一套档：不少模型的输出上限是输入的 1/2 或 3/8，384K 是常见档。
  const CTX_PRESETS_IN = [1048576, 524288, 262144, 131072]; // 1M / 512K / 256K / 128K
  const CTX_PRESETS_OUT = [393216, 262144, 131072];         // 384K / 256K / 128K

  // 把 token 数渲染成档位标签：1048576 → 1M、393216 → 384K。
  // 不是整 K 的（如历史配置里的 1040000）**原样显示**，不要四舍五入成假档位 ——
  // 那会让用户以为自己配的是 1M。
  function fmtCtxSlot(n) {
    n = Number(n) || 0;
    if (n >= 1048576 && n % 1048576 === 0) return (n / 1048576) + 'M';
    if (n >= 1024 && n % 1024 === 0) return (n / 1024) + 'K';
    return String(n);
  }

  // 上下文控件：**保留数字输入框**（可自由填任意值），右侧挂几个直接点的档位按钮。
  // 点档位 = 把数值写进输入框，不做下拉、不弹层；输入框里的值命中某档时该档高亮。
  // 返回 { node, value(), set(v) }。
  //
  // 为什么不做成纯下拉：配置里本来就有非整档的值（实测 deepseek-flash 是
  // ctx_in=1040000 / ctx_out=384000），纯下拉会退化成第一项、一保存就静默改数。
  // 保留输入框就天然没有这个问题，也不用再搞「自定义…」出口。
  function buildCtxPicker(current, presets, fallback) {
    const wrap = domEl('span', 'ctx-pick');
    const num = domInput('number', '', '');
    num.className = 'ctx-num';
    const chips = domEl('span', 'ctx-chips');
    const btns = [];

    // 输入值命中某档 → 该档高亮；否则全部熄灭（表示这是自定义值）
    function sync() {
      const v = Number(num.value);
      btns.forEach(function (b) { b.classList.toggle('on', Number(b.dataset.v) === v); });
    }

    presets.forEach(function (v) {
      const b = domEl('button', 'ctx-chip', fmtCtxSlot(v));
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
  function domEl(tag, cls, text) {
    const e = document.createElement(tag);
    if (cls) e.className = cls;
    if (text !== undefined && text !== null) e.textContent = text;
    return e;
  }
  function domField(label, control) {
    const w = domEl('label', 'f-field');
    w.appendChild(domEl('span', null, label));
    w.appendChild(control);
    return w;
  }
  function domInput(type, value, placeholder) {
    const i = domEl('input');
    i.type = type;
    if (value) i.value = value;
    if (placeholder) i.placeholder = placeholder;
    return i;
  }
  function domButton(text, cls, onClick) {
    const b = domEl('button', cls, text);
    b.type = 'button';
    b.addEventListener('click', onClick);
    return b;
  }
  function setNote(node, ok, msg) {
    if (!node) return;
    node.className = 'mf-test-result' + (ok === true ? ' ok' : ok === false ? ' fail' : '');
    node.textContent = msg || '';
  }

  // 模型行：显示名 / id / 上下文 + 密钥状态 + 操作。
  function modelRowEl(m, opts) {
    opts = opts || {};
    const wrap = domEl('div', 'model-wrap');
    const row = domEl('div', 'model-item');
    const main = domEl('div', 'mi-main');
    const nm = domEl('div', 'mi-name', m.name || m.id);
    if (m.id === activeModelId) {
      const tag = domEl('span', 'mi-badge ok', '当前');
      tag.style.marginLeft = '8px';
      nm.appendChild(tag);
    }
    main.appendChild(nm);
    main.appendChild(domEl('div', 'mi-id',
      m.id + ' · 上下文 ' + fmtCtx(m.ctx_in) + ' / ' + fmtCtx(m.ctx_out)));

    const badge = domEl('span', 'mi-badge' + (m.key_set ? '' : ' err'),
      m.key_set ? '密钥✓' : '无密钥');

    const acts = domEl('div', 'mi-actions');
    if (m.id !== activeModelId) {
      acts.appendChild(domButton('设为当前', 'apply', function () { applyModel(m.id); }));
    }
    acts.appendChild(domButton('编辑', '', function () { toggleEdit(); }));

    const delBtn = domButton('删除', '', function () {
      const confirmBtn = domButton('确认删除', 'confirming', function () {
        fetch('/api/models/delete', {
          method: 'POST', headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ model: m.id })
        }).then(function (r) { return r.json(); }).then(function () {
          modelsLoaded = false; // 删掉的可能是当前模型，刷新主界面模型显示
          loadLibrary().then(function () {
            renderModelItems();
            if (opts.afterChange) opts.afterChange();
            loadModels();
          });
        });
      });
      const cancelBtn = domButton('取消', '', function () {
        acts.removeChild(confirmBtn);
        acts.removeChild(cancelBtn);
        acts.appendChild(delBtn);
      });
      acts.replaceChild(confirmBtn, delBtn);
      acts.appendChild(cancelBtn);
    });
    acts.appendChild(delBtn);

    row.appendChild(main);
    row.appendChild(badge);
    row.appendChild(acts);
    wrap.appendChild(row);

    // 就地展开编辑区。
    // ⚠️ 不再跳去「添加模型」标签页复用那张表单 —— 那张表单含供应商下拉、地址、协议、
    // 密钥分段控件，对「改个名字 / 调上下文」这种小修改来说太重，也容易让人误以为
    // 要重填密钥。这里只放模型自己的字段，连接信息以只读方式展示来源。
    let editor = null;
    function toggleEdit() {
      if (editor) { wrap.removeChild(editor); editor = null; row.classList.remove('editing'); return; }
      editor = buildModelEditor(m, opts, function () {
        if (editor) { wrap.removeChild(editor); editor = null; }
        row.classList.remove('editing');
      });
      wrap.appendChild(editor);
      row.classList.add('editing');
      const first = editor.querySelector('input');
      if (first) first.focus();
    }

    return wrap;
  }

  // 就地编辑一个模型：只改模型自身字段，不动连接信息。
  function buildModelEditor(m, opts, close) {
    const box = domEl('div', 'model-editor');
    const grid = domEl('div', 'form-grid');

    const nameIn = domInput('text', m.name || '', '界面显示的名称');
    const idIn = domInput('text', m.id || '', '如 gpt-4o');
    grid.appendChild(domField('显示名称', nameIn));
    grid.appendChild(domField('模型 id', idIn));

    const ctxRow = domEl('div', 'f-field ctx-row');
    ctxRow.appendChild(domEl('span', null, '上下文'));
    const ctxInputs = domEl('div', 'ctx-inputs');
    // 与「添加模型」表单同一套控件：预置档位 + 自定义
    const ctxIn = buildCtxPicker(m.ctx_in, CTX_PRESETS_IN, 262144);
    const ctxOut = buildCtxPicker(m.ctx_out, CTX_PRESETS_OUT, 131072);
    const inLab = domEl('label', null, '输入 ');
    inLab.appendChild(ctxIn.node);
    const outLab = domEl('label', null, '输出 ');
    outLab.appendChild(ctxOut.node);
    ctxInputs.appendChild(inLab);
    ctxInputs.appendChild(outLab);
    ctxRow.appendChild(ctxInputs);
    grid.appendChild(ctxRow);

    // 归属供应商：只读展示连接信息来源，编辑区里不重复出现地址与密钥
    const p = m.provider_id ? findProvider(m.provider_id) : null;
    const provRow = domEl('div', 'f-field');
    provRow.appendChild(domEl('span', null, '连接信息'));
    provRow.appendChild(domEl('div', 'me-inherit',
      p ? ((p.name || p.id) + ' · ' + p.base_url + '（地址与密钥继承自该供应商，无需在此填写）')
        : '写在本条目上（该模型未归属供应商）'));
    grid.appendChild(provRow);
    box.appendChild(grid);

    const foot = domEl('div', 'me-foot');
    const note = domEl('span', 'mf-test-result');
    foot.appendChild(note);

    foot.appendChild(domButton('取消', '', close));
    const saveBtn = domButton('保存修改', 'primary', function () {
      const id = idIn.value.trim();
      if (!id) { setNote(note, false, '模型 id 不能为空'); return; }
      const body = {
        name: nameIn.value.trim(),
        id: id,
        model: id,
        ctx_in: ctxIn.value(),
        ctx_out: ctxOut.value()
      };
      if (m.provider_id) {
        body.provider_id = m.provider_id;
        body.protocol = '';
        body.inherit_key = true; // 清掉条目上历史遗留的自带密钥，否则它会一直压着供应商的
      } else {
        // 未归属供应商的旧条目：连接信息写在自己身上，必须原样带回，否则会被清空。
        // 密钥**不提交** —— 列表是脱敏视图拿不到明文，服务端按「保持已存密钥」处理。
        body.provider_id = '';
        body.base_url = m.base_url || '';
        body.protocol = m.protocol || 'openai';
        body.key_source = m.key_source || 'env';
        body.key_name = m.key_name || '';
      }
      saveBtn.disabled = true;
      setNote(note, null, '保存中…');
      fetch('/api/models/save', {
        method: 'POST', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(body)
      }).then(function (r) { return r.json(); }).then(function (d) {
        if (!d.ok) { setNote(note, false, d.error || '保存失败'); saveBtn.disabled = false; return; }
        modelsLoaded = false; // 改的可能是当前生效模型，主界面显示需刷新
        loadLibrary().then(function () {
          renderModelItems();
          if (opts.afterChange) opts.afterChange();
          loadModels();
        });
      }).catch(function (e) {
        setNote(note, false, '保存失败：' + e);
        saveBtn.disabled = false;
      });
    });
    foot.appendChild(saveBtn);
    box.appendChild(foot);
    return box;
  }

  // 切换生效模型：服务端热切换后刷新主界面（对话框弹层 + 常规页显示）。
  function applyModel(id) {
    fetch('/api/models/apply', {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ model: id })
    }).then(function (r) { return r.json(); }).then(function (d) {
      if (!d.ok) { alert('应用失败：' + (d.error || '未知错误')); return; }
      activeModelId = id;
      modelsLoaded = false;
      loadModels().then(function () { renderModelBtn(); });
      loadLibrary().then(function () {
        renderModelItems();
        if (document.getElementById('mtab-prov').classList.contains('active')) renderProviders();
      });
      showPage('general');
    });
  }

  // 编辑模型已改为**就地展开**（见 modelRowEl / buildModelEditor），
  // 不再跳到「添加模型」标签页复用那张表单 —— 所以原来的 editModel() 已删除。

  // ---------- 模型列表：按供应商分组 ----------
  // 模型列表里「供应商分组」的折叠状态：provider_id → 是否折叠（内存态，刷新重置）。
  // 与侧栏 wsFolded 同一套做法，只是作用对象换成模型分组。
  const modelGroupFolded = {};

  function renderModelItems() {
    const box = document.getElementById('model-items');
    box.innerHTML = '<div class="mi-empty">加载中…</div>';
    loadLibrary().then(function () {
      box.innerHTML = '';
      if (!modelsCache.length) {
        box.innerHTML = '<div class="mi-empty">还没有模型：切到「添加模型」，选定供应商后只需填模型 id</div>';
        return;
      }
      // 按 provider_id 分组；未归属供应商的（旧格式条目）单独成组。
      const order = [];
      const groups = {};
      modelsCache.forEach(function (m) {
        const key = m.provider_id || '';
        if (!groups[key]) { groups[key] = []; order.push(key); }
        groups[key].push(m);
      });
      order.forEach(function (pid) {
        const p = pid ? findProvider(pid) : null;
        const folded = !!modelGroupFolded[pid];
        const head = domEl('div', 'model-group');

        // 折叠箭头：供应商下的子模型可以整组收起（模型多的时候不用一直滚）
        const fold = document.createElement('button');
        fold.type = 'button';
        fold.className = 'label-btn mg-fold' + (folded ? ' folded' : '');
        fold.title = folded ? '展开该供应商的模型' : '收起该供应商的模型';
        const arrow = svgIcon('M6 9l6 6 6-6', 12);
        arrow.setAttribute('stroke', 'currentColor');
        arrow.setAttribute('fill', 'none');
        arrow.setAttribute('stroke-width', '2');
        arrow.setAttribute('stroke-linecap', 'round');
        arrow.setAttribute('stroke-linejoin', 'round');
        fold.appendChild(arrow);
        fold.addEventListener('click', function () {
          modelGroupFolded[pid] = !modelGroupFolded[pid];
          renderModelItems();
        });
        head.appendChild(fold);

        head.appendChild(domEl('span', 'mg-name', p ? (p.name || p.id) : '未归属供应商'));
        head.appendChild(domEl('span', 'mg-base',
          p ? p.base_url : '各条目自带连接信息'));
        head.appendChild(domEl('span', 'mi-badge', groups[pid].length + ' 个模型'));
        if (p) {
          head.appendChild(domEl('span', 'mi-badge' + (p.disabled ? ' err' : ' ok'),
            p.disabled ? '已停用' : '密钥共用'));
        }
        head.appendChild(domEl('span', 'mg-sp'));
        if (p) {
          head.appendChild(domButton('＋ 添加模型', '', function () {
            gotoAddModel(p.id);
          }));
        }
        box.appendChild(head);

        if (folded) return;   // 收起时子模型整组不渲染（也省掉一批 DOM）

        // 子模型缩进一层，视觉上从属于上面的供应商
        const kids = domEl('div', 'model-children');
        groups[pid].forEach(function (m) { kids.appendChild(modelRowEl(m)); });
        box.appendChild(kids);
      });
    }).catch(function () {
      box.innerHTML = '<div class="mi-empty">模型列表加载失败，请确认服务正在运行</div>';
    });
  }

  // 跳到「添加模型」并预选供应商：地址与密钥继承，用户只需填 id。
  function gotoAddModel(providerId) {
    showPage('models');
    showMTab('config');
    loadLibrary().then(function () {
      resetForm();
      const sel = document.getElementById('mf-prov');
      if (sel && providerId) { sel.value = providerId; syncProviderForm(); }
      const idEl = document.getElementById('mf-id');
      if (idEl) idEl.focus();
    });
  }

  // ---------- 供应商管理：左列供应商 / 右侧详情 ----------
  function renderProviders() {
    const listEl = document.getElementById('prov-list');
    const detailEl = document.getElementById('prov-detail');
    if (!listEl || !detailEl) return;
    loadLibrary().then(function () {
      if (!provSelected && providersCache.length) provSelected = providersCache[0].id;
      listEl.innerHTML = '';
      providersCache.forEach(function (p) {
        const b = domEl('button', 'prov-item' + (p.id === provSelected ? ' active' : ''));
        b.type = 'button';
        b.appendChild(domEl('span', 'pv-dot' + (p.disabled ? ' off' : '')));
        b.appendChild(domEl('span', null, p.name || p.id));
        b.appendChild(domEl('span', 'pv-cnt', String(modelsOfProvider(p.id).length)));
        b.addEventListener('click', function () { provSelected = p.id; renderProviders(); });
        listEl.appendChild(b);
      });
      const add = domEl('button', 'prov-item prov-add', '＋ 添加供应商');
      add.type = 'button';
      if (provSelected === PROV_NEW) add.classList.add('active');
      add.addEventListener('click', function () { provSelected = PROV_NEW; renderProviders(); });
      listEl.appendChild(add);
      renderProviderDetail(detailEl);
    });
  }

  // 密钥输入控件（供应商表单用）：环境变量 / 明文 分段 + 眼睛。
  // 返回取值接口；touched 记录用户是否动过，决定保存时是否提交 key_value。
  function buildKeyControl(state) {
    const wrap = domEl('div', 'key-row');
    const seg = domEl('div', 'key-seg');
    const envBtn = domEl('button', 'active', '环境变量');
    envBtn.type = 'button';
    const plainBtn = domEl('button', null, '明文密钥');
    plainBtn.type = 'button';
    seg.appendChild(envBtn); seg.appendChild(plainBtn);

    const envIn = domInput('text', state.name, '变量名，如 OPENAI_API_KEY');
    const eyeWrap = domEl('span', 'key-eye-wrap');
    const plainIn = domInput('password', state.plain, state.placeholder || 'sk-...');
    const eye = domEl('button', 'key-eye', '👁');
    eye.type = 'button';
    eyeWrap.appendChild(plainIn); eyeWrap.appendChild(eye);

    wrap.appendChild(seg); wrap.appendChild(envIn); wrap.appendChild(eyeWrap);

    let src = state.source === 'plain' ? 'plain' : 'env';
    let touched = false;
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
      const show = plainIn.type === 'password';
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

  function renderProviderDetail(host) {
    // PROV_NEW（或 id 已失效）→ findProvider 返回 undefined → 走「新建供应商」分支
    const p = (provSelected && provSelected !== PROV_NEW) ? findProvider(provSelected) : null;
    host.innerHTML = '';

    const title = domEl('div', 'mf-title', p ? (p.name || p.id) : '新建供应商');
    if (p) {
      const tag = domEl('span', 'mi-badge' + (p.disabled ? ' err' : ' ok'),
        p.disabled ? '已停用' : '已启用');
      tag.style.marginLeft = '8px';
      title.appendChild(tag);
    }
    host.appendChild(title);

    // 一次性回执（如「已添加 N 个模型」）：跨重绘保留，显示一次即清空
    if (provFlash) {
      const flash = domEl('div', 'prov-flash', provFlash);
      provFlash = '';
      host.appendChild(flash);
    }

    const form = domEl('div', 'prov-form');
    const nameIn = domInput('text', p ? p.name : '', '如 上海模型实验室 / SiliconFlow');
    const urlIn = domInput('text', p ? p.base_url : '', 'https://api.example.com/v1');
    const protoSel = domEl('select');
    ['openai', 'anthropic'].forEach(function (v) {
      const o = domEl('option', null, v === 'anthropic' ? 'Anthropic' : 'OpenAI');
      o.value = v;
      protoSel.appendChild(o);
    });
    protoSel.value = (p && p.protocol === 'anthropic') ? 'anthropic' : 'openai';
    const key = buildKeyControl({
      source: p ? p.key_source : 'env',
      name: p ? p.key_name : '',
      plain: (p && p.key_plain) ? p.key_plain : '',
      placeholder: (p && p.key_set && !p.key_plain) ? '已保存（留空保持不变）' : 'sk-...'
    });

    form.appendChild(domField('名称', nameIn));
    form.appendChild(domField('Base URL', urlIn));
    form.appendChild(domField('兼容协议', protoSel));
    form.appendChild(domField('密钥', key.node));

    const disWrap = domEl('label', 'f-field');
    disWrap.style.flexDirection = 'row';
    disWrap.style.alignItems = 'center';
    disWrap.style.gap = '8px';
    const disIn = domEl('input');
    disIn.type = 'checkbox';
    disIn.checked = !!(p && p.disabled);
    disWrap.appendChild(disIn);
    disWrap.appendChild(domEl('span', null, '停用该供应商（其下模型不可选用，配置保留）'));
    form.appendChild(disWrap);
    host.appendChild(form);

    const result = domEl('span', 'mf-test-result');
    result.id = 'pv-result'; // 保存成功后整块重绘，靠 id 找回新节点写回执
    const acts = domEl('div', 'prov-actions');
    acts.appendChild(result);

    const testBtn = domButton('测试连接', '', function () {
      if (!p) { setNote(result, false, '先保存供应商再测试'); return; }
      setNote(result, null, '测试中…');
      // 用「拉取上游模型列表」当连通性测试：一次请求同时验证地址与密钥，
      // 还能顺带告诉用户这个供应商下有多少模型可用。
      fetch('/api/models/discover', {
        method: 'POST', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ provider_id: p.id })
      }).then(function (r) { return r.json(); }).then(function (d) {
        if (d.ok) setNote(result, true, '连接正常，上游 ' + (d.count || 0) + ' 个模型可用');
        else setNote(result, false, '失败：' + (d.error || '未知错误'));
      }).catch(function (e) { setNote(result, false, '失败：' + e); });
    });
    acts.appendChild(testBtn);

    const saveBtn = domButton('保存供应商', 'primary', function () {
      const name = nameIn.value.trim();
      const url = urlIn.value.trim();
      if (!name) { setNote(result, false, '名称不能为空'); return; }
      if (!url) { setNote(result, false, 'Base URL 不能为空'); return; }
      const body = {
        id: p ? p.id : newProviderId(name),
        name: name,
        base_url: url,
        protocol: protoSel.value,
        key_source: key.source(),
        key_name: key.name(),
        key_value: key.value(),
        key_touched: key.touched(),
        disabled: disIn.checked
      };
      setNote(result, null, '保存中…');
      fetch('/api/providers/save', {
        method: 'POST', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(body)
      }).then(function (r) { return r.json(); }).then(function (d) {
        if (!d.ok) { setNote(result, false, d.error || '保存失败'); return; }
        provSelected = body.id;
        modelsLoaded = false; // 改地址 / 轮换密钥会热切换当前模型，主界面显示需刷新
        loadModels();
        loadLibrary().then(function () {
          renderProviders();
          const fresh = document.getElementById('pv-result');
          setNote(fresh, true, d.hot ? '已保存，当前生效模型已改用新配置' : '已保存');
        });
      }).catch(function (e) { setNote(result, false, '保存失败：' + e); });
    });
    acts.appendChild(saveBtn);

    if (p) {
      const delBtn = domButton('删除', '', function () {
        const n = modelsOfProvider(p.id).length;
        const confirmBtn = domButton('确认删除', 'confirming', function () {
          fetch('/api/providers/delete', {
            method: 'POST', headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({ provider: p.id })
          }).then(function (r) { return r.json(); }).then(function (d) {
            if (!d.ok) { setNote(result, false, d.error || '删除失败'); return; }
            provSelected = '';
            loadLibrary().then(function () {
              if (!provSelected && providersCache.length) provSelected = providersCache[0].id;
              renderProviders();
              renderModelItems();
            });
          });
        });
        const cancelBtn = domButton('取消', '', function () {
          acts.removeChild(confirmBtn);
          acts.removeChild(cancelBtn);
          acts.insertBefore(delBtn, testBtn);
        });
        acts.replaceChild(confirmBtn, delBtn);
        acts.appendChild(cancelBtn);
        if (n) setNote(result, null, '其下 ' + n + ' 个模型不会被删除，连接信息会保留到各自条目上');
      });
      delBtn.style.color = 'var(--danger)';
      acts.appendChild(delBtn);
    }
    host.appendChild(acts);

    if (!p) {
      host.appendChild(domEl('div', 'prov-empty',
        '填好名称、Base URL 与密钥后保存；之后在同一供应商下添加模型只需填模型 id。'));
      return;
    }

    const list = modelsOfProvider(p.id);
    const sub = domEl('div', 'prov-sub');
    sub.appendChild(document.createTextNode('模型 · ' + list.length + ' 个'));
    sub.appendChild(domEl('span', 'pv-hint', '共用上面的 Base URL 与 Key，切换只需换 id'));
    host.appendChild(sub);

    if (list.length) {
      list.forEach(function (m) { host.appendChild(modelRowEl(m)); });
    } else {
      host.appendChild(domEl('div', 'prov-empty',
        '该供应商下暂无模型 —— 点下方按钮添加，只需填模型 id。'));
    }

    // ---- 获取模型列表：就地展开，**不弹窗** ----
    // 放在供应商详情里，因为它用的就是这个供应商的 Base URL 与密钥；
    // 勾选后一次性批量入库，天然归到该供应商名下（符合「按供应商分类」）。
    const discBox = domEl('div', 'pv-disc hidden');
    let discItems = [], discPick = {};

    const discHead = domEl('div', 'disc-head');
    discHead.appendChild(domEl('div', 'disc-title', '上游可用模型'));
    const discCountEl = domEl('span', 'disc-count');
    discHead.appendChild(discCountEl);
    const discTools = domEl('div', 'disc-tools');
    const discFilter = domInput('text', '', '筛选模型 id…');
    const discCloseBtn = domButton('收起', '', function () {
      discItems = []; discPick = {};
      discBox.classList.add('hidden');
    });
    discTools.appendChild(discFilter);
    discTools.appendChild(discCloseBtn);
    discHead.appendChild(discTools);
    discBox.appendChild(discHead);

    const discListEl = domEl('div', 'disc-list');
    discBox.appendChild(discListEl);

    const discFoot = domEl('div', 'disc-foot');
    const discNote = domEl('span', 'mf-test-result');
    const discAddBtn = domButton('批量添加选中', 'primary', function () { commitPicked(); });
    discAddBtn.disabled = true;
    discFoot.appendChild(discNote);
    discFoot.appendChild(discAddBtn);
    discBox.appendChild(discFoot);

    function discSync() {
      const n = Object.keys(discPick).length;
      discCountEl.textContent = discItems.length ? '共 ' + discItems.length + ' 个' +
        (n ? '，已选 ' + n + ' 个' : '') : '';
      discAddBtn.disabled = n === 0;
      discAddBtn.textContent = n ? '批量添加选中（' + n + ' 个）' : '批量添加选中';
    }

    function discRender() {
      const q = (discFilter.value || '').trim().toLowerCase();
      const shown = discItems.filter(function (m) {
        return !q || m.id.toLowerCase().indexOf(q) >= 0 ||
          (m.name || '').toLowerCase().indexOf(q) >= 0;
      });
      discListEl.innerHTML = '';
      if (!shown.length) {
        discListEl.innerHTML = '<div class="disc-empty">' +
          (discItems.length ? '没有匹配的模型' : '没有可添加的模型') + '</div>';
        discSync();
        return;
      }
      shown.forEach(function (m) {
        const row = domEl('label', 'disc-item' + (m.in_library ? ' added' : ''));
        const cb = document.createElement('input');
        cb.type = 'checkbox';
        cb.checked = !!discPick[m.id];
        cb.disabled = !!m.in_library; // 已在库中：不可选，也不覆盖
        cb.addEventListener('change', function () {
          if (cb.checked) discPick[m.id] = true; else delete discPick[m.id];
          discSync();
        });
        row.appendChild(cb);
        row.appendChild(domEl('span', 'disc-id', m.id));
        if (m.in_library) row.appendChild(domEl('span', 'disc-tag', '已在库'));
        discListEl.appendChild(row);
      });
      discSync();
    }

    function discNoteSet(ok, msg) {
      discNote.className = 'mf-test-result' + (ok === true ? ' ok' : ok === false ? ' fail' : '');
      discNote.textContent = msg || '';
    }

    // 拉取上游模型列表：直接用该供应商已保存的地址与密钥（provider_id），
    // 不必先「测试连接」——两者其实是同一个请求，合并成一个按钮更省事。
    function fetchUpstream() {
      discBox.classList.remove('hidden');
      discNoteSet(null, '正在获取模型列表…');
      fetch('/api/models/discover', {
        method: 'POST', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ provider_id: p.id })
      }).then(function (r) { return r.json(); }).then(function (d) {
        if (!d.ok) {
          discItems = []; discPick = {};
          discListEl.innerHTML = '';
          discSync();
          discNoteSet(false, '获取失败：' + (d.error || '未知错误'));
          return;
        }
        discItems = d.models || [];
        discPick = {};
        discFilter.value = '';
        discRender();
        discNoteSet(true, '已获取 ' + discItems.length + ' 个模型' +
          (d.key_from_active ? '（密钥取自当前生效模型）' : ''));
      }).catch(function (e) {
        discNoteSet(false, '获取失败：' + e);
      });
    }

    function commitPicked() {
      const ids = Object.keys(discPick);
      if (!ids.length) { discNoteSet(false, '请先勾选模型 id'); return; }
      const pick = function (id) {
        return discItems.filter(function (m) { return m.id === id; })[0] || {};
      };
      discNoteSet(null, '正在添加 ' + ids.length + ' 个模型…');
      fetch('/api/models/save_batch', {
        method: 'POST', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          provider_id: p.id,
          ctx_in: 262144,
          ctx_out: 131072,
          models: ids.map(function (id) {
            const hit = pick(id);
            return { id: id, name: (hit.name && hit.name !== id) ? hit.name : '' };
          })
        })
      }).then(function (r) { return r.json(); }).then(function (d) {
        if (!d.ok) { discNoteSet(false, d.error || '批量添加失败'); return; }
        discItems = []; discPick = {};
        modelsLoaded = false;
        // 回执写成一次性闪信再重绘 —— 直接写 DOM 会被这次重绘冲掉。
        provFlash = '已添加 ' + d.added + ' 个模型（共用该供应商的地址与密钥）' +
          (d.skipped ? '，跳过已存在 ' + d.skipped + ' 个' : '');
        loadLibrary().then(function () {
          renderModelItems();
          loadModels();
          renderProviders();   // 顺带刷新详情里的模型列表与计数
        });
      }).catch(function (e) {
        discNoteSet(false, '批量添加失败：' + e);
      });
    }

    discFilter.addEventListener('input', discRender);
    host.appendChild(discBox);

    const acts2 = domEl('div', 'prov-actions');
    acts2.appendChild(domButton('＋ 添加模型到此供应商', 'primary', function () {
      gotoAddModel(p.id);
    }));
    acts2.appendChild(domButton('获取模型列表', '', fetchUpstream));
    host.appendChild(acts2);
  }

  // 由名称生成一个稳定的供应商 id（重名追加序号）。
  function newProviderId(name) {
    let slug = String(name).toLowerCase().replace(/[^a-z0-9]+/g, '-').replace(/^-+|-+$/g, '');
    if (!slug) slug = 'provider';
    let id = 'p-' + slug;
    for (let n = 2; findProvider(id); n++) id = 'p-' + slug + '-' + n;
    return id;
  }

  // ---------- 添加 / 编辑模型表单 ----------

  // 供应商下拉：'' 表示「新建供应商」，此时连接信息在本表单填写。
  function populateProviderSelect(selected) {
    const sel = document.getElementById('mf-prov');
    if (!sel) return;
    sel.innerHTML = '';
    const optNew = domEl('option', null, '＋ 新建供应商（在本表单填地址与密钥）');
    optNew.value = '';
    sel.appendChild(optNew);
    providersCache.forEach(function (p) {
      const o = domEl('option', null, (p.name || p.id) + (p.disabled ? '（已停用）' : ''));
      o.value = p.id;
      sel.appendChild(o);
    });
    sel.value = selected || '';
  }

  // 选定供应商后，地址 / 协议 / 密钥三块换成「继承自供应商」只读块 ——
  // 表单只剩 模型 id / 显示名 / 上下文。这就是「同供应商下加模型只填 id」。
  function syncProviderForm() {
    const inheritField = document.getElementById('mf-inherit-field');
    if (!inheritField) return;
    const pid = document.getElementById('mf-prov').value;
    const p = pid ? findProvider(pid) : null;
    inheritField.hidden = !p;
    document.getElementById('mf-url-field').hidden = !!p;
    document.getElementById('mf-key-field').hidden = !!p;
    document.getElementById('mf-proto-field').hidden = !!p;
    if (!p) return;

    const box = document.getElementById('mf-inherit');
    box.innerHTML = '';
    box.appendChild(domEl('div', 'ih', '继承自供应商「' + (p.name || p.id) + '」，无需重复填写'));
    [
      ['Base URL', p.base_url],
      ['API 格式', p.protocol === 'anthropic'
        ? 'Anthropic Messages (/v1/messages)'
        : 'Chat Completions (/chat/completions)'],
      ['API Key', providerKeyLabel(p)]
    ].forEach(function (pair) {
      const r = domEl('div', 'ir');
      r.appendChild(domEl('span', null, pair[0]));
      r.appendChild(domEl('code', null, pair[1] || '—'));
      box.appendChild(r);
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
    if (ctxInPicker) ctxInPicker.set(m.ctx_in || 262144);
    if (ctxOutPicker) ctxOutPicker.set(m.ctx_out || 131072);
    populateProviderSelect(m.provider_id || '');
    syncProviderForm();
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
    const pid = document.getElementById('mf-prov').value;
    const id = document.getElementById('mf-id').value.trim();
    const f = {
      name: document.getElementById('mf-name').value.trim(),
      id: id,
      model: id, // 测试 / 发现接口的字段名（后端 modelTestReq.model）
      provider_id: pid,
      ctx_in: ctxInPicker ? ctxInPicker.value() : 262144,
      ctx_out: ctxOutPicker ? ctxOutPicker.value() : 131072
    };
    if (pid) {
      // 归属供应商：连接信息全部继承，条目上不写任何覆盖值；
      // inherit_key 让服务端清掉条目上历史遗留的自带密钥，否则它会一直压着供应商的密钥。
      f.protocol = '';
      f.inherit_key = true;
    } else {
      f.base_url = document.getElementById('mf-url').value.trim();
      f.key_source = keySrc;
      f.key_name = document.getElementById('mf-key-env').value.trim();
      f.key_value = document.getElementById('mf-key-plain').value;
      f.protocol = document.getElementById('mf-proto').value;
    }
    return f;
  }
  function setTestResult(ok, msg) {
    const el = document.getElementById('mf-test-result');
    el.className = 'mf-test-result' + (ok === true ? ' ok' : ok === false ? ' fail' : '');
    el.textContent = msg || '';
  }

  // 密钥来源分段；keyTouched 记录本次编辑是否动过密钥（决定保存时是否提交 key_value）。
  let keySrc = 'env';
  let keyTouched = false;

  // 上下文输入/输出控件（预置档位 + 自定义）。在下方 init 里挂到 #mf-ctx-*-host 上，
  // 表单其余部分一律通过这两个对象读写，不再直接碰 DOM —— 免得预置/自定义两套状态各写各的。
  let ctxInPicker = null;
  let ctxOutPicker = null;
  (function initCtxPickers() {
    const inHost = document.getElementById('mf-ctx-in-host');
    const outHost = document.getElementById('mf-ctx-out-host');
    if (!inHost || !outHost) return;
    ctxInPicker = buildCtxPicker(262144, CTX_PRESETS_IN, 262144);
    ctxOutPicker = buildCtxPicker(131072, CTX_PRESETS_OUT, 131072);
    inHost.appendChild(ctxInPicker.node);
    outHost.appendChild(ctxOutPicker.node);
  })();
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
  // 切换归属供应商 → 地址与密钥在「继承 / 手填」之间切换。
  document.getElementById('mf-prov').addEventListener('change', function () {
    syncProviderForm();
    updateDiscCount();
  });

  // 「模型」导航项 → 模型管理页（列表视图）
  document.getElementById('nav-models').addEventListener('click', function () {
    settingsOverlay.querySelectorAll('.nav-item').forEach(function (n) { n.classList.remove('active'); });
    this.classList.add('active');
    showPage('models');
    showMTab('list');
    renderModelItems();
  });
  // 子 Tab：模型列表 / 供应商管理 / 添加模型
  // （点「添加模型」即回到空白新建态，编辑态复用同一表单）
  document.querySelectorAll('.models-tab').forEach(function (t) {
    t.addEventListener('click', function () {
      const name = t.dataset.mtab;
      showMTab(name);
      if (name === 'config') {
        loadLibrary().then(function () { resetForm(); });
      } else if (name === 'prov') {
        renderProviders();
      } else {
        renderModelItems();
      }
    });
  });

  // 把探测结论说成人话。探不到时必须明说「未探到」并保留手填值 ——
  // 显示成 0 或悄悄清空字段，比不探测更糟（压缩线会按 0 回退到默认预算）。
  function ctxProbeText(probe) {
    if (!probe) return '';
    const steps = probe.steps || [];
    const got = Number(probe.ctx_in) || 0;
    if (got <= 0) {
      return '；上下文未探到：' + (probe.note || '上游未给出长度信号') + '（保留手填值）';
    }
    let idx = -1;
    for (let i = 0; i < steps.length; i++) { if (steps[i].accepted) { idx = i; break; } }
    const above = idx > 0 ? steps[idx - 1].want : 0;
    return '；上下文 ≈ ' + fmtTokens(got) + ' tokens' +
      (above ? '（' + fmtTokens(above) + ' 那一档被上游拒了）' : '（最大档就被接受，可能被高估）');
  }

  // 测试连接（连通后顺带逐档探测输入上下文）
  document.getElementById('mf-test').addEventListener('click', function () {
    const m = collectForm();
    m.probe_context = true;
    setTestResult(null, '测试中…（通过后开始逐档探测上下文，最大档要上传数 MB 输入，可能要几十秒）');
    fetch('/api/models/test', {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(m)
    }).then(function (r) { return r.json(); }).then(function (d) {
      if (!d.ok) { setTestResult(false, '失败：' + (d.error || '未知错误')); return; }
      setTestResult(true, '连接成功' + ctxProbeText(d.probe));
      // 探到了就填进表单（保存时随表单落库）；探不到保留用户手填值，绝不清零。
      const got = Number((d.probe && d.probe.ctx_in) || 0);
      if (got > 0 && ctxInPicker) ctxInPicker.set(got);
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
    if (m.provider_id) {
      // 归属供应商：连接信息继承，不提交任何覆盖值。
      delete m.key_value;
    } else if (editingIndex >= 0 && !keyTouched) {
      // 编辑已有模型且本次没动过密钥 → 不提交 key_value，服务端按「保持库里已存
      // 密钥」处理。前端拿到的列表是脱敏视图，明文框为空只是回显受限，绝不能被
      // 当成「用户清空了密钥」而覆盖掉。
      delete m.key_value;
    } else if (m.key_source === 'plain' && !String(m.key_value || '').trim()) {
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
        modelsLoaded = false; // 置假后 loadModels 会重拉模型库快照，对话框的模型选择同步出现新条目
        loadLibrary().then(function () { renderModelItems(); loadModels(); });
      } else {
        setTestResult(false, d.error || '保存失败');
      }
    }).catch(function (e) {
      setTestResult(false, '保存失败：' + e);
    });
  });

  // ---------- 添加模型：从上游 /models 拉取列表 → 勾选 → 批量入库 ----------
  // 与「测试连接」的区别：测试是「所见即所测」，这里允许在表单没填地址/密钥时
  // 回退到当前生效模型，服务端会在响应里用 key_from_active 如实告知。
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
    if (btn) {
      const pid = document.getElementById('mf-prov').value;
      btn.disabled = n === 0;
      // 有供应商 → 直接批量入库；还没确认连接信息 → 只填入表单。
      btn.textContent = pid ? ('批量添加选中（' + n + ' 个）') : '填入表单';
    }
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
      // 多选：选中的 id 会一次性批量写入同一供应商（地址与密钥共用），
      // 这正是「同一个供应商下不必一个一个添加」的实现方式。
      cb.type = 'checkbox';
      cb.checked = !!discSelected[m.id];
      cb.disabled = !!m.in_library; // 已在库中：不可选，也不覆盖
      cb.addEventListener('change', function () {
        if (cb.checked) discSelected[m.id] = true;
        else delete discSelected[m.id];
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

  // 「批量添加选中」：
  //  - 已选定供应商 → 一次 POST 把全部选中的 id 写进该供应商（地址与密钥共用）；
  //  - 还没选供应商（正在新建）→ 只把第一个 id 填进表单，由用户补好地址与密钥后
  //    再点「保存」正式入库 —— 所见即所得，不把还没确认的连接信息偷偷写进模型库。
  function addSelectedDiscModels() {
    const ids = Object.keys(discSelected);
    if (!ids.length) { setDiscResult(false, '请先勾选模型 id'); return; }
    const pick = function (id) {
      return (discModels || []).filter(function (m) { return m.id === id; })[0] || {};
    };
    const pid = document.getElementById('mf-prov').value;

    if (!pid) {
      const id = ids[0];
      const hit = pick(id);
      document.getElementById('mf-id').value = id;
      const nameEl = document.getElementById('mf-name');
      if (!nameEl.value.trim() && hit.name && hit.name !== id) nameEl.value = hit.name;
      setTestResult(true, '已填入模型 id：' + id + '，补好地址与密钥后点「保存」');
      collapseDiscover();
      return;
    }

    const payload = {
      provider_id: pid,
      ctx_in: ctxInPicker ? ctxInPicker.value() : 262144,
      ctx_out: ctxOutPicker ? ctxOutPicker.value() : 131072,
      models: ids.map(function (id) {
        const hit = pick(id);
        return { id: id, name: (hit.name && hit.name !== id) ? hit.name : '' };
      })
    };
    setDiscResult(null, '正在添加 ' + ids.length + ' 个模型…');
    fetch('/api/models/save_batch', {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(payload)
    }).then(function (r) { return r.json(); }).then(function (d) {
      if (!d.ok) { setDiscResult(false, d.error || '批量添加失败'); return; }
      const msg = '已添加 ' + d.added + ' 个模型（共用该供应商的地址与密钥）' +
        (d.skipped ? '，跳过已存在 ' + d.skipped + ' 个' : '');
      setDiscResult(true, msg);
      collapseDiscover();
      modelsLoaded = false;
      loadLibrary().then(function () { renderModelItems(); loadModels(); });
    }).catch(function (e) {
      setDiscResult(false, '批量添加失败：' + e);
    });
  }

  (function bindModelDiscover() {
    const btn = document.getElementById('mf-discover');
    if (!btn) return;
    btn.addEventListener('click', discoverModels);
    document.getElementById('disc-close').addEventListener('click', collapseDiscover);
    document.getElementById('disc-filter').addEventListener('input', renderDiscList);
    document.getElementById('disc-add').addEventListener('click', addSelectedDiscModels);
  })();
})();
