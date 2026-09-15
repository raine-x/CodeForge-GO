#!/usr/bin/env node
// CodeForge-Go 前端渲染器回归测试（零依赖，直接用 node 运行）
//
//   node web/test/render_md.test.js
//
// 原理：renderMD() / escapeHtml() / toolLabel() 是纯函数，但被包在 ui.js 的 IIFE 里，
// 且该文件加载时会直接访问 DOM。因此这里不加载整个 ui.js，而是从源码中按花括号配对
// 截出这几个函数体，注入到一个干净的 Function 作用域里调用 —— 不需要浏览器或 jsdom。
//
// 覆盖：基础 Markdown、表格（含对齐）、LaTeX 公式抽取、工具卡片文案（toolLabel），
// 以及几处易回归的边界（货币 $5…$10 不误判、代码块内公式不处理、块级 HTML 不被包进 <p>），
// 另含 app.css 的样式契约检查（如 .md 必须重置 white-space: pre-wrap）与侧栏
// 「项目 / 会话」契约（会话行三点必须限定到 li.session-item，否则会连带命中项目行头部）。
//
// 退出码：全部通过为 0，有失败为 1（可直接接进 CI / make test）。

'use strict';

const fs = require('fs');
const path = require('path');

const DIST = path.join(__dirname, '..', 'dist');
const UI_PATH = path.join(DIST, 'ui.js');
const CSS_PATH = path.join(DIST, 'app.css');

// ---------------------------------------------------------------------------
// 从 ui.js 源码中截取具名函数（按花括号配对，能正确处理字符串里的括号）
// ---------------------------------------------------------------------------
function extractFunction(src, name) {
  const start = src.indexOf('function ' + name + '(');
  if (start < 0) throw new Error('在 ui.js 中找不到函数 ' + name + '()');
  let i = src.indexOf('{', start);
  let depth = 0;
  for (; i < src.length; i++) {
    if (src[i] === '{') depth++;
    else if (src[i] === '}') {
      depth--;
      if (depth === 0) break;
    }
  }
  if (depth !== 0) throw new Error('函数 ' + name + '() 的花括号不配对');
  return src.slice(start, i + 1);
}

function loadRenderer() {
  const src = fs.readFileSync(UI_PATH, 'utf8');
  // 注意：新增纯函数必须同时改两处 —— 这里的截取列表，以及文件下方的顶层 let 声明。
  // 漏改后者不会报「哪个函数缺了」，而是整段注入执行时 ReferenceError，
  // 表现成「无法从 ui.js 加载 renderMD」，极易误判。
  const code = [
    extractFunction(src, 'escapeHtml'),
    extractFunction(src, 'renderMD'),
    extractFunction(src, 'toolLabel'),
    extractFunction(src, 'countLines'),
    extractFunction(src, 'fmtTokens'),
    extractFunction(src, 'wsDisplayName'),
    'module.exports = { renderMD: renderMD, toolLabel: toolLabel, countLines: countLines, fmtTokens: fmtTokens, wsDisplayName: wsDisplayName };',
  ].join('\n');
  return new Function('module', code + '\nreturn module.exports;')({});
}

// ---------------------------------------------------------------------------
// 极简断言框架
// ---------------------------------------------------------------------------
let passed = 0;
const failures = [];

function group(title) {
  console.log('\n' + title);
}
function check(name, cond) {
  if (cond) {
    passed++;
    console.log('  \u2713 ' + name);
  } else {
    failures.push(name);
    console.log('  \u2717 ' + name);
  }
}

// ---------------------------------------------------------------------------
// 开始
// ---------------------------------------------------------------------------
let renderMD, toolLabel, countLines, fmtTokens, wsDisplayName;
try {
  const api = loadRenderer();
  renderMD = api.renderMD;
  toolLabel = api.toolLabel;
  countLines = api.countLines;
  fmtTokens = api.fmtTokens;
  wsDisplayName = api.wsDisplayName;
} catch (err) {
  console.error('无法从 ui.js 加载 renderMD：' + err.message);
  process.exit(1);
}

// ---------- 1. 基础 Markdown ----------
group('基础 Markdown');
check('h1–h4', /<h1>a<\/h1>/.test(renderMD('# a')) &&
               /<h2>b<\/h2>/.test(renderMD('## b')) &&
               /<h3>c<\/h3>/.test(renderMD('### c')) &&
               /<h4>d<\/h4>/.test(renderMD('#### d')));
check('粗体 / 斜体', renderMD('**b** 和 *i*').includes('<strong>b</strong>') &&
                     renderMD('**b** 和 *i*').includes('<em>i</em>'));
check('行内代码', renderMD('用 `x` 表示').includes('<code>x</code>'));
check('链接', renderMD('[标题](https://example.com)')
  .includes('<a href="https://example.com" target="_blank" rel="noopener">标题</a>'));
check('引用', renderMD('> 引用行').includes('<blockquote>引用行</blockquote>'));
check('分隔线', renderMD('---').includes('<hr>'));
check('无序列表', /<ul><li>a<\/li><li>b<\/li><\/ul>/.test(renderMD('- a\n- b')));
check('有序列表', /<ol><li>a<\/li><li>b<\/li><\/ol>/.test(renderMD('1. a\n2. b')));
check('段内单换行转 <br>', renderMD('第一行\n第二行').includes('<br>'));
check('HTML 被转义（防注入）', renderMD('<img src=x onerror=alert(1)>')
  .includes('&lt;img src=x onerror=alert(1)&gt;'));
check('代码块内容不被二次处理', (function () {
  const out = renderMD('```\n**not bold**\n```');
  return out.includes('<pre><code>**not bold**') && !out.includes('<strong>');
})());

// ---------- 2. 表格 ----------
group('表格');
const tableSrc = [
  '## 常见变体',
  '| 名称 | 适用场景 | 特点 |',
  '|------|:--:|------:|',
  '| 傅里叶变换 | 连续信号 | 理论基础 |',
  '| 快速傅里叶变换 | 数字信号 | 工程必备 |',
].join('\n');
const tableOut = renderMD(tableSrc);
const firstTable = (tableOut.match(/<table>[\s\S]*?<\/table>/) || [''])[0];

check('生成 table/thead/tbody', /^<table><thead><tr><th>名称<\/th>/.test(firstTable) &&
                                firstTable.includes('<tbody>'));
check('表头列数正确', (firstTable.match(/<th[ >]/g) || []).length === 3);
check('数据行数正确', (firstTable.match(/<tbody><tr>/g) || []).length === 1 &&
                      (firstTable.match(/<tr>/g) || []).length === 3);
check('左对齐', /<th>名称<\/th>/.test(firstTable));
check('居中 :--:', /<th style="text-align:center">适用场景<\/th>/.test(firstTable));
check('右对齐 ---:', /<th style="text-align:right">特点<\/th>/.test(firstTable));
check('表格未被包进 <p>', !/<p>(?:(?!<\/p>)[\s\S])*<table/.test(tableOut));
check('标题紧邻表格仍各自成块', /<h2>常见变体<\/h2>\n<table>/.test(tableOut));
check('表格不吞掉后续块', /<\/table>\n<h2>实际应用<\/h2>/.test(
  renderMD('| a | b |\n|---|---|\n| 1 | 2 |\n\n## 实际应用')));
