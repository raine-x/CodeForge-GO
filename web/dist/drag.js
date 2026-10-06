// 左右分栏拖拽调宽：拖动中分割线（或中央小竖线）调整左栏宽度
// 同时支持鼠标与触摸（touch-action:none 已禁浏览器手势），
// 下限 200px，上限 40vw（即左右 4:6）。
(function () {
  const MIN = 200;
  const handle = document.getElementById('drag-handle');
  const sidebar = document.getElementById('sidebar');
  if (!handle || !sidebar) return;

  // 窄屏/矮屏上侧栏是抽屉（宽度由 app.css 的移动端断点给，见 #sidebar 那组），
  // 拖拽把手在那个断点里是 display:none —— 但**内联 width 的优先级高于媒体
  // 查询**：只要用户先在桌面拖过一次，sidebar.style.width 就留在那儿，
  // 旋转屏幕 / 缩窗口到窄屏时会把抽屉宽度一起带过去（可能变成 200px 的细条）。
  // 所以必须在这里就停手，不能指望 CSS 覆盖 inline style。
  // 条件与 app.css 的断点、ui.js 的 mobileShell 逐字一致，三处要一起改。
  const MOBILE_MQ = window.matchMedia
    ? window.matchMedia('(max-width: 820px), (max-height: 520px)')
    : null;
  if (MOBILE_MQ && MOBILE_MQ.matches) return;

  const MAX = () => Math.max(MIN, Math.floor(window.innerWidth * 0.4));

  // #app 带 10px 内缩（app.css），侧栏左缘不在 x=0；clientX 减去内边距才是侧栏宽度。
  const appPad = () => {
    const el = document.getElementById('app');
    return el ? (parseFloat(getComputedStyle(el).paddingLeft) || 0) : 0;
  };

  let dragging = false;

  function setActive(on) {
    document.body.classList.toggle('resizing', on);
    handle.classList.toggle('active', on);
  }

  function moveTo(clientX) {
    const w = Math.max(MIN, Math.min(clientX - appPad(), MAX()));
    sidebar.style.width = w + 'px';
  }

  // ---- 鼠标 ----
  handle.addEventListener('mousedown', (e) => {
    dragging = true;
    setActive(true);
    e.preventDefault();
    document.addEventListener('mousemove', onMouseMove);
    document.addEventListener('mouseup', onMouseUp);
  });

  function onMouseMove(e) {
    if (dragging) moveTo(e.clientX);
  }
  function onMouseUp() {
    if (!dragging) return;
    dragging = false;
    setActive(false);
    document.removeEventListener('mousemove', onMouseMove);
    document.removeEventListener('mouseup', onMouseUp);
  }

  // ---- 触摸（手机无鼠标） ----
  handle.addEventListener('touchstart', (e) => {
    dragging = true;
    setActive(true);
    const t = e.touches[0];
    if (t) moveTo(t.clientX);
    e.preventDefault();
  }, { passive: false });

  handle.addEventListener('touchmove', (e) => {
    if (!dragging) return;
    const t = e.touches[0];
    if (t) moveTo(t.clientX);
    e.preventDefault();
  }, { passive: false });

  handle.addEventListener('touchend', (e) => {
    e.preventDefault();
    dragging = false;
    setActive(false);
  }, { passive: false });
})();