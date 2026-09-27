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
    extractFunction(src, 'retryReasonBrief'),
    extractFunction(src, 'describeLLMError'),
    extractFunction(src, 'diffLineKind'),
    'module.exports = { renderMD: renderMD, toolLabel: toolLabel, countLines: countLines, fmtTokens: fmtTokens, wsDisplayName: wsDisplayName, retryReasonBrief: retryReasonBrief, describeLLMError: describeLLMError, diffLineKind: diffLineKind };',
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
let renderMD, toolLabel, countLines, fmtTokens, wsDisplayName, retryReasonBrief, describeLLMError, diffLineKind;
try {
  const api = loadRenderer();
  renderMD = api.renderMD;
  toolLabel = api.toolLabel;
  countLines = api.countLines;
  fmtTokens = api.fmtTokens;
  wsDisplayName = api.wsDisplayName;
  retryReasonBrief = api.retryReasonBrief;
  describeLLMError = api.describeLLMError;
  diffLineKind = api.diffLineKind;
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
check('未知工具兜底也不显示工具 ID（只给中文描述）',
  toolLabel('some_plugin_tool', {}) === '调用了外部能力' &&
  toolLabel('some_unknown_thing', { path: 'a' }) === '调用了外部能力');
check('delegate_subagents 显示插件名与数量',
  toolLabel('delegate_subagents', { tasks: [{}, {}, {}] }) === '使用插件 Multi-Agent：并行委派 3 个子智能体');
check('delegate_subagents 缺任务数组时仍可用',
  toolLabel('delegate_subagents', {}) === '使用插件 Multi-Agent：并行委派 子智能体');
check('save_memory 文案', toolLabel('save_memory', {}) === '保存了记忆');
check('todo_write 文案', toolLabel('todo_write', {}) === '更新了任务清单');
check('web_fetch 文案带 URL', toolLabel('web_fetch', { url: 'https://example.com/p' }) === '读取了网页 https://example.com/p');
check('web_search 文案带 query', toolLabel('web_search', { query: 'codeforge' }) === '搜索了 codeforge');
// 远程 MCP 工具名带插件前缀（parallel_search.web_search），按去前缀后的本名匹配文案，
// 且要认 Parallel 的入参形状（objective / urls[]）。
check('插件限定名按本名匹配文案',
  toolLabel('parallel_search.web_search', { objective: 'Go MCP' }) === '搜索了 Go MCP');
check('插件限定名 web_fetch 认 urls 数组',
  toolLabel('parallel_search.web_fetch', { urls: ['https://a.com'] }) === '读取了网页 https://a.com');
// 工具 ID 绝不能出现在用户可见文案里（含兜底分支）—— 与后端 System Prompt 的
// 「不得出现工具 ID」是同一条产品纪律，两侧都要钉住。
check('未知插件工具也不吐限定名',
  toolLabel('parallel_search.whatever', {}) === '调用了外部能力');
check('Exa 同族工具按前缀匹配到中文文案（不落到兜底）',
  toolLabel('exa_search.web_search_exa', { query: 'codeforge' }) === '搜索了 codeforge' &&
  toolLabel('exa_search.web_fetch_exa', { urls: ['https://a.com'] }) === '读取了网页 https://a.com');
check('全量兜底扫描：任何工具 ID 都不出现在文案里',
  ['parallel_search.web_search', 'parallel_search.web_fetch', 'exa_search.web_search_exa',
   'exa_search.web_fetch_exa', 'unknown.plugin_tool_x'].every(function (n) {
    const out = toolLabel(n, {});
    return out.indexOf('.') < 0 && !/^调用了 [a-z_]+$/.test(out);
  }));
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

// 后端源码（跨语言契约断言：前端事件名、服务端语义必须同源，改一边就要改另一边）
const goFile = (rel) => fs.readFileSync(path.join(__dirname, '../..', rel), 'utf8');
// 去掉行注释再断言「代码里没有 X」：注释里常常正好在解释那个被删掉的旧写法
// （「早先是 sendAsUserText('继续')」），不剥掉就会把注释当成残留代码。
const goCode = (rel) => goFile(rel).replace(/^\s*\/\/.*$/gm, '');
const agentSrc = goFile('pkg/agent/agent.go');
const agentCode = goCode('pkg/agent/agent.go');
const subagentSrc = goFile('pkg/agent/subagent.go');
const subagentCode = goCode('pkg/agent/subagent.go');
const historySrc = goFile('pkg/agent/history.go');
const todosSrc = goFile('pkg/agent/todos.go');
const wsSrc = goFile('pkg/server/ws_handler.go');
const termuxSrc = goFile('pkg/server/termux.go');
const appearanceSrc = goFile('pkg/server/appearance.go');
const toolsSrc = goFile('pkg/tools/tool.go') + goFile('pkg/tools/scope.go');
const appearanceCode = goCode('pkg/server/appearance.go');
// 剥掉 ui.js 的行注释（同一理由：注释里会引用被替换掉的旧写法）
const uiCode = uiSrc.replace(/^\s*\/\/.*$/gm, '');

// 截出 ui.js 里某个 case 分支（到下一个 case 为止）：
// 比「case 后 N 字符内必须出现 X」稳，分支里加注释/前置守卫都不会误报。
function uiCase(name) {
  const start = uiSrc.indexOf("case '" + name + "':");
  if (start < 0) return '';
  const next = uiSrc.indexOf("case '", start + 1);
  return uiSrc.slice(start, next < 0 ? undefined : next);
}
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
check('明细弹层向左取齐、向右生长（贴右锚定会被顶出侧栏左边缘）',
  // 实测：侧栏只有 ~217px，而弹层 250px 宽（模型名一行就撑开），
  // 用 right:0 锚定时左边缘跑到 x=-16 → 标题和每行标签都被裁掉。
  // 改为 left:0 向右长（右侧是主对话区，永远在视口内）+ max-width 兜底。
  /\.ctx-anchor\s+\.ctx-pop\s*\{[^}]*left:\s*0;[^}]*right:\s*auto/.test(css) &&
  /\.ctx-anchor\s+\.ctx-pop\s*\{[^}]*max-width/.test(css));
