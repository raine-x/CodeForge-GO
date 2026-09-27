// agent-browser 的 page init script：在页面任何脚本之前挂钩 WebSocket 构造器，
// 把 app 创建的 socket 挂到 window.__cfws，并记录它 addEventListener 的处理器，
// 于是可以用**真实的 message 监听器**派发合成帧，走 ui.js 里那条真实的分发路径。
//
// 为什么要拦 addEventListener：ui.js 用的是 ws.addEventListener('message', ...)
// 而不是 ws.onmessage = ...，后者在原生 socket 上取不到监听器。
//
// 用途：本机连不上上游模型，真实 E2E 跑不了；前端事件类修复只能这样验证。
(function () {
  var Native = window.WebSocket;
  window.__cfws = null;

  window.WebSocket = function (url, proto) {
    var s = proto === undefined ? new Native(url) : new Native(url, proto);
    // 记录监听器（addEventListener 注册的函数无法从 JS 侧读回来，只能在挂载时截获）
    s.__cfHandlers = {};
    var nativeAdd = s.addEventListener.bind(s);
    s.addEventListener = function (type, fn, opts) {
      (s.__cfHandlers[type] = s.__cfHandlers[type] || []).push(fn);
      return nativeAdd(type, fn, opts);
    };
    window.__cfws = s;
    return s;
  };
  window.WebSocket.prototype = Native.prototype;
  window.WebSocket.CONNECTING = Native.CONNECTING;
  window.WebSocket.OPEN = Native.OPEN;
  window.WebSocket.CLOSING = Native.CLOSING;
  window.WebSocket.CLOSED = Native.CLOSED;

  // 派发一帧到 app 的真实 handler（ui.js 那边是 JSON.parse(e.data)）
  window.__feed = function (obj) {
    var s = window.__cfws;
    if (!s) return { err: 'no socket' };
    var list = (s.__cfHandlers && s.__cfHandlers.message) || [];
    if (!list.length) return { err: '没有 message listener' };
    var ev = { data: JSON.stringify(obj) };
    for (var i = 0; i < list.length; i++) {
      try { list[i].call(s, ev); } catch (e) { return { err: 'handler 抛异常: ' + e }; }
    }
    return { ok: true, n: list.length };
  };

  // 清空聊天列，让每组断言从干净状态开始
  window.__clearCol = function () {
    var c = document.querySelector('.msg-col');
    if (c) c.innerHTML = '';
    return true;
  };

  // 读最后一张工具卡的实测状态
  window.__lastTool = function () {
    var els = document.querySelectorAll('.msg-col .msg-tool');
    var el = els[els.length - 1];
    if (!el) return null;
    var m = el.querySelector('.tool-denied');
    return {
      count: els.length,
      denied: el.classList.contains('denied'),
      mark: m ? m.textContent : null,
      spinner: !!el.querySelector('.tool-spinner'),
      running: el.classList.contains('running'),
      stats: !!el.querySelector('.diffstat.add'),
      text: el.textContent.trim()
    };
  };
})();