check('普通竖线文本不被误判', renderMD('a | b 只是文本').includes('a | b 只是文本'));
check('缺少分隔行时不当作表格', !renderMD('| a | b |').includes('<table>'));

// ---------- 3. LaTeX 公式 ----------
group('LaTeX 公式（零依赖样式化）');
const blockMath = renderMD('$$F(\\omega) = \\int_{-\\infty}^{+\\infty} f(t)\\, dt$$');
const inlineMath = renderMD('把 $O(N^2)$ 降到 $O(N\\log N)$');

check('$$…$$ 渲染为块级', /^<span class="math-block">F\(\\omega\) = \\int_\{-\\infty\}\^\{\+\\infty\} f\(t\)\\, dt<\/span>$/.test(blockMath));
check('块级公式未被包进 <p>', !blockMath.includes('<p>'));
check('行内 $…$ 渲染为行内', (inlineMath.match(/<span class="math-inline">/g) || []).length === 2);
check('$a*b*c$ 未被吃成斜体', (function () {
  const out = renderMD('公式 $a*b*c$ 不能被吃成斜体');
  return out.includes('<span class="math-inline">a*b*c</span>') && !out.includes('<em>');
})());
check('$x_1$ 下标不被吞', renderMD('下标 $x_1$').includes('<span class="math-inline">x_1</span>'));
check('公式不干扰行内代码', renderMD('$x_1$ 与 `y`').includes('<code>y</code>'));
check('货币「$5 到 $10」不误判', (function () {
  const out = renderMD('价格 $5 到 $10 不算公式');
  return out.includes('价格 $5 到 $10 不算公式') && !out.includes('math-inline');
})());
check('\\(…\\) 行内', renderMD('行内 \\(x+y\\)').includes('<span class="math-inline">x+y</span>'));
check('\\[…\\] 块级', renderMD('\\[x+y\\]').includes('<span class="math-block">x+y</span>'));
check('代码块内的公式不被处理', (function () {
  const out = renderMD('```\n$$不应被处理$$\n```');
  return out.includes('$$不应被处理$$') && !out.includes('math-');
})());
check('表格单元格内公式可用', renderMD('| a |\n|---|\n| $O(N^2)$ |')
  .includes('<td><span class="math-inline">O(N^2)</span></td>'));

// ---------- 4. 工具卡片文案 ----------
group('工具卡片文案（toolLabel）');
check('read_file 拼文件名', toolLabel('read_file', { path: 'src/main.go' }) === '读取了文件 main.go');
check('list_dir 不带目标', toolLabel('list_dir', { path: 'a/b' }) === '查看项目');
check('search_files 不带目标', toolLabel('search_files', { query: 'x' }) === '搜索项目');
check('write_file 只取末段', toolLabel('write_file', { path: 'C:\\tmp\\a.txt' }) === '创建 a.txt');
check('edit_file 只取末段', toolLabel('edit_file', { path: '/x/y/z.py' }) === '编辑 z.py');
check('delete_file 删除了文件', toolLabel('delete_file', { path: 'old.log' }) === '删除了文件 old.log');
check('run_command 运行命令原文', toolLabel('run_command', { command: 'go build ./...' }) === '运行 go build ./...');
check('缺参数时降级为纯短语', toolLabel('write_file', {}) === '创建了文件' &&
                              toolLabel('run_command', {}) === '运行了命令');
check('未知工具回退为「调用了 <name>」',
  toolLabel('some_plugin_tool', {}) === '调用了 some_plugin_tool');
check('delegate_subagents 显示插件名与数量',
  toolLabel('delegate_subagents', { tasks: [{}, {}, {}] }) === '使用插件 Multi-Agent：并行委派 3 个子智能体');
check('delegate_subagents 缺任务数组时仍可用',
  toolLabel('delegate_subagents', {}) === '使用插件 Multi-Agent：并行委派 子智能体');
check('save_memory 文案', toolLabel('save_memory', {}) === '保存了记忆');
check('字符串形式的 tool_input 也能解析',
  toolLabel('read_file', '{"path":"a/b.js"}') === '读取了文件 b.js');
check('超长命令被截断', toolLabel('run_command', { command: 'x'.repeat(200) }).length <= 90);
check('超长文件名被截断',
  toolLabel('read_file', { path: 'y'.repeat(100) + '.go' }).length <= 80);

// ---------- 4.5 思考过程行数统计（>10 行触发页内页滚动） ----------
const css = fs.readFileSync(CSS_PATH, 'utf8');
group('思考过程行数（countLines）');
check('空串为 0 行', countLines('') === 0);
check('无换行的单行文本为 1 行', countLines('hello') === 1);
check('10 个换行 = 11 行（超过阈值）', countLines('a\nb\nc\nd\ne\nf\ng\nh\ni\nj\nk') === 11);
check('9 个换行 = 10 行（未超阈值）', countLines('a\nb\nc\nd\ne\nf\ng\nh\ni\nj') === 10);
check('末尾换行也被计入', countLines('a\nb\n') === 3);
check('样式契约：reason-body.scroll 限高滚动',
  /\.msg-reasoning\s+\.reason-body\.scroll\s*\{[^}]*max-height:\s*220px[^}]*overflow-y:\s*auto/.test(css));