check('上下文面板提供「立即压缩上下文」动作（含后端接口与结果说明）',
  /function ctxCompressBox\(\)/.test(uiSrc) &&
  /fetch\('\/api\/context\/compress'/.test(uiSrc) &&
  /立即压缩上下文/.test(uiSrc) &&
  /\.ctx-pop \.ctx-compress:disabled/.test(css));
check('主动压缩的结果与失败原因存成状态并回写面板（不静默、不被占用重绘冲掉）',
  /let ctxManualNote = /.test(uiSrc) &&
  /ctxManualNote = \(d && d\.error\)/.test(uiSrc) &&
  /hint\.textContent = \(ctxManualNoteFor === sessionID && ctxManualNote\)/.test(uiSrc) &&
  /ctxManualNoteFor = owner;/.test(uiSrc));
check('前端订阅服务端 context 事件',
  /case 'context':[\s\S]{0,220}?renderCtxUsage\(ev\)/.test(uiSrc));
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

// ---------- 6.2b 任务清单（todo）----------
// 服务端 load_session / new_session / todo_write 完成后推 {type:'todo', todos}；
// 前端只在会话匹配时渲染。
//
// ⚠️ 面板位置是回归重点：必须常驻在输入卡片**上方**（#composer-wrap 内、#composer 外），
// 不能像早先那样 messagesEl.prepend() 塞进滚动容器顶部——那样用户往上一滚就再也看不见，
// 表现成「模型说更新了任务清单，但界面上什么都没有」。
group('任务清单 todo');
check('前端订阅 todo 事件且按会话过滤',
  /case 'todo':[\s\S]{0,220}?renderTodoBar\(ev\.todos \|\| \[\]\)/.test(uiSrc) &&
  /ev\.session_id === sessionID/.test(uiSrc));
check('清单栏渲染 4 种状态图标（○/◐/✓/✕）',
  /pending: '○', in_progress: '◐', completed: '✓', cancelled: '✕'/.test(uiSrc));
check('默认关闭：空清单整个面板隐藏（无占位空壳）',
  /if \(!list\.length\) \{[\s\S]{0,220}?todoBar\.classList\.add\('hidden'\);/.test(uiSrc) &&
  !/TODO_EMPTY_HINT/.test(uiSrc) &&
  !/todo-empty/.test(uiSrc + css));
check('有清单时显形（去掉 hidden）',
  /todoBar\.classList\.remove\('hidden'\);/.test(uiSrc));
check('HTML 初始就带 hidden（避免 JS 执行前闪一下空壳）',
  /<div id="todo-bar" class="todo-bar hidden">/.test(htmlSrc));
check('面板挂在输入卡片上方（#composer-wrap 内、#composer 之前）',
  /<div id="todo-bar" class="todo-bar hidden">[\s\S]*?<\/div>\s*<form id="composer">/.test(htmlSrc) &&
  !/messagesEl\.prepend\(todoBar\)/.test(uiSrc));
check('显隐都会重算消息区底部留白（面板出现/消失改变卡片高度）',
  /if \(!list\.length\) \{[\s\S]{0,300}?requestAnimationFrame\(function \(\) \{ syncComposerPadding\(\); \}\)/.test(uiSrc));
check('点表头折叠/展开，状态持久化到 localStorage',
  /TODO_COLLAPSE_KEY\s*=\s*'cf_todo_collapsed'/.test(uiSrc) &&
  /localStorage\.setItem\(TODO_COLLAPSE_KEY/.test(uiSrc));
check('样式契约：todo 栏存在且有完成态删除线',
  /\.todo-bar\s*\{/.test(css) && /\.todo-text\.done\s*\{[^}]*line-through/.test(css));
check('样式契约：面板宽度 100%（撑满 #composer-wrap，与输入卡片等宽）',
  /\.todo-bar\s*\{[^}]*width:\s*100%/.test(css) &&
  // 不能再套一次 min(80%, --composer-max-w)：父级已是该宽度，子级再乘 80% 会窄一截
  !/\.todo-bar\s*\{[^}]*width:\s*min\(80%/.test(css));
check('样式契约：条数多时列表内滚，不把输入卡片顶出屏幕',
  /\.todo-bar\s*\{[^}]*max-height:\s*38vh/.test(css) &&
  /\.todo-body\s*\{[^}]*overflow-y:\s*auto/.test(css));
check('样式契约：.todo-bar.hidden 彻底不占位（连 margin 一起收起）',
  /\.todo-bar\.hidden\s*\{\s*display:\s*none;\s*\}/.test(css));
check('todo 文案进 toolPhrases（审批/卡片显示）',
  /todo_write:\s+'更新任务清单'/.test(uiSrc));

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
check('居中态不参与**折叠**（scroll 回调在折叠前提前返回）',
  /addEventListener\('scroll',\s*function[\s\S]{0,420}?if\s*\(composerCentered\)\s*return/.test(uiSrc));
check('但居中态仍要维护「跟随态 + 回到底部」判定（提前返回必须在它们之后）',
  (function () {
    const fn = uiSrc.slice(uiSrc.indexOf("addEventListener('scroll', function"));
    const tail = fn.slice(0, fn.indexOf('if (composerCentered) return'));
    return /followTail\s*=\s*isNearBottom\(\)/.test(tail) && /syncToBottomBtn\(\)/.test(tail);
  })());
check('页面加载自动回放历史时不做过渡（composer-no-anim）',
  /#composer-wrap\.composer-no-anim\s*\{\s*transition:\s*none/.test(css));
check('ready 自动恢复前重新武装「直接落位」标记',
  // 落位标记由 restoreStartSession 统一设：不论走 ?s= 深链还是回退到最近会话，
  // 开场回放都不该让输入卡片滑一下。
  /case 'ready':[\s\S]{0,900}?restoreStartSession\(\)/.test(uiSrc) &&
  /function restoreStartSession\(\)\s*\{[\s\S]{0,200}?composerSnap\s*=\s*true;/.test(uiSrc));
check('用户发言走平滑下放（submit 清掉落位标记后 addUser）',
  /composerSnap\s*=\s*false;[\s\S]{0,600}?addUser\(p\.display\)/.test(uiSrc));

// ---------- 8. 输入框 @ 提及：技能 + 插件 ----------
// 输入 @ 弹出面板，同时列「技能」和「内置插件」两组；不含 MCP 服务。
// 易回归点：① 每敲一个字符就打一轮接口（应该是「每次新打开 @ 拉一次 + 本地过滤」）；
// ② 分组标题被当成可选项（上下键会把标题算进去）。
group('输入框 @ 提及：技能 + 内置插件');
check('同时拉技能与内置插件列表（不含 MCP 服务）',
  /fetch\('\/api\/skills'\)/.test(uiSrc) && /fetch\('\/api\/builtin-plugins'\)/.test(uiSrc));

group('内置目录选择器：层级与层级可见性');

check('选择器层级高于设置面板（否则从设置里弹出时看不见）',
  // #picker-overlay 挂在 document.body 上、与 .settings-overlay 是兄弟节点；
  // 早先 z-index 200 < 设置面板的 300，于是「设置 → 外观 → 选背景图」
  // 弹出的选择器被整个设置面板盖住（2026-09-27 反馈）。
  /#picker-overlay \{[\s\S]{0,200}?z-index: 400;/.test(css) &&
  /\.settings-overlay \{[\s\S]{0,200}?z-index: 300;/.test(css));
check('「.. 返回上一级」置顶，且由服务端给的 parent 驱动',
  // 父目录在服务端算：根目录（/ 与 C:\）、UNC 前缀、结尾分隔符各平台规则不同，
  // 前端自己切字符串会在 Termux / Windows 上错位。
  /const parent = data\.parent \|\| '';/.test(uiSrc) &&
  /parent && parent !== \(data\.path \|\| ''\)/.test(uiSrc) &&
  /📂 \.\. 返回上一级/.test(uiSrc) &&
  /className = 'picker-item picker-up'/.test(uiSrc) &&
  /browse\(parent\)/.test(uiSrc));
check('在根目录上不显示「..」（parent 为空即隐藏）',
  /if \(parent && parent !== \(data\.path \|\| ''\)\) \{/.test(uiSrc));
check('空态提示排除「..」那一行（否则空目录看起来像漏了内容）',
  /querySelectorAll\('\.picker-item:not\(\.picker-up\)'\)/.test(uiSrc) &&
  !/if \(!list\.children\.length\)/.test(uiSrc));
check('「..」行有区分样式（是导航不是目标）',
  /\.picker-item\.picker-up \{/.test(css));

group('MCP 服务的显示别名');

check('插件配置有 display_name（标识与文案分开）',
  /DisplayName\s+string\s+`yaml:"display_name"`/.test(
    fs.readFileSync(path.join(__dirname, '../..', 'config/config.go'), 'utf8')));
check('Label 优先别名、无别名回退到标识',
  /func \(p PluginConfig\) Label\(\) string/.test(
    fs.readFileSync(path.join(__dirname, '../..', 'config/config.go'), 'utf8')));
check('内建插件都配了中文别名（用户反馈「显示代号不好看」）',
  /display_name: 并行搜索/.test(
    fs.readFileSync(path.join(__dirname, '../..', 'config/plugins.yaml'), 'utf8')));
check('接口下发 display_name，但仍原样下发 name 供定位',
  /"display_name": p\.Label\(\)/.test(
    fs.readFileSync(path.join(__dirname, '../..', 'pkg/server/api_handlers.go'), 'utf8')));
check('侧栏与设置列表用显示名，标识退到 title / 小字',
  /nm\.textContent = \(p\.display_name \|\| p\.name\)/.test(uiSrc) &&
  /const label = p\.display_name \|\| p\.name;/.test(uiSrc) &&
  /alias\.className = 'mi-alias'/.test(uiSrc) &&
  /\.mi-name \.mi-alias \{/.test(css));
check('启停 / 删除仍按内部标识（显示名只是文案，不能拿去定位）',
  /togglePlugin\(p\.name, !p\.configured\)/.test(uiSrc) &&
  /deletePlugin\(p\.name\)/.test(uiSrc));
check('添加表单有「显示名称」字段并随请求提交',
  /id="mcp-f-display"/.test(htmlSrc) &&
  /display_name: displayName,/.test(uiSrc) &&
  /document\.getElementById\('mcp-f-display'\)\.value = '';/.test(uiSrc));

group('设置 → 常规：刷新页面');

check('常规页有「界面」卡片与刷新按钮',
  /id="reload-page"/.test(htmlSrc) &&
  /<div class="row-title">界面<\/div>/.test(htmlSrc));
check('点击后真的重载当前页面',
  /function bindReloadPage\(\)/.test(uiSrc) &&
  /location\.reload\(\);/.test(uiSrc));
check('运行中刷新要先确认（连接一断服务端就取消该轮）',
  /if \(running\) \{[\s\S]{0,400}?confirm\(/.test(uiSrc) &&
  /打断这一轮/.test(uiSrc) &&
  /已改过的文件不会回退/.test(uiSrc));
check('上传中刷新也要确认',
  /if \(pendingUploads\) \{[\s\S]{0,200}?confirm\(/.test(uiSrc));
check('说明文字讲清「资源不走缓存」',
  // 服务端对静态资源设的是 Cache-Control: no-store（pkg/server/server.go），
  // 所以不需要 cache-busting 参数，普通 reload 就能拿到当前二进制内嵌的那份资源。
  /资源不走浏览器缓存/.test(htmlSrc) &&
  /Cache-Control", "no-store"/.test(
    fs.readFileSync(path.join(__dirname, '../..', 'pkg/server/server.go'), 'utf8')));

group('工具卡片的失败标记（⛔）');

check('tool_result 必须读 result.success（不看就分不出失败）',
  // 卡片文案在 tool_call 阶段就用过去式写死，徽标/diff 用的还是预演结果；
  // 不看 result.success 的话 Deny、「未找到 old_string」、「用户拒绝」全渲染成成功。
  /settleActiveTool\(!!\(ev\.result && ev\.result\.success === false\)\)/.test(uiSrc));
check('失败时追加 ⛔ 且只追加一次', /className = 'tool-denied'/.test(uiSrc) &&
  /textContent = '⛔'/.test(uiSrc) &&
  /if \(!activeToolEl\.querySelector\('\.tool-denied'\)\)/.test(uiSrc));
check('成功路径刻意一行不改（产品要求）',
  // settleActiveTool 无参调用 = 成功收尾；只有显式传 true 才加 ⛔
  /if \(failed\) \{/.test(uiSrc) &&
  /activeToolEl\.classList\.remove\('running'\);/.test(uiSrc) &&
  !/success === true/.test(uiSrc));
check('失败卡片用 danger 色，且 +N 徽标去色（避免「绿色成功」的错觉）',
  /\.msg-tool\.denied \{ color: var\(--danger\); \}/.test(css) &&
  /\.msg-tool\.denied \.diffstat\.add \{[^}]*color: var\(--text-dim\)/.test(css));

group('审批卡片的失效处理（⛔ 同批修复）');

check('expireApprovals 作废待决审批（禁用按钮 + 标注已失效）',
  uiSrc.includes('function expireApprovals()') &&
  /\.msg-approval:not\(\.decided\):not\(\.expired\)/.test(uiSrc) &&
  /wrap\.classList\.add\('expired'\)/.test(uiSrc) &&
  /已失效/.test(uiSrc));
check('decide 拒绝已失效的卡片（否则点了会谎报「已批准」）',
  // 服务端 resolve 对未知 id 是静默 no-op，早先这里不查活性就改文案。
  /if \(wrap\.classList\.contains\('expired'\)\) return;/.test(uiSrc) &&
  /classList\.add\('decided'\)/.test(uiSrc));
check('四个终止点都作废审批：idle / error / busy / ready',
  uiCase('idle').includes('expireApprovals()') &&
  uiCase('error').includes('expireApprovals()') &&
  uiCase('busy').includes('expireApprovals()') &&
  uiCase('ready').includes('expireApprovals()'));
check('失效样式存在（一眼看出点不动了）',
  /\.msg-approval\.expired \{[^}]*border-style: dashed/.test(css) &&
  /\.msg-approval\.expired \.approval-btn \{[^}]*cursor: not-allowed/.test(css));

group('视图状态复位：收敛成单一入口');

check('清屏只有 clearViewState 一个入口（消除 6 处分散清单）',
  uiSrc.includes('function clearViewState(opts)') &&
  (uiSrc.match(/messagesEl\.innerHTML = '';/g) || []).length === 1 &&
  // 唯一的内联清屏必须就在 clearViewState 里
  /function clearViewState\(opts\)[\s\S]{0,400}?messagesEl\.innerHTML = '';/.test(uiSrc));
check('clearViewState 作废全部指向已销毁节点的引用',
  // 漏 thinkingEl → 归档后再发消息，「等待模型响应」永不出现（showThinking 有 ref 守卫）
  // 漏 resumeRingEl → 「继续」圆环永久挂不出来（showResumeRing 同款守卫）
  /function clearViewState\(opts\)[\s\S]{0,600}?thinkingEl = null; activeToolEl = null; retryEl = null; pendingToolEl = null;/.test(uiSrc) &&
  /function clearViewState\(opts\)[\s\S]{0,600}?resumeRingEl = null;/.test(uiSrc) &&
  /function clearViewState\(opts\)[\s\S]{0,700}?optimisticBubble = null;/.test(uiSrc));
check('切会话保留 lastUserText，归档/删除才清（两处旧行为不同）',
  /clearViewState\(\{ keepLastUser: true \}\)/.test(uiSrc) &&
  /if \(!opts\.keepLastUser\) lastUserText = '';/.test(uiSrc));
check('resetStreamRefs 也作废 resumeRingEl（edit 帧后视图即将重建）',
  /function resetStreamRefs\(\)[\s\S]{0,400}?resumeRingEl = null;/.test(uiSrc));
check('truncate 路径同样作废 resumeRingEl',
  /function truncateAfterEditableMessage[\s\S]{0,700}?resumeRingEl = null;/.test(uiSrc));
check('一轮收尾用 clearRunVisuals（idle/error/ready 共用）',
  uiSrc.includes('function clearRunVisuals()') &&
  uiCase('idle').includes('clearRunVisuals()') &&
  uiCase('error').includes('clearRunVisuals()') &&
  uiCase('ready').includes('clearRunVisuals()'));
check('busy 不用 clearRunVisuals 全量（紧接着要 showThinking，会被拆掉）',
  !uiCase('busy').includes('clearRunVisuals()') &&
  uiCase('busy').includes('settleActiveTool()') &&
  uiCase('busy').includes('pendingToolEl'));
check('ready 必须收尾工具卡（漏了就是一次网络抖动后永久转圈）',
  uiCase('ready').includes('clearRunVisuals()'));

group('幻影气泡回滚（避免 ✎ 改错消息）');

check('addUser 返回新建的行（供回滚）',
  /function addUser\(text\)[\s\S]{0,700}?return row;/.test(uiSrc));
check('提交时记下乐观气泡',
  /optimisticBubble = lastAddedUserRow\(\);/.test(uiSrc));
check('busy 到达即视为已落库、清掉引用（此刻的 error 不该删用户的话）',
  /case 'busy':[\s\S]{0,700}?optimisticBubble = null;/.test(uiSrc));
check('error 与 idle 收尾都回滚未落库的气泡',
  uiCase('error').includes('dropOptimisticBubble()') &&
  uiCase('idle').includes('dropOptimisticBubble()'));
check('回滚后重挂编辑按钮（少一条用户消息 → back 序号全体前移）',
  /function dropOptimisticBubble\(\)[\s\S]{0,500}?editableBacks = new Set\(\);[\s\S]{0,120}?syncUserEditButtons\(\);/.test(uiSrc));
check('已脱离文档的气泡不重复移除',
  /if \(!el\.isConnected\) return false;/.test(uiSrc));

group('history 帧的会话守卫（防视图被拽到别的会话）');

check('Agent Event 带 session_id（Agent 层不填，由 WS 层统一补）',
  /SessionID\s+string\s+`json:"session_id,omitempty"`/.test(agentSrc) &&
  /ev\.SessionID = sessionID/.test(wsSrc));
check('前端记账「我请求的那一帧 history」',
  uiSrc.includes('awaitingHistoryFor') &&
  /awaitingHistoryFor = id;/.test(extractFunction(uiSrc, 'loadSession')));
check('非请求触发且会话不匹配的 history 必须被丢弃',
  // 场景：A 会话编辑重发 → 点 B → 服务端补的 historyEvent(A) 把整屏拽回 A。
  /if \(ev\.session_id && awaitingHistoryFor !== ev\.session_id &&[\s\S]{0,80}?ev\.session_id !== sessionID\)/.test(uiSrc) &&
  /另一个会话的历史更新已忽略/.test(uiSrc));
check('请求触发的那一帧必须放行（切会话就靠它）',
  /awaitingHistoryFor = null;\s*\n\s*pendingEdit = false;\s*\n\s*replayHistory\(ev\);/.test(uiSrc));
check('老服务端不带 session_id 时按原样放行（不因升级卡住）',
  /if \(ev\.session_id &&/.test(uiSrc));
check('edit 帧的守卫终于生效（此前 session_id 恒为空，是死代码）',
  uiCase('edit').includes('ev.session_id === sessionID') &&
  uiCase('edit').includes('resetStreamRefs()'));

group('子智能体审批的身份标识');

check('SubagentScope 带 TaskID，Runner 注入时填上',
  /TaskID string/.test(toolsSrc) &&
  /tools\.WithSubagentScope\(ctx, tools\.SubagentScope\{[\s\S]{0,120}?TaskID:\s*task\.ID/.test(
    fs.readFileSync(path.join(__dirname, '../..', 'pkg/agent/subagent.go'), 'utf8')));
check('身份与围栏是两个 accessor（explore 不必声明 paths）',
  // SubagentScopeFrom 的 ok 依赖 len(Allowed)>0；用它取身份会让无 paths 的
  // explore 子任务把 TaskID 丢掉，审批卡退回「不知道是谁要的」。
  /func SubagentIdentityFrom\(ctx context\.Context\) \(id, mode string, ok bool\)/.test(toolsSrc) &&
  /SubagentIdentityFrom\(ctx\)/.test(
    fs.readFileSync(path.join(__dirname, '../..', 'pkg/tools/executor.go'), 'utf8')));
check('ApprovalRequest 带 subagent_id/mode（omitempty）',
  /SubagentID string `json:"subagent_id,omitempty"`/.test(toolsSrc));
check('hitl_request 帧透传子任务身份',
  /"subagent_id":\s*req\.SubagentID/.test(wsSrc) &&
  /"subagent_mode":\s*req\.SubagentMode/.test(wsSrc));
check('审批卡显示「子任务 X · 探索/实现」',
  /const subLabel = req\.subagent_id/.test(uiSrc) &&
  /子任务 /.test(uiSrc) &&
  /req\.subagent_mode === 'implement' \? ' · 实现' : ' · 探索'/.test(uiSrc));
check('子智能体审批卡有区分样式（并行委派时多张并存）',
  /\.msg-approval\.from-subagent \{[^}]*border-style: dashed/.test(css));

// ---------- 8.5 ＋菜单 / @文件 stage / @高亮（2026-09-14） ----------group('＋菜单与 @文件');
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
check('分「技能」「内置插件」两组',
  /addGroup\('技能'\)/.test(uiSrc) && /addGroup\('内置插件'\)/.test(uiSrc));
check('技能显示 display_name、内置插件显示 @id 触发',
  /sk\.display_name \|\| sk\.name/.test(uiSrc) && /it\.builtin \? it\.id : \(it\.token \|\| it\.name\)/.test(uiSrc));
check('分组标题不参与上下键选择（dataset.idx 对齐 atItems）',
  /b\.dataset\.idx = String\(idx\)/.test(uiSrc) &&
  /Number\(b\.dataset\.idx\) === atIdx/.test(uiSrc));
check('数据按「每次新打开 @ 拉一次」缓存，不在输入时反复请求',
  /atLoadedFor !== cur\.start/.test(uiSrc) && /if \(!atData\) \{ loadAtData\(\); return; \}/.test(uiSrc));
check('一条都没有时给提示而不是静默无反应',
  /还没有可提及的技能或内置插件/.test(uiSrc) && /at-empty/.test(uiSrc));
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
check('别名展开、附件准备与 WS 发送成功后才清空输入框', (function () {
  const submit = extractFunction(uiSrc, 'submitMessage');
  const expand = submit.indexOf('expandFileAliases(raw)');
  const prepare = submit.indexOf('await prepareMentions(outgoing, raw)');
  const send = submit.indexOf('if (!wsSend({');
  const clear = submit.indexOf("input.value = '';");
  return expand >= 0 && expand < prepare && prepare < send && send < clear;
})());
check('prepareMentions 显式分开「发送文本」与「气泡显示文本」',
  /async function prepareMentions\(text, display\)/.test(uiSrc) &&
  /return \{ text: text, display: display === undefined \? text : display, notes: notes, attachments: attachments \}/.test(uiSrc));
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
  /input\.value = before \+ '@' \+ token \+ ' ' \+ after;\s*\n\s*syncInputMirror\(\)/.test(uiSrc) &&
  /input\.value = before \+ sep \+ ins \+ after;\s*\n\s*syncInputMirror\(\)/.test(uiSrc));

// ---------- 9. 发送 / 打断按钮共色 ----------
group('发送 / 打断按钮颜色');
check('运行中的打断按钮为危险色（区别于发送按钮的 accent）',
  // 发送按钮是「实心 accent + 白字」→ 用 accent-solid（白字对比度才够）；
  // 断言意图不变：它仍与 danger 区分开、也不该退化成 text-dim。
  /#send-btn\s*\{[^}]*background:\s*var\(--accent-solid\)/.test(css) &&
  /#send-btn\.running\s*\{[^}]*background:\s*var\(--danger\)/.test(css) &&
  !/#send-btn\.running\s*\{[^}]*background:\s*var\(--text-dim\)/.test(css));

// ---------- 10. 任务等待提示：模型响应前 / 工具返回后 ----------
group('任务等待提示');
check('等待文案为「等待模型响应」',
  /等待模型响应/.test(uiSrc) && !/innerHTML = '思考中/.test(uiSrc));
check('用户发送后立即显示等待提示',
  // 窗口放宽：addUser 与 showThinking 之间新增了「记下乐观气泡」的注释（2026-09-27）。
  /addUser\(p\.display\);[\s\S]{0,700}?showThinking\(\)/.test(uiSrc));
check('busy 事件显示等待提示',
  /case 'busy':[\s\S]{0,600}?showThinking\(\)/.test(uiSrc));
check('tool_result 后模型再次等待时显示提示',
  // 窗口放宽：tool_result 分支里现在多了失败判定的注释（2026-09-27），
  // 350 字符刚好卡在边界上，改动无关的行就会假失败。
  /case 'tool_result':[\s\S]{0,600}?if \(running\) showThinking\(\)/.test(uiSrc));
check('reasoning/text/tool_call 到来时移除等待提示',
  uiCase('reasoning').includes('removeThinking()') &&
  uiCase('text').includes('removeThinking()') &&
  uiCase('tool_call').includes('removeThinking()'));
check('idle/error/hitl 结束或暂停时移除等待提示',
  // idle/error 现在走 clearRunVisuals()（内含 removeThinking），
  // hitl_request 仍直接调 —— 两种写法都算「移除了等待提示」。
  (uiCase('idle').includes('removeThinking()') || uiCase('idle').includes('clearRunVisuals()')) &&
  (uiCase('error').includes('removeThinking()') || uiCase('error').includes('clearRunVisuals()')) &&
  uiCase('hitl_request').includes('removeThinking()'));

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
// 勾选后入库：**选了供应商就一次写入多个 id** —— 这是「同一个供应商下不必一个一个
// 添加」的主路径；没选供应商（新建供应商态）才退回「填入单个 id 到表单」。
check('勾选后批量入库：选了供应商就一次写入多个 id',
  /function addSelectedDiscModels/.test(uiSrc) &&
  /const ids = Object\.keys\(discSelected\)/.test(uiSrc) &&
  /provider_id: pid/.test(uiSrc) &&
  /models: ids\.map/.test(uiSrc) &&
  /fetch\('\/api\/models\/save_batch'/.test(uiSrc));
check('未选供应商时退回「填入单个模型 id」到表单',
  /if \(!pid\) \{[\s\S]{0,420}?document\.getElementById\('mf-id'\)\.value = id/.test(uiSrc));
check('上游列表多选（checkbox），勾选状态按 id 记录',
  /cb\.type = 'checkbox'/.test(uiSrc) &&
  /discSelected\[m\.id\] = true/.test(uiSrc) &&
  /delete discSelected\[m\.id\]/.test(uiSrc));
check('上游列表只保留筛选 + 收起，去掉全选/清空',
  /id="disc-filter"/.test(htmlSrc) &&
  /id="disc-close"/.test(htmlSrc) &&
  !htmlSrc.includes('id="disc-all"') && !htmlSrc.includes('id="disc-none"') &&
  /document\.getElementById\('disc-filter'\)\.addEventListener\('input', renderDiscList\)/.test(uiSrc));
check('填入后收起面板并提示补密钥保存',
  /setTestResult\(true, '已填入模型 id/.test(uiSrc) &&
  /collapseDiscover\(\);\s*\n\s*return;/.test(uiSrc));
check('切换/保存模型时收起上游列表（防状态串味）',
  /collapseDiscover\(\); \/\/ 换了模型就收起上一次的上游列表/.test(uiSrc) &&
  /collapseDiscover\(\);\s*\n\s*showMTab\('list'\)/.test(uiSrc));

// ---------------------------------------------------------------------------
// 模型管理：子 Tab「添加模型」+ 列表「编辑」复用同款表单 + 保存后进入对话框模型选择
// 需求链路：设置 → 模型 → 添加模型 → 保存 = 模型列表与对话框弹层同时出现新条目。
group('模型管理：添加 / 编辑 / 对话框选择');
check('子 Tab 文案为「模型列表 / 供应商管理 / 添加模型」',
  /data-mtab="list">模型列表</.test(htmlSrc) &&
  /data-mtab="prov">供应商管理</.test(htmlSrc) &&
  /data-mtab="config">添加模型</.test(htmlSrc) &&
  !htmlSrc.includes('模型配置'));
check('表单标题区分添加 / 编辑（编辑复用同一表单）',
  /id="mf-title"/.test(htmlSrc) &&
  /editingIndex >= 0 \? '编辑模型' : '添加模型'/.test(uiSrc));
check('列表「编辑」改为**就地展开**，不再跳去添加模型面板',
  // 2026-09-20 改造：编辑不再复用「添加模型」那张表单（含供应商下拉/地址/密钥，太重），
  // 改成在模型行下方就地展开一个只含模型自身字段的编辑区。
  /acts\.appendChild\(domButton\('编辑', '', function \(\) \{ toggleEdit\(\); \}\)\)/.test(uiSrc) &&
  /function buildModelEditor/.test(uiSrc) &&
  !/showMTab\('config'\)[\s\S]{0,80}?fillForm/.test(uiSrc));
check('「添加模型」tab 仍是独立的空白新建态（未被编辑复用）',
  /name === 'config'\) \{[\s\S]{0,140}?loadLibrary\(\)\.then\(function \(\) \{ resetForm\(\); \}\)/.test(uiSrc) &&
  /function resetForm/.test(uiSrc));
check('点「添加模型」tab 回到空白新建态',
  /name === 'config'\) \{[\s\S]{0,140}?loadLibrary\(\)\.then\(function \(\) \{ resetForm\(\); \}\)/.test(uiSrc));
check('供应商 tab 切换时重新渲染供应商列表',
  /name === 'prov'\) \{[\s\S]{0,80}?renderProviders\(\)/.test(uiSrc));
check('对话框模型弹层列出模型库全部条目（不再只显示当前一个）',
  // 现在遍历的是「按当前供应商过滤后」的列表（modelChoices 的子集），
  // 全部条目仍来自 modelChoices；切供应商时自动换列表。
  /filtered\.forEach\(function \(m\)/.test(uiSrc) &&
  /modelChoices\.filter\(function \(m\) \{[\s\S]{0,80}?m\.provider_id === curProvId/.test(uiSrc) &&
  /b\.dataset\.model = m\.id/.test(uiSrc) &&
  /fetchModelChoices[\s\S]{0,200}?fetch\('\/api\/models\/list'\)/.test(uiSrc));
check('弹层点选即切换生效模型（/api/models/apply）',
  /function switchActiveModel/.test(uiSrc) &&
  /switchActiveModel\(m\.id\)/.test(uiSrc) &&
  /fetch\('\/api\/models\/apply'/.test(uiSrc));
check('保存成功：刷新模型列表并用新快照刷新对话框选择',
  /showMTab\('list'\);[\s\S]{0,240}?modelsLoaded = false;[\s\S]{0,140}?loadLibrary\(\)\.then\(function \(\) \{ renderModelItems\(\); loadModels\(\); \}\)/.test(uiSrc));
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

// ---------------------------------------------------------------------------
// 运行中体验：思考过程自动贴底 / 允许切换会话 + 运行中会话转圈 / 事件按会话路由
// 历史事故：思考超过 10 行被限高后，新内容全在「页内页」视口外，看起来像卡住。
group('运行中体验：思考滚动 / 会话切换 / 运行标记');
check('思考过程限高盒自动贴底（用户上翻时不打扰）',
  /let reasonPinned = false/.test(uiSrc) &&
  /if \(!reasonPinned\) body\.scrollTop = body\.scrollHeight;/.test(uiSrc) &&
  /body\.addEventListener\('wheel', function \(\) \{ reasonPinned = true; \}/.test(uiSrc));
check('运行中允许切换会话（点击不再被 running 挡住）',
  /if \(s\.id === sessionID\) return;\s*\n\s*loadSession\(s\.id\); \/\/ 运行中也允许切换/.test(uiSrc));
check('正在运行的会话带转圈标记',
  /function addRunBadge\(li\)/.test(uiSrc) &&
  /function syncRunBadges\(\)/.test(uiSrc) &&
  /if \(running && s\.id === runSessionID\) addRunBadge\(li\)/.test(uiSrc) &&
  /#session-list\s+li\.session-item\s+>\s*\.session-spin\s*\{[^}]*animation:\s*toolSpin/.test(css));
check('后台事件不画进当前视图（runAway 路由）',
  /function runAway\(\)/.test(uiSrc) &&
  /runReason \+= ev\.text \|\| '';\s*\n\s*if \(runAway\(\)\) break;/.test(uiSrc) &&
  /runText \+= ev\.text \|\| '';\s*\n\s*if \(runAway\(\)\) \{ foldReason\(\); break; \}/.test(uiSrc));
check('切回运行中会话补渲染未落盘片段',
  /if \(runReason\) appendReason\(runReason\);/.test(uiSrc) &&
  /if \(runText\) appendText\(runText\);/.test(uiSrc) &&
  /runReason = ''; runText = '';/.test(uiSrc));
check('idle 复位运行态并撤销转圈',
  /runSessionID = '';/.test(uiSrc) && /syncRunBadges\(\);/.test(uiSrc) &&
  /running = false;\s*\n\s*runSessionID = '';/.test(uiSrc));
// HITL 审批可能来自用户切走的后台会话：服务端必须带上 session_id，前端要标注来源。
const wsHandlerSrc = fs.readFileSync(
  path.join(__dirname, '..', '..', 'pkg', 'server', 'ws_handler.go'), 'utf8');
check('HITL 审批带会话来源（服务端下发 + 前端标注）',
  /"session_id":\s*a\.currentSession\(\)/.test(wsHandlerSrc) &&
  /c\.approver\.setSession\(sessionID\)/.test(wsHandlerSrc) &&
  /const fromOther = !!req\.session_id && req\.session_id !== sessionID;/.test(uiSrc) &&
  /\.msg-approval\.from-other\s*\{/.test(css));

// ---------------------------------------------------------------------------
// 思考强度拉条：关闭档 + 按协议记忆（localStorage）
// 关闭档 = 直接不发上游参数，兼容不支持 reasoning_effort / thinking 的模型。
group('思考强度：关闭档 / 按协议记忆');
check('后端规格含关闭档（openai 首档 none / anthropic Min=0）',
  /OffThinkingValue\s*=\s*"none"/.test(fs.readFileSync(
    path.join(__dirname, '..', '..', 'pkg', 'llm', 'thinking.go'), 'utf8')) &&
  /isOffThinking\(v\)/.test(fs.readFileSync(
    path.join(__dirname, '..', '..', 'pkg', 'llm', 'thinking.go'), 'utf8')));
check('档位按协议记忆（cf_thinking_<mode>）',
  /function thinkingStorageKey\(spec\) \{ return 'cf_thinking_' \+/.test(uiSrc) &&
  /function pickThinking\(spec\)/.test(uiSrc) &&
  /function saveThinking\(\)/.test(uiSrc));
check('加载 / 切模型时恢复记忆档位（不再一律吃默认值）',
  /thinkingVal = pickThinking\(thinkingSpec\);/.test(uiSrc) &&
  !/thinkingVal = thinkingSpec\.default \|\| '';/.test(uiSrc));
check('拖拽 / 点档后落盘记忆',
  (uiSrc.match(/saveThinking\(\);/g) || []).length >= 2 &&
  /saveThinking\(\);\s*\n\s*syncLevelUI\(\);/.test(uiSrc));
check('关闭档显示为「关闭」',
  /if \(thinkingVal === 'none'\) return '关闭';/.test(uiSrc) &&
  /if \(!v \|\| v === '0' \|\| v === 'none'\) return '关闭';/.test(uiSrc));
check('思考档位枚举首字母大写（Minimal/Low/Medium/High）',
  /function capitalizeFirst\(s\)/.test(uiSrc) &&
  /return val \? capitalizeFirst\(val\) : 'Medium';/.test(uiSrc) &&
  /return v === 'none' \? '关闭' : capitalizeFirst\(v\);/.test(uiSrc));

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
group('工具 ID 不外泄（前后端同一条纪律）');
check('审批文案（toolPhrase）兜底不吐工具 ID',
  /toolPhrases\[name\] \|\| toolPhrases\[local\] \|\| '外部能力'/.test(uiSrc));
check('源码里不存在「调用 <name>」式兜底',
  !/'调用 ' \+ name/.test(uiSrc) && !/'调用了 ' \+ name/.test(uiSrc));
check('后端 System Prompt 显式禁止在正文/计划/步骤里出现工具代号',
  (function () {
    const p = require('node:fs').readFileSync(
      require('node:path').join(__dirname, '..', '..', 'pkg', 'agent', 'prompt.go'), 'utf8');
    return p.indexOf('都不得出现任何工具 ID、函数名、调用代号') >= 0;
  })());

// ---------------------------------------------------------------------------
group('重试提示文案（retryReasonBrief）');
// 用户第一眼要「为什么失败」，不能只给一句笼统的「正在重试」
check('429 归类为上游限流',
  retryReasonBrief('LLM 请求失败 (429): {"error":{"code":"1305"}}') === '上游限流');
check('503 归类为上游过载',
  retryReasonBrief('LLM 请求失败 (503): busy') === '上游过载');
check('502/504 归类为上游网关异常',
  retryReasonBrief('LLM 请求失败 (502): bad gateway') === '上游网关异常' &&
  retryReasonBrief('LLM 请求失败 (504): timeout') === '上游网关异常');
check('4xx 归类为被上游拒绝',
  retryReasonBrief('LLM 请求失败 (401): unauthorized') === '请求被上游拒绝');
check('网络层错误归类为网络连接异常',
  retryReasonBrief('LLM 请求失败: Post "https://x/v1": EOF') === '网络连接异常' &&
  retryReasonBrief('LLM 请求失败: dial tcp: connection refused') === '网络连接异常');
check('上游错误体里的人话被带出来',
  retryReasonBrief('LLM 请求失败 (429): {"error":{"code":"1305","message":"该模型当前访问量过大，请您稍后再试"}}')
    === '上游限流：该模型当前访问量过大，请您稍后再试');
check('空/缺失原因降级为「未知原因」',
  retryReasonBrief('') === '未知原因' && retryReasonBrief(null) === '未知原因' &&
  retryReasonBrief(undefined) === '未知原因');
check('配额耗尽仍被识别为配额已用尽（而非限流）',
  retryReasonBrief('LLM 请求失败 (429): {"error":{"code":"quota_exceeded"}}') === '模型配额已用尽');

// ---------------------------------------------------------------------------
group('错误显示（describeLLMError）');
check('配额耗尽 → 明确提示去设置换模型/充值',
  /模型配额已用尽/.test(describeLLMError('LLM 请求失败 (429): {"error":{"message":"quota exceeded","type":"quota_exceeded"}}')));
check('401 → 提示检查密钥',
  /API 密钥无效或未授权/.test(describeLLMError('LLM 请求失败 (401): unauthorized')));
check('429 非配额 → 提示稍后再试',
  /请求过于频繁/.test(describeLLMError('LLM 请求失败 (429): {"error":{"code":"1305"}}')));
check('5xx → 提示上游服务异常',
  /上游服务异常/.test(describeLLMError('LLM 请求失败 (503): busy')));
check('带 message 人话时直接展示',
  describeLLMError('LLM 请求失败 (429): {"error":{"message":"该模型当前访问量过大，请您稍后再试"}}')
    === '该模型当前访问量过大，请您稍后再试');
check('空值降级为「未知错误」',
  describeLLMError('') === '未知错误' && describeLLMError(null) === '未知错误');
// 契约：主文案必须把「第几次/共几次」摆出来，且完整原因仍可点击展开
check('重试文案带次数与原因摘要',
  /'请求失败，正在重试…' \+ times/.test(uiSrc) &&
  /第 ' \+ attempt/.test(uiSrc) &&
  /maxAttempts > 0/.test(uiSrc) &&
  /次 · ' \+ brief/.test(uiSrc));
check('完整原因保留在可展开的详情里',
  /classList\.toggle\('show'\)/.test(uiSrc) && /'原因：' \+ \(reason \|\| '未知错误'\)/.test(uiSrc));
check('重试事件把 attempt/max_attempts 透传给 addRetry',
  /addRetry\(ev\.error \|\| '', Number\(ev\.attempt\) \|\| 0, Number\(ev\.max_attempts\) \|\| 0\)/.test(uiSrc));
check('样式契约：详情小标签 + 可点击手型',
  /\.retry-more\s*\{/.test(css) && /\.msg-tool\.retry\s*\{[^}]*cursor:\s*pointer/.test(css));

// ---------------------------------------------------------------------------
check('保存重试设置使用统一主按钮样式',
  /id="retry-save" class="btn primary"/.test(fs.readFileSync(path.join(DIST, 'index.html'), 'utf8')));

group('工具调用轮数（max_steps）');
{
  const html = fs.readFileSync(path.join(DIST, 'index.html'), 'utf8');
  check('常规页有「工具调用轮数」卡片与保存按钮',
    html.includes('工具调用轮数') && /id="max-steps-save" class="btn primary"/.test(html));
  check('输入框不设上限（min=1，无 max 属性）——用户要求不限制轮数',
    /id="max-steps" type="number" min="1"/.test(html) && !/id="max-steps"[^>]*max=/.test(html));
  check('前端接线：打开设置时回填、保存时 POST max_steps',
    uiSrc.includes('refreshMaxStepsForm') && /\{ max_steps: v \}/.test(uiSrc));
}

group('外观设置：液态玻璃开关');
{
  const html = fs.readFileSync(path.join(DIST, 'index.html'), 'utf8');
  const appearance = html.slice(html.indexOf('id="page-appearance"'), html.indexOf('id="page-models"'));
  check('外观页有「新外观」开关行',
    appearance.includes('新外观') && /id="opt-liquid-glass"/.test(appearance));
  check('开关复用系统通知同款 switch 结构',
    /<label class="switch">\s*<input type="checkbox" id="opt-liquid-glass">/.test(appearance) &&
    appearance.includes('switch-track') && appearance.includes('switch-knob'));
  // 玻璃效果契约：body 类开关 + 四个切换组件 + 技能参数移植
  check('CSS 以 body.liquid-glass 为开关，默认关闭时不生效',
    css.includes('body.liquid-glass .settings-nav') && !/\:root[^{]*liquid/.test(css));
  check('玻璃效果覆盖四个切换组件（左导航/皮肤分段/模型tab/MCP tab）',
    css.includes('body.liquid-glass .settings-nav .nav-item.active') &&
    css.includes('body.liquid-glass .skin-seg button.active') &&
    css.includes('body.liquid-glass .models-tab.active') &&
    css.includes('body.liquid-glass .mcp-tab.active'));
  check('透镜参数来自 liquid-glass-button 技能（磨砂 blur18 / 白渐变 / 辉光）',
    css.includes('backdrop-filter: blur(18px)') &&
    css.includes('linear-gradient(135deg, rgba(255,255,255,.82)') &&
    css.includes('rgba(67,115,245,.20)'));
  // JS 接线：localStorage 记忆 + 动态切换 + 默认关闭
  check('开关接线：toggle body 类并写 localStorage（cf_liquid_glass）',
    uiSrc.includes("classList.toggle('liquid-glass', !!on)") &&
    uiSrc.includes("localStorage.getItem('cf_liquid_glass') === '1'") &&
    uiSrc.includes("localStorage.setItem('cf_liquid_glass', el.checked ? '1' : '0')"));
  check('默认关闭：非 "1" 一律按关闭处理',
    uiSrc.includes('applyLiquidGlass(localStorage.getItem(\'cf_liquid_glass\') === \'1\'); // 默认关闭'));
}

group('移动端触摸高亮');
check('禁用 WebView 默认点击遮罩，不改动全局焦点轮廓',
  /\*\s*\{[^}]*-webkit-tap-highlight-color:\s*transparent\s*;/.test(css) &&
  !/\*\s*\{[^}]*outline\s*:/.test(css));

group('删除菜单原位确认');
{
  const menus = [];
  const document = {
    createElement: function () {
      return {
        children: [], style: {}, offsetWidth: 100,
        appendChild: function (child) { this.children.push(child); },
        addEventListener: function (type, fn) { this[type] = fn; },
        remove: function () { this.removed = true; },
      };
    },
  };
  const anchor = {
    closest: function () { return { appendChild: function (menu) { menus.push(menu); } }; },
    getBoundingClientRect: function () { return { right: 200, bottom: 40 }; },
  };
  const api = new Function('document', 'let openMenu = null;\n' +
    extractFunction(uiSrc, 'closeInlineMenu') + '\n' +
    extractFunction(uiSrc, 'showInlineMenu') + '\nreturn {show: showInlineMenu, close: closeInlineMenu};')(document);
  ['永久删除', '删除项目'].forEach(function (label) {
    let calls = 0;
    const items = [{ label: label, danger: true, confirmDelete: true, fn: function () { calls++; } }];
    api.show(anchor, items);
    const menu = menus[menus.length - 1];
    const button = menu.children[0];
    button.click();
    check(label + '首次点击只在原按钮显示确认删除', calls === 0 && button.textContent === '确认删除' && !menu.removed);
    button.click();
    check(label + '第二次点击执行删除并关闭菜单', calls === 1 && menu.removed);
    api.show(anchor, items);
    menus[menus.length - 1].children[0].click();
    api.close();
    api.show(anchor, items);
    const reopened = menus[menus.length - 1].children[0];
    check(label + '关闭后重新打开重置确认状态', reopened.textContent === label);
    reopened.click();
    check(label + '重新打开后仍需二次确认', calls === 1);
  });
  let calls = 0;
  api.show(anchor, [{ label: '归档', fn: function () { calls++; } }]);
  menus[menus.length - 1].children[0].click();
  check('其他菜单操作仍单击执行', calls === 1);
  const sidebar = extractFunction(uiSrc, 'renderSessions');
  check('项目和会话删除均启用原位确认',
    /label: '删除项目', danger: true, confirmDelete: true/.test(sidebar) &&
    /label: '永久删除', danger: true, confirmDelete: true/.test(sidebar));
  check('项目和会话删除不调用浏览器确认框', !/\bconfirm\(/.test(sidebar +
    extractFunction(uiSrc, 'deleteSession') + extractFunction(uiSrc, 'deleteWorkspace')));
}

group('工具调用生成中的实时反馈（tool_pending）');
check('前端处理 tool_pending 事件并显示占位提示',
  uiSrc.includes("case 'tool_pending':") && uiSrc.includes('正在生成工具调用参数…'));
check('占位提示声明与终态清理齐全（tool_call/tool_result/error/idle/回放）',
  /let pendingToolEl = null/.test(uiSrc) &&
  (uiSrc.match(/pendingToolEl\.remove\(\); pendingToolEl = null;/g) || []).length >= 4 &&
  uiSrc.includes('thinkingEl = null; activeToolEl = null; retryEl = null; pendingToolEl = null;'));
check('占位提示只提示状态，不泄露工具 ID',
  !/正在生成工具调用参数[^']*(read_file|write_file|edit_file|run_command)/.test(uiSrc));

group('模型删除原位确认（不弹浏览器框）');
{
  // 删除按钮现在建在 modelRowEl()（逐行渲染）里，不在 renderModelItems()（分组装配）里。
  const render = extractFunction(uiSrc, 'modelRowEl');
  check('删除为按钮级原位确认：首次点「删除」变「确认删除」，配「取消」按钮',
    render.includes("domButton('删除'") &&
    render.includes("domButton('确认删除', 'confirming'") &&
    render.includes("domButton('取消'"));
  check('「确认删除」再次点击才执行，取消按钮恢复原删除按钮',
    render.includes('acts.removeChild(confirmBtn)') && render.includes('acts.appendChild(delBtn)'));
  check('模型删除不再调用浏览器 confirm 确认框',
    !/\bconfirm\('从模型库删除/.test(uiSrc));
  const css = fs.readFileSync(path.join(DIST, 'app.css'), 'utf8');
  check('「确认删除」态有红色危险样式',
    /\.mi-actions button\.confirming \{ [^}]*color: var\(--danger\)/.test(css));
}

group('上下文占用说明');
check('新增平均缓存命中率行（hit/(hit+miss)）',
  uiSrc.includes("'平均缓存命中率'") && uiSrc.includes("hit / (hit + miss) * 100"));
check('移除「自服务启动累计（重启重新计数）」说明',
  !uiSrc.includes('用量与缓存统计自服务启动后累计'));

// ---------------------------------------------------------------------------
// 编辑最近 N 条用户消息并重发（检查点 / 回滚的入口）
// ---------------------------------------------------------------------------
group('编辑并重发用户消息');

check('用户消息渲染时把原文存进 dataset（编辑时回填输入框要用）',
  /b\.dataset\.rawText = text;/.test(uiSrc));
check('编辑按钮只对服务端下发的白名单显形（前端不自行推断）',
  uiSrc.includes('editableBacks') && uiSrc.includes('setEditables') &&
  /editableBacks\.has\(back\)/.test(uiSrc));
check('白名单按 back（位置）定位，不按文本（重复提问会改错消息）',
  // 旧实现是 Map<text, back>：连着问两句一样的，Map 只留一条、后写的覆盖先写的，
  // 于是第一条的编辑按钮也拿到最后那条的 back，点下去改错消息。
  !/editableByText/.test(uiCode) &&
  /const back = total - 1 - i;/.test(uiSrc) &&
  /it\.back >= 0/.test(uiSrc));
check('提问行计数排除 steer 插话，且与定位共用同一个取行函数',
  /function userQuestionRows\(\)/.test(uiSrc) &&
  /\.msg-user:not\(\.msg-steer\)/.test(uiSrc) &&
  /const rows = userQuestionRows\(\);/.test(uiSrc) &&
  // findEditableUserRow 也必须走它，两处各写一份选择器必然漂
  /function findEditableUserRow\(back\)[\s\S]{0,220}?userQuestionRows\(\)/.test(uiSrc));
check('运行中不给编辑入口（上下文正在被消费）',
  /editableBacks\.has\(back\) && !running/.test(uiSrc));
check('编辑按钮默认隐形、悬停整行显形（CSS 契约）',
  /\.msg-user \.edit-btn \{[\s\S]*?opacity: 0;/.test(css) &&
  /\.msg-user:hover \.edit-btn \{ opacity: 1; \}/.test(css));
check('编辑按钮不遮挡气泡（order 归位到左侧）',
  /\.msg-user \.edit-btn \{[\s\S]*?order: -1;/.test(css));

check('编辑态把该条消息填入输入框并露出发送/取消',
  uiSrc.includes('function startEditMessage') && uiSrc.includes('function exitEditMode') &&
  /input\.value = text;/.test(uiSrc) && /editBar\.classList\.remove\('hidden'\)/.test(uiSrc));
check('取消会清空输入框并收起编辑条（不留下残影）',
  /function exitEditMode\(restore\)[\s\S]*?if \(restore\) \{[\s\S]*?input\.value = '';[\s\S]*?editBar\.classList\.add\('hidden'\)/.test(uiSrc));
check('编辑态下按发送走编辑重发，而非普通新消息',
  /if \(isEditing\(\) && override === undefined\) \{ submitEdit\(\); return; \}/.test(uiSrc));

check('发送携带 back（距最后一条的距离）与 rollback_files',
  /type: 'edit_user_message'/.test(uiSrc) &&
  /back: back,/.test(uiSrc) &&
  /rollback_files: true,/.test(uiSrc));
check('发送前把被编辑那条**就地改写**、只删它之后的节点',
  // 早先的写法是「连被编辑那条一起删掉、再补一条新气泡」：只要中途 wsSend 失败、
  // 或服务端在 emit edit 之前就 return error（back 定位不到），那条消息永远回不来
  // —— 用户看到的正是「被编辑的消息直接就没了」（2026-09-26 反馈）。
  uiSrc.includes('function applyEditInPlace') &&
  /applyEditInPlace\(back, text\);/.test(uiSrc) &&
  /bubble\.dataset\.rawText = text;/.test(uiSrc) &&
  /renderUserText\(bubble, text\);/.test(uiSrc) &&
  // 只从目标的 nextSibling 开始删：目标本身绝不在删除范围内
  /let node = row\.nextSibling;/.test(uiSrc));
check('就地改写时清掉指向已删节点的流式引用，并摘掉失效的编辑按钮',
  /currentTextEl = null; textBuffer = '';/.test(uiSrc) &&
  /const btn = row\.querySelector\('\.edit-btn'\);\s*if \(btn\) btn\.remove\(\);/.test(uiSrc));
check('倒计数排除 steer 插话（与服务端 isUserQuestion 同一口径）',
  // .msg-steer 也是 .msg-user，插话不是提问；算进去会数错条数、截错位置。
  uiSrc.includes('.msg-user:not(.msg-steer)') &&
  /!node\.classList\.contains\('msg-steer'\)/.test(uiSrc));
check('找不到目标行时退回全删兜底（宁可位置算错也不让消息消失）',
  /if \(!row\) \{[\s\S]{0,200}?truncateAfterEditableMessage\(back\);/.test(uiSrc) &&
  uiSrc.includes('function truncateAfterEditableMessage'));
check('兜底路径按 back 精确停在被编辑那条用户消息',
  /let remaining = \(Number\(back\) \|\| 0\) \+ 1;/.test(uiSrc) &&
  /if \(remaining <= 0\) break;/.test(uiSrc));
check('编辑重发失败时主动拉权威快照把视图拉回真相',
  // 服务端在截断之前失败（例如 back 定位不到）时既无 edit 帧也无 history 帧，
  // 界面会停在乐观改动后的状态，与库里的实际历史对不上。
  uiSrc.includes('pendingEdit') &&
  uiSrc.includes('function restoreAfterFailedEdit') &&
  /if \(pendingEdit\) \{[\s\S]{0,120}?pendingEdit = false;[\s\S]{0,120}?restoreAfterFailedEdit\(\);/.test(uiSrc) &&
  /type: 'load_session'/.test(uiSrc));
check('服务端 edit 事件只重置流式引用，视图交给随后的 history 快照重建',
  uiSrc.includes("case 'edit':") &&
  uiSrc.includes('function resetStreamRefs') &&
  // 旧做法是 beginEditedView 里 messagesEl.innerHTML = '' 清空整列 ——
  // 那会把编辑点**之前**的历史一并抹掉（2026-09-22 反馈），已移除。
  !uiSrc.includes('function beginEditedView') &&
  // 服务端必须在 EventEdit 时补一帧 history 权威快照，前端才有得重建。
  /agent\.EventEdit[\s\S]{0,220}?c\.srv\.historyEvent/.test(
    fs.readFileSync(path.join(__dirname, '../..', 'pkg/server/ws_handler.go'), 'utf8')));

check('编辑条样式存在且与输入区同族',
  /\.edit-bar \{/.test(css) && /\.edit-bar-btn\.primary \{/.test(css) &&
  /\.edit-bar\.hidden \{ display: none; \}/.test(css));
check('index.html 有编辑条 DOM（发送 / 取消两个按钮）',
  /id="edit-bar"/.test(htmlSrc) && /id="edit-send"/.test(htmlSrc) && /id="edit-cancel"/.test(htmlSrc));

// ---------------------------------------------------------------------------
// 「继续」：从断点续跑，而不是往历史里塞一句假提问
// ---------------------------------------------------------------------------
group('继续上一轮（从断点续跑）');

check('点「继续」发 continue_turn，不再发送字面量「继续」',
  uiSrc.includes('function sendContinueRequest') &&
  /type: 'continue_turn'/.test(uiSrc) &&
  // 旧实现是 sendAsUserText('继续')：那会在历史里留下一条用户从未说过的假提问。
  !/sendAsUserText\('继续'\)/.test(uiCode));
check('「继续」带当前会话与思考强度，且有运行中/未连接的守卫',
  /session_id: sessionID, thinking: thinkingVal/.test(uiSrc) &&
  /if \(running \|\| sending \|\| !wsReady\)/.test(uiSrc));
check('圆环提示讲清语义：不回退文件、不新增提问',
  /保留已完成的记录，不回退文件，不新增提问/.test(uiSrc));
check('服务端实现了 ContinueTurn（不追加用户消息，只挂一次性系统提示）',
  /func \(a \*Agent\) ContinueTurn\(ctx context\.Context, sessionID string, emit Emitter\) error/.test(agentSrc) &&
  /sess\.SetContinueHint\(continueHintText\)/.test(agentSrc) &&
  /defer sess\.SetContinueHint\(""\)/.test(agentSrc));
check('「继续」提示说清「被打断而非新需求」，并要求不重复已完成的工作',
  /本轮是「继续上一轮」/.test(agentSrc) &&
  /不要重复已完成的操作/.test(agentSrc) &&
  /没有拿到结果/.test(agentSrc));
check('continueHint 只在本次运行内有效，不进历史也不落库',
  /continueHint string/.test(historySrc) &&
  !/continueHint/.test(goCode('pkg/store/sessions.go')));
check('WS 分发 continue_turn（同步打断旧任务后起新轮）',
  /case "continue_turn":/.test(wsSrc) &&
  /c\.srv\.agent\.ContinueTurn\(ctx, msg\.SessionID, emit\)/.test(wsSrc));
check('系统提示拼装把「继续」尾巴接在任务清单之后',
  /if hint := sess\.ContinueHint\(\); hint != ""/.test(todosSrc));

group('子智能体：步数上限与上下文窗口继承');

check('子智能体步数默认继承主循环（不再写死 8）',
  /func resolveSubagentSteps\(sp SubagentPolicy, parent int\) int/.test(subagentSrc) &&
  /if sp\.MaxSteps > 0 \{/.test(subagentSrc) &&
  /return parent/.test(subagentSrc) &&
  // 旧实现在 newSubagentIn 里写死 8，删掉即可防止复发。
  !/cfg\.MaxSteps > 8 \|\| cfg\.MaxSteps <= 0/.test(subagentCode));
check('子智能体继承上下文窗口（否则压缩线回退到写死的 120000）',
  /child\.contextWindow\.Store\(int64\(a\.ContextWindow\(\)\)\)/.test(subagentSrc));
check('子智能体不继承内置插件开关（白名单里没有那些工具）',
  /child\.builtinOn = map\[string\]bool\{\}/.test(subagentSrc) &&
  !/child\.builtinOn = a\.builtinOnSnapshot\(\)/.test(subagentCode));
check('子智能体继承工具可见面（隐藏的工具不该又出现）',
  /child\.SetExposure\(a\.exposureFn\(\)\)/.test(subagentSrc));
check('子智能体策略只在父策略里取一次（避免白名单与步数来自不同版本）',
  /sp := a\.SubagentPolicy\(\)/.test(subagentSrc) &&
  /subagentToolSetPolicy\(mode, sp\)/.test(subagentSrc) &&
  /resolveSubagentSteps\(sp, a\.MaxSteps\(\)\)/.test(subagentSrc) &&
  // 旧写法是对 allowed 与步数各调一次 SubagentPolicy()，两次热更新之间会读到中间态。
  !/allowed := subagentToolSetPolicy\(mode, a\.SubagentPolicy\(\)\)/.test(subagentSrc));
check('设置页有「单任务步数上限」输入，0 显示为「跟随主循环（N 步）」',
  /id="sub-steps"/.test(htmlSrc) &&
  uiSrc.includes('function stepsLabel') &&
  /跟随主循环（/.test(uiSrc) &&
  /postSubagentSetting\(\{ max_steps: n \}/.test(uiSrc));
check('总开关关闭时步数输入一并置灰',
  /'sub-max', 'sub-steps', 'sub-allow-write'/.test(uiSrc));
check('子智能体页有说明文字（0 的语义否则没人懂）',
  /单任务步数上限/.test(htmlSrc) && /id="sub-steps-inherit-val"/.test(htmlSrc));

group('Termux 换背景图');

check('termux-tools 探测覆盖多个命令并显式并入 $PREFIX/bin',
  // 只探测 PATH 里的 termux-open-url 会在「从 Termux 外部拉起」时误判未安装。
  /probes := \[\]string\{"termux-open-url", "termux-storage-get", "termux-setup-storage"\}/.test(termuxSrc) &&
  /filepath\.Join\(prefix, "bin", name\)/.test(termuxSrc));
check('选图失败时区分「没装」与「装了但调用失败」，不再一律说需要 termux-tools',
  /func termuxToolsHint\(err error, out \[\]byte\) string/.test(termuxSrc) &&
  /termux-tools 未安装或命令不在 PATH/.test(termuxSrc) &&
  /termux-setup-storage 授权访问手机存储后重试/.test(termuxSrc) &&
  !/需要 termux-tools/.test(appearanceCode));
check('termux-storage-get 失败时回落到内置选择器（否则彻底换不了图）',
  /"ok": true, "builtin": true,/.test(appearanceSrc) &&
  /"degraded_from": "termux-storage-get"/.test(appearanceSrc) &&
  /"warning":/.test(appearanceSrc));
check('落盘路径是工作区下的绝对路径（DataDir 是相对值）',
  /func \(s \*Server\) bgPickDest\(\) \(string, error\)/.test(appearanceSrc) &&
  /!filepath\.IsAbs\(base\)/.test(appearanceSrc) &&
  /未选择工作区，无法确定背景图的保存位置/.test(appearanceSrc));
check('降级提示如实告知用户为什么换了选图方式',
  /if \(d\.warning\) addInfo\(d\.warning\);/.test(uiSrc));
check('内置选择器起点优先已授权的手机存储，否则退回 ~',
  /func termuxStartDir\(\) string/.test(appearanceSrc) &&
  /filepath\.Join\(home, "storage", "shared"\)/.test(appearanceSrc) &&
  /if dirAccessible\(shared\) \{/.test(appearanceSrc));
check('安装完成的复核只看关键命令（探测点太多会把整次安装误判为失败）',
  /lookTermuxPath\("termux-open-url"\)/.test(termuxSrc) &&
  !/err == nil && !termuxToolsReady\(\)/.test(goCode('pkg/server/termux.go')));

group('检查点 / 回滚');

check('处理 checkpoints 事件（白名单 + 回滚点）',
  uiSrc.includes("case 'checkpoints':") && uiSrc.includes('setEditables(ev.editables || [])') &&
  uiSrc.includes('checkpointSteps = ev.steps || []'));
check('处理 rewind 事件并给出人话反馈',
  uiSrc.includes("case 'rewind':") && uiSrc.includes('describeRewind'));
check('回滚反馈区分还原 / 删除 / 失败三类计数',
  /function describeRewind\(res\)[\s\S]*?res\.restored[\s\S]*?res\.deleted[\s\S]*?res\.failed/.test(uiSrc));
check('无改动时不谎报「已回退」（明确说没有需要回退的内容）',
  /if \(n === 0\) return '没有需要回退的文件改动';/.test(uiSrc));
check('切会话时清掉上一个会话的白名单（避免挂错按钮）',
  /editableBacks = new Set\(\);\s*\n\s*syncUserEditButtons\(\);/.test(uiSrc));
check('一轮结束（idle）后重新挂编辑按钮',
  /case 'idle': \{[\s\S]*?syncUserEditButtons\(\);/.test(uiSrc));

// ---------- 滚动：向上翻历史 + 回到底部 ----------
// 故障：主 agentloop 期间无法向上滑动查看历史 —— 每次模型吐字/追加内容都把视口
// 拽回底部。修法是引入 followTail：用户主动上滚即停止自动跟随，滚回底部附近恢复。
// 另加右下角「回到底部」箭头，仅在不在底部时出现。
group('滚动：跟随态与回到底部按钮');
check('scrollBottom 在非跟随态不再强行滚底',
  /function scrollBottom\(force\)[\s\S]{0,180}?if \(!force && !followTail\)/.test(uiSrc));
check('scroll 回调据「是否贴近底部」判定跟随态（不靠滚动方向）',
  /followTail\s*=\s*isNearBottom\(\);/.test(uiSrc) &&
  /function isNearBottom\(\)[\s\S]{0,160}?scrollHeight - messagesEl\.scrollTop - messagesEl\.clientHeight <= FOLLOW_NEAR_BOTTOM/.test(uiSrc));
check('用户重新发送消息即恢复跟随（否则发问后看不到回复）',
  /followTail = true;[\s\S]{0,260}?addUser\(p\.display\)/.test(uiSrc));
check('回放历史后恢复跟随；截断信号只重置流式引用、不清空聊天列',
  extractFunction(uiSrc, 'replayHistory').includes('followTail = true') &&
  // 历史被截断（edit 帧）时**不得**再清空整列：旧实现就是这么把编辑点之前的
  // 历史一并抹掉，用户看到「操作记录全没了」（2026-09-22 反馈）。
  !extractFunction(uiSrc, 'resetStreamRefs').includes('innerHTML') &&
  /case 'edit':[\s\S]{0,900}?resetStreamRefs\(\);/.test(uiSrc));
check('清空视图（新建/归档/删除）也回到跟随态',
  /function syncComposerMode\(\)[\s\S]{0,320}?if \(empty\) \{[\s\S]{0,80}?followTail = true;/.test(uiSrc));
check('右下角箭头只在不在底部时显示',
  /function syncToBottomBtn\(\)[\s\S]{0,140}?classList\.toggle\('show', !isNearBottom\(\)\)/.test(uiSrc));
check('点箭头回到底部并恢复跟随',
  /toBottomBtn\.addEventListener\('click',[\s\S]{0,200}?followTail = true;[\s\S]{0,160}?scrollTo\(\{ top: messagesEl\.scrollHeight, behavior: 'smooth' \}\)/.test(uiSrc));
check('箭头 DOM 存在且有 aria-label',
  /<button type="button" id="to-bottom" class="to-bottom" title="回到底部" aria-label="回到底部">/.test(htmlSrc));
check('样式契约：箭头绝对定位在右下、隐藏时不可点',
  /\.to-bottom\s*\{[^}]*position:\s*absolute/.test(css) &&
  /\.to-bottom\s*\{[^}]*bottom:\s*118px/.test(css) &&
  /\.to-bottom\s*\{[^}]*pointer-events:\s*none/.test(css) &&
  /\.to-bottom\.show\s*\{[^}]*pointer-events:\s*auto/.test(css));
check('消息区底部留白改为按输入卡片实测高度动态计算（不再写死 200px）',
  /#messages\s*\{[^}]*padding:\s*24px 32px var\(--composer-pad-bottom/.test(css) &&
  !/#messages\s*\{[^}]*padding:[^;]*200px 44px/.test(css));
check('留白随卡片尺寸变化（ResizeObserver）+ 折叠后重算',
  /new ResizeObserver\(syncComposerPadding\)\.observe\(composerPadEl\)/.test(uiSrc) &&
  /function applyTodoCollapsed\(\)[\s\S]{0,600}?requestAnimationFrame\(function \(\) \{ syncComposerPadding\(\); \}\)/.test(uiSrc));
check('不用 CSS 平滑滚动（避免与跟随态滚底打架）',
  /#messages\s*\{[^}]*scroll-behavior:\s*auto/.test(css));

// ---------- 工具卡片与 diff 面板 ----------
// 需求（2026-09-22）：工具卡片不再把原始参数堆在 hover 提示里（以前 title 就是
// JSON.stringify(input)）；改过文件的卡片点击可打开 diff 看改动位置。
group('工具卡片与 diff 面板');
check('工具卡片不再把原始参数塞进 hover 提示',
  !/if \(detail\) d\.title = detail/.test(uiSrc) &&
  !/addTool\([\s\S]{0,160}?JSON\.stringify/.test(uiSrc));
check('只有改文件的工具卡片可点击（edit/write/delete）',
  /const DIFF_TOOLS = \{ edit_file: 1, write_file: 1, delete_file: 1 \};/.test(uiSrc) &&
  extractFunction(uiSrc, 'toolFilePath').includes('DIFF_TOOLS'));
check('可点击卡片键盘可达（role / tabindex / Enter）',
  extractFunction(uiSrc, 'addTool').includes("setAttribute('role', 'button')") &&
  extractFunction(uiSrc, 'addTool').includes("setAttribute('tabindex', '0')") &&
  extractFunction(uiSrc, 'addTool').includes("ev.key === 'Enter'"));
check('diff 行分类：文件头优先于增删行（否则 --- / +++ 会被染成增删）',
  diffLineKind('--- a.txt') === 'file' && diffLineKind('+++ a.txt') === 'file' &&
  diffLineKind('+added') === 'add' && diffLineKind('-removed') === 'del' &&
  diffLineKind('@@ -1,3 +1,4 @@') === 'hunk' && diffLineKind(' context') === 'ctx');
check('diff 面板逐行复用 .diff 的配色类',
  extractFunction(uiSrc, 'renderDiffBody').includes('diffLineKind(') &&
  /class="diff diff-body"/.test(uiSrc) &&
  /\.diff \.add\s*\{/.test(css) && /\.diff \.del\s*\{/.test(css));
check('diff 面板 Esc 只关面板、不误打断任务',
  /if \(diffPanelOpen\) \{ e\.preventDefault\(\); closeDiffPanel\(\); return; \}/.test(uiSrc));
check('历史回放的卡片按路径向 /api/diff 取 diff',
  extractFunction(uiSrc, 'openToolDiff').includes("'/api/diff?session_id='"));
check('回放的卡片只带路径、不带 diff 文本（diff 不随历史持久化）',
  !/addTool\([\s\S]{0,160}?JSON\.stringify/.test(uiSrc));
check('改过的卡片可点开看 diff（role / tabindex / Enter）',
  extractFunction(uiSrc, 'addTool').includes("setAttribute('role', 'button')") &&
  extractFunction(uiSrc, 'addTool').includes("setAttribute('tabindex', '0')") &&
  extractFunction(uiSrc, 'addTool').includes("ev.key === 'Enter'"));
check('历史回放的卡片带 path（点开时向 /api/diff 取 diff）',
  // ⚠️ 这里刻意**不**断言路径归一化：前端带的是模型传入的原始路径（历史里存的
  // 也是原始 Input），归一化由服务端 CheckpointFor 的三级容错负责
  // （精确 → 绝对化 → 边界后缀，见 pkg/agent/checkpoints.go 的 pathMatchers）。
  // 前端自己拼绝对路径反而会与「同后缀不同文件」的情况打架。
  /addTool\(toolLabel\(b\.name, b\.input\), \{ path: toolFilePath\(b\.name, b\.input\) \}\)/.test(uiSrc) &&
  /function toolFilePath\(name, input\)/.test(uiSrc));
check('未匹配到记录时不谎报「已被回退」',
  // 大多数未命中是路径写法不同（模型传相对路径 / 大小写差异），
  // 改动其实好好地记着 —— 说成「已被回退」是对用户说了假话。
  /按这个路径没匹配到本会话的改动记录/.test(
    fs.readFileSync(path.join(__dirname, '../..', 'pkg/server/diff_api.go'), 'utf8')));
check('服务端路径查找三级容错且落在分隔符边界上',
  /func pathMatchers\(path, root string\) \[\]func\(string\) bool/.test(
    fs.readFileSync(path.join(__dirname, '../..', 'pkg/agent/checkpoints.go'), 'utf8')) &&
  /strings\.HasSuffix\(lp, "\/"\+lower\)/.test(
    fs.readFileSync(path.join(__dirname, '../..', 'pkg/agent/checkpoints.go'), 'utf8')));
check('样式契约：只有可点击卡片给手型',
  /\.msg-tool\s*\{[^}]*cursor:\s*default/.test(css) &&
  /\.msg-tool\.clickable, \.msg-tool\.retry \{ cursor: pointer; \}/.test(css));

// ---------- 背景图：不透明底色必须在 html 上 ----------
// 故障：选好图却看不见。#bg-layer 是 body 的子节点、靠 z-index:-1 沉底，
// 但 html/body 都挂了不透明 background:var(--bg)，于是图层被永久遮住。
// 修法：不透明底色只挂 html，body 透明 —— 图层排在 body 之上、内容之下，层级成立。
// 切会话时 replayHistory 一旦中途抛异常，末尾的 renderSessions 就跑不到，
// 侧栏高亮会停在上一个会话（2026-09-19 实际故障：某个会话的历史里有 name 缺失的
// tool_use 块，toolLabel 裸用 name.indexOf 直接抛 TypeError）。
group('历史回放的健壮性（切会话不能丢侧栏高亮）');
check('toolPhrase 先归一化 name（缺失时不崩）',
  /function toolPhrase\(name\)\s*\{[\s\S]{0,900}?name = typeof name === 'string' \? name : ''/.test(uiSrc));
check('toolLabel 先归一化 name（缺失时走 default 的中文兜底）',
  /function toolLabel\(name, input\)\s*\{[\s\S]{0,300}?name = typeof name === 'string' \? name : ''/.test(uiSrc));
check('回放时单块渲染失败被隔离（否则整次回放中断、高亮不更新）',
  /\[replay\] 跳过无法渲染的历史块/.test(uiSrc) &&
  /catch \(err\) \{\s*console\.warn\('\[replay\]/.test(uiSrc));
check('renderSessions 仍挂在 replayHistory 末尾（高亮的落点）',
  /renderSessions\(sessionsCache\); \/\/ 高亮切换后的 active/.test(uiSrc));
// 「运行中转向」注入的插话在历史里带 origin=steer：回放时必须仍渲染成转向气泡，
// 否则切会话/重启后它就成了一条普通提问（用户分不清插话与提问，
// 后端也只把 origin 为空的纯文本消息当作「真正的提问」，两侧口径要一致）。
check('回放时带 origin=steer 的消息仍按转向渲染',
  /if \(m\.origin === 'steer'\) addSteer\(b\.text \|\| '', true\)/.test(uiSrc));
check('回放出来的转向标记写「已并入」（它本来就在历史里）',
  /function addSteer\(text, merged\)/.test(uiSrc) &&
  /merged\s*\n?\s*\? '转向 · 已并入上下文，本步起生效'/.test(uiSrc));

// ---------- 命名：retry* 与 resume* 是两件事 ----------
// retry*  = 上游自己重试，前端只把「正在重试」透出来；
// resume* = 这一轮没跑完（打断/报错/刷新），用户点一下接着跑（发一句「继续」）。
// 二者曾共用 retry 前缀（协议字段还叫 retry_back），名字与行为不符，
// 长期共存会误导后续维护（2026-09-23 收口）。
group('「继续」与「上游重试」的命名区分');
check('「继续」圆环用 resume 前缀，不再与上游重试提示共用 retry',
  /function showResumeRing\(\)/.test(uiSrc) &&
  /function removeResumeRing\(\)/.test(uiSrc) &&
  /let resumeRingEl = null/.test(uiSrc) &&
  !/retryRing/.test(uiSrc) && !/retryBack/.test(uiSrc));
check('继续圆环的样式类跟着改名（.resume-ring，不留 .retry-ring）',
  /\.resume-ring\s*\{/.test(css) && !/\.retry-ring/.test(css));

// ---------- 模型 / 供应商管理（2026-09-20 改造）----------
group('模型与供应商管理');
check('「供应商机制」说明横幅已删除',
  !/供应商机制/.test(htmlSrc) && !/只在供应商上维护一份/.test(htmlSrc));
check('「添加供应商」用真值哨兵，不会被「自动选第一个」吃掉',
  // 空串是假值，会被 renderProviders 开头的自动选择重置 → 点添加没反应
  /const PROV_NEW = '__new__'/.test(uiSrc) &&
  /provSelected = PROV_NEW; renderProviders\(\)/.test(uiSrc) &&
  !/provSelected = ''; renderProviders\(\)/.test(uiSrc));
check('详情区对哨兵值走「新建」分支（不会当成某个供应商）',
  /provSelected !== PROV_NEW\) \? findProvider\(provSelected\) : null/.test(uiSrc));
check('供应商详情里有「获取模型列表」，就地展开不弹窗',
  /function fetchUpstream\(\)/.test(uiSrc) &&
  /domButton\('获取模型列表'/.test(uiSrc) &&
  /domEl\('div', 'pv-disc hidden'\)/.test(uiSrc));
check('获取到的模型可勾选并批量入库（按供应商归类）',
  /function commitPicked\(\)/.test(uiSrc) &&
  /provider_id: p\.id/.test(uiSrc) &&
  /\/api\/models\/save_batch/.test(uiSrc));
check('模型列表：子模型有独立容器（供缩进）',
  /const kids = domEl\('div', 'model-children'\)/.test(uiSrc) &&
  /\.model-children\s*\{[^}]*margin-left/.test(css));
check('模型列表：供应商分组可折叠，且折叠时不渲染子模型',
  /modelGroupFolded/.test(uiSrc) && /mg-fold/.test(uiSrc) &&
  /if \(folded\) return;/.test(uiSrc));
check('编辑模型改为就地展开，不再复用「添加模型」表单',
  /function buildModelEditor/.test(uiSrc) &&
  !/function editModel/.test(uiSrc) &&
  !/function editModel/.test(uiSrc));
check('就地编辑不再提交密钥明文（列表是脱敏视图，提交会覆盖掉已存密钥）',
  /body\.key_source = m\.key_source/.test(uiSrc) &&
  !/key_value:/.test(uiSrc.slice(uiSrc.indexOf('function buildModelEditor'), uiSrc.indexOf('function buildModelEditor') + 3000)));

// ---------- 上下文档位（2026-09-20）----------
group('上下文长度预置档位');
check('输入档 = 1M / 512K / 256K / 128K（按 1024 的二进制 K）',
  /CTX_PRESETS_IN\s*=\s*\[1048576,\s*524288,\s*262144,\s*131072\]/.test(uiSrc));
check('输出档 = 384K / 256K / 128K',
  /CTX_PRESETS_OUT\s*=\s*\[393216,\s*262144,\s*131072\]/.test(uiSrc));
check('档位标签按二进制 K 渲染（256K→262144、384K→393216）',
  /function fmtCtxSlot\(n\)/.test(uiSrc) &&
  /n % 1048576 === 0/.test(uiSrc) && /n % 1024 === 0/.test(uiSrc));
check('保留数字输入框（非整档的既有值不会被静默改掉）',
  /const num = domInput\('number', '', ''\)/.test(uiSrc) &&
  /num\.value = String\(Number\(current\) \|\| fallback\)/.test(uiSrc));
check('档位做成**直接点击**的按钮（不是下拉、不弹层）',
  /domEl\('button', 'ctx-chip', fmtCtxSlot\(v\)\)/.test(uiSrc) &&
  /b\.addEventListener\('click', function \(\) \{ num\.value = String\(v\); sync\(\); \}\)/.test(uiSrc) &&
  !/CTX_CUSTOM/.test(uiSrc) &&
  !/buildCtxPicker[\s\S]{0,900}?domEl\('select'\)/.test(uiSrc));
check('输入值命中某档时该档高亮（否则看不出当前是哪个档）',
  /function sync\(\)[\s\S]{0,200}?classList\.toggle\('on', Number\(b\.dataset\.v\) === v\)/.test(uiSrc) &&
  /num\.addEventListener\('input', sync\)/.test(uiSrc));
check('表单不再直接读写 mf-ctx-in / mf-ctx-out 的值（统一走控件）',
  !/getElementById\('mf-ctx-in'\)\.value/.test(uiSrc) &&
  !/getElementById\('mf-ctx-out'\)\.value/.test(uiSrc) &&
  /ctxInPicker \? ctxInPicker\.value\(\) : 262144/.test(uiSrc) &&
  /ctxOutPicker \? ctxOutPicker\.value\(\) : 131072/.test(uiSrc));
check('回填走控件的 set（否则输入框不会更新）',
  /ctxInPicker\.set\(m\.ctx_in \|\| 262144\)/.test(uiSrc) &&
  /ctxOutPicker\.set\(m\.ctx_out \|\| 131072\)/.test(uiSrc));
check('就地编辑模型也用同一套控件',
  /const ctxIn = buildCtxPicker\(m\.ctx_in, CTX_PRESETS_IN, 262144\)/.test(uiSrc) &&
  /const ctxOut = buildCtxPicker\(m\.ctx_out, CTX_PRESETS_OUT, 131072\)/.test(uiSrc));
check('样式契约：整行铺开（输入组靠左、输出组靠右）',
  /\.ctx-inputs\s*\{[^}]*justify-content:\s*space-between/.test(css));
check('样式契约：档位按钮 + 命中态高亮',
  /\.ctx-chip\s*\{/.test(css) && /\.ctx-chip\.on\s*\{/.test(css));

// ---------- 供应商密钥回显 / 模型弹层两行 / 连不通的报错（2026-09-20）----------
const modelsGoSrc = fs.readFileSync(path.join(__dirname, '..', '..', 'pkg', 'server', 'models.go'), 'utf8');
const discoverGoSrc = fs.readFileSync(path.join(__dirname, '..', '..', 'pkg', 'server', 'model_discover.go'), 'utf8');
const cfgModelsGoSrc = fs.readFileSync(path.join(__dirname, '..', '..', 'config', 'models.go'), 'utf8');

group('供应商密钥回显 / 模型弹层 / 超时报错');
check('每个供应商都回填 key_plain（不再只给当前生效模型所属那个）',
  // 早先只回填 active 那个 → 切到别的供应商密钥框是空的，用户以为「保存的 key 丢了」
  /for _, pv := range providers \{[\s\S]{0,260}?pv\["key_plain"\] = v/.test(modelsGoSrc) &&
  !/if activeProvider != "" \{[\s\S]{0,300}?pv\["key_plain"\] = v/.test(modelsGoSrc));
check('模型条目下发解析好的 provider_name（供弹层显示来源）',
  /"provider_name":\s*provName/.test(cfgModelsGoSrc) &&
  /if p, ok := ps\.Find\(raw\.ProviderID\); ok \{/.test(cfgModelsGoSrc));
check('模型弹层按「供应商 ↓ 显示名称」两行渲染',
  /const pv = document\.createElement\('span'\);\s*\n\s*pv\.className = 'pop-opt-prov'/.test(uiSrc) &&
  /nm\.className = 'pop-opt-name'/.test(uiSrc) &&
  /\.pop-opt-prov\s*\{/.test(css) && /\.pop-opt-name\s*\{/.test(css));
check('模型弹层两段式：顶部**单独**的供应商选择器（不再「模型和供应商一起选中」）',
  /let modelProvFilter = ''/.test(uiSrc) &&
  /let provListOpen = false/.test(uiSrc) &&
  /className = 'pop-prov-row'/.test(uiSrc) &&
  /lbl\.textContent = '供应商'/.test(uiSrc) &&
  // 点供应商项：切 filter 并收起列表
  /item\.addEventListener\('click', function \(e\) \{\s*e\.stopPropagation\(\);\s*modelProvFilter = p\.id;\s*provListOpen = false/.test(uiSrc));
check('模型列表只显示当前供应商下的条目（按 provider_id 过滤）',
  /curProvId \|\| m\.provider_id === curProvId/.test(uiSrc));
check('切换模型时把供应商过滤同步到该模型的供应商（上下两段始终一致）',
  /const m = \(modelChoices \|\| \[\]\)\.find\(function \(x\) \{ return x\.id === id; \}\)/.test(uiSrc) &&
  /if \(m && m\.provider_id\) \{[\s\S]{0,80}?modelProvFilter = m\.provider_id/.test(uiSrc));
check('每次打开弹层复位供应商过滤（关掉再开重新跟随生效模型）',
  // 否则上一次「切去别家浏览」的选择会一直留着，而生效模型可能已从设置页换成别家的，
  // 顶上显示 A、实际生效 B —— 自相矛盾。浏览选择只在本次展开期间有效。
  /modelProvFilter = '';\s*provListOpen = false;\s*loadModels\(\);/.test(uiSrc));
check('⚠️ 弹层供应商列表从 modelChoices 推导，不依赖 providersCache',
  // providersCache 只在**设置页** loadLibrary() 时填充，主界面从来没加载过它。
  // 早先 renderModelPop 直接读它 → 顶段供应商行在主界面根本不渲染（实测拿到 null）。
  // ⚠️ 先剥掉注释再检查 —— 上面那段注释里就写到了 providersCache 这个词
  (function () {
    const i = uiSrc.indexOf('function renderModelPop()');
    const j = uiSrc.indexOf('\n  }', i);
    const raw = i >= 0 && j > i ? uiSrc.slice(i, j) : '';
    const body = raw.replace(/\/\/[^\n]*/g, '');
    return body.indexOf('providersCache') < 0 && /seenProv\[key\]/.test(body);
  })());
check('「供应商行」与「供应商列表」两个新元素的样式',
  /\.pop-prov-row\s*\{/.test(css) &&
  /\.pop-prov-list\s*\{/.test(css) &&
  /\.pop-prov-item\.selected\s*\{/.test(css));
check('供应商名过长会截断（不把弹层撑宽）',
  /\.pop-opt-prov\s*\{[^}]*text-overflow:\s*ellipsis/.test(css));
check('模型弹层整体居中，且不波及权限弹层',
  // 只作用于 #model-list —— 权限弹层共用 .pop-opt 类，那里要左对齐
  /#model-list \.pop-opt\s*\{[^}]*text-align:\s*center/.test(css) &&
  /#model-list \.pop-opt-prov\s*\{[^}]*margin-left:\s*auto/.test(css) &&
  !/^\.pop-opt\s*\{[^}]*text-align:\s*center/m.test(css));
check('超时报错能区分「连不通」与「上游慢」（别把人往错方向带）',
  /未能连上 %s/.test(discoverGoSrc) && /未能连上 %s/.test(modelsGoSrc) &&
  /域名能解析、但 TCP 连不上/.test(discoverGoSrc) &&
  !/上游响应过慢/.test(discoverGoSrc) && !/上游响应过慢/.test(modelsGoSrc));
check('超时时长取自 upstreamTimeout，不写死（写死会随超时调整而失真）',
  /var upstreamTimeout = 30 \* time\.Second/.test(discoverGoSrc) &&
  /WithTimeout\(r\.Context\(\), upstreamTimeout\)/.test(discoverGoSrc) &&
  /WithTimeout\(r\.Context\(\), upstreamTimeout\)/.test(modelsGoSrc) &&
  !/请求超时（30s）/.test(discoverGoSrc) && !/请求超时（30s）/.test(modelsGoSrc));

// ---------- 错误处理与兜底（2026-09-21）----------
const rootDir = path.join(__dirname, '..', '..');
const errsSrc = fs.readFileSync(path.join(rootDir, 'pkg', 'errs', 'errs.go'), 'utf8');
const llmOpenAISrc = fs.readFileSync(path.join(rootDir, 'pkg', 'llm', 'openai.go'), 'utf8');
const llmAnthropicSrc = fs.readFileSync(path.join(rootDir, 'pkg', 'llm', 'anthropic.go'), 'utf8');
const apiHandlersSrc = fs.readFileSync(path.join(rootDir, 'pkg', 'server', 'api_handlers.go'), 'utf8');
const wsHandlerSrc2 = fs.readFileSync(path.join(rootDir, 'pkg', 'server', 'ws_handler.go'), 'utf8');
const webProxySrc = fs.readFileSync(path.join(rootDir, 'pkg', 'tools', 'builtin', 'web_proxy.go'), 'utf8');
const webProxyWinSrc = fs.readFileSync(path.join(rootDir, 'pkg', 'tools', 'builtin', 'web_proxy_windows.go'), 'utf8');

group('错误处理：翻译层 / 统一出口 / 兜底');

check('错误翻译层存在（成因 + 建议 + 可重试性）',
  /func Classify\(err error\) Kind/.test(errsSrc) &&
  /func Cause\(k Kind\) string/.test(errsSrc) &&
  /func Hint\(k Kind\) string/.test(errsSrc) &&
  /func Retryable\(k Kind\) bool/.test(errsSrc));

check('⚠️ 流被截断单独归类（不再被误报成「文件读错」）',
  /errors\.Is\(err, io\.ErrUnexpectedEOF\)/.test(errsSrc) &&
  /KindStreamCut/.test(errsSrc) &&
  /传到一半/.test(errsSrc));

check('⚠️ FriendlyOr 只补系统故障，不动业务层写好的中文说明',
  /func FriendlyOr\(action string, err error\) string/.test(errsSrc) &&
  /if Classify\(err\) == KindUnknown \{\s*\n\s*return err\.Error\(\)/.test(errsSrc));

check('取消类错误不可重试（用户点了停止还自动重试最招人烦）',
  /case KindCanceled:\s*\n\s*return ""/.test(errsSrc) &&
  /Retryable\(KindCanceled\)/.test(fs.readFileSync(path.join(rootDir, 'pkg', 'errs', 'errs_test.go'), 'utf8')));

check('服务端所有错误响应走统一出口 writeErr（不再裸抛 err.Error()）',
  /func writeErr\(w http\.ResponseWriter, status int, action string, err error\)/.test(apiHandlersSrc) &&
  /errs\.FriendlyOr\(action, err\)/.test(apiHandlersSrc) &&
  !/map\[string\]any\{"error": err\.Error\(\)\}/.test(apiHandlersSrc));

check('WS 层错误同样翻译后才下发',
  /func \(c \*wsClient\) sendErr\(action string, err error\)/.test(wsHandlerSrc2) &&
  !/c\.send\(map\[string\]any\{"type": "error", "error": err\.Error\(\)\}\)/.test(wsHandlerSrc2));

check('⚠️ LLM 流「未产出内容就断」时自动重试（本次故障的直接兜底）',
  /func pumpStream\(/.test(llmOpenAISrc) &&
  /if emitted \|\| !errs\.Retryable\(kind\) \|\| ctx\.Err\(\) != nil/.test(llmOpenAISrc) &&
  /consume\(ctx, r, out\)/.test(llmOpenAISrc));

check('⚠️ 已产出内容后不重试（避免重复输出 / 重复执行工具）',
  /emitted = true/.test(llmOpenAISrc) &&
  /// 一旦已经吐出正文或工具调用/.test(llmOpenAISrc) &&
  /return emitted, err/.test(llmOpenAISrc));

check('anthropic 与 openai 共用同一套重试骨架（避免两边漂移）',
  /pumpStream\(ctx, p\.retry, resp, reopen,/.test(llmAnthropicSrc) &&
  /return emitted, err/.test(llmAnthropicSrc));

check('⚠️ 系统代理的绕过列表被读取（ProxyOverride，此前被整个忽略）',
  /func systemProxyRaw\(\) \(string, \[\]string\)/.test(webProxyWinSrc) &&
  /GetStringValue\("ProxyOverride"\)/.test(webProxyWinSrc));

check('回环地址无条件绕过代理（交给代理永远是错的）',
  /func shouldBypassProxy\(host string, bypass \[\]string\) bool/.test(webProxySrc) &&
  /ip\.IsLoopback\(\)/.test(webProxySrc) &&
  /h == "localhost" \|\| h == "::1"/.test(webProxySrc));

check('ProxyOverride 的各类通配规则都被支持',
  /p == "<local>"/.test(webProxySrc) &&
  /strings\.HasPrefix\(p, "\*\."\)/.test(webProxySrc) &&
  /strings\.HasSuffix\(p, "\*"\)/.test(webProxySrc));

// ---------- 上下文超窗：按时压缩 + 撞墙自救（2026-09-21）----------
const agentGoSrc = fs.readFileSync(path.join(rootDir, 'pkg', 'agent', 'agent.go'), 'utf8');
const agentHistSrc = fs.readFileSync(path.join(rootDir, 'pkg', 'agent', 'history.go'), 'utf8');
const agentCtxSrc = fs.readFileSync(path.join(rootDir, 'pkg', 'agent', 'context.go'), 'utf8');

group('上下文超窗：按时压缩 + 撞墙自救');

check('「输入+输出超过上下文窗口」单独归类（不再当普通 400 直接抛给用户）',
  /KindContextOverflow/.test(errsSrc) &&
  /func isContextOverflowMessage\(msg string\) bool/.test(errsSrc) &&
  /maximum context length/.test(errsSrc) &&
  /reduce the length of the input/.test(errsSrc));

check('超窗不可「原样重试」（同一请求再发一次还是超窗，必须压缩后重发）',
  /上下文超窗同理：不改变请求内容，重发没有意义/.test(errsSrc) &&
  /KindContextOverflow/.test(errsSrc));

check('⚠️ 撞墙后自动收紧压缩线重试，而不是整轮白跑',
  /errs\.Classify\(err\) == errs\.KindContextOverflow && attempt <= maxOverflowShrinks/.test(agentGoSrc) &&
  /shrink \*= overflowShrinkRatio/.test(agentGoSrc) &&
  /maxOverflowShrinks = 2/.test(agentCtxSrc) &&
  /overflowShrinkRatio = 0\.7/.test(agentCtxSrc));

check('自救过程会告知用户（否则界面只是突然多出一段摘要）',
  /上游报告上下文超窗，正在压缩历史后重试/.test(agentGoSrc));

check('⚠️ 估算器用上游真实用量校准（这才是「在合适的时间压缩」）',
  /func \(s \*Session\) calibrateTokenFactor\(realInput int\)/.test(agentHistSrc) &&
  /tokenFactor float64/.test(agentHistSrc) &&
  /s\.calibrateTokenFactor\(u\.InputTokens\)/.test(agentHistSrc));

check('校准只放大不缩小（宁可早压，漏压的代价是整轮失败）',
  /if f < 1 \{[\s\S]{0,60}?f = 1/.test(agentHistSrc) &&
  /只放大不缩小/.test(agentHistSrc));

check('校准系数有上限（避免异常用量把压缩线压得过低、每轮无谓压缩）',
  /maxTokenFactor = 2\.0/.test(agentCtxSrc) &&
  /if f > maxTokenFactor \{/.test(agentHistSrc));

check('压缩判定改用校准后的估算，而不是裸估算',
  /used := sess\.calibratedEstimate\(view\)/.test(agentGoSrc) &&
  /sess\.calibratedEstimate\(messages\) > budget/.test(agentGoSrc));

check('每次请求前记录估算基准（含系统提示与工具定义，口径才对得上）',
  /sess\.setReqEstimate\(EstimateTokens\(messages\) \+ overhead\)/.test(agentGoSrc));

// ---------- 上下文占用实时刷新（2026-09-21）----------
const wsPushSrc = fs.readFileSync(path.join(rootDir, 'pkg', 'server', 'ws_handler.go'), 'utf8');

// checkpoints 帧的锚点字段：名字必须是 resume_back。它的语义是「用户点一下接着跑」，
// 而不是「重试（截断重跑）」—— 名字与行为不符会误导后续维护（2026-09-23 改名）。
check('checkpoints 帧用 resume_back，而不是语义已变的 retry_back',
  /"resume_back": s\.agent\.UnfinishedTurnAnchor\(sessionID\)/.test(wsPushSrc) &&
  // 只看字段本身，注释里提到旧名是允许的（那正是改名的缘由）。
  !/"retry_back"\s*:/.test(wsPushSrc));

group('上下文占用：轮次进行中也实时刷新');

check('⚠️ 轮次进行中就推送上下文占用（不再等本轮 idle 才跳一下）',
  /func pushesContextOn\(t string\) bool/.test(wsPushSrc) &&
  /if pushesContextOn\(ev\.Type\) \{\s*\n\s*c\.send\(c\.srv\.contextUsage\(sessionID\)\)/.test(wsPushSrc));

check('刷新时机覆盖占用变化的全过程（发话 / 步骤 / 工具结果 / 压缩回落）',
  /case agent\.EventUser, agent\.EventStep, agent\.EventToolResult, agent\.EventCompress:/.test(wsPushSrc));

check('⚠️ 高频 delta 事件不触发刷新（每秒上百条，会白烧 CPU）',
  !/agent\.EventText,/.test(wsPushSrc) &&
  /刻意不选 delta 类事件/.test(wsPushSrc));

check('实时读取会话是安全的（emit 在 agent 自己的 goroutine 里同步调用）',
  /emit 是 agent 在\*\*自己的 goroutine 里同步调用\*\*的/.test(wsPushSrc));

check('前端进度条有过渡动画（频繁更新才不会一跳一跳）',
  /\.ctx-fill\s*\{[^}]*transition:\s*width/.test(css));

// ---------- 压缩状态持久化 + 切换前先压缩（2026-09-21）----------
const storeGoSrc = fs.readFileSync(path.join(rootDir, 'pkg', 'store', 'store.go'), 'utf8');
const storeSessSrc = fs.readFileSync(path.join(rootDir, 'pkg', 'store', 'sessions.go'), 'utf8');
const serverModelsSrc = fs.readFileSync(path.join(rootDir, 'pkg', 'server', 'models.go'), 'utf8');

group('压缩状态持久化 + 切换模型前先压缩');

check('⚠️ 压缩状态落盘（否则重启即丢，重开同一会话立刻又超限）',
  /ALTER TABLE sessions ADD COLUMN compressed_up_to/.test(storeGoSrc) &&
  /ALTER TABLE sessions ADD COLUMN summary_text/.test(storeGoSrc) &&
  /CompressedUpTo int/.test(storeSessSrc) &&
  /SummaryText    string/.test(storeSessSrc));

check('存取两侧都带上压缩状态',
  /compressed_up_to, summary_text/.test(storeSessSrc) &&
  /SET title = \?, updated_at = \?, compressed_up_to = \?, summary_text = \?/.test(storeSessSrc) &&
  /CompressedUpTo: s\.compressedUpTo/.test(agentHistSrc) &&
  /compressedUpTo: row\.CompressedUpTo/.test(agentHistSrc));

// 载入后先自愈再入缓存（中间隔着 cache 的加锁代码，用 [\s\S] 跨越）。
check('恢复后立即自愈越界状态（历史可能被回退或整体替换过）',
  /sess\.normalizeCompression\(\)[\s\S]{0,120}?h\.cache\[id\] = sess/.test(agentHistSrc));

check('⚠️ 切换模型装不下时**先压缩**，不再直接拦下来',
  /if _, err := s\.agent\.CompressNow\(r\.Context\(\), req\.SessionID\); err != nil/.test(serverModelsSrc) &&
  /已尝试压缩上下文，但仍放不进该模型的窗口/.test(serverModelsSrc));

check('⚠️ 超窗判定用压缩后的送模量，不用原始历史（否则刚压好也被拦）',
  /func \(a \*Agent\) SessionOverflowFor\(sess \*Session, ctxIn int\) int/.test(agentGoSrc) &&
  /used := sess\.calibratedEstimate\(a\.requestView\(sess\)\)/.test(agentGoSrc) &&
  /s\.agent\.SessionOverflowFor\(sess, m\.CtxIn\)/.test(serverModelsSrc));

check('提示文案里的占用是压缩后量（不误导用户）',
  /func \(a \*Agent\) RequestViewTokens\(sess \*Session\) int/.test(agentGoSrc) &&
  /humanTokens\(s\.agent\.RequestViewTokens\(sess2\)\)/.test(serverModelsSrc));

// ---------- 摘要分块：单条切开 + 失败减半（2026-09-21）----------
group('摘要分块：超大单条切开 + 单块失败减半重试');

check('⚠️ 超大单条历史按字符切开（否则单条顶满一块，减半也没用）',
  /func nextChunk\(msgs \[\]llm\.Message, idx, end, budget int, pending string\) \(string, int, string\)/.test(agentCtxSrc) &&
  /func splitByTokens\(s string, budget int\) \(string, string\)/.test(agentCtxSrc) &&
  /单条就超预算 → 切开，剩下的留给下一次/.test(agentCtxSrc));

check('⚠️ 单块摘要失败时减半重试，而不是回退机械压缩（= 丢弃历史）',
  /if chunkBudget > floor \{[\s\S]{0,120}?chunkBudget \/= 2/.test(agentCtxSrc) &&
  /continue \/\/ idx \/ pending 不前进，用更小的块重来/.test(agentCtxSrc) &&
  /不要直接截断，可以分块压缩啊/.test(agentCtxSrc));

check('收缩下限按初始预算比例算（否则预算小的时候减半一次就触底）',
  /func summarizeChunkFloor\(initial int\) int/.test(agentCtxSrc) &&
  /summaryChunkFloorDiv = 8/.test(agentCtxSrc) &&
  /summarizeChunkFloor\(chunkBudget\)/.test(agentCtxSrc));

check('用户主动打断时不重试（摘要自身超时则要重试，用父 ctx 区分）',
  /if ctx\.Err\(\) != nil \{[\s\S]{0,80}?return cur, err/.test(agentCtxSrc) &&
  /必须用\*\*父\*\* ctx 判断/.test(agentCtxSrc));

check('摘要彻底失败时仍回退机械压缩（最后的保命手段，不能卡死整轮）',
  /func \(a \*Agent\) degradedCompress/.test(agentGoSrc) &&
  /摘要不可用，已回退机械压缩/.test(agentGoSrc) &&
  /ci\.degraded/.test(uiSrc));

// ---------- 「每次启动的第一次老是炸上下文」（2026-09-21）----------
group('每次启动的第一次不该炸上下文');

check('⚠️ 校准系数落盘（不存则重启归零，第一次请求用最乐观的估算判定）',
  /ALTER TABLE sessions ADD COLUMN token_factor REAL NOT NULL DEFAULT 0/.test(storeGoSrc) &&
  /TokenFactor float64/.test(storeSessSrc) &&
  /token_factor/.test(storeSessSrc) &&
  /TokenFactor: s\.tokenFactor/.test(agentHistSrc) &&
  /tokenFactor: row\.TokenFactor/.test(agentHistSrc));

check('⚠️ 未校准时用保守系数，而不是 1.0（1.0 正是「第一次炸」的成因）',
  /uncalibratedTokenFactor = 1\.15/.test(agentCtxSrc) &&
  /f := uncalibratedTokenFactor[\s\S]{0,120}?if s\.tokenFactor > 0 \{\s*\n\s*f = s\.tokenFactor/.test(agentHistSrc));

check('⚠️ 压缩一发生就立刻落盘（中途被打断也不该丢）',
  /compressedBefore := sess\.compressedUpTo/.test(agentGoSrc) &&
  /if sess\.compressedUpTo != compressedBefore \{\s*\n\s*a\.save\(sess, persist/.test(agentGoSrc));

group('自定义背景图可见性');
check('不透明底色只挂在 html 上，body 透明',
  /html\s*\{\s*background:\s*var\(--bg\);\s*\}/.test(css) &&
  /body\s*\{\s*background:\s*transparent;\s*\}/.test(css));
check('html,body 共用规则里不再有不透明底色（否则会盖住图层）',
  !/html,\s*body\s*\{[^}]*background:\s*var\(--bg\)/.test(css));
check('#bg-layer 用**负** z-index（画在在流块背景之下，否则会盖住侧栏底色）',
  /#bg-layer\s*\{[^}]*z-index:\s*-1;/.test(css) &&
  // 写成 0 会让它晚于在流块背景绘制 → 侧栏被背景图铺满（2026-09-19 实际踩到）
  !/#bg-layer\s*\{[^}]*z-index:\s*0;/.test(css));
check('图层仍是 fixed 满铺 + pointer-events:none（不遮挡交互）',
  /#bg-layer\s*\{[^}]*position:\s*fixed;\s*inset:\s*0;/.test(css) &&
  /#bg-layer\s*\{[^}]*pointer-events:\s*none;/.test(css));
check('开启背景时左右两块面板都透明化让图铺满（含侧栏）',
  /body\.has-bg\s*\{\s*background:\s*transparent;\s*\}/.test(css) &&
  /body\.has-bg #app\s*\{\s*background:\s*transparent;\s*\}/.test(css) &&
  /body\.has-bg #main\s*\{\s*background:\s*transparent;\s*\}/.test(css) &&
  /body\.has-bg #sidebar\s*\{\s*background:\s*transparent;\s*\}/.test(css));
check('透明靠「面板自己变透明」实现，而不是让图层去盖（z-index 必须为负）',
  /#bg-layer\s*\{[^}]*z-index:\s*-1;/.test(css));
check('图层是 body 的首个子节点（DOM 顺序决定它垫在最下）',
  /document\.body\.insertBefore\(el, document\.body\.firstChild\)/.test(uiSrc));

// 「选完图啥也没有」的根因修复（2026-09-19）：
// Windows 分支只回 path 不落库 → /api/appearance/background 永远 404。
// 后端已修（pkg/server/appearance_test.go 锁住），前端再加一层兜底防复发。
group('背景图选择：不信任「已落库」，拿不到确认就自己补保存');
check('saveBg 返回 Promise（补保存要等它完成再应用）',
  /function saveBg\(patch\) \{[\s\S]{0,200}?return fetch\('\/api\/appearance'/.test(uiSrc));
check('服务端确认 background:true 时直接生效',
  /if \(d\.background\) \{ applyBgImage\(true\); return; \}/.test(uiSrc));
check('未确认时先补保存、**等它完成**再 applyBgImage（否则请求赶在落库前照样 404）',
  /saveBg\(\{ background_path: d\.path \}\)\.then\(function \(\) \{ applyBgImage\(true\); \}\)/.test(uiSrc));
check('内置选择器分支同样等保存完成再应用',
  /openBuiltinPicker\([\s\S]{0,260}?saveBg\(\{ background_path: path \}\)\.then\(function \(\) \{ applyBgImage\(true\); \}\)/.test(uiSrc));

// 模糊/亮度共用防抖定时器：两个 handler 各自 setTimeout 会互相取消，
// 导致「拖完模糊紧接着拖亮度」时模糊值永远存不进服务端（2026-09-19 实测抓到）。
group('背景模糊 / 亮度：共用防抖且提交完整状态');
check('存在统一的 scheduleBgSave（单一防抖入口）',
  /function scheduleBgSave\(\) \{[\s\S]{0,200}?clearTimeout\(bgSaveTimer\);[\s\S]{0,200}?setTimeout\(/.test(uiSrc));
check('保存时提交完整状态（blur + brightness），不是各自的单字段',
  /saveBg\(\{ blur: blur, brightness: bright \}\)/.test(uiSrc));
check('两个滑条都走 scheduleBgSave（全页只有一个防抖 setTimeout）',
  (uiSrc.match(/scheduleBgSave\(\);/g) || []).length === 2 &&
  (uiSrc.match(/bgSaveTimer = setTimeout/g) || []).length === 1);
check('不再有「只提交自己那个字段」的保存（正是它会被对方取消）',
  !/saveBg\(\{ blur: blur \}\)/.test(uiSrc) && !/saveBg\(\{ brightness: bright \}\)/.test(uiSrc));

// 深浅色自适应：背景图偏暗时文字必须转白，否则深色字压暗背景完全不可读。
// 判据是「图片实际亮度 × 亮度系数」，不是滑条值本身 —— 亮图调暗后仍可能偏亮。
group('深浅色自适应（文字永远清楚）');
check('按「图片平均亮度 × 亮度系数」判定，而不是只看滑条值',
  /BG_DARK_THRESHOLD\s*=\s*0\.4/.test(uiSrc) &&
  /const effective = bgAvgLum \* \(bgBright \/ 100\)/.test(uiSrc) &&
  /classList\.toggle\('bg-dark', effective < BG_DARK_THRESHOLD\)/.test(uiSrc));
check('平均亮度按感知权重计算（0.2126/0.7152/0.0722）',
  /0\.2126 \* d\[i\] \+ 0\.7152 \* d\[i \+ 1\] \+ 0\.0722 \* d\[i \+ 2\]/.test(uiSrc));
check('亮度按图片 URL 缓存，拖动滑条不重复解码',
  /if \(bgLumFor === url && bgAvgLum !== null\)/.test(uiSrc));
check('没有背景图时清掉 bg-dark（不误伤默认浅色主题）',
  /!bgLayer\.classList\.contains\('on'\) \|\| bgAvgLum === null[\s\S]{0,120}?classList\.remove\('bg-dark'\)/.test(uiSrc));
check('量不出亮度时退回浅色主题（不让主题卡在半路）',
  /catch \(e\) \{[\s\S]{0,80}?bgAvgLum = null;/.test(uiSrc));
check('亮度一变立即重判深浅色',
  /function applyBgFilters\(blur, bright\) \{[\s\S]{0,160}?syncBgTheme\(\)/.test(uiSrc));

check('样式契约：body.bg-dark 覆盖整套配色变量（而非逐个组件补丁）',
  /body\.bg-dark\s*\{[^}]*--bg:/.test(css) &&
  /body\.bg-dark\s*\{[^}]*--bg-elev:/.test(css) &&
  /body\.bg-dark\s*\{[^}]*--bg-elev-2:/.test(css) &&
  /body\.bg-dark\s*\{[^}]*--border:/.test(css) &&
  /body\.bg-dark\s*\{[^}]*--text:/.test(css) &&
  /body\.bg-dark\s*\{[^}]*--text-dim:/.test(css));
check('样式契约：深色下 --text 是浅色（这是「文字转白」的落点）',
  (function () {
    const m = css.match(/body\.bg-dark\s*\{[\s\S]*?\}/);
    if (!m) return false;
    const t = m[0].match(/--text:\s*#([0-9a-fA-F]{6})/);
    if (!t) return false;
    const r = parseInt(t[1].slice(0, 2), 16);
    const g = parseInt(t[1].slice(2, 4), 16);
    const b = parseInt(t[1].slice(4, 6), 16);
    return r > 200 && g > 200 && b > 200;
  })());
check('样式契约：深色下用户气泡跟着翻（否则亮粉块在暗主题里刺眼）',
  /body\.bg-dark \.msg-user \.bubble\s*\{/.test(css));
check('样式契约：声明 color-scheme 让原生控件/滚动条也转深色',
  /body\.bg-dark\s*\{[^}]*color-scheme:\s*dark/.test(css));
check('样式契约：深色规则不得改动「透明」——背景图仍要透出来',
  !/body\.bg-dark\s+[#.]?(app|sidebar|main)\s*\{[^}]*background:\s*(?!transparent)/.test(css));

// 深色主题下有两处**必须不跟着变**：
//   ① 固定浅底的代码类块 → 块内文字钉回深色（否则白字压浅底，整块一片纯白）
//   ② 设置面板 → 整屏不透明层，不坐在背景图上，不该跟着背景亮度变黑
group('深色主题的两处例外（不跟着变）');
check('固定浅底的代码块在深色下把文字钉回深色',
  /body\.bg-dark \.msg-assistant\.md pre[\s\S]{0,220}?color:\s*#1f2430/.test(css) &&
  /body\.bg-dark \.tool-card pre/.test(css) &&
  /body\.bg-dark \.diff/.test(css) &&
  /body\.bg-dark \.code/.test(css));
check('代码块底色保持写死的浅色（#f6f7f9 ×3）——用户要求不动',
  (css.match(/background:\s*#f6f7f9/g) || []).length >= 3);
check('设置面板与主界面一致：has-bg 下三层面板都透明（同步背景图）',
  /body\.has-bg \.settings-shell,\s*body\.has-bg \.settings-nav,\s*body\.has-bg \.settings-card\s*\{\s*background:\s*transparent;\s*\}/.test(css));
check('设置面板**不**单独覆盖配色变量（曾因此出现「白底 + 深色主题文字」的矛盾）',
  !/body\.bg-dark #settings-overlay\s*\{/.test(css) &&
  !/#settings-overlay\s*\{[^}]*--text:/.test(css) &&
  !/#settings-overlay\s*\{[^}]*--bg:/.test(css));
check('设置面板跟着 body.bg-dark 一起深浅自适应（不再有例外）',
  /body\.bg-dark\s*\{[^}]*--text:\s*#eef0f6/.test(css) &&
  !/body\.bg-dark[^{]*#settings-overlay[^{]*\{[^}]*--text/.test(css));
// 设置是整屏层，开着背景图时透明 → 底下的主界面会透出来重叠，必须把主界面藏起来。
check('打开设置时主界面被藏起来（否则透明的设置与主界面重叠）',
  /#app:has\(~ #settings-overlay:not\(\.hidden\)\)\s*\{\s*visibility:\s*hidden;\s*\}/.test(css));
check('藏主界面用 visibility 而非 display（保布局与滚动位置，且不接收点击）',
  !/#app:has\([^)]*settings-overlay[^)]*\)\s*\{[^}]*display:\s*none/.test(css));
check('#bg-layer 不在 #app 内（藏主界面后背景图仍铺满）',
  /document\.body\.insertBefore\(el, document\.body\.firstChild\)/.test(uiSrc));

// 实心主按钮：深色主题下 --accent 必须提亮才当得了文字，提亮后压白字就糊了
// （白字对比度掉到 2.49）—— 两处需求相反，所以单独给一个填充变量。
// ⚠️ 浅色主题下 --accent-solid 必须与 --accent **同值**：浅色外观保持原样，一点不改。
group('实心主按钮（浅色外观不变，只有深色单独给填充色）');
check('定义了 --accent-solid 与 --on-accent',
  /:root\s*\{[^}]*--accent-solid:/.test(css) && /:root\s*\{[^}]*--on-accent:/.test(css));
check('浅色主题下 --accent-solid 与 --accent 同值（浅色外观零改动）',
  (function () {
    const root = css.match(/:root\s*\{[\s\S]*?\}/);
    if (!root) return false;
    const g = (n) => {
      const m = root[0].match(new RegExp('--' + n + ':\\s*(#[0-9a-fA-F]{6})'));
      return m ? m[1].toLowerCase() : null;
    };
    const a = g('accent'), s = g('accent-solid');
    return !!a && a === s;
  })());
check('深色主题单独给了更深的填充色（不跟着 --accent 一起提亮）',
  /body\.bg-dark\s*\{[^}]*--accent-solid:/.test(css));
check('实心按钮改用 accent-solid / on-accent',
  (css.match(/background:\s*var\(--accent-solid\)/g) || []).length >= 8 &&
  !/background:\s*var\(--accent\);\s*(border-color:\s*var\(--accent\);\s*)?color:\s*#fff/.test(css));

// 思考强度指示器：浅色保持原色不动，只在深色主题里提亮。
group('思考强度指示器');
check('浅色主题保持原色 #d6336c（未改成变量、未改值）',
  /#model-level\s*\{[^}]*color:\s*#d6336c/.test(css));
check('深色主题单独提亮',
  /body\.bg-dark #model-level\s*\{[^}]*color:\s*#/.test(css));
check('深色主题下滑条档位气泡文字钉回深色（body.bg-dark .fs-tip）',
  /body\.bg-dark\s+\.fs-tip\s*\{\s*color:\s*#1f2430;?\s*\}/.test(css));

// ⚠️ 用户明确要求：**不要动浅色主题的配色**。
// 之前我擅自按 WCAG 把这几个值都压暗了，被要求全部撤回 —— 这组断言锁住原值。
group('浅色主题配色保持原值（用户要求不得改动）');
check('--text-dim / --accent / --accent-dim / --ok / --warn / --danger 均为原值',
  (function () {
    const root = css.match(/:root\s*\{[\s\S]*?\}/);
    if (!root) return false;
    const want = {
      'text-dim': '#6b7280', 'accent': '#4373f5', 'accent-dim': '#e8eefc',
      'ok': '#16a34a', 'warn': '#d97706', 'danger': '#dc2626',
    };
    return Object.keys(want).every((n) => {
      const m = root[0].match(new RegExp('--' + n + ':\\s*(#[0-9a-fA-F]{6})'));
      return m && m[1].toLowerCase() === want[n];
    });
  })());
check('没有给文字加 text-shadow（会产生重影，用户明确否掉）',
  !/text-shadow/.test(css));

group('输入框外观：黑 / 白 / 透明（2026-09-27）');

check('设置 → 外观 有「输入框外观」三档分段控件',
  /id="composer-seg"/.test(htmlSrc) &&
  /data-cbox="black"[^>]*>黑色</.test(htmlSrc) &&
  /data-cbox="white"[^>]*>白色</.test(htmlSrc) &&
  /data-cbox="clear"[^>]*>透明</.test(htmlSrc));

check('没有「跟随主题」这一档（用户明确只要三档）',
  !/data-cbox="(auto|theme|default)"/.test(htmlSrc) &&
  !/跟随主题/.test(htmlSrc));

check('黑/白两档在卡片上重映射主题变量（不是逐个覆盖后代颜色）',
  // 逐个枚举后代必然漏一处 —— 漏的那处就是白底白字。
  // 变量重映射让 .input-mirror / placeholder / @提及 / icon-btn / send-btn 一起变。
  /#composer\.cbox-black\s*\{[\s\S]*?--text:\s*#eef0f6/.test(css) &&
  /#composer\.cbox-white\s*\{[\s\S]*?--text:\s*#1f2430/.test(css));

check('两档都重映射了 --text-dim 与 --accent（@提及高亮跟着变）',
  /#composer\.cbox-black\s*\{[\s\S]*?--text-dim:[\s\S]*?--accent:/.test(css) &&
  /#composer\.cbox-white\s*\{[\s\S]*?--text-dim:[\s\S]*?--accent:/.test(css));

check('黑/白档都重映射了 --bg-elev-2（#composer 的底色取的就是它）',
  /#composer\.cbox-black\s*\{[\s\S]*?--bg-elev-2:[\s\S]*?\}/.test(css) &&
  /#composer\.cbox-white\s*\{[\s\S]*?--bg-elev-2:[\s\S]*?\}/.test(css) &&
  /#composer\s*\{[\s\S]*?background:\s*var\(--bg-elev-2\)/.test(css));

check('实心按钮填充用 --accent-solid 而非 --accent（白字对比度）',
  /#composer\.cbox-black\s*\{[\s\S]*?--accent-solid:[\s\S]*?--on-accent:/.test(css));

check('透明档只去掉填充、不重映射任何变量（文字继续跟随主题）',
  /#composer\.cbox-clear\s*\{\s*background:\s*transparent;\s*\}/.test(css) &&
  !/#composer\.cbox-clear\s*\{[^}]*--/.test(css));

check('黑档补了黑投影（原浅色阴影落在近黑底上等于没有）',
  /#composer\.cbox-black\s*\{[\s\S]*?box-shadow:[\s\S]*?rgba\(0,\s*0,\s*0/.test(css));

check('JS 用白名单校验后落回默认，不给 #composer 挂野 class',
  /const CBOX_STYLES = \['black', 'white', 'clear'\]/.test(uiSrc) &&
  /CBOX_STYLES\.indexOf\(v\) >= 0 \? v : CBOX_DEFAULT/.test(uiSrc));

check('默认黑色，且写入 localStorage（不落服务端）',
  /const CBOX_DEFAULT = 'black'/.test(uiSrc) &&
  /localStorage\.setItem\('cf_composer_box'/.test(uiSrc) &&
  /fetch\('\/api\/appearance'[\s\S]{0,80}cf_composer_box/.test(uiSrc) === false);

check('初始化时就应用样式（不是只挂 handler 等点按钮才生效）',
  // 与液态玻璃 initLiquidGlass 同形：进入即读 localStorage 并落到 DOM 上，
  // 所以页面加载完第一眼就是选中的外观，不会先闪一下默认样式。
  /\(function initComposerBox\(\)\s*\{\s*const seg[\s\S]{0,200}?applyComposerBox\(localStorage/.test(uiSrc));

console.log('\n' + '-'.repeat(52));
if (failures.length) {
  console.log('失败 ' + failures.length + ' 项 / 通过 ' + passed + ' 项：');
  failures.forEach(function (f) { console.log('  - ' + f); });
  process.exit(1);
}
console.log('全部通过（' + passed + ' 项断言）');
