// 把「设置界面」导出成一个可独立打开的 HTML，供别的模型重构。
//
//   node test/export_settings_ui.mjs [输出路径]
//
// 产出：单文件、零依赖、离线可开；开关与导航**仍然可交互**。
//
// 取舍说明：
//   - 样式表**整份附上**（app.css 原样）。设置界面复用了全局变量（--text/--bg…）
//     与一堆通用组件（.row / .switch / .btn / .slider），按选择器裁剪必然漏样式，
//     所以不做裁剪，只加分隔注释。
//   - 动态内容（模型列表 / MCP 列表 / 技能 / 记忆条目）是服务端填充的，导出里是空壳。
//   - 所有后端请求（保存、测试连接、选图…）都不包含 —— 这是 UI 重构稿，不是可用的应用。

import fs from 'node:fs';
import path from 'node:path';

const ROOT = path.join(import.meta.dirname, '..');
const OUT = process.argv[2] || path.join(ROOT, '..', 'settings-ui-export.html');

const html = fs.readFileSync(path.join(ROOT, 'web', 'dist', 'index.html'), 'utf8');
const css = fs.readFileSync(path.join(ROOT, 'web', 'dist', 'app.css'), 'utf8');

// ---- 1) 抠出 #settings-overlay（做标签配平，不靠行号）----
const start = html.indexOf('<div id="settings-overlay"');
if (start < 0) throw new Error('index.html 里找不到 #settings-overlay');
let depth = 0, end = -1;
const tagRe = /<div\b|<\/div>/g;
tagRe.lastIndex = start;
for (let m; (m = tagRe.exec(html)); ) {
  if (m[0] === '</div>') { if (--depth === 0) { end = tagRe.lastIndex; break; } }
  else depth++;
}
if (end < 0) throw new Error('#settings-overlay 的闭合标签没配平');

let panel = html.slice(start, end);

// ---- 2) 去掉初始的 hidden（导出里要直接显示）----
panel = panel.replace('<div id="settings-overlay" class="settings-overlay hidden">',
                      '<div id="settings-overlay" class="settings-overlay">');
if (/class="settings-overlay hidden"/.test(panel)) throw new Error('未能去掉 overlay 的 hidden 类');

// ---- 3) 让界面活起来的最小脚本 ----
const script = `
(function () {
  var ov = document.getElementById('settings-overlay');
  if (!ov) return;

  // 左导航：点哪页切哪页。
  // ⚠️ 「模型」那一项（#nav-models）**没有 data-page**，原版 ui.js 是单独给它绑事件的
  // （判定条件形如 data-page 匹配，或 id 为 models 且元素 id 为 nav-models）。
  // 这里要一起认，否则点「模型」毫无反应。
  // 「返回工作区」(#nav-back) 不切页 —— 原版它是关闭设置面板。
  function navTarget(item) {
    if (item.dataset && item.dataset.page) return item.dataset.page;
    if (item.id === 'nav-models') return 'models';
    return null;
  }
  ov.querySelectorAll('.nav-item').forEach(function (item) {
    var page = navTarget(item);
    if (!page) return;
    item.addEventListener('click', function () {
      ov.querySelectorAll('.nav-item').forEach(function (n) { n.classList.remove('active'); });
      item.classList.add('active');
      ov.querySelectorAll('.settings-page').forEach(function (p) {
        p.classList.toggle('active', p.id === 'page-' + page);
      });
    });
  });

  // 分段控件（思考皮肤 / 密钥来源…）：单选高亮
  ov.querySelectorAll('.skin-seg, .key-seg').forEach(function (seg) {
    seg.querySelectorAll('button').forEach(function (b) {
      b.addEventListener('click', function () {
        seg.querySelectorAll('button').forEach(function (x) { x.classList.remove('active'); });
        b.classList.add('active');
      });
    });
  });

  // MCP 服务页的「已注册 / 添加」分栏
  var mcpPage = document.getElementById('page-mcp');
  if (mcpPage) {
    mcpPage.querySelectorAll('.mcp-tab').forEach(function (t) {
      t.addEventListener('click', function () {
        mcpPage.querySelectorAll('.mcp-tab').forEach(function (x) {
          x.classList.toggle('active', x === t);
        });
        mcpPage.querySelectorAll('.mcp-pane').forEach(function (p) {
          p.classList.toggle('active', p.id === t.dataset.pane);
        });
      });
    });
  }

  // 滑条：把数值同步到旁边的 -val 显示（单位按 id 区分，与原版一致）
  var UNIT = { 'opt-comp-w': 'px', 'opt-comp-h': 'px', 'opt-bg-blur': 'px', 'opt-bg-bright': '%' };
  ov.querySelectorAll('input[type=range]').forEach(function (r) {
    var out = document.getElementById(r.id + '-val');
    function sync() {
      if (!out) return;
      var unit = UNIT[r.id] || '';
      out.textContent = r.value + unit;
    }
    r.addEventListener('input', sync);
    sync();
  });

  // 开关是原生 checkbox（.switch > input），无需脚本即生效。
  // 「返回工作区」按钮在独立页里没有可返回的主界面，点了给个提示。
  var back = ov.querySelector('.nav-back');
  if (back) {
    back.addEventListener('click', function () {
      console.log('[export] 「返回工作区」在独立导出页里无目标，原版用于关闭设置面板');
    });
  }
})();
`;

const banner = `<!DOCTYPE html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>CodeForge · 设置界面（导出用于重构）</title>
<!--
  这是从 CodeForge 网页端导出的**设置界面独立版**，用于交给别的模型重构。

  ▍包含
    · 完整的设置面板 DOM（原样，只去掉了初始的 hidden 类）
    · 完整样式表（app.css 原样附上，见下方 <style>）
    · 让界面可交互的最小脚本：导航切换 / 分段控件 / 滑条数值 / MCP 分栏

  ▍不包含（原版由服务端数据填充 / 驱动，这里都是空壳）
    · 模型列表、MCP 服务列表、技能列表、记忆条目等动态内容
    · 所有后端请求（保存、测试连接、选图、启停插件…）

  ▍主题
    默认浅色。给 <body> 加 class="bg-dark" 可看深色主题；
    加 class="has-bg" 可看「开着背景图」时的透明面板效果（需自行提供背景图）。

  ▍为什么样式表不裁剪
    设置界面复用了全局变量（--text / --bg / --border …）与大量通用组件
    （.row / .switch / .btn / .settings-card），按选择器裁剪必然漏样式，
    所以整份带上。真正只服务设置面板的规则集中在 .settings-* 与 .row* 附近。
-->
<style>
${css}
</style>
</head>
<body>
${panel}
<script>
${script}
</script>
</body>
</html>
`;

fs.writeFileSync(OUT, banner);
const kb = (Buffer.byteLength(banner) / 1024).toFixed(0);
console.log('已导出: ' + OUT + '  (' + kb + ' KB)');
console.log('  面板 DOM: ' + (Buffer.byteLength(panel) / 1024).toFixed(0) + ' KB');
console.log('  样式表  : ' + (Buffer.byteLength(css) / 1024).toFixed(0) + ' KB');