// ---------- 5. app.css 样式契约 ----------
group('app.css 样式契约');
check('.md 重置 white-space（否则块间凭空多一行空隙）',
  /\.msg-assistant\.md\s*\{\s*white-space:\s*normal/.test(css));
check('表格样式存在', /\.msg-assistant\.md\s+table\s*\{/.test(css));
check('行内公式样式存在', /\.msg-assistant\.md\s+\.math-inline/.test(css));
check('块级公式样式存在', /\.msg-assistant\.md\s+\.math-block\s*\{/.test(css));
check('样式契约：子智能体卡片三终态（运行 spinner / 完成绿 / 失败红）',
  /\.msg-subagent\[data-status="running"\]::before/.test(css) &&
  /\.msg-subagent\[data-status="completed"\]/.test(css) &&
  /\.msg-subagent\[data-status="failed"\]/.test(css));

// ---------- 6. 侧栏：项目 / 会话 契约 ----------
// 术语：左栏分组 = 项目（键 = 工作区），分组内条目 = 会话。
// 易回归点：项目行 li.ws-group 也挂在 #session-list 下，会话行三点若用
// `#session-list li` / `#session-list .dots-btn` 限定，会把项目头部的 ⋯ 一起
// 绝对定位到整组中点（表现为「项目的三点渲染到会话里去了」）。
const uiSrc = fs.readFileSync(UI_PATH, 'utf8');
const htmlSrc = fs.readFileSync(path.join(DIST, 'index.html'), 'utf8');
group('侧栏：项目 / 会话');
check('会话行带专属类 session-item', /li\.className\s*=\s*'session-item'/.test(uiSrc));
check('会话行三点规则限定到 li.session-item',
  /#session-list\s+li\.session-item\s*>\s*\.dots-btn\s*\{/.test(css));
check('不存在会误命中项目头的宽泛选择器',
  !/#session-list\s+li\s*\{/.test(css) && !/#session-list\s+\.dots-btn\s*\{/.test(css));
check('项目行 ＋ / ⋯ 统一为同级方形按钮',
  /\.ws-group-head\s+\.label-btn\s*,\s*\n?\s*\.ws-group-head\s+\.dots-btn\s*\{/.test(css));
check('项目行按钮顺序为 ＋ 在前、⋯ 在后',
  uiSrc.indexOf('head.appendChild(add)') >= 0 &&
  uiSrc.indexOf('head.appendChild(add)') < uiSrc.indexOf('head.appendChild(dots)'));
check('侧栏入口文案为「新建项目」', htmlSrc.includes('<span>新建项目</span>') && !htmlSrc.includes('新建对话'));
check('空态文案为「暂无项目」', uiSrc.includes('暂无项目'));

// ---------- 6.1 项目显示名与工作区键解耦（2026-09-14） ----------
// 「重命名项目」只改显示名（workspace_names 表），绝不动 sessions.workspace ——
// 那个键同时是 agent 的工作目录路径，一改项目就失去工作区。
// 历史事故：改键把 C:\...\Desktop\test 抹成 test；中文名还会被转义成 ????。
group('项目显示名（与工作区键解耦）');
check('wsDisplayName 优先用自定义显示名',
  wsDisplayName('C:\\Users\\26536\\Desktop\\test', '我的项目') === '我的项目');
check('没有自定义名时回落路径末段',
  wsDisplayName('C:\\Users\\26536\\Desktop\\test', '') === 'test' &&
  wsDisplayName('/home/u/proj', '') === 'proj');
check('空工作区显示「新项目」',
  wsDisplayName('', '') === '新项目' && wsDisplayName('', undefined) === '新项目');
check('路径末尾有分隔符也能取到末段',
  wsDisplayName('C:\\Users\\26536\\Desktop\\test\\', '') === 'test');
check('后端重命名走 SetWorkspaceName，不再有 RenameWorkspace',
  uiSrc.indexOf('new_name: v') > 0);
check('会话元信息带 workspace_name，侧栏按它渲染项目名',
  /list\[0\]\s*&&\s*list\[0\]\.workspace_name/.test(uiSrc));
check('重命名预填当前显示名（改回原样无副作用）',
  /input\.value = currentName \|\| wsDisplayName\(ws\)/.test(uiSrc));
check('输入框上方的工作区标签已移除（选工作区只在「新建项目」弹窗）',
  !htmlSrc.includes('id="workspace-label"') &&
  !uiSrc.includes('syncWorkspaceLabel') && !uiSrc.includes('wsLabel'));
check('项目 ⋯ 菜单里「恢复默认名」只在设过自定义名时出现',
  /if \(list\[0\] && list\[0\]\.workspace_name\) \{\s*\n\s*items\.push\(\{ label: '恢复默认名'/.test(uiSrc));
check('「恢复默认名」用 new_name 空串清除（后端据此删记录）',
  /function clearWorkspaceName\(ws\)[\s\S]{0,220}?JSON\.stringify\(\{ workspace: ws, new_name: '' \}\)/.test(uiSrc));
check('重命名输入框留空仍是「取消」（不会误清显示名）',
  /if \(!v\) \{ renderSessions\(sessionsCache\); return; \}/.test(uiSrc));

// ---------- 6.15 工作区切换：失败要报错、不能静默错位（2026-09-14） ----------
// 服务端现在会拒绝不存在的目录（与启动时的检查同源），但 **fetch 对 400 也是 resolve** ——
// 前端若不看 res.ok，「切换失败」会被当成成功：新会话会带着没换成的旧工作区
// 悄悄落进上一个项目下。所以这条链路必须显式检查状态码。
group('工作区切换的失败处理');
check('setWorkspace 把状态码暴露成 {ok, error}',
  /return \{ ok: r\.ok, error: \(d && d\.error\) \|\| '' \};/.test(uiSrc));
check('newSessionInWorkspace 先看 res.ok 再建会话',
  /setWorkspace\(ws\)\.then\(function \(res\) \{\s*\n\s*if \(!res\.ok\)/.test(uiSrc));
check('切换失败时报错并回滚 workspaceRoot',
  /addError\(res\.error \|\| \('无法切换到该项目的目录：' \+ ws\)\)/.test(uiSrc) &&
  /loadSessionList2\(\); \/\/ workspaceRoot 回滚成服务端真实状态/.test(uiSrc));
check('选择器目录模式只回传路径，切不切由调用方决定（新建项目弹窗）',
  /if \(pickerOnPick\) pickerOnPick\(cur\);/.test(uiSrc) &&
  !uiSrc.includes('switchWorkspace'));
check('空工作区会话只在切换成功时挂到新工作区（失败时路径是坏的）',
  /if \(res\.ok && path && sessionID\)/.test(uiSrc));

// ---------- 6.2 上下文占用进度条（底部栏右侧） ----------
// 放在设置按钮右侧的空位里：条宽 = 已用 token / 压缩预算，点击向上弹明细。
// 易回归点：① 弹层必须贴右边缘，用 .popup 默认的 left:0 会在窄侧栏里溢出视口；
// ② 百分比数字要用等宽数字，否则跳动时条会左右抖；③ 条宽必须封顶 100%，
// 超预算（历史已被压缩）时数字照实显示 >100%，但条不能画出容器外。
group('上下文占用进度条');
const footerHtml = htmlSrc.slice(htmlSrc.indexOf('class="sidebar-footer"'), htmlSrc.indexOf('</aside>'));
check('进度条挂在底部栏、位于设置按钮右侧',
  footerHtml.indexOf('id="ctx-meter"') > footerHtml.indexOf('id="open-settings"'));
check('进度条含 条 / 填充 / 百分比 / 明细弹层 四个部件',
  htmlSrc.includes('id="ctx-meter"') && htmlSrc.includes('id="ctx-fill"') &&
  htmlSrc.includes('id="ctx-pct"') && htmlSrc.includes('id="ctx-pop"'));
check('底部栏用 space-between 把进度条推到右侧',
  /\.sidebar-footer\s*\{[^}]*justify-content:\s*space-between/.test(css));
check('百分比用等宽数字防抖动', /\.ctx-pct\s*\{[^}]*tabular-nums/.test(css));
check('占用过高变警告 / 危险色',
  /\.ctx-meter\.warn\s+\.ctx-fill\s*\{[^}]*var\(--warn\)/.test(css) &&
  /\.ctx-meter\.danger\s+\.ctx-fill\s*\{[^}]*var\(--danger\)/.test(css));
check('明细弹层贴右边缘（否则侧栏内会溢出视口）',
  /\.ctx-anchor\s+\.ctx-pop\s*\{[^}]*right:\s*0/.test(css));
check('前端订阅服务端 context 事件',
  /case 'context':\s*\n\s*renderCtxUsage\(/.test(uiSrc));
check('点击进度条主动拉取最新占用', uiSrc.indexOf("type: 'context', session_id: sessionID") > 0);
check('条宽封顶 100%（超预算不越界）', /Math\.min\(100,\s*pct\)/.test(uiSrc));
check('阈值与 CSS 类同步（70% 警告 / 90% 危险）',
  /classList\.toggle\('warn',\s*pct >= 70/.test(uiSrc) &&
  /classList\.toggle\('danger',\s*pct >= 90\)/.test(uiSrc));
// ⚠️ 服务端 over_budget 与 compressed 是**两件事**，别再混用（这里踩过一次）：
//   over_budget = 送模占用越过压缩线；compressed = 会话里已经存在摘要。
// 刚超线那一瞬间是 over_budget=true 而 compressed=false。
check('区分 over_budget 与 compressed 两个状态位',
  uiSrc.includes('d.over_budget && !d.compressed') && /classList\.toggle\('compressed'/.test(uiSrc));
check('明细五项：模型名称 / 上下文长度 / 已使用总 / 缓存命中 / 缓存未命中',
  uiSrc.includes("'模型名称'") && uiSrc.includes("'上下文长度'") &&
  uiSrc.includes("'已使用总 tokens'") && uiSrc.includes("'缓存命中'") && uiSrc.includes("'缓存未命中'"));
check('缓存三项来自服务端 usage 字段（total_tokens / cache_hit / cache_miss）',
  uiSrc.includes('d.total_tokens') && uiSrc.includes('d.cache_hit') && uiSrc.includes('d.cache_miss'));
check('样式契约：压缩态有独立标记（.ctx-meter.compressed）',
  /\.ctx-meter\.compressed\s+\.ctx-pct\s*\{/.test(css));

group('token 数缩写（fmtTokens）');
check('0 → 0', fmtTokens(0) === '0');
check('未定义按 0 处理', fmtTokens(undefined) === '0');
check('999 不缩写', fmtTokens(999) === '999');
check('1000 → 1k（去掉多余的 .0）', fmtTokens(1000) === '1k');
check('1050 → 1.1k', fmtTokens(1050) === '1.1k');
check('120000 → 120k', fmtTokens(120000) === '120k');
check('1200000 → 1.2M', fmtTokens(1200000) === '1.2M');

// ---------- 6.5 新建项目：弹窗设置后创建，切工作区必须先于建会话 ----------
// 「新建项目」先弹窗选 工作区/项目名/默认权限；确认后 setWorkspace（HTTP）成功才
// doNewSession（WS）。不等待就会抢跑，新会话被挂到**上一个项目**下。
group('新建项目：弹窗与清空工作区、建会话的先后');
check('setWorkspace 返回 Promise 供调用方等待',
  /const done = fetch\('\/api\/workspace'/.test(extractFunction(uiSrc, 'setWorkspace')) &&
  /return done;/.test(extractFunction(uiSrc, 'setWorkspace')));
check('「新建项目」先弹窗（工作区/项目名/默认权限），确认才创建',
  /openNewProjectDialog\(\);/.test(uiSrc) &&
  /function openNewProjectDialog\(\)/.test(uiSrc) &&
  /id="np-name"/.test(uiSrc) && /id="np-ws-pick"/.test(uiSrc) && /id="np-perm-seg"/.test(uiSrc));
check('确认创建：先等服务端切好工作区，再新建会话',
  /setWorkspace\(npWs\)\.then\(function \(res\)/.test(uiSrc) &&
  /doNewSession\(\); \/\/ 清空对话区 \+ WS new_session/.test(uiSrc));

// ---------- 6.6 Termux 工具安装建议弹窗（安卓平台） ----------
// 服务端报告「Termux 且未装」时弹窗；安装走后台 + 轮询；「不再提示」持久化。
group('Termux 工具安装建议弹窗');
check('启动后探测 /api/termux/tools，非 Termux / 已装 / 不再提示时不弹',
  /function maybeShowTermuxHint\(\)/.test(uiSrc) &&
  /!d\.termux \|\| d\.installed \|\| d\.installing/.test(uiSrc) &&
  /TH_NEVER_KEY\) === 'never'/.test(uiSrc));
check('弹窗文案含建议安装 termux-tools 与跳过/不再提示/安装按钮',
  uiSrc.includes('建议您安装以下工具') && uiSrc.includes('<b>termux-tools</b>') &&
  /id="th-skip"/.test(uiSrc) && /id="th-never"/.test(uiSrc) && /id="th-install"/.test(uiSrc));
check('「不再提示」写 localStorage（跳过只关本次）',
  /localStorage\.setItem\(TH_NEVER_KEY, 'never'\)/.test(uiSrc));
check('安装走 POST + 轮询 GET，装好显示已安装并自动关窗',
  /fetch\('\/api\/termux\/tools', \{ method: 'POST' \}\)/.test(uiSrc) &&
  /function startThPolling\(\)/.test(uiSrc) &&
  /renderThInstall\('已安装 ✓', true\)/.test(uiSrc));
check('失败可重试：错误显示在弹窗内，按钮恢复',
  /function thFail\(msg\)/.test(uiSrc) &&
  /renderThInstall\('重试安装'\)/.test(uiSrc));

// ---------- 6.7 内置选择器：picker 模式修「选目录列表为空」 ----------
// Termux 上选择器起始目录（~ 等）在工作区外，tree 不带 picker 会 403 → 列表空。
group('内置选择器 picker 模式');
check('browse 请求带 picker=1（选工作区必须能浏览工作区外目录）',
  /fetch\('\/api\/tree\?depth=1&picker=1'/.test(uiSrc));
check('tree 返回错误时显示错误而非静默渲染成「无子目录」',
  /if \(data\.error\) \{[\s\S]{0,120}?picker-empty/.test(uiSrc));

// ---------- 7. 输入框：空对话居中 / 有对话下放 ----------
// 空对话（消息区无任何记录）时输入卡片在主对话栏内上下左右居中；用户发出第一句话
// 后下放回底部。易回归点：① 居中态必须由 CSS 类控制（JS 只切类，滚动折叠会写内联
// transform，会把居中态覆盖掉）；② bottom 必须参与 transition，否则切换是硬跳变。
group('输入框：空对话居中 / 有对话下放');
// 实现约定：居中就是「直接平移」——锚点 bottom 全程不动，只有 transform 在变。
// 这样切换走合成器（不逐帧重排），且和滚动折叠共用同一个属性、不会互相打架。
check('居中态是纯平移（只改 transform）',
  /#composer-wrap\.composer-centered\s*\{[^}]*transform:\s*translate\(-50%,\s*calc\(5vh - 50vh \+ 50%\)\)/.test(css));
check('居中态不动 bottom（锚点恒定，否则会逐帧重排）',
  !/#composer-wrap\.composer-centered\s*\{[^}]*bottom:/.test(css));
check('位置变化只由 transform 过渡（不把 bottom 放进 transition）',
  /#composer-wrap\s*\{[^}]*transition:\s*transform\s+\.\d+s[^;]*;/.test(css) &&
  !/#composer-wrap\s*\{[^}]*transition:[^;]*bottom/.test(css));
check('输入区不强制长期合成层（避免文字低清纹理）',
  !/#composer-wrap\s*\{[^}]*will-change:\s*transform/.test(css));
check('JS 切类时不写死位置（居中态清掉内联 transform 让样式类生效）',
  /composerWrap\.style\.transform\s*=\s*''/.test(extractFunction(uiSrc, 'syncComposerMode')) &&
  !/style\.bottom\s*=/.test(extractFunction(uiSrc, 'syncComposerMode')));
check('空态判定 = 消息区无子元素',
  /messagesEl\.childElementCount\s*===\s*0/.test(extractFunction(uiSrc, 'syncComposerMode')));
check('用户发言（addUser）后下放',
  extractFunction(uiSrc, 'addUser').includes('syncComposerMode()'));
check('新建会话（doNewSession）后回到居中',
  extractFunction(uiSrc, 'doNewSession').includes('syncComposerMode()'));
check('会话回放（replayHistory）后同步（空会话居中 / 有历史下放）',
  extractFunction(uiSrc, 'replayHistory').includes('syncComposerMode()'));
check('居中态不参与滚动折叠（scroll 提前返回）',
  /addEventListener\('scroll',\s*function[\s\S]{0,200}?if\s*\(composerCentered\)\s*return/.test(uiSrc));
check('页面加载自动回放历史时不做过渡（composer-no-anim）',
  /#composer-wrap\.composer-no-anim\s*\{\s*transition:\s*none/.test(css));
check('ready 自动恢复前重新武装「直接落位」标记',
  /case 'ready':[\s\S]{0,500}?composerSnap\s*=\s*true/.test(uiSrc));
check('用户发言走平滑下放（submit 清掉落位标记后 addUser）',
  /composerSnap\s*=\s*false;[\s\S]{0,300}?addUser\(p\.display\)/.test(uiSrc));

// ---------- 8. 输入框 @ 提及：技能 + 插件 ----------
// 输入 @ 弹出面板，同时列「技能」和「插件」两组；插件条目还要显示它注册的工具
// （MCP 工具以「插件名.工具名」注册，故可从 /api/config 的 tools 表按前缀归属）。
// 易回归点：① 每敲一个字符就打一轮接口（应该是「每次新打开 @ 拉一次 + 本地过滤」）；
// ② 分组标题被当成可选项（上下键会把标题算进去）。
group('输入框 @ 提及：技能 + 插件');
check('同时拉技能与插件列表',
  /fetch\('\/api\/skills'\)/.test(uiSrc) && /fetch\('\/api\/plugins'\)/.test(uiSrc));
check('插件工具按「插件名.」前缀从工具表归属',
  /indexOf\(p\.name \+ '\.'\)\s*===\s*0/.test(uiSrc));

// ---------- 8.5 ＋菜单 / @文件 stage / @高亮（2026-09-14） ----------
group('＋菜单与 @文件');
check('＋菜单含「当前目录的文件」入口（内置选择器·文件模式）',
  /more-browse-workspace/.test(uiSrc) && /more-browse-workspace/.test(
    fs.readFileSync(path.join(__dirname, '../dist/index.html'), 'utf8')));
check('浏览入口起点 = 工作区根，未选工作区给提示',
  /openBuiltinPicker\(workspaceRoot, 'file'/.test(uiSrc) &&
  /还没有选择工作区/.test(uiSrc));
check('@文件路径提及只显示文件名（title 悬浮完整路径）',
  uiSrc.indexOf("'at-mention'") > 0 && uiSrc.indexOf('s.title = body') > 0);
check('@提及蓝色高亮样式契约（.at-mention 用 accent）',
  /\.at-mention\s*\{[^}]*color:\s*var\(--accent\)/.test(css));
check('发送前 stage 区外文件到 attachments（/api/stage_file）',
  /\/api\/stage_file/.test(uiSrc) && /attachments/.test(uiSrc));
check('分「技能」「插件」两组',
  /addGroup\('技能'\)/.test(uiSrc) && /addGroup\('插件'\)/.test(uiSrc));
check('插件条目显示可调用工具 / 未启用状态',
  /'工具：' \+ p\.toolNames\.join/.test(uiSrc) && uiSrc.includes('未启用（见 设置 → MCP 服务）'));
check('分组标题不参与上下键选择（dataset.idx 对齐 atItems）',
  /b\.dataset\.idx = String\(idx\)/.test(uiSrc) &&
  /Number\(b\.dataset\.idx\) === atIdx/.test(uiSrc));
check('数据按「每次新打开 @ 拉一次」缓存，不在输入时反复请求',
  /atLoadedFor !== cur\.start/.test(uiSrc) && /if \(!atData\) \{ loadAtData\(\); return; \}/.test(uiSrc));
check('一条都没有时给提示而不是静默无反应',
  /还没有可提及的技能或插件/.test(uiSrc) && /at-empty/.test(uiSrc));
check('只有提示行时上下键不越界',
  /if \(!atItems\.length\) return;/.test(uiSrc));
check('样式契约：分组标题 / 工具清单 / 空提示',
  /\.at-skill-pop\s+\.at-group\s*\{/.test(css) &&
  /\.at-skill-pop\s+button\s+\.at-tools\s*\{/.test(css) &&
  /\.at-skill-pop\s+\.at-empty\s*\{/.test(css));

// ---------- 8.55 ＋添加文件只插入「@文件名」 ----------
// 需求：＋→添加文件 不要往输入框塞完整路径，只放 @文件名（蓝色高亮），
// 真实路径记进别名表、发送前展开。文件名含空白时用 @"名字"（提及语法以空白分隔）。
group('＋添加文件：只插 @文件名');
check('插入的是提及 token 而不是完整路径',
  /function insertPickedFile\(p\) \{[\s\S]{0,200}?const token = mentionTokenFor\(p\)[\s\S]{0,200}?insertIntoInput\(token\)/.test(uiSrc));
check('文件名含空白 / 引号时改用 @"..." 形式',
  /function mentionToken\(name\) \{[\s\S]{0,120}?@"/.test(uiSrc));
check('别名表 + 发送前展开（在 staging 之前）',
  /const fileAlias = new Map\(\)/.test(uiSrc) &&
  /function expandFileAliases\(text\)/.test(uiSrc) &&
  /const outgoing = expandFileAliases\(raw\);[\s\S]{0,200}?prepareMentions\(outgoing, raw\)/.test(uiSrc));
check('工作区外文件的提示也只报文件名（不暴露完整路径）',
  /已插入工作区外的文件 ' \+ name/.test(uiSrc));
check('服务端说 inside（本来就在区内、没复制）时不弹「已复制到 attachments」的假提示',
  /if \(!d\.inside\) \{\s*\n\s*notes\.push\('已把 '/.test(uiSrc));
check('别名表随输入内容收缩（删掉提及后同名 token 不被旧路径劫持）',
  /function pruneFileAliases\(text\)/.test(uiSrc) &&
  /function syncInputMirror\(\) \{\s*\n\s*\/\/[^\n]*\n\s*pruneFileAliases\(input\.value\);/.test(uiSrc));
check('fileAlias 声明早于 syncInputMirror 定义（否则首次调用撞 TDZ）',
  uiSrc.indexOf('const fileAlias = new Map()') > 0 &&
  uiSrc.indexOf('const fileAlias = new Map()') < uiSrc.indexOf('function syncInputMirror()'));
check('同名文件不互相顶掉（token 冲突时逐级多带父目录）',
  /function mentionTokenFor\(fullPath\)/.test(uiSrc) &&
  /const token = mentionTokenFor\(p\)/.test(uiSrc));
check('气泡显示用户输入原文（p.display），发送用展开后的 p.text',
  /addUser\(p\.display\)/.test(uiSrc) && /lastUserText = p\.text/.test(uiSrc));
check('别名展开必须早于清空输入框（否则 prune 先把别名删光，展开拿不到）',
  /const outgoing = expandFileAliases\(raw\);[\s\S]{0,200}?input\.value = '';[\s\S]{0,200}?prepareMentions\(outgoing, raw\)/.test(uiSrc));
check('prepareMentions 显式分开「发送文本」与「气泡显示文本」',
  /async function prepareMentions\(text, display\)/.test(uiSrc) &&
  /return \{ text: text, display: display === undefined \? text : display, notes: notes \}/.test(uiSrc));
check('别名展开后重新加引号（路径含空白时不被切断）',
  /const real = fileAlias\.get\(part\);\s*\n\s*return real \? mentionToken\(real\) : part;/.test(uiSrc));
check('气泡去掉引号、镜像层逐字原样（否则与 textarea 错位）',
  /s\.textContent = shorten \? '@' \+ body : p;/.test(uiSrc));

// 提及 token 切分：String.split 会把**所有**捕获组都塞进结果数组，正则里多一个分组
// 就会把内容多切一份（引号内的名字曾被重复吐出来）。这里抽出真正则跑一遍。
const mentionReSrc = (uiSrc.match(/const MENTION_SPLIT = (\/.*?\/);/) || [])[1];
const mentionRe = mentionReSrc ? eval(mentionReSrc) : null;
const splitMention = (s) => String(s).split(mentionRe);
group('提及切分（MENTION_SPLIT）');
check('正则可抽出，且只有一个捕获组（无嵌套分组）',
  !!mentionRe && (mentionRe.source.match(/\((?!\?:|\?=|\?!)/g) || []).length === 1);
check('不带 g 标志（带 g 的正则被拿去做 test/exec 会残留 lastIndex）',
  !!mentionRe && !mentionRe.global);
check('普通 @token 只切一份',
  JSON.stringify(splitMention('a @b c')) === JSON.stringify(['a ', '@b', ' c']));
check('带引号的 @token 只切一份（含空白）',
  JSON.stringify(splitMention('x @"c d.txt" y')) === JSON.stringify(['x ', '@"c d.txt"', ' y']));
check('无提及时不切分',
  JSON.stringify(splitMention('纯文本')) === JSON.stringify(['纯文本']));

// ---------- 8.6 输入框里的 @提及 蓝色高亮（2026-09-14） ----------
// textarea 自身无法局部着色，方案是垫一层排版完全一致的 .input-mirror 画字，
// textarea 文字设成 transparent（只留光标）。两层排版参数一旦不同步就会错位，
// 所以这里把「共享排版块」和几个同步点都锁住。
group('输入框 @提及 高亮（镜像层）');
check('输入框外包一层 #input-wrap，镜像层与 textarea 同级',
  /id="input-wrap"/.test(htmlSrc) &&
  /id="input-mirror"[\s\S]{0,400}?id="input"/.test(htmlSrc));
check('镜像层与 textarea 共用同一套排版（font/行高/内边距/换行）',
  /#composer textarea,\s*\n?\s*\.input-mirror\s*\{[^}]*font-size:\s*14px[^}]*line-height:\s*1\.5[^}]*padding:\s*2px 2px 6px[^}]*white-space:\s*pre-wrap/.test(css));
check('textarea 文字透明、只留光标（否则两层字会叠影）',
  /#composer textarea\s*\{[^}]*color:\s*transparent[^}]*caret-color:\s*var\(--text\)/.test(css));
check('镜像层绝对定位铺满、不接收事件、不可选中',
  /\.input-mirror\s*\{[^}]*position:\s*absolute[^}]*inset:\s*0[^}]*pointer-events:\s*none/.test(css));
check('隐藏 textarea 滚动条（滚动条会挤掉宽度 → 错位）',
  /#composer textarea\s*\{[^}]*scrollbar-width:\s*none/.test(css));
check('镜像层里渲染 @提及 时原样显示（不缩成文件名，否则与真实文字错位）',
  /renderUserText\(inputMirror, input\.value, \{ shortenPath: false \}\)/.test(uiSrc));
check('末尾补零宽字符，保证以换行结尾时镜像也保留空行',
  /inputMirror\.appendChild\(document\.createTextNode\('\\u200b'\)\)/.test(uiSrc));
check('内容变化即重画：input 事件',
  /input\.addEventListener\('input', function \(\) \{\s*\n?\s*syncInputMirror\(\)/.test(uiSrc));
check('滚动位置同步：scroll 事件',
  /input\.addEventListener\('scroll', syncInputMirror\)/.test(uiSrc));
check('程序化改动也同步：发出后清空',
  /input\.value = '';\s*\n\s*syncInputMirror\(\)/.test(uiSrc));
check('程序化改动也同步：@面板选中 / ＋菜单插入',
  /input\.value = before \+ '@' \+ it\.name \+ ' ' \+ after;\s*\n\s*syncInputMirror\(\)/.test(uiSrc) &&
  /input\.value = before \+ sep \+ ins \+ after;\s*\n\s*syncInputMirror\(\)/.test(uiSrc));

// ---------- 9. 发送 / 打断按钮共色 ----------
group('发送 / 打断按钮颜色');
check('运行中的打断按钮与发送按钮共用 accent 色',
  /#send-btn\s*\{[^}]*background:\s*var\(--accent\)/.test(css) &&
  /#send-btn\.running\s*\{[^}]*background:\s*var\(--accent\)/.test(css) &&
  !/#send-btn\.running\s*\{[^}]*background:\s*var\(--text-dim\)/.test(css));

// ---------- 10. 任务等待提示：模型响应前 / 工具返回后 ----------
group('任务等待提示');
check('等待文案为「等待模型响应」',
  /等待模型响应/.test(uiSrc) && !/innerHTML = '思考中/.test(uiSrc));
check('用户发送后立即显示等待提示',
  /addUser\(p\.display\);[\s\S]{0,260}?showThinking\(\)/.test(uiSrc));
check('busy 事件显示等待提示',
  /case 'busy':[\s\S]{0,350}?showThinking\(\)/.test(uiSrc));
check('tool_result 后模型再次等待时显示提示',
  /case 'tool_result':[\s\S]{0,350}?if \(running\) showThinking\(\)/.test(uiSrc));
check('reasoning/text/tool_call 到来时移除等待提示',
  /case 'reasoning':[\s\S]{0,100}?removeThinking\(\)/.test(uiSrc) &&
  /case 'text':[\s\S]{0,100}?removeThinking\(\)/.test(uiSrc) &&
  /case 'tool_call':[\s\S]{0,120}?removeThinking\(\)/.test(uiSrc));
check('idle/error/hitl 结束或暂停时移除等待提示',
  /case 'idle':[\s\S]{0,100}?removeThinking\(\)/.test(uiSrc) &&
  /case 'error':[\s\S]{0,100}?removeThinking\(\)/.test(uiSrc) &&
  /case 'hitl_request':[\s\S]{0,100}?removeThinking\(\)/.test(uiSrc));

// ---------- 10. ＋ 更多菜单：添加文件 / Skills（右展） ----------
// 点 ＋ → 图标变 ✕ + 上拉菜单；「添加文件」按平台分流（Windows 资源管理器 / 其他内置选择器），
// 「Skills」向右展开已加载技能并点选插入 @技能名。
group('＋ 更多菜单：添加文件 / Skills');
// ＋↔✕ 用「加号旋转 45°」做形变（旋转 45° 的加号就是叉号），所以有过渡动画；
// 换字符（textContent='✕'）是硬跳，不能再退回去。
check('＋→✕ 用旋转形变而非换字符',
  /moreBtn\.classList\.toggle\('open',/.test(uiSrc) && !/moreBtn\.textContent\s*=/.test(uiSrc));
check('旋转带过渡，且尊重「减少动态效果」',
  /#more-btn\.open svg\s*\{\s*transform:\s*rotate\(45deg\)/.test(css) &&
  /#more-btn svg\s*\{\s*transition:\s*transform/.test(css) &&
  /#more-btn svg\s*\{\s*transition:\s*none/.test(css));
check('按钮内含加号 SVG（旋转的是图标本身）',
  /id="more-btn"[\s\S]{0,300}?<svg[\s\S]{0,200}?M12 5v14M5 12h14/.test(htmlSrc));
check('菜单两项存在', /id="more-add-file"/.test(htmlSrc) && /id="more-skills"/.test(htmlSrc));
check('Skills 子菜单向右展开',
  /popup popup-right/.test(htmlSrc) && /\.popup\.popup-right\s*\{[^}]*left:\s*calc\(100% \+/.test(css));
check('添加文件按平台分流：Windows 资源管理器 / 其他内置选择器',
  /fetch\('\/api\/pick_file'/.test(uiSrc) &&
  /if \(res\.data\.builtin\)/.test(uiSrc) &&
  /openBuiltinPicker\(res\.data\.start_path \|\| '', 'file', insertPickedFile\)/.test(uiSrc));
check('内置选择器支持文件模式（文件可点选 + 确认键禁用态）',
  /pickerMode === 'file' \? '选择此文件' : '选择当前目录'/.test(uiSrc) &&
  /okBtn\.disabled = pickerMode === 'file'/.test(uiSrc));
check('选中后把路径插到输入框光标处',
  /function insertIntoInput/.test(uiSrc) && /insertPickedFile\(res\.data\.path\)/.test(uiSrc));
check('工作区外文件给出提示（代理默认读不到）',
  /代理默认只能读工作区内的文件/.test(uiSrc));
// 关闭路径要走全局机制（点空白处 mousedown → closeAllPops、打开权限/模型弹层时互斥），
// 并且**图标要跟着弹层的实际显隐走** —— 否则别的路径关掉菜单后图标停在 ✕，下次得点两下。
check('打开时收起其它弹层（与权限/模型弹层互斥）',
  /closeAllPops\(morePop\)/.test(uiSrc));
check('关闭走全局 hideWithAnim / 点空白处 closeAllPops',
  /hideWithAnim\(morePop\)/.test(uiSrc) &&
  /if \(!e\.target\.closest\('\.pop-anchor'\)\) closeAllPops\(null\)/.test(uiSrc));
check('图标状态跟随弹层实际显隐（防停在 ✕）',
  /new MutationObserver/.test(uiSrc) &&
  /if \(visible !== moreOpen\) syncMoreIcon\(visible\)/.test(uiSrc));
check('Esc 关闭', /e\.key === 'Escape' && moreOpen/.test(uiSrc));
check('Skills 点选插入 @技能名（复用 @ 提及语义）',
  /insertIntoInput\('@' \+ sk\.name\)/.test(uiSrc));
check('样式契约：禁用态 / 选中态 / 右展子菜单',
  /\.picker-actions button:disabled\s*\{/.test(css) &&
  /\.picker-item\.sel\s*\{/.test(css) &&
  /\.popup\.popup-right\s*\{/.test(css));

// ---------------------------------------------------------------------------
// 任务完成系统通知：前端必须上报页面可见性（窗口可见时不弹通知、不打扰）
check('连接建立后上报页面可见性',
  /function sendVisibility/.test(uiSrc) &&
  /wsSend\(\{ type: 'visibility', hidden: !!document\.hidden \}\)/.test(uiSrc));
check('切换标签页/最小化时同步可见性',
  /document\.addEventListener\('visibilitychange', sendVisibility\)/.test(uiSrc));
check('open 事件里主动上报一次可见性',
  /sendVisibility\(\); \/\/ 告知服务端当前页面是否可见/.test(uiSrc));
// 设置页「任务完成通知」开关：读 /api/notify 回填、切换写回；样式契约随开关组件
check('设置页通知开关会读回服务端偏好',
  /function refreshNotifySwitch/.test(uiSrc) &&
  /fetch\('\/api\/notify'\)/.test(uiSrc));
check('通知开关切换后写回服务端（失败回滚）',
  /fetch\('\/api\/notify',\s*\{[\s\S]{0,80}method: 'POST'/.test(uiSrc) &&
  /JSON\.stringify\(\{ enabled: want \}\)/.test(uiSrc) &&
  /el\.checked = !want/.test(uiSrc));
check('通知开关 DOM 与样式契约',
  /id="settings-notify"/.test(htmlSrc) &&
  /\.switch-track/.test(css) && /\.switch-knob/.test(css) &&
  /\.switch input:checked \+ \.switch-track/.test(css));
// 「总开关」语义必须写在界面上，否则用户无法判断关掉它到底管多宽
check('通知为总开关：标题带总开关标记且描述点明关闭即全关',
  /id="settings-notify"/.test(htmlSrc) &&
  /总开关/.test(htmlSrc) &&
  /class="tag-total"/.test(htmlSrc) &&
  /\.tag-total\s*\{/.test(css));

// ---------------------------------------------------------------------------
// 子智能体设置：总开关 / 并发上限 / 能力限制
check('子智能体设置页存在且四项控件齐全',
  /id="page-subagents"/.test(htmlSrc) &&
  /data-page="subagents"/.test(htmlSrc) &&
  /id="sub-enabled"/.test(htmlSrc) &&
  /id="sub-max"/.test(htmlSrc) &&
  /id="sub-allow-write"/.test(htmlSrc) &&
  /id="sub-allow-delete"/.test(htmlSrc) &&
  /id="sub-allow-memory"/.test(htmlSrc));
check('子智能体设置读写服务端 /api/subagents',
  /function loadSubagentSettings/.test(uiSrc) &&
  /fetch\('\/api\/subagents'\)/.test(uiSrc) &&
  /fetch\('\/api\/subagents',\s*\{[\s\S]{0,80}method: 'POST'/.test(uiSrc));
// 部分更新：只提交被改动的一项，避免用界面旧值覆盖别处刚改的开关
check('子智能体改动按单项提交（部分更新语义）',
  /postSubagentSetting\(\{ enabled: want \}/.test(uiSrc) &&
  /postSubagentSetting\(\{ max_concurrent: Number\(range\.value\) \}/.test(uiSrc) &&
  /patch\[pair\[1\]\] = el\.checked/.test(uiSrc));
// 失败回滚：写失败时把控件恢复原状，不能让界面显示成「已保存」
check('子智能体设置失败回滚',
  /if \(!ok\) on\.checked = !want/.test(uiSrc) &&
  /if \(!ok\) el\.checked = !el\.checked/.test(uiSrc));
// 联动置灰：总开关关闭 → 其余禁用；禁写 → 删除开关也禁用
check('子智能体控件联动置灰（总开关/禁写）',
  /function syncSubagentLocks/.test(uiSrc) &&
  /disabled = !enabled/.test(uiSrc) &&
  /sub-allow-delete'\)\.disabled = true/.test(uiSrc));

// ---------------------------------------------------------------------------
// 添加模型：/models 拉取列表 → 选中 → 填入模型 id 到表单
check('模型表单有「获取模型列表」入口',
  /id="mf-discover"/.test(htmlSrc) &&
  /id="mf-discover-panel"/.test(htmlSrc) &&
  /id="disc-list"/.test(htmlSrc) &&
  /id="disc-add"/.test(htmlSrc));
check('发现模型走 /api/models/discover',
  /function discoverModels/.test(uiSrc) &&
  /fetch\('\/api\/models\/discover'/.test(uiSrc));
// 已在库中的模型不可选、加「已添加」标记；选中只填 id，不再直接批量写库
check('已在库的模型标记为已添加且不可选',
  /cb\.disabled = !!m\.in_library/.test(uiSrc) &&
  /badge\.textContent = '已添加'/.test(uiSrc) &&
  /'disc-item' \+ \(m\.in_library \? ' added' : ''\)/.test(uiSrc) &&
  /\.disc-item\.added\s*\{/.test(css));
check('「添加选中」把模型 id 填入表单（不再调 save_batch）',
  /function fillSelectedDiscModel/.test(uiSrc) &&
  /document\.getElementById\('mf-id'\)\.value = id/.test(uiSrc) &&
  !/models\/save_batch/.test(uiSrc));
check('上游列表单选（radio）',
  /cb\.type = 'radio'/.test(uiSrc) &&
  /discSelected = \{\}; discSelected\[m\.id\] = true;/.test(uiSrc));
check('上游列表只保留筛选 + 收起，去掉全选/清空',
  /id="disc-filter"/.test(htmlSrc) &&
  /id="disc-close"/.test(htmlSrc) &&
  !htmlSrc.includes('id="disc-all"') && !htmlSrc.includes('id="disc-none"') &&
  /document\.getElementById\('disc-filter'\)\.addEventListener\('input', renderDiscList\)/.test(uiSrc));
check('填入后收起面板并提示补密钥保存',
  /setTestResult\(true, '已填入模型 id/.test(uiSrc) &&
  /collapseDiscover\(\);\s*\n\s*\}/.test(uiSrc));
check('切换/保存模型时收起上游列表（防状态串味）',
  /collapseDiscover\(\); \/\/ 换了模型就收起上一次的上游列表/.test(uiSrc) &&
  /collapseDiscover\(\);\s*\n\s*showMTab\('list'\)/.test(uiSrc));

// ---------------------------------------------------------------------------
// 模型管理：子 Tab「添加模型」+ 列表「编辑」复用同款表单 + 保存后进入对话框模型选择
// 需求链路：设置 → 模型 → 添加模型 → 保存 = 模型列表与对话框弹层同时出现新条目。
group('模型管理：添加 / 编辑 / 对话框选择');
check('子 Tab 文案为「模型列表 / 添加模型」',
  /data-mtab="config">添加模型</.test(htmlSrc) && !htmlSrc.includes('模型配置'));
check('表单标题区分添加 / 编辑（编辑复用同一表单）',
  /id="mf-title"/.test(htmlSrc) &&
  /editingIndex >= 0 \? '编辑模型' : '添加模型'/.test(uiSrc));
check('列表「编辑」回填表单并切到添加模型面板',
  /editingIndex = i;[\s\S]{0,160}?fillForm\(m\);[\s\S]{0,160}?showMTab\('config'\)/.test(uiSrc));
check('点「添加模型」tab 回到空白新建态',
  /t\.dataset\.mtab === 'config'\) resetForm\(\)/.test(uiSrc));
check('对话框模型弹层列出模型库全部条目（不再只显示当前一个）',
  /function renderModelPop[\s\S]{0,700}?modelChoices\.forEach/.test(uiSrc) &&
  /b\.dataset\.model = m\.id/.test(uiSrc) &&
  /fetchModelChoices[\s\S]{0,200}?fetch\('\/api\/models\/list'\)/.test(uiSrc));
check('弹层点选即切换生效模型（/api/models/apply）',
  /function switchActiveModel/.test(uiSrc) &&
  /switchActiveModel\(m\.id\)/.test(uiSrc) &&
  /fetch\('\/api\/models\/apply'/.test(uiSrc));
check('保存成功：刷新模型列表并用新快照刷新对话框选择',
  /showMTab\('list'\);[\s\S]{0,220}?renderModelItems\(\);[\s\S]{0,260}?modelsLoaded = false;[\s\S]{0,80}?loadModels\(\)/.test(uiSrc));
check('样式契约：弹层列表限高滚动 + 表单标题',
  /#model-list\s*\{[^}]*max-height[^}]*overflow-y:\s*auto/.test(css) &&
  /\.mf-title\s*\{/.test(css));
// 编辑已有模型：脱敏回显的明文框是空的，保存时绝不能把空值当成「用户清空了密钥」。
// 前端用 keyTouched 记录本次是否动过密钥，没动过就不提交 key_value，服务端保持旧值。
check('编辑未动密钥时不提交 key_value（不删除已存 key）',
  /let keyTouched = false/.test(uiSrc) &&
  /editingIndex >= 0 && !keyTouched/.test(uiSrc) &&
  /keyTouched = false; \/\/ 刚回填的表单没有改过密钥/.test(uiSrc) &&
  /keyTouched = true/.test(uiSrc));

// ---------- 归档页：项目级恢复（与侧栏「归档」对称） ----------
// 侧栏 ⋯ 的「归档」一次点掉整组会话；归档页必须能一次恢复整组，
// 否则归档是一步、恢复是 N 步。API 靠 `archive:false` 表达。
group('归档页：项目级恢复');
check('归档页按项目分组渲染（arch-group）',
  /function renderArchived\(items\)[\s\S]{0,1200}?const groups = new Map\(\)/.test(uiSrc) &&
  /arch-group-head/.test(uiSrc));
check('组头有「恢复整个项目」按钮',
  /restore\.textContent = '恢复整个项目'/.test(uiSrc));
check('恢复走 /api/workspaces + archive:false',
  /function restoreWorkspace\(ws, n\)[\s\S]{0,300}?JSON\.stringify\(\{ workspace: ws, archive: false \}\)/.test(uiSrc));
check('恢复后刷新归档页与侧栏，失败有提示',
  /addInfo\('已把 ' \+ \(n \? n \+ ' 条' : ''\) \+ '会话恢复到侧栏'\)/.test(uiSrc) &&
  /addError\('恢复项目失败，请重试'\)/.test(uiSrc));
check('侧栏「归档」仍发 archive:true（指针改动没破坏它）',
  /JSON\.stringify\(\{ workspace: ws, archive: true \}\)/.test(uiSrc));
check('单条会话仍可单独恢复',
  /label: '恢复到侧栏'/.test(uiSrc) && /id: s\.id, archived: false/.test(uiSrc));
check('样式契约：组头 / 项目名 / 计数 / 恢复按钮',
  /\.arch-group-head\s*\{/.test(css) && /\.arch-group-name\s*\{/.test(css) &&
  /\.arch-group-count\s*\{/.test(css) && /\.arch-group-restore\s*\{/.test(css));

// ---------------------------------------------------------------------------
console.log('\n' + '-'.repeat(52));
if (failures.length) {
  console.log('失败 ' + failures.length + ' 项 / 通过 ' + passed + ' 项：');
  failures.forEach(function (f) { console.log('  - ' + f); });
  process.exit(1);
}
console.log('全部通过（' + passed + ' 项断言）');
