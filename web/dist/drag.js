// 左右分栏拖拽调宽：拖动中分割线（或中央小竖线）调整左栏宽度
// 同时支持鼠标与触摸（touch-action:none 已禁浏览器手势），
// 下限 200px，上限 40vw（即左右 4:6）。
(function () {
  const MIN = 200;
  const handle = document.getElementById('drag-handle');
  const sidebar = document.getElementById('sidebar');
  if (!handle || !sidebar) return;

  const MAX = () => Math.max(MIN, Math.floor(window.innerWidth * 0.4));

  let dragging = false;

  function setActive(on) {
    document.body.classList.toggle('resizing', on);
    handle.classList.toggle('active', on);
  }

  function moveTo(clientX) {
    const w = Math.max(MIN, Math.min(clientX, MAX()));
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