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

// messagesEl 上挂了多个 scroll 监听（minimap 等），要挑**含 composerHide 的那个**。
// 直接取第一个会锁到不相干的监听上，断言变成永远为真。
function composerScrollHandler() {
  const src = fs.readFileSync(UI_PATH, 'utf8');
  const key = "messagesEl.addEventListener('scroll', ";
  let from = 0, hit = -1;
  while (hit < 0) {
    const at = src.indexOf(key, from);
    if (at < 0) throw new Error('ui.js 里找不到输入卡折叠的 scroll 监听');
    const end = src.indexOf('\n  });', at);
    if (src.slice(at, end).includes('composerHide')) hit = at;
    from = at + 1;
  }
  return src.slice(hit, src.indexOf('\n  });', hit) + 5);
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
// 断言「代码里没有 X」时必须先剥掉注释 —— 注释里常常正好在解释那个被删掉的旧写法
// （「别写 calc(100dvh - Nxx)」「原先这里是 min-height:420px」），不剥就会把注释当成残留。
const cssCode = css.replace(/\/\*[\s\S]*?\*\//g, '');
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
const goalSrc = goFile('pkg/tools/goal.go');
const toolsGoalSrc = goalSrc;
const toolsBuiltinSrc = goFile('pkg/tools/builtin/goal.go');
const executorSrc = goFile('pkg/tools/executor.go');
const configSrc = goFile('config/config.go');
const mainSrc = goFile('cmd/agent/main.go');
const bpHandlerSrc = goFile('pkg/server/builtin_plugins_handler.go');
const bpSrc = goFile('pkg/agent/builtin_plugins.go');
const goalRunnerSrc = goFile('pkg/agent/goal_runner.go');
const roleSrc = goFile('pkg/tools/role.go');

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

// ---------- 6.0 侧栏高度链：项目列表必须是滚动容器（2026-09-29） ----------
// 历史 bug：#session-list 只是个普通的 flex 子项 —— 没有 flex、没有 min-height:0、
// 也没有 overflow-y:auto，于是它**不是滚动容器**。项目一多，内容顶破 #sidebar，
// 而 #app 是 overflow:hidden，溢出部分被视口边缘硬裁掉：内容还在，但滚不到
// （表现 = 左栏拉不动、底部设置按钮被一起顶出屏幕）。
// 三件套缺一不可：flex 给它高度、min-height:0 允许收缩（flex 子项默认 auto，
// 不置 0 永远不缩）、overflow-y:auto 让溢出变成滚动而不是裁剪。
group('侧栏滚动：项目列表');
check('项目列表被滚动容器包住（标签留在外面固定，只有列表内部滚）',
  /<div class="sidebar-scroll">\s*<ul id="session-list" class="session-list"><\/ul>\s*<\/div>/.test(htmlSrc));
check('滚动容器三件套齐全：flex 分配高度 + min-height:0 允许收缩 + overflow-y:auto',
  /\.sidebar-scroll\s*\{[^}]*flex:\s*1 1 0[^}]*min-height:\s*0[^}]*overflow-y:\s*auto/.test(css));
check('#sidebar 自身也允许被压扁（min-height:0，否则内部仍会溢出）',
  /#sidebar\s*\{[^}]*flex-direction:\s*column[^}]*min-height:\s*0/.test(css));
check('不再留没有挂载点的旧滚动规则（.sidebar-body 从未被任何元素使用）',
  /\.sidebar-scroll\s*\{/.test(css) && !/\.sidebar-body\s*\{/.test(css) &&
  !/sidebar-body/.test(htmlSrc) && !/sidebar-body/.test(uiSrc));
check('拉条藏掉但滚动能力保留（Firefox / Chromium / 旧 Edge 三条都要覆盖）',
  /\.sidebar-scroll\s*\{[^}]*scrollbar-width:\s*none/.test(css) &&
  /\.sidebar-scroll\s*\{[^}]*-ms-overflow-style:\s*none/.test(css) &&
  /\.sidebar-scroll::\-webkit-scrollbar\s*\{\s*width:\s*0;\s*height:\s*0;\s*\}/.test(css));
check('没有对 #sidebar 加 overflow:hidden（会裁掉底部栏里向上弹的 .ctx-pop）',
  !/#sidebar\s*\{[^}]*overflow:\s*hidden/.test(css));
check('列表本身不再自带高度/滚动约束（那属于外层滚动容器的职责）',
  !/^\.session-list\s*\{[^}]*overflow/.test(css) && !/^\.session-list\s*\{[^}]*min-height/.test(css));

// 侧栏内部按「左右大分栏」的手法再分两卡：MCP 一张、项目一张，
// 缝隙里露出 #sidebar 的底色，隔离带是缝中间那根圆角手柄。
// 不是画一条横线 —— 上下两块必须读成两张卡片（用户明确要求「类似左右大分栏的处理方式」）。
group('侧栏分区：MCP 与项目各成一张卡');
check('MCP 区被 .sidebar-card 包住',
  /<div class="sidebar-card">\s*<div class="sidebar-section-label" id="mcp-entry">[\s\S]{0,700}?id="mcp-list"[\s\S]{0,60}?<\/div>/.test(htmlSrc));
check('项目区被 .sidebar-card-main 包住，且滚动区在它里面',
  /<div class="sidebar-card sidebar-card-main">[\s\S]{0,900}?id="new-chat-btn"[\s\S]{0,900}?id="sessions-entry"[\s\S]{0,400}?class="sidebar-scroll"/.test(htmlSrc));
check('卡片四件套齐全：自带底色 + 1px 边 + 圆角 + 内边距（与 #sidebar/#main 同构）',
  /\.sidebar-card\s*\{[^}]*background:\s*var\(--bg\)[^}]*border:\s*1px solid var\(--border\)[^}]*border-radius:\s*12px[^}]*padding:\s*8px/.test(css) &&
  /\.sidebar-card\s*\{[^}]*min-height:\s*0/.test(css));
check('卡片底色比缝隙深一档（照搬 #app 衬底 / 面板的明暗关系）',
  /#app\s*\{[^}]*background:\s*var\(--bg-elev-2\)/.test(css) &&
  /#sidebar\s*\{[^}]*background:\s*var\(--bg-elev\)/.test(css) &&
  /\.sidebar-card\s*\{[^}]*background:\s*var\(--bg\);/.test(css));
check('开启背景图时两张小卡一起透明化（否则白卡盖在图上，与左右两栏不一致）',
  /body\.has-bg \.sidebar-card\s*\{\s*background:\s*transparent;/.test(css));
check('项目卡吃掉侧栏剩余高度，滚动区才能在它内部收缩',
  /\.sidebar-card-main\s*\{[^}]*flex:\s*1 1 auto[^}]*min-height:\s*0/.test(css));
check('隔离带落在两张卡之间（不是画一条横线）',
  /id="mcp-list"[\s\S]{0,700}?class="sidebar-sep"[\s\S]{0,700}?class="sidebar-card sidebar-card-main"[\s\S]{0,700}?id="new-chat-btn"/.test(htmlSrc));
check('隔离带不可聚焦也不进无障碍树（纯装饰）',
  /class="sidebar-sep" role="separator" aria-hidden="true"><span class="grip"><\/span><\/div>/.test(htmlSrc));
check('隔离带复用 #drag-handle 的构造：自身透明 + 正中一根手柄',
  /\.sidebar-sep\s*\{[^}]*background:\s*transparent[^}]*align-items:\s*center[^}]*justify-content:\s*center/.test(css) &&
  /\.sidebar-sep \.grip\s*\{/.test(css));
check('手柄两端收圆角，且横放（横向分隔；#drag-handle 的 .grip 是竖的）',
  /\.sidebar-sep \.grip\s*\{[^}]*width:\s*28px[^}]*height:\s*3px[^}]*border-radius:\s*3px/.test(css));
check('负外边距抵消 #sidebar 的 gap，卡间距落到 6px（与 #app 的 padding 对齐）',
  /\.sidebar-sep\s*\{[^}]*height:\s*6px[^}]*margin:\s*-10px 0/.test(css) &&
  /#sidebar\s*\{[^}]*gap:\s*10px/.test(css) &&
  /#app\s*\{[^}]*padding:\s*6px/.test(css));
check('隔离带不参与高度分配、也不假装可拖（不复制 #drag-handle 的 hover/光标）',
  /\.sidebar-sep\s*\{[^}]*flex:\s*0 0 auto/.test(css) &&
  !/\.sidebar-sep:hover/.test(css) && !/\.sidebar-sep\s*\{[^}]*cursor:\s*col-resize/.test(css));

// ---------- 6.05 左栏 MCP 只报启用数量（2026-09-29） ----------
// 早先这里逐个列 MCP 服务名。侧栏只有 240px，几个名字就把下面的项目列表顶没了，
// 而项目列表才是左栏主体。改成一行「已启用 N 个」，明细（谁在跑 / 谁加载失败 /
// 端点）挪进这一行的 title：悬停看得到，不占垂直空间。
group('左栏 MCP：只报启用数量');
const mcpListFn = extractFunction(uiSrc, 'renderMcpList');
check('只渲染一行汇总（不再 items.forEach 逐个建 li）',
  /nm\.textContent = '已启用 ' \+ on\.length \+ ' 个'/.test(mcpListFn) &&
  !/items\.forEach\(function \(p\)/.test(mcpListFn));
check('计数口径 = configured 的 MCP 插件里 enabled 的个数（与运行态同源）',
  /const on = items\.filter\(function \(p\) \{ return p\.enabled; \}\)/.test(mcpListFn) &&
  /p\.type === 'mcp' \|\| p\.type === 'mcp-http'\) && p\.configured/.test(mcpListFn));
check('圆点跟着计数走 on/off（复用既有配色：全绿=都在跑，全灰=一个都没起来）',
  /li\.className = 'mcp-count ' \+ \(on\.length \? 'on' : 'off'\)/.test(mcpListFn) &&
  /\.mcp-list li\.on \.dot\s*\{/.test(css) && /\.mcp-list li\.off \.dot\s*\{/.test(css));
check('明细保留在 title 里（启用数 + 加载失败数 + 逐条端点），不静默丢掉',
  /li\.title = '共 ' \+ items\.length/.test(mcpListFn) &&
  /items\.map\(function \(p\) \{ return \(p\.enabled \? '● ' : '○ '\) \+ mcpTitle\(p\); \}\)/.test(mcpListFn) &&
  /设置 → MCP 服务/.test(mcpListFn));
check('未配置时仍是「尚未添加」提示（0 个不能显示成「已启用 0 个」）',
  /if \(!items\.length\) \{\s*\n\s*list\.innerHTML = '<li class="mcp-empty"[^>]*>尚未添加/.test(mcpListFn));
check('MCP 汇总行不参与高度分配（固定一行，不许被压缩）',
  /\.mcp-list\s*\{[^}]*flex:\s*none/.test(css));
check('汇总行样式：有服务在跑时用正文色，数字等宽防刷新抖动',
  /\.mcp-list li\.mcp-count\s*\{[^}]*color:\s*var\(--text\)/.test(css) &&
  /\.mcp-list li\.mcp-count \.mcp-n\s*\{[^}]*font-variant-numeric:\s*tabular-nums/.test(css));

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
check('设置列表用显示名，标识退到小字；侧栏明细同源（共用 mcpTitle）',
  // 侧栏早先逐个列服务名，现已收成一行汇总（明细在 title 里），所以「侧栏那一半」
  // 的契约改为：mcpTitle 里显示名在前、标识进括号，两者不再各写一份文案。
  /function mcpTitle\(p\)/.test(uiSrc) &&
  /p\.display_name \+ '（' \+ p\.name \+ '）'/.test(extractFunction(uiSrc, 'mcpTitle')) &&
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
  /if \(anyRunning\(\)\) \{[\s\S]{0,400}?confirm\(/.test(uiSrc) &&
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
  // 窗口放宽：busy 分支里 2026-09-29 加了 runSessionId 取服务端 session_id 的说明
  // （并行多会话必需），把 optimisticBubble 推出了 700 字符窗口 —— 那是断言脆，
  // 不是行为变了。用 uiCase 取整个分支，不再数窗口。
  uiCase('busy').includes('optimisticBubble = null;'));
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
  /if \(ev\.session_id && !mine && ev\.session_id !== sessionID\)/.test(uiSrc) &&
  /另一个会话的历史更新已忽略/.test(uiSrc));
check('请求触发的那一帧必须放行（切会话就靠它）',
  /if \(mine\) awaitingHistoryFor = null;\s*\n\s*pendingEdit = false;\s*\n\s*replayHistory\(ev\);/.test(uiSrc));
check('老服务端不带 session_id 时按原样放行（不因升级卡住）',
  /if \(ev\.session_id &&/.test(uiSrc));
// ⚠️ 下面两条是这个守卫真正被咬到的地方（2026-09 反馈：同项目两个会话不能并发，
// 后台那个会话里凭空多出一条「转向」消息）。
//
// 原来 awaitingHistoryFor 是**无条件**清空的，而守卫的第二个条件
// 「ev.session_id === 当前视图」会放行「当前视图会话自己」的非请求帧
// （编辑重发/断点重试截断后服务端补的那一帧，ws_handler.go 的 EventEdit 分支）。
// 于是：视图停在 A 且 A 正在编辑重发 → 用户点 B（awaitingHistoryFor=B）→
// history(A) 到达并被放行，顺手把 awaitingHistoryFor 清空 → 随后真正的
// history(B) 因「已无待认领」而被判成别的会话的推送丢弃 → sessionID 停在 A →
// 用户以为在 B 发的消息被 steerNow 判成转向灌进 A。
//
// 修法：只有**确实认领**的那一帧（mine）才能清；被拒绝的帧也不能清
// ——它不是回包，清掉等于凭空取消用户的切换。
check('只有认领了的那一帧 history 才能清 awaitingHistoryFor',
  /const mine = !!ev\.session_id && awaitingHistoryFor === ev\.session_id;/.test(uiSrc) &&
  /if \(mine\) awaitingHistoryFor = null;/.test(uiSrc));
check('被拒绝的 history 帧不得清 awaitingHistoryFor（否则凭空取消用户的切换）',
  /另一个会话的历史更新已忽略（当前视图未切换）'\);\s*\n\s*break;\s*\n\s*\}\s*\n\s*if \(mine\) awaitingHistoryFor = null;/.test(uiSrc));
// 上一条把「误清」堵住了，但把风险挪到了另一头：awaitingHistoryFor 一旦残留成
// 一个再也等不到回包的 id，将来某一帧**非请求**的同会话 history 会被当成
// 「我请求的回包」放行，把整屏拽到那个会话去。彻底无人回包的路径必须清干净。
check('断线必须清 awaitingHistoryFor（回包永远不会来了）',
  /addEventListener\('close',[\s\S]{0,400}?awaitingHistoryFor = null;/.test(uiSrc));
// 但 error 帧**不能**无条件清 —— 它也承载后台会话的报错（见 case 'error' 的
// runAway 分支）。清了的话，用户点着 B 而 A 恰好报错，就会把待认领的 id 清掉，
// history(B) 随后被判成「别的会话的推送」丢弃，sessionID 留在 A：
// 本次修的 bug 换条路径复发。这条断言是防这个回退的。
check('error 帧不得清 awaitingHistoryFor（后台会话报错也会走这一支）',
  !uiCase('error').includes('awaitingHistoryFor = null;'));
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
  const clear = submit.indexOf("setInputValue('');");
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
check('程序化改动走 setInputValue 单一出口（发出后清空 / @面板 / ＋菜单 / 转向）',
  // 2026-09-29：拆出 setInputValue 之前，这些点各自写 `input.value = X; syncInputMirror()`，
  // 少调一次 syncSendBtn 就是「打完字按钮还停在打断态」。断言锁住出口 + 无残留裸写。
  /function setInputValue\(v\) \{\s*\n\s*input\.value = v;\s*\n\s*syncInputMirror\(\);\s*\n\s*syncSendBtn\(\);/.test(uiSrc) &&
  /if \(override === undefined\) setInputValue\(''\);/.test(uiSrc) &&
  /setInputValue\(before \+ '@' \+ token \+ ' ' \+ after\)/.test(uiSrc) &&
  /setInputValue\(before \+ sep \+ ins \+ after\)/.test(uiSrc) &&
  // 除了 setInputValue 自身，源码里不该再有裸的 input.value = … 紧跟 syncInputMirror
  !(uiSrc.replace(/function setInputValue\(v\) \{[\s\S]*?\n  \}/, '')
    .match(/input\.value = [^;]+;\s*\n\s*syncInputMirror\(\)/)));

// ---------- 9. 发送 / 打断按钮：状态与实际点击后果一致 ----------
group('发送按钮三态（打断 / 发送 / 无内容变暗）');
check('运行中的打断按钮为危险色（区别于发送按钮的 accent）',
  // 发送按钮是「实心 accent + 白字」→ 用 accent-solid（白字对比度才够）；
  // 断言意图不变：它仍与 danger 区分开、也不该退化成 text-dim。
  /#send-btn\s*\{[^}]*background:\s*var\(--accent-solid\)/.test(css) &&
  /#send-btn\.running\s*\{[^}]*background:\s*var\(--danger\)/.test(css) &&
  !/#send-btn\.running\s*\{[^}]*background:\s*var\(--text-dim\)/.test(css));
check('三态判定与 submitMessage 的实际分支一一对应',
  // 提交分支：当前视图在跑 && 有字 → steerNow（只转向**当前会话**）；
  // 当前视图在跑 && 无字 → cancel（打断，同样只针对当前会话）；
  // 否则 raw 为空直接 return。按钮显示的必须是「点下去会发生什么」。
  //
  // ⚠️ 必须用 viewRunning() 而不是「本连接有任务在跑」：别的会话在后台跑时，
  // 当前会话并没有任务在跑，输入框里的字是**新消息**，不能被改道成转向。
  // 少了这个区分就会出现「B 会话的消息跑进 A 会话」。
  /const stop = viewRunning\(\) && !hasText;/.test(uiSrc) &&
  /const dim = !viewRunning\(\) && !hasText;/.test(uiSrc) &&
  /if \(viewRunning\(\)\) \{[\s\S]{0,400}?steerNow\(\)/.test(uiSrc) &&
  /type: 'cancel', session_id: sessionID/.test(uiSrc) &&
  /const raw = String\([^)]*\)\.trim\(\);\s*\n\s*if \(!raw\) return;/.test(uiSrc));
check('单一定态出口：改 running / 写 input.value 都走 syncSendBtn',
  /function syncSendBtn\(\)/.test(uiSrc) &&
  // 不允许再各自 add/remove class —— 早先 3 处各改各的，漏一处就与实际行为不符
  !/sendBtn\.classList\.(add|remove)\('(running|dim)'\)/.test(uiSrc) &&
  (uiSrc.match(/syncSendBtn\(\)/g) || []).length >= 6);
check('无内容时按钮变暗，且 hover 不再提亮成「能点」',
  /#send-btn\.dim\s*\{[^}]*background:\s*var\(--bg-elev\)[^}]*cursor:\s*default/.test(css) &&
  /#send-btn\.dim:hover \{ filter: none; \}/.test(css));

// ---------- 8.5 并发会话的视图隔离 ----------
//
// 这一整组是 2026-09 加的，起因：同项目两个会话并发时，一个会话的输出整段
// 出现在另一个会话里，两屏内容互相覆盖。根因是视图状态是**单槽**的
// （running / runSessionID / runText / runReason），而服务端按会话分运行槽、
// 允许多个会话同时跑 —— 单任务时代码里两套概念恰好等价，并发时立刻分叉。
group('并发会话：帧级归属判定（不再靠单槽代理）');
check('frameIsMine 是唯一的归属判据：按**这一帧**的 session_id 比',
  /function frameIsMine\(ev\) \{/.test(uiSrc) &&
  /return !ev\.session_id \|\| ev\.session_id === sessionID;/.test(uiSrc));
check('删除单槽代理 runAway()（它问的是「槽在不在视图」，不是「这帧是不是我的」）',
  // 允许注释里提到它（那里正是在解释为什么删），但不许再有定义或调用。
  !/function runAway\(/.test(uiSrc) &&
  !/[^/]\brunAway\(\)/.test(uiSrc.replace(/^\s*\/\/.*$/gm, '')));
check('每个会改视图的 case 都必须先过 frameIsMine',
  // 结构断言，防止以后再漏一个 case —— 这次的根因正是「一半 case 有守卫、
  // 一半没有」，靠人肉 review 拦不住。
  ['retry', 'steer', 'compress', 'reasoning', 'text', 'tool_pending',
   'tool_call', 'subagent', 'tool_result', 'rewind', 'busy']
    .every(function (c) { return uiCase(c).includes('frameIsMine(ev)'); }));
check('busy 帧：先按归属分流，后台会话的 busy 一行视图状态都不许碰',
  // 原来 busy 无守卫却做了 15 件事（清 runText / lastReply / optimisticBubble、
  // resetSubagentCards / expireApprovals / settleActiveTool / showThinking…），
  // 别的会话一起跑，它会把当前这一屏正在流的内容清掉。
  uiCase('busy').indexOf('frameIsMine(ev)') >= 0 &&
  uiCase('busy').indexOf('frameIsMine(ev)') <
  uiCase('busy').indexOf('resetSubagentCards()') &&
  uiCase('busy').indexOf('frameIsMine(ev)') < uiCase('busy').indexOf('showThinking()'));
check('idle 帧：先摘运行态，再按归属决定要不要做 DOM 收尾',
  uiCase('idle').includes('runningSessions.delete(') &&
  uiCase('idle').indexOf('runningSessions.delete(') < uiCase('idle').indexOf('clearRunVisuals()'));
check('session 帧：只认本视图的，别把用户拽去别的会话',
  // 原来判据是「没在跑就照单全收」：视图停在 A、B 起跑发来 session(B)，
  // 视图就被拽到 B。合法场景只有两个 —— 新建会话（sessionID 为空）
  // 与本会话自己的启动回报。
  /if \(ev\.session_id && \(ev\.session_id === sessionID \|\| sessionID === ''\)\)/.test(uiSrc));
check('流式片段按会话分桶，切回正在跑的会话不丢最后一截',
  /const streams = new Map\(\)/.test(uiSrc) &&
  /function streamOf\(/.test(uiSrc) &&
  uiCase('text').includes('streamOf(') &&
  uiCase('reasoning').includes('streamOf(') &&
  extractFunction(uiSrc, 'replayHistory').includes('streams.get('));
check('运行态是集合而不是单个 id（单 id 记不住「同时有两个在跑」）',
  /let runningSessions = new Set\(\)/.test(uiSrc) &&
  !/let runSessionID = /.test(uiSrc) &&
  !/^  let running = false;$/m.test(uiSrc));
check('两种运行态语义必须分开：当前视图在跑 vs 本连接有任务在跑',
  // 混用就会出现「别的会话在跑，当前会话被当成在跑」——输入框里的新消息
  // 被改道成转向，正是最初那个症状。
  /function viewRunning\(\)/.test(uiSrc) &&
  /function anyRunning\(\)/.test(uiSrc) &&
  /runningSessions\.has\(sessionID\)/.test(uiSrc));
check('侧栏转圈按会话查集合（两个会话可以同时转）',
  /runningSessions\.has\(s\.id\)\) addRunBadge\(li\)/.test(uiSrc) &&
  /const on = runningSessions\.has\(li\.dataset\.id\);/.test(uiSrc));
check('打断提示随「当前视图是否在跑」变化',
  // viewRunning() 比的是 sessionID，所以必须在 sessionID 换掉之后才刷新
  /sessionID = ev\.session_id \|\| '';[\s\S]{0,200}?syncSendBtn\(\);/.test(uiSrc) &&
  // 打断态只由「当前视图在跑」决定：别的会话在后台跑时不该把本会话的
  // 发送键变成停止方块（点下去会变成打断一个不存在的任务）。
  /const stop = viewRunning\(\) && !hasText;/.test(uiSrc) &&
  /!viewRunning\(\) && !hasText/.test(uiSrc) &&
  !/if \(running\) sendBtn\.title/.test(uiSrc));

// ---------- 10. 任务等待提示：模型响应前 / 工具返回后 ----------
group('任务等待提示');
check('等待文案为「等待模型响应」',
  /等待模型响应/.test(uiSrc) && !/innerHTML = '思考中/.test(uiSrc));
check('用户发送后立即显示等待提示',
  // 窗口放宽：addUser 与 showThinking 之间新增了「记下乐观气泡」的注释（2026-09-27）。
  /addUser\(p\.display\);[\s\S]{0,700}?showThinking\(\)/.test(uiSrc));
check('busy 事件显示等待提示',
  uiCase('busy').includes('showThinking()'));
check('tool_result 后模型再次等待时显示提示',
  // 窗口放宽：tool_result 分支里现在多了失败判定的注释（2026-09-27），
  // 350 字符刚好卡在边界上，改动无关的行就会假失败。
  /case 'tool_result':[\s\S]{0,900}?if \(viewRunning\(\)\) showThinking\(\)/.test(uiSrc));
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

// ---------- 10. ＋ 更多菜单：添加文件 / Skills和技能（右展） ----------
// 点 ＋ → 图标变 ✕ + 上拉菜单；「添加文件」按平台分流（Windows 资源管理器 / 其他内置选择器），
// 「Skills和技能」向右展开**两类**：工作区已加载的技能（插 @技能名）+ 内置插件（插 @id）。
group('＋ 更多菜单：添加文件 / Skills和技能');
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
// 菜单名必须写全：这个二级菜单同时列技能（SKILL.md）和内置插件（@id 触发词），
// 只写「Skills」会让人以为里面只有 SKILL.md，进去发现还有 Plan 之类就以为点错了。
check('菜单名为「Skills和技能」（子菜单确实两类都列）',
  /id="more-skills"[\s\S]{0,200}?<span>Skills和技能<\/span>/.test(htmlSrc) &&
  !/<span>Skills<\/span>/.test(htmlSrc));
check('子菜单两类都列：技能（@slug）+ 内置插件（@id）',
  /fetch\('\/api\/skills'\)/.test(extractFunction(uiSrc, 'renderMoreSkills')) &&
  /fetch\('\/api\/builtin-plugins'\)/.test(extractFunction(uiSrc, 'renderMoreSkills')) &&
  /insertIntoInput\('@' \+ sk\.name\)/.test(uiSrc));
check('Skills和技能 子菜单向右展开',
  /popup popup-right/.test(htmlSrc) && /\.popup\.popup-right\s*\{[^}]*left:\s*calc\(100% \+/.test(css));
check('添加文件按平台分流：Windows 资源管理器 / 其他内置选择器',
  /fetch\('\/api\/pick_file'/.test(uiSrc) &&
  /if \(res\.data\.builtin\)/.test(uiSrc) &&
  /openBuiltinPicker\(res\.data\.start_path \|\| '', 'file', insertPickedFile\)/.test(uiSrc));
check('内置选择器支持文件模式（文件可点选 + 确认键禁用态）',
  /pickerMode === 'file' \? '选择此文件' : '选择当前目录'/.test(uiSrc) &&
  /okBtn\.disabled = pickerMode === 'file'/.test(uiSrc));
// ⚠️ 换目录时重置确认键的那一行必须**单独**钉住，且要限定在 browse() 内。
//
// 原因：打开时的复位和换目录时的复位是**两处独立赋值**，只断言「文件里有
// okBtn.disabled = pickerMode === 'file'」的话，打开那一行就能把断言喂饱，
// browse() 里写成反的 `!== 'file'` 完全测不出来（2026-09 漏网即此）。
//
// 写反的后果只在 dir 模式显形：dir 模式下 `pickerMode !== 'file'` 恒为 true，
// 于是「选择当前目录」在选择器弹出的第一帧就被置灰且再也点不开 ——
// 非 Windows 平台（Linux/macOS/Termux）选工作区全废，Windows 走系统对话框
// 免疫，所以只在 Termux 上被看见。file 模式的反向错误（换目录后误放开）被
// onPick 里 `if (!pickerSel) return;` 兜住，同样长期无人察觉。
check('换目录重置确认键：只在 file 模式禁用，dir 模式必须始终可点',
  /okBtn\.disabled = pickerMode === 'file'/.test(extractFunction(uiSrc, 'browse')));
check('确认键禁用条件不得写成反向比较（dir 模式会被置灰点不开）',
  !/okBtn\.disabled = pickerMode !==/.test(uiSrc) &&
  !/okBtn\.disabled = !pickerMode/.test(uiSrc));
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
check('技能点选插入 @技能名（复用 @ 提及语义）',
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
  // 2026-09-29：模型列表与供应商详情数据同源，切到任一子 Tab 都走 loadModelViews()
  // （一次取数 + 重绘两个视图），不再分别调各自的 render。
  /name === 'config'\)[\s\S]{0,400}?\} else \{\s*loadModelViews\(\)/.test(uiSrc) &&
  /function loadModelViews\(\)[\s\S]{0,120}?loadLibrary\(\)\.then\(renderModelViews\)/.test(uiSrc));
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
  // 2026-09-29：收敛到 refreshModelViews()——它内部置 modelsLoaded=false 并调
  // loadModels()，所以「新条目出现在对话框模型选择里」这个语义由出口保证。
  /showMTab\('list'\);[\s\S]{0,200}?refreshModelViews\(\);/.test(uiSrc) &&
  /function refreshModelViews\(\)[\s\S]{0,200}?return loadModels\(\)/.test(uiSrc));
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

// ---------- 模型多模态能力（vision / video）：三态，不是布尔 ----------
//
// 「不声明」与「不支持」混为一谈是这个功能最容易出的错，且症状极隐蔽：
// 用户点错一次 → 该模型永久收不到图片 → 界面上看不出任何异常。
// 所以这三条钉的是「三态存在」与「留空时不下发字段」。
check('多模态能力是三态分段（不声明 / 支持 / 不支持），不是两个复选框',
  /capState = \{ vision: 'unset', video: 'unset' \}/.test(uiSrc) &&
  /value === true \? 'yes' : value === false \? 'no' : 'unset'/.test(uiSrc) &&
  /v === 'yes'\) return true;[\s\S]{0,80}?v === 'no'\) return false;[\s\S]{0,40}?return null; \/\/ 不声明/.test(uiSrc) &&
  /id="mf-vision-seg"/.test(htmlSrc) && /id="mf-video-seg"/.test(htmlSrc) &&
  /data-cap="unset"[^>]*>不声明</.test(htmlSrc) &&
  /data-cap="yes"[^>]*>支持</.test(htmlSrc) &&
  /data-cap="no"[^>]*>不支持</.test(htmlSrc));
check('留「不声明」时不下发 vision/video 字段（而非下发 false）',
  // 服务端把「没写」读成三态里的 unknown；下发 false 会被读成「明确不支持」，
  // 于是模型永久收不到图，而用户没有任何入口纠正。
  /const vision = getCap\('vision'\);[\s\S]{0,120}?if \(vision !== null\) f\.vision = vision;/.test(uiSrc) &&
  /if \(video !== null\) f\.video = video;/.test(uiSrc));
check('表单回填按三态还原（下发 true/false/null 各落到对应档位）',
  /setCap\('vision', m\.vision\);/.test(uiSrc) && /setCap\('video', m\.video\);/.test(uiSrc) &&
  /setCap\(which, b\.dataset\.cap === 'yes' \? true : b\.dataset\.cap === 'no' \? false : null\)/.test(uiSrc));
check('模型列表只给「显式声明支持」加徽标（未声明不加）',
  // 未声明是绝大多数，全标出来等于没标；而用户判断「贴截图它看没看见」
  // 只能看这个徽标。
  /\[\['vision', '图'\], \['video', '视频'\]\]/.test(uiSrc) &&
  /if \(m\[pair\[0\]\] === true\)/.test(uiSrc));
check('能力文案说清「视频是原生解析，不是本地抽帧」',
  /原生解析（整段视频直接交给上游），不是本地抽帧/.test(htmlSrc));

// ---------- 模型管理页：tab 条独占一行且居中；供应商管理收敛成一屏 ----------
group('模型管理页：tab 条独占一行且居中');
check('HTML 里有独立的 tab 条行，里面只有那颗 pill',
  /<div class="models-tabs-bar">\s*<div class="models-tabs">[\s\S]{0,400}?<\/div>\s*<\/div>/.test(htmlSrc));
// ⚠️ 契约：**不许**做 sticky。悬浮在内容之上会盖住从下方滚过去的文字
//（用户反馈：「不能覆盖到下面的滚动文字」）。它只是普通的一行，
// 下面内容从这一行的下沿开始排。
check('tab 条不悬浮（无 sticky / 无 z-index 抬层）',
  /\.models-tabs-bar\s*\{[^}]*position:\s*sticky/.test(cssCode) === false &&
  !/\.models-tabs\s*\{[^}]*position:\s*sticky/.test(cssCode) &&
  !/\.models-tabs-bar\s*\{[^}]*z-index/.test(cssCode));
check('tab 条行不吃高度也不需要不透明背板（既然不悬浮，遮盖的问题就不存在）',
  /\.models-tabs-bar\s*\{[^}]*margin:\s*-10px 0 14px/.test(cssCode) &&
  /\.models-tabs-bar\s*\{[^}]*background/.test(cssCode) === false);
check('负上外边距把 pill 贴向顶部（吃掉 .settings-content 的 26px 上内边距）',
  /\.models-tabs-bar\s*\{[^}]*margin:\s*-10px 0 14px/.test(cssCode) &&
  /\.settings-content\s*\{[^}]*padding:\s*26px 30px/.test(css));
check('pill 在这一行里居中：块级 flex + fit-content + auto 外边距（inline-flex 无法 auto 居中）',
  /\.models-tabs\s*\{[^}]*display:\s*flex[^}]*width:\s*fit-content[^}]*margin:\s*0 auto/.test(cssCode) &&
  // 居中后不能再靠 margin-bottom 撑开与内容的距离
  !/\.models-tabs\s*\{[^}]*margin-bottom/.test(cssCode));
check('三个 tab 按钮与 data-mtab 接线未被动过',
  /class="models-tab active" data-mtab="list">模型列表</.test(htmlSrc) &&
  /data-mtab="prov">供应商管理</.test(htmlSrc) &&
  /data-mtab="config">添加模型</.test(htmlSrc));

group('模型管理页：供应商管理收敛成一屏');
check('高度预算走 flex 链，不写 100dvh - Nxx 魔数（内边距一改就错位成「差几像素也要滚」）',
  /\.settings-page#page-models\.active\s*\{[^}]*display:\s*flex[^}]*flex-direction:\s*column[^}]*height:\s*100%/.test(css) &&
  /#mtab-prov\.active\s*\{[^}]*display:\s*flex[^}]*flex:\s*1 1 auto[^}]*min-height:\s*0/.test(css) &&
  !/calc\(100dvh/.test(cssCode));
check('去掉 min-height:420px 硬地板（有它在，内容短时白撑空白把页面顶到要滚）',
  /\.prov-md\s*\{[^}]*flex:\s*1 1 auto[^}]*min-height:\s*260px/.test(css) &&
  !/min-height:\s*420px/.test(cssCode));
check('两列各自在内部滚（min-height:0 + overflow-y:auto），页面本身永远不滚',
  /\.prov-list\s*\{[^}]*min-height:\s*0[^}]*overflow-y:\s*auto/.test(cssCode) &&
  /\.prov-detail\s*\{[^}]*min-height:\s*0[^}]*overflow-y:\s*auto/.test(cssCode));
// ⚠️ 两处会让「内部滚动」静默失效的坑：
//  ① 行高必须是 minmax(0,1fr)。隐式行是 auto（按内容高），内容比容器高时行会溢出、
//     被 .prov-md 的 overflow:hidden 裁掉，两列的 overflow-y:auto 压根不触发 ——
//     症状是「下半截看不见，也滚不动」。
//  ② .prov-detail 绝不能是 flex 列容器。它是块流排版，flex 没用；换上 flex 反而
//     让 .pv-disc（获取模型列表，overflow:hidden）的自动最小尺寸变成 0，
//     空间不够时被压到 0 高，里面的 .disc-list 一起消失（2026-09 反馈）。
check('行高锁成 minmax(0,1fr)，两列才有确定的行高可分（auto 行会溢出被裁，滚不动）',
  /\.prov-md\s*\{[^}]*grid-template-rows:\s*minmax\(0, 1fr\)/.test(cssCode));
check('.prov-detail 保持块流（不得改成 flex 列，否则获取模型列表面板被压扁消失）',
  /\.prov-detail\s*\{[^}]*\}/.test(cssCode) && // 能匹配到规则
  !/\.prov-detail\s*\{[^}]*display:\s*flex/.test(cssCode) &&
  // 判据的另一半：那个面板确实是 overflow:hidden（所以在 flex 里会被压到 0 高）
  /\.pv-disc\s*\{[^}]*overflow:\s*hidden/.test(cssCode) &&
  /\.disc-list\s*\{[^}]*max-height:\s*300px[^}]*overflow-y:\s*auto/.test(cssCode));
check('表单改两列网格（原先 5 个整宽竖排字段就 ~320px，单它一个就撑爆一屏）',
  /\.prov-form\s*\{[^}]*display:\s*grid[^}]*grid-template-columns:\s*repeat\(2, minmax\(0, 1fr\)\)/.test(css) &&
  /\.prov-form \.f-field-wide\s*\{\s*grid-column:\s*1 \/ -1/.test(css));
check('长值字段（Base URL / 密钥 / 停用开关）占满整行，其余两两并排',
  /form\.appendChild\(domField\('名称', nameIn\)\);\s*\n\s*form\.appendChild\(domField\('兼容协议', protoSel\)\);\s*\n\s*form\.appendChild\(domField\('Base URL', urlIn, 'f-field-wide'\)\);\s*\n\s*form\.appendChild\(domField\('密钥', key\.node, 'f-field-wide'\)\)/.test(uiSrc) &&
  /domEl\('label', 'f-field f-field-wide'\)/.test(uiSrc));
check('domField 的第三参是可选类名（共享函数，加必填会连带改掉模型表单的 4 处调用）',
  /function domField\(label, control, cls\) \{[\s\S]{0,200}?domEl\('label', 'f-field' \+ \(cls \? ' ' \+ cls : ''\)\)/.test(uiSrc) &&
  // 模型表单那两处仍是两参调用
  /domField\('显示名称', nameIn\)/.test(uiSrc) && /domField\('模型 id', idIn\)/.test(uiSrc));
check('内边距同步收窄（省下的高度正是给表单两列用的）',
  /\.prov-detail\s*\{[^}]*padding:\s*14px 18px 16px/.test(css) &&
  /\.prov-item\s*\{[^}]*padding:\s*7px 10px/.test(css) &&
  /\.prov-sub\s*\{[^}]*margin:\s*14px 0 6px/.test(css) &&
  /\.prov-actions\s*\{[^}]*margin-top:\s*10px/.test(css));

group('模型管理页：添加模型的按钮不用滚到底才够得着');
// 故障：「保存 / 测试连接」在表单最下方，7 个整宽竖排字段把它顶到一屏开外，
// 想点保存得先滚到底（2026-09 反馈）。修法两条：表单改两列（高度直接砍半），
// 并且滚动只发生在表单区内部，动作条留在卡片底部常驻。
const cfgHtml = (function () {
  const a = htmlSrc.indexOf('id="mtab-config"');
  // 切到本 section 结束（要含整个动作条，不能切在 #mf-save 之前）
  const b = htmlSrc.indexOf('</section>', a);
  return a < 0 ? '' : htmlSrc.slice(a, b);
})();
// 「滚动区内容」= 从 <div class="mf-scroll"> 到**真正**把它关掉的那个 </div> 之间。
// ⚠️ 只能用深度计数找边界：光比较 indexOf 大小 / 用 lastIndexOf('</div>') 都判不出来 ——
// 把面板挪到滚动区外面之后，index 顺序照样不变、动作条前照样有个 </div>，断言会假过。
function innerOfDiv(html, openMarker) {
  const start = html.indexOf(openMarker);
  if (start < 0) return '';
  const re = /<div\b|<\/div>/g;
  re.lastIndex = start + openMarker.length;
  let depth = 1, m;
  while ((m = re.exec(html))) {
    depth += m[0] === '</div>' ? -1 : 1;
    if (depth === 0) return html.slice(start + openMarker.length, m.index);
  }
  return '';
}
const cfgScrollInner = innerOfDiv(cfgHtml, '<div class="mf-scroll">');
check('表单区与动作条分开：.mf-scroll 里是表单 + 上游模型面板，动作条在它外面',
  /class="mf-scroll">\s*<div class="form-grid">/.test(cfgHtml) &&
  // 上游模型勾选面板也必须放进滚动区（否则展开后把动作条顶下去）
  cfgScrollInner.includes('id="mf-discover-panel"') &&
  !cfgScrollInner.includes('class="mf-actions"'));
check('动作条（测试连接 / 重置 / 保存）完整且在滚动区之外',
  /id="mf-test">测试连接</.test(cfgHtml) && /id="mf-reset">重置</.test(cfgHtml) &&
  /id="mf-save" class="primary">保存</.test(cfgHtml) &&
  cfgHtml.lastIndexOf('class="mf-actions"') > cfgHtml.indexOf('id="mf-discover-panel"'));
check('高度链与滚动边界：pane / 卡片 / .mf-scroll 各负其职，标题与动作条不收缩',
  /#mtab-config\.active\s*\{[^}]*display:\s*flex[^}]*flex:\s*1 1 auto[^}]*min-height:\s*0/.test(cssCode) &&
  /#mtab-config > \.settings-card\s*\{[^}]*flex:\s*1 1 auto[^}]*min-height:\s*0[^}]*display:\s*flex[^}]*flex-direction:\s*column/.test(cssCode) &&
  /\.mf-scroll\s*\{[^}]*flex:\s*1 1 auto[^}]*min-height:\s*0[^}]*overflow-y:\s*auto/.test(cssCode) &&
  /#mtab-config \.mf-title,\s*\n?\s*#mtab-config \.mf-actions\s*\{\s*flex:\s*none/.test(cssCode));
check('表单改两列，且作用域限死在本页（.form-grid 是共享类，全局改会连带变形另三个表单）',
  /#mtab-config \.form-grid\s*\{[^}]*display:\s*grid[^}]*grid-template-columns:\s*repeat\(2, minmax\(0, 1fr\)\)/.test(cssCode) &&
  // 共享的 .form-grid 本身必须还是单列竖排
  /^\.form-grid\s*\{[^}]*display:\s*flex[^}]*flex-direction:\s*column/m.test(cssCode) &&
  // 另外三个表单 + ui.js 的就地编辑都还在用它
  (htmlSrc.match(/class="form-grid"/g) || []).length >= 4 &&
  /domEl\('div', 'form-grid'\)/.test(uiSrc));
check('长值字段占满整行：模型 id（带「获取模型列表」按钮）/ 密钥 / 上下文 / 继承块',
  /class="f-field f-field-wide" id="mf-inherit-field"/.test(cfgHtml) &&
  /class="f-field f-field-wide"><span>模型 id</.test(cfgHtml) &&
  /class="f-field f-field-wide" id="mf-key-field"/.test(cfgHtml) &&
  /class="f-field ctx-row f-field-wide"/.test(cfgHtml) &&
  /#mtab-config \.f-field-wide\s*\{\s*grid-column:\s*1 \/ -1/.test(cssCode));
// 窄字段必须成对相邻，否则网格里会留下半行空洞。
// 选供应商时 请求地址 / 协议 / 密钥 由 JS 整组隐藏（ui.js syncProvForm），
// 隐藏的格子在 grid 里不留空位，所以两种组合都是整行。
check('窄字段两两成对：供应商|显示名称 一行、请求地址|协议 一行（DOM 顺序已按网格排）',
  cfgHtml.indexOf('id="mf-prov"') < cfgHtml.indexOf('id="mf-name"') &&
  cfgHtml.indexOf('id="mf-name"') < cfgHtml.indexOf('id="mf-url-field"') &&
  cfgHtml.indexOf('id="mf-url-field"') < cfgHtml.indexOf('id="mf-proto-field"') &&
  cfgHtml.indexOf('id="mf-proto-field"') < cfgHtml.indexOf('id="mf-id"') &&
  cfgHtml.indexOf('id="mf-id"') < cfgHtml.indexOf('id="mf-key-field"'));
check('隐藏靠 hidden 属性，且全局有 [hidden]{display:none!important} 兜住 .f-field 的 display:flex',
  /document\.getElementById\('mf-url-field'\)\.hidden = !!p/.test(uiSrc) &&
  /document\.getElementById\('mf-key-field'\)\.hidden = !!p/.test(uiSrc) &&
  /document\.getElementById\('mf-proto-field'\)\.hidden = !!p/.test(uiSrc) &&
  /\[hidden\] \{ display: none !important; \}/.test(cssCode));

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
  /if \(runningSessions\.has\(s\.id\)\) addRunBadge\(li\)/.test(uiSrc) &&
  /#session-list\s+li\.session-item\s+>\s*\.session-spin\s*\{[^}]*animation:\s*toolSpin/.test(css));
// ⚠️ 判据必须是「**这一帧**的 session_id」而不是单槽代理 runAway()。后者问的是
  // 「我追踪的那一个运行槽在不在当前视图」—— 单任务时与前者等价（所以长期没暴露），
  // 两个会话并发时立刻分叉：A 的流式帧被 appendText 画进 B 的屏幕。
check('后台事件不画进当前视图（按帧的 session_id 判定）',
  /function frameIsMine\(ev\)/.test(uiSrc) &&
  uiCase('reasoning').includes('streamOf(ev.session_id).reason +=') &&
  uiCase('reasoning').indexOf('streamOf(ev.session_id)') < uiCase('reasoning').indexOf('if (!frameIsMine(ev)) break;') &&
  uiCase('text').includes("if (!frameIsMine(ev)) { foldReason(); break; }"));
// 补的必须是**该会话自己**的桶：并发时若读一个全局槽，就会把别的会话的片段
  // 贴到这一屏 —— 切换不但没修好，还把污染重新贴一遍。
check('切回运行中会话补渲染未落盘片段（读该会话自己的桶）',
  extractFunction(uiSrc, 'replayHistory').includes('streams.get(ev.session_id)') &&
  extractFunction(uiSrc, 'replayHistory').includes('if (st.reason) appendReason(st.reason);') &&
  extractFunction(uiSrc, 'replayHistory').includes('if (st.text) appendText(st.text);'));
// 运行态必须**无条件**摘除。原实现用一句 `if (session_id !== sessionID) break;`
  // 把「摘运行态」和「DOM 收尾」一起挡掉，别的会话跑完后运行态永远清不掉。
check('idle 复位运行态并撤销转圈（运行态无条件摘除）',
  uiCase('idle').includes('runningSessions.delete(') &&
  uiCase('idle').includes('streams.delete(') &&
  uiCase('idle').indexOf('runningSessions.delete(') < uiCase('idle').indexOf('clearRunVisuals()'));
// HITL 审批可能来自用户切走的后台会话：服务端必须带上 session_id，前端要标注来源。
const wsHandlerSrc = fs.readFileSync(
  path.join(__dirname, '..', '..', 'pkg', 'server', 'ws_handler.go'), 'utf8');
check('HITL 审批带会话来源（服务端下发 + 前端标注）',
  /"session_id":\s*a\.currentSession\(\)/.test(wsHandlerSrc) &&
  /c\.approver\.setSession\(sessionID\)/.test(wsHandlerSrc) &&
  /const fromOther = !!req\.session_id && req\.session_id !== sessionID;/.test(uiSrc) &&
  /\.msg-approval\.from-other\s*\{/.test(css));

// ---------------------------------------------------------------------------
// 思考强度拉条：档位集合 + 按协议记忆（localStorage）
// 2026-09-29：OpenAI 侧从 5 档扩到 8 档（none/default/minimal/low/medium/high/xhigh/max），
// 界面一律显示首字母大写的英文原名；Anthropic 仍是 token 连续滑条。
group('思考强度：档位集合 / 按协议记忆');
check('后端规格：无参关闭语义已拆成 none 与 default 两档',
  // 2026-09-29 之前只有「关闭 = 不发参数」一档，于是「不思考」和「让模型自己定」
  // 不可表达。两者在请求上必须不同，否则默认档就是假分档。
  /OffThinkingValue\s*=\s*"none"/.test(fs.readFileSync(
    path.join(__dirname, '..', '..', 'pkg', 'llm', 'thinking.go'), 'utf8')) &&
  /DefaultThinkingValue\s*=\s*"default"/.test(fs.readFileSync(
    path.join(__dirname, '..', '..', 'pkg', 'llm', 'thinking.go'), 'utf8')) &&
  /isOffThinking\(v\)/.test(fs.readFileSync(
    path.join(__dirname, '..', '..', 'pkg', 'llm', 'thinking.go'), 'utf8')));
check('openai 档位是官方八档，顺序与取值都锁住',
  (function () {
    const go = fs.readFileSync(path.join(__dirname, '..', '..', 'pkg', 'llm', 'thinking.go'), 'utf8');
    const m = /Steps: \[\]ThinkingStep\{([\s\S]*?)\n\t\t\t\}/.exec(go);
    if (!m) return false;
    const vals = (m[1].match(/Value:\s*(?:"[a-z]+"|[A-Za-z][A-Za-z0-9_]*)/g) || [])
      .map(function (s) { return s.replace(/^Value:\s*/, '').replace(/"/g, ''); });
    return vals.join(',') === 'OffThinkingValue,DefaultThinkingValue,minimal,low,medium,high,xhigh,max';
  })());
check('none 显式下发、default 不下发（两者在请求上可区分）', (function () {
  const go = fs.readFileSync(path.join(__dirname, '..', '..', 'pkg', 'llm', 'thinking.go'), 'utf8');
  const openai = fs.readFileSync(path.join(__dirname, '..', '..', 'pkg', 'llm', 'openai.go'), 'utf8');
  return /Default 档是唯一返回空串的合法档/.test(go) &&
    /EqualFold\(st\.Value, DefaultThinkingValue\)\s*\{\s*return ""/.test(go) &&
    // none 不在特判里 → 落到 return st.Value，即被显式下发
    !/EqualFold\(st\.Value, OffThinkingValue\)\s*\{\s*return "";/.test(go) &&
    // 空串才不发参数，所以「返回 default 字符串」这种错会被挡住
    /if effort := reasoningEffort\(req\.Thinking\); effort != "" \{/.test(openai);
})());
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
check('steps 模式用服务端 label 显示（None/Minimal/…/Xhigh/Max），none 不再中文化成「关闭」',
  // none 是真实下发的档位，中文化成「关闭」会和「不设置」混淆
  /hit\.label \|\| capitalizeFirst\(hit\.value\)/.test(uiSrc) &&
  !/if \(thinkingVal === 'none'\) return '关闭'/.test(uiSrc) &&
  // range（Anthropic）的 0 档仍是「关闭」
  /if \(!thinkingVal \|\| thinkingVal === 'none' \|\| thinkingVal === '0'\) return '关闭';/.test(uiSrc));
check('思考档位名首字母大写（缺 label 时退回大写原值）',
  /function capitalizeFirst\(s\)/.test(uiSrc) &&
  /return thinkingVal \? capitalizeFirst\(thinkingVal\) : 'Medium';/.test(uiSrc) &&
  /return hit \? \(hit\.label \|\| capitalizeFirst\(hit\.value\)\) : capitalizeFirst\(v\);/.test(uiSrc));

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
  /editableBacks\.has\(back\) && !viewRunning\(\)/.test(uiSrc));
check('编辑按钮默认隐形、悬停整行显形（CSS 契约）',
  /\.msg-user \.edit-btn \{[\s\S]*?opacity: 0;/.test(css) &&
  /\.msg-user:hover \.edit-btn \{ opacity: 1; \}/.test(css));
check('编辑按钮不遮挡气泡（order 归位到左侧）',
  /\.msg-user \.edit-btn \{[\s\S]*?order: -1;/.test(css));

check('编辑态把该条消息填入输入框并露出发送/取消',
  uiSrc.includes('function startEditMessage') && uiSrc.includes('function exitEditMode') &&
  /setInputValue\(text\);/.test(uiSrc) && /editBar\.classList\.remove\('hidden'\)/.test(uiSrc));
check('取消会清空输入框并收起编辑条（不留下残影）',
  /function exitEditMode\(restore\)[\s\S]*?if \(restore\) setInputValue\(''\);[\s\S]*?editBar\.classList\.add\('hidden'\)/.test(uiSrc));
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
  /if \(anyRunning\(\) \|\| sending \|\| !wsReady\)/.test(uiSrc));
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
// 输入卡折叠：上翻按滚动量藏、往回滑按滚动量露，**只有到底才完全弹出**。
// 早先只要 delta > 0 就整块弹回（composerHide = 0），往回滑 1px 输入框整个跳出来 ——
// 卡片位移量远大于那点滚动量，视觉上是「弹」而不是「滑」，翻历史看到一半就被糊住。
check('折叠与收回对称：都按滚动量渐进，不是整块跳',
  (function () {
    const h = composerScrollHandler();
    return /composerHide = Math\.max\(0, Math\.min\(max, composerHide - delta\)\);/.test(h) &&
      !/if \(delta > 0 \|\|/.test(h);      // 旧写法：任意向下滚动即归零
  })());
check('只有滑到最底部才完全弹出（中途只收回一部分）',
  /if \(followTail\) composerHide = 0;/.test(composerScrollHandler()));
check('位移写入只有一处，且无位移时不必带 translateY',
  (function () {
    const h = composerScrollHandler();
    const n = (h.match(/composerWrap\.style\.transform =/g) || []).length;
    return n === 1 && /composerHide \? ' translateY\(' \+ Math\.round\(composerHide\) \+ 'px\)' : ''/.test(h);
  })());
check('不再另写 < 80 的魔数（与 FOLLOW_NEAR_BOTTOM 同值，改一处就漂）',
  !/messagesEl\.scrollHeight - st - messagesEl\.clientHeight < 80/.test(uiSrc));
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
     /addTool\(toolLabel\(b\.name, b\.input\), \{\s*\n?\s*name: b\.name,\s*\n?\s*path: toolFilePath\(b\.name, b\.input\)\s*\n?\s*\}\)/.test(uiSrc) &&
     /function toolFilePath\(name, input\)/.test(uiSrc));
check('未匹配到记录时不谎报「已被回退」',
  // 大多数未命中是路径写法不同（模型传相对路径 / 大小写差异），
  // 改动其实好好地记着 —— 说成「已被回退」是对用户说了假话。
  /按这个路径没匹配到本会话的改动记录/.test(
    fs.readFileSync(path.join(__dirname, '../..', 'pkg/server/diff_api.go'), 'utf8')));
// 第 ③ 级现在由 pathMatchersCase 承载（大小写策略是注入参数），
// 边界后缀的变量名从 lower 改成 key —— 断言跟到新位置。
//
// 顺带盯住本次修的核心：大小写折叠**只在 Windows 开启**。
// 在区分大小写的文件系统上折叠会把 `Pkg/x.go` 与 `pkg/x.go` 判成同一个，
// 于是 /api/diff 返回另一个文件的 diff。
check('服务端路径查找三级容错且落在分隔符边界上',
  /func pathMatchers\(path, root string\) \[\]func\(string\) bool/.test(
    fs.readFileSync(path.join(__dirname, '../..', 'pkg/agent/checkpoints.go'), 'utf8')) &&
  /strings\.HasSuffix\(lp, "\/"\+key\)/.test(
    fs.readFileSync(path.join(__dirname, '../..', 'pkg/agent/checkpoints.go'), 'utf8')));
check('路径大小写折叠只在 Windows 生效（Linux 上 Pkg/ 与 pkg/ 是两个目录）',
  /func caseInsensitiveOS\(\) bool \{ return platform\.OSName\(\) == "windows" \}/.test(
    fs.readFileSync(path.join(__dirname, '../..', 'pkg/agent/checkpoints.go'), 'utf8')) &&
  /if caseInsensitive \{/.test(
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

// ---------- 模型库刷新：单一出口（2026-09-29）----------
// 故障：在「供应商管理」里删模型，模型确实没了，列表却不动，重新进入该页面才对。
// 原因：模型行同时渲染在两个视图（模型列表 #model-items / 供应商详情 #prov-detail），
// 而删除回调只刷了 renderModelItems —— 供应商详情那块 DOM 从没重绘。
// 修法：抽出 refreshModelViews() 作唯一出口，两个 render 退化为纯渲染。
group('模型库视图刷新走单一出口');
check('存在统一出口，一次取数后重绘两个视图 + 同步主界面',
  /function refreshModelViews\(\)/.test(uiSrc) &&
  /modelsLoaded = false;[\s\S]{0,120}?loadLibrary\(\)\.then\(function \(\) \{\s*renderModelItems\(\);\s*renderProviders\(\);/.test(uiSrc) &&
  /function loadModelViews\(\)/.test(uiSrc));
check('两个 render 都不再自带 loadLibrary（纯渲染，避免重复取数）',
  // 取数收敛到出口后，render 里再拉一次就是同一 endpoint 打两次
  !/function renderModelItems\(\)[\s\S]{0,400}?loadLibrary\(\)/.test(uiSrc) &&
  !/function renderProviders\(\)[\s\S]{0,400}?loadLibrary\(\)/.test(uiSrc));
check('没有任何取数点自带「取完就刷视图」的组合', (function () {
  // 不变量（比数个数更耐改）：loadLibrary 的每个调用点，后面都不允许直接跟
  // renderModelItems / renderProviders —— 那正是本次漏刷的形态。
  // 先把两个出口函数体摘掉，它们内部的取数是设计如此（由下一条断言锁住职责）。
  const body = uiSrc
    .replace(/function refreshModelViews\(\)[\s\S]*?\n  \}/, '')
    .replace(/function loadModelViews\(\)[\s\S]*?\n  \}/, '');
  const re = /loadLibrary\(\)/g;
  let m, bad = 0, n = 0;
  while ((m = re.exec(body)) !== null) {
    n++;
    if (/renderModelItems\(\)|renderProviders\(\)|renderModelViews\(\)/.test(body.slice(m.index, m.index + 90))) bad++;
  }
  return n >= 3 && bad === 0;   // 摘掉出口后应剩：gotoAddModel、切 config tab
})());
check('两个出口各自承担职责：refreshModelViews 刷全部并同步主界面，loadModelViews 只重绘',
  /function refreshModelViews\(\) \{\s*\n\s*modelsLoaded = false;[\s\S]{0,200}?renderModelItems\(\);\s*\n\s*renderProviders\(\);\s*\n\s*return loadModels\(\);/.test(uiSrc) &&
  /function loadModelViews\(\) \{\s*\n\s*return loadLibrary\(\)\.then\(renderModelViews\);/.test(uiSrc));
check('无条件重绘两个视图，不按当前 tab 条件刷',
  // 早先 applyModel 里的 `if (mtab-prov.active) renderProviders()` 就是条件判断
  // 漏刷的第二处；且它后面紧跟 showPage('general')，条件恒真、本就是死代码。
  !/mtab-prov'\)\.classList\.contains\('active'\)/.test(uiSrc));
check('modelRowEl 不再收 afterChange 钩子（钩子只有一个调用点传且没传）',
  /function modelRowEl\(m\)/.test(uiSrc) &&
  !/opts\.afterChange/.test(uiSrc));
check('模型删除成功后走统一出口（供应商页删模型即时消失）', (function () {
  const i = uiSrc.indexOf('function modelRowEl');
  const body = uiSrc.slice(i, uiSrc.indexOf('\n  }', i));
  return /\/api\/models\/delete/.test(body) && /refreshModelViews\(\)/.test(body);
})());

// ---------- 删除类操作：原位二次确认的一致性（2026-09-29 审计）----------
group('删除操作的二次确认');
check('模型删除：查 d.ok，失败时先提示再退出、不刷新界面', (function () {
  const i = uiSrc.indexOf('function modelRowEl');
  const body = uiSrc.slice(i, uiSrc.indexOf('\n  }', i));
  // ok 分支在 !d.ok 分支**之后**才调 refreshModelViews —— 顺序反了就是失败也刷
  const bad = body.indexOf('if (!d.ok)');
  const good = body.indexOf('refreshModelViews()');
  return /删除失败/.test(body) && bad >= 0 && good > bad;
})());
check('模型 / 供应商删除都有防连点（确认按钮点下即禁用）',
  (uiSrc.match(/confirmBtn\.disabled = true;/g) || []).length >= 2);
check('供应商删除取消后按钮回到原位（appendChild，不用 insertBefore 挪位）',
  /acts\.appendChild\(delBtn\)/.test(uiSrc) && !/insertBefore\(delBtn, testBtn\)/.test(uiSrc));
check('归档会话的「永久删除」也有二次确认',
  // 不可逆操作，两处保护等级必须一致：侧栏 renderSessions 早有，归档页曾漏
  /label: '永久删除', danger: true, confirmDelete: true, fn: function \(\) \{ deleteSession/.test(uiSrc) &&
  /label: '永久删除', danger: true, confirmDelete: true, fn: function \(\) \{[\s\S]{0,120}?api\/sessions\?id=/.test(uiSrc));
check('MCP 插件删除改为原位二次确认（曾一点即删）',
  /mkBtn\('确认删除', 'confirming'/.test(uiSrc) &&
  /deletePlugin\(p\.name\)/.test(uiSrc));
check('记忆删除不再用浏览器 confirm，改原位确认',
  !/confirm\('删除这条记忆/.test(uiSrc) &&
  /className = 'label-btn confirming'/.test(uiSrc));
check('「确认删除」危险色样式覆盖 .label-btn（记忆/插件行不在 .mi-actions 内）',
  /\.label-btn\.confirming \{[^}]*color: var\(--danger\)/.test(css));

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

// 透明档的三条契约（2026-09-30 重写）：
// 目标从「别让文字看不见」改成「输入框只透背景图，不透被覆盖的消息文字」。
// 后者严格更强 —— 前者是后者的一种特殊情况。

check('透明档：无背景图时退回不透明底（半透明在纯色背景上必然叠字）', (function () {
  // 这条钉的是一个真实缺陷：曾经 cbox-clear 无条件用 rgba(0,0,0,.14)，
  // 而 14% 的黑**挡不住文字** —— 后面的消息照样清晰可读地透出来。
  // 没有 has-bg 限定时，纯色背景（没设背景图）下叠字尤其明显。
  const m = /^#composer\.cbox-clear\s*\{([^}]*)\}/m.exec(css);
  if (!m) return false;
  // 默认必须是**不透明**的 --bg-elev-2（与黑/白档同源）
  if (!/background:\s*var\(--bg-elev-2\)/.test(m[1])) return false;
  // 半透明只允许出现在 body.has-bg 限定的那条里
  if (/background:\s*rgba\(/.test(m[1])) return false;
  return /body\.has-bg #composer\.cbox-clear\s*\{[^}]*background:\s*rgba\(/.test(css);
})());

check('透明档：消息列在输入区那一段淡出（只透背景图、不透被覆盖的文字）', (function () {
  // 故障机理：#messages 的底部留白只在**跟随态**把内容顶上来；上翻看历史时
  // 内容填满视口就钻到卡片底下。而 #composer-wrap 是 position:absolute、
  // #messages 是在流块 —— CSS 绘制顺序决定卡片**必然画在消息之上**。
  //
  // 解法给消息列加底部 mask：背景图在 #bg-layer（body 子节点、z-index:-1），
  // **不在 #messages 的绘制范围内**，所以 mask 只吃掉文字，图原样透出且清晰。
  //
  // 为什么不用 backdrop-filter：毛玻璃会把背景图一起模糊，而要的是图清晰。
  // 为什么不用不透明底：那等于退回黑/白档，透明档失去意义。
  const m = /body\.has-bg #messages\s*\{([^}]*)\}/.exec(css);
  if (!m) return false;
  const blk = m[1];
  if (!/mask-image:\s*linear-gradient/.test(blk)) return false;
  // 渐变末端必须是全透明（这才是「淡出」）
  if (!/transparent\s+100%\)/.test(blk)) return false;
  // 高度必须走动态变量，不能写死 —— 卡片会随任务清单/打字变高变矮
  if (!/var\(--composer-mask-h/.test(blk)) return false;
  // 必须同时给 -webkit- 前缀（旧 Safari / 部分 Chromium 需要）
  return /-webkit-mask-image:/.test(css);
})());

check('透明档：--composer-mask-h 跟着折叠实时收窄，且初始化时就算一次', (function () {
  // 这个变量有**两处**写入，各管一件事，缺一不可：
  //   ① syncComposerPadding（初始化 + ResizeObserver + resize）
  //      —— 页面刚加载、用户没滚动时那个 scroll 回调不会跑，只靠它；
  //      任务清单展开 / 输入框长高 / 底栏换行也要跟着重算。
  //   ② scroll 回调 —— 折叠时实时收窄。少了它，卡片滑下去之后遮罩还是满高，
  //      底部糊着一条无意义的淡出带：「翻历史时靠近屏幕底部的文字凭空消失」。
  //
  // ① 要去抖（值没变不写 DOM，否则 ResizeObserver 会自激）→ 用 lastMaskH 记一份，
  //    且不能与 lastComposerPad 共用：折叠时 mask 变了而 pad 不变，共用会误判。
  if (!/lastMaskH/.test(uiSrc)) return false;
  if (!/lastComposerPad/.test(uiSrc)) return false;
  if (!/function syncComposerPadding\(\)[\s\S]{0,900}?--composer-mask-h/.test(uiSrc)) {
    return false;
  }

  // ② scroll 回调那处：同一个表达式里必须同时有卡片高度与 composerHide。
  //    按写入点切段，第 2 段（index 1）的前文就是 scroll 回调里的计算。
  const parts = uiSrc.split("setProperty('--composer-mask-h'");
  if (parts.length < 3) return false;                // 至少两处写入
  const calc = parts[1].slice(-400);                 // 第 2 处写入之前的代码
  if (!/composerHide/.test(calc)) return false;
  if (!/offsetHeight/.test(calc)) return false;
  // 收起到底会算出负高度（渐变两端颠倒 → 整列反色），必须夹住
  return /Math\.max\(\s*0\s*,/.test(calc);
})());

check('黑/白档不加消息遮罩（它们本就不透明，加了纯浪费合成层）', (function () {
  // 遮罩只能挂在 has-bg + 透明档这条路径上。挂到全局或黑/白档上，
  // 会让「不透明输入框 + 底部消息淡出」这种视觉自相矛盾的组合出现。
  const g = /body\.has-bg #messages\s*\{[^}]*mask-image/.test(css);
  if (!g) return false;
  // 不得出现「不带条件」的全局 #messages mask
  return !/^\s*#messages\s*\{[^}]*mask-image/m.test(css);
})());

check('透明档的 placeholder 提一档对比度（--text-dim 是为实色底调的）', (function () {
  // 浅色主题的 --text-dim 是 #6b7280，落在亮背景图上几乎消失 —— 「随心输入」看不见。
  // 允许两种解法：给 --text-dim 提亮，或单出一条 placeholder 规则。
  const m = /body\.has-bg #composer\.cbox-clear\s*\{([^}]*)\}/.exec(css);
  if (!m) return false;
  return /--text-dim\s*:/.test(m[1]) ||
    /#composer\.cbox-clear\s+textarea::placeholder/.test(css) ||
    /#composer\.cbox-clear\s+::placeholder/.test(css);
})());

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

group('目标模式（Goal Mode）2026-09-27');

check('插件已注册：BuiltinGoalMode.ID = goal_mode',
  /ID:\s*"goal_mode"/.test(bpSrc) &&
  /BuiltinSkillCreator, BuiltinMultiAgent, BuiltinPlan, BuiltinGoalMode/.test(bpSrc));

check('工具名与前端文案同源（goal_verify）',
  /func \(t \*GoalVerifyTool\) Name\(\) string \{ return "goal_verify" \}/.test(toolsBuiltinSrc) &&
  /case 'goal_verify'/.test(uiSrc));

check('仅 @goal_mode 注入（非常驻）',
  /func GoalTriggered\(input string\) bool/.test(toolsGoalSrc) &&
  /triggered := tools\.GoalTriggered\(lastInput\)/.test(bpSrc));

check('注入条件含「目标已开启」——否则自循环第二轮起约束消失',
  /if !triggered && !goal\.Open\(\) \{/.test(bpSrc));

check('注入点在 systemPromptFor（按会话），不是 systemPrompt',
  /if sec := a\.goalPluginSection\(lastInput, sess\)/.test(todosSrc) &&
  /func \(a \*Agent\) systemPromptFor\(sess \*Session\)/.test(todosSrc));

check('WS 层把账本装进 ctx（装错会话 → 强约束静默失效）',
  /if ledger, ok := c\.srv\.agent\.GoalLedgerFor\(sessionID\); ok \{/.test(wsSrc) &&
  /ctx = tools\.WithGoalLedger\(ctx, ledger\)/.test(wsSrc));

check('工具执行时取不到账本就报错，而不是静默通过',
  /tools\.Err\("取不到当前会话的目标账本/.test(toolsBuiltinSrc));

check('main.go 注册 goal_verify + 目标模式开关',
  /goalTool = builtin\.RegisterGoalVerify\(registry, goalVerifier\)/.test(mainSrc) &&
  /ag\.SetGoalModeEnabled\(true\)/.test(mainSrc));

check('热切换：注销工具时 SetGoalModeEnabled(false) 同步',
  /registry\.Unregister\("goal_verify"\)/.test(mainSrc) &&
  /ag\.SetGoalModeEnabled\(on\)/.test(mainSrc));

check('设置项走通用 setting 描述符（前端不按插件 id 硬编码）',
  /func \(s \*Server\) builtinSetting\(id string\) \*builtinSettingDesc/.test(bpHandlerSrc) &&
  /item\["setting"\] = set/.test(bpHandlerSrc) &&
  /if \(p\.setting && typeof p\.setting\.min === 'number'\)/.test(uiSrc));

check('轮数夹在 [1, 20] 且有硬顶常量',
  /GoalModeMaxRoundsCap = 20/.test(configSrc) &&
  /if n > GoalModeMaxRoundsCap/.test(configSrc));

check('审查者白名单不含任何写工具（有测试锁住，这里锁住白名单定义）',
  /var goalVerifierTools = \[\]string\{[\s\S]*?"run_command",\s*\n\}/.test(goalRunnerSrc) &&
  !/goalVerifierTools = \[\]string\{[^}]*write_file/.test(goalRunnerSrc));

// 断言方式在 2.1（执行器角色工厂）之后改了：审查者不再手工写
// `a.executor.Audit()`，而是走 `NewExecutorFor(parent, RoleGoalReviewer, ...)`
// 由工厂从 parent 派生 audit —— 手工传参在签名里就没有这个位置，想传错都传不了。
//
// 行为没变（审查者仍然用真审计），变的是断言要盯住的那条路径。
// 更强的护栏在 Go 侧：pkg/agent/executor_role_test.go 的
// TestNewSubagentHasAudit 直接断言 newGoalVerifier(...).executor.Audit() != nil。
check('审查者用的是真审计（自主执行命令必须留痕）',
  /tools\.NewExecutorFor\(\s*a\.executor,\s*tools\.RoleGoalReviewer/.test(goalRunnerSrc) &&
  /audit:\s*parent\.Audit\(\)/.test(roleSrc) &&
  /func \(e \*Executor\) Audit\(\) \*security\.AuditLogger/.test(executorSrc));

check('预算用尽：不跑审查、不判 PASS、明确要求人工介入',
  /if before\.Open\(\) && before\.Exhausted\(\)/.test(toolsBuiltinSrc) &&
  /不要宣布完成/.test(toolsBuiltinSrc) &&
  /人工介入/.test(toolsBuiltinSrc));

check('审批卡能看出是验证子任务（复用 SubagentIdentityFrom）',
  /tools\.WithSubagentScope\(ctx, tools\.SubagentScope\{[\s\S]*?TaskID:\s*GoalVerifierID/.test(goalRunnerSrc) &&
  /const GoalVerifierID = "goal-verify"/.test(goalRunnerSrc));

check('进度事件复用子智能体卡（不发 todo，避免污染真实任务清单）',
  /sink, hasSink := tools\.SubagentSinkFrom\(ctx\)/.test(goalRunnerSrc) &&
  /emitEvent\("started", "开始验证目标", ""\)/.test(goalRunnerSrc) &&
  /emitEvent\("completed", "判定 "/.test(goalRunnerSrc) &&
  !/WithTodoSink/.test(goalRunnerSrc) &&
  /const MODE_LABEL = \{ implement: '实现', explore: '探索', verify: '验证' \}/.test(uiSrc));

check('modeLabel 认识 verify（否则卡片显示「探索 · goal-verify」）',
  /if mode == "verify" \{/.test(subagentSrc) &&
  /return "验证"/.test(subagentSrc));

// ---------- 探索分组：连续搜索/读取收成可折叠块（2026-09-29）----------
// 动机：探代码时 read_file / search_files 连着来十几二十次，一次一张卡片会把
// 真正的回答挤出视野。行为照着参考实现：标题「已探索 N 次搜索」，点开看明细。
group('探索分组（已探索 N 次搜索/读取）');
check('入组范围只含只读探索工具，不含写/改/删与运行命令', (function () {
  // 折叠的代价是「看不见」，只对可回看的只读信息才划算；
  // 写/改/删要显示 diff，运行命令是显式动作，都不该被藏起来。
  const m = /const EXPLORE_KINDS = \{([\s\S]*?)\};/.exec(uiSrc);
  if (!m) return false;
  const names = (m[1].match(/(\w+):/g) || []).map(function (s) { return s.slice(0, -1); });
  return names.sort().join(',') === 'find_files,list_dir,read_file,search_files' &&
    !/write_file|edit_file|delete_file|run_command/.test(m[1]);
})());
check('判定「连续」靠分组是否仍贴着消息列末尾，不靠标志位', (function () {
  // 少一个状态变量，也不会漏掉某条边界：期间插进任何**内容**它就不再是末尾，
  // 下一个探索卡片自然另起一组。
  //
  // ⚠️ 但「等待模型响应」那条提示**不算**内容 —— 它在每个 ReAct 步骤前都会插到
  // 列末尾、工具到达时又被移除。拿它当插队信号，症状是「连续 read_file 被拆成
  // 一个个单独的『已探索』」，一次分组彻底失效。所以判定要经 lastContentIn
  // 跳过 TRANSIENT_COL_CLASSES。
  return /function lastElementIn\(node\)/.test(uiSrc) &&
    // lastElementIn 必须跳过文本节点：列里夹着文本节点，直接看 lastChild 拿不到元素
    /if \(n\.nodeType === 1\) return n;/.test(uiSrc) &&
    /function lastContentIn\(node\)/.test(uiSrc) &&
    /const TRANSIENT_COL_CLASSES = \['msg-thinking'\]/.test(uiSrc) &&
    /function isTransientColNode\(n\)/.test(uiSrc) &&
    /function exploreStillLast\(col\)/.test(uiSrc) &&
    // 三个判定点都必须走 exploreStillLast
    (uiSrc.match(/exploreStillLast\(/g) || []).length >= 3 &&
    !/lastElementIn\(col\) === exploreGroup/.test(uiSrc);
})());
check('收尾挂在列的 appendChild（追加之后判，不是追加之前）', (function () {
  // ⚠️ 这里踩过一个坑：检查早先挂在 ensureCol() 里，而 ensureCol() 是
  // `ensureCol().appendChild(x)` 的**接收者表达式** —— 它求值时新内容还没插进去，
  // 分组仍贴着末尾，检查恒为真，分组永远不会被收起。差一步的顺序，
  // 症状是「该折的没折」。所以必须包在 appendChild 上、在追加**之后**同步。
  const col = extractFunction(uiSrc, 'ensureCol');
  return /const rawAppend = msgCol\.appendChild\.bind\(msgCol\);/.test(col) &&
    /msgCol\.appendChild = function \(node\) \{\s*\n\s*const r = rawAppend\(node\);\s*\n\s*if \(node !== exploreGroup\) syncExploreGroup\(msgCol\);/.test(col) &&
    (uiSrc.match(/ensureCol\(\)\.appendChild\(/g) || []).length >= 10;
})());
check('一轮结束时收起末尾那组，用户手动开过的不动', /function collapseExploreGroup\(\)[\s\S]*?if \(exploreUserToggled\) return;/.test(uiSrc) &&
  /function clearRunVisuals\(\)[\s\S]*?collapseExploreGroup\(\);/.test(uiSrc) &&
  /exploreUserToggled = false;/.test(uiSrc));
check('折叠禁令只有一处判定，且只看「失败」不看「运行中」', (function () {
  // 早先 update 因「有失败」强制展开、collapse 又无条件折上，两处各判各的，
  // 结果组内失败时仍然被折起来 —— 正是这个功能最该避免的事。
  //
  // 2026-09-30 去掉 `.msg-tool.running` 这一条：每张新卡片都是 running 状态，
  // 而 updateExploreSummary 每次都据此 setExploreOpen(true) —— 于是**连续读取时
  // 分组永远是展开的**，用户要读十几行卡片，真正的回答被挤出视野。
  // 「模型此刻在读什么」由折叠块标题的计数与展开状态共同表达，不需要强制摊开。
  const fn = extractFunction(uiSrc, 'exploreMustStayOpen');
  if (!fn) return false;
  if (!/\.msg-tool\.denied/.test(fn)) return false;          // 失败仍必须看得见
  if (/\.msg-tool\.running/.test(fn)) return false;          // 运行中不再强制展开
  return /function updateExploreSummary\(g\)[\s\S]*?if \(exploreMustStayOpen\(g\)\) setExploreOpen\(g, true\);/.test(uiSrc) &&
    /function collapseExploreGroup\(\)[\s\S]*?updateExploreSummary\(g\);\s*\n\s*if \(exploreMustStayOpen\(g\)\) return;/.test(uiSrc) &&
    /function syncExploreGroup\(col\)[\s\S]*?if \(exploreMustStayOpen\(g\)\) return;/.test(uiSrc) &&
    // 判定只写一次：open 的写入都经 setExploreOpen（靠 _auto 区分自动/用户）
    (uiSrc.match(/function exploreMustStayOpen\(/g) || []).length === 1 &&
    !/g\._auto = true;\s*\n\s*g\.open = true/.test(uiSrc);
})());
check('新分组默认收起（details 不带 open）', (function () {
  // <details> 不加 open 属性默认就是收起态；这里钉住「别顺手加上 open」。
  const fn = extractFunction(uiSrc, 'createExploreGroup');
  if (!fn) return false;
  if (/\.open\s*=\s*true/.test(fn)) return false;
  if (/setAttribute\(.open./.test(fn)) return false;
  if (/'open'/.test(fn)) return false;
  return /createElement\('details'\)/.test(fn);
})());
check('创建后立即折上，不等下一张卡片或轮次收尾', (function () {
  // 早先新组是「开着」的，要等 collapseExploreGroup（轮次收尾）才折 ——
  // 于是模型跑的那段时间里它一直摊着，正是要改掉的。
  const fn = extractFunction(uiSrc, 'appendExploreCard');
  if (!fn) return false;
  // 建组之后必须有一次显式收起
  return /setExploreOpen\(exploreGroup, false\)|exploreGroup\.open = false/.test(fn);
})());
check('写 open 走 setExploreOpen，靠 _auto 区分自动与用户点击', /function setExploreOpen\(g, on\) \{[\s\S]*?g\._auto = true;/.test(uiSrc) &&
  /g\.addEventListener\('toggle', function \(\) \{\s*\n\s*if \(!g\._auto\) exploreUserToggled = true;/.test(uiSrc));
check('标题按类别计数（搜索/读取/查看），混合时并列', /const EXPLORE_ORDER = \['搜索', '读取', '查看'\]/.test(uiSrc) &&
  /parts\.push\(n \+ ' 次' \+ k\);/.test(uiSrc) &&
  /return parts\.join\(' · '\);/.test(uiSrc));
check('两处 addTool 都传 name（实时与回放分组结果才一致）',
  /addTool\(toolLabel\(name, ev\.tool_input\), \{\s*\n\s*name: name,/.test(uiSrc) &&
  /addTool\(toolLabel\(b\.name, b\.input\), \{\s*\n\s*name: b\.name,/.test(uiSrc));
check('清空视图时分组引用一并作废（节点已被 innerHTML 销毁）',
  /function clearViewState\(opts\)[\s\S]{0,900}?exploreGroup = null;/.test(uiSrc));
check('用 <details> 实现，与思考过程折叠块同一套做法', (function () {
  const g = extractFunction(uiSrc, 'createExploreGroup');
  return /document\.createElement\('details'\)/.test(g) &&
    /document\.createElement\('summary'\)/.test(g) &&
    /\.msg-explore\s*\{/.test(css) &&
    /\.msg-explore summary::after/.test(css) &&
    /\.msg-explore \.explore-body\s*\{[^}]*flex-direction:\s*column/.test(css);
})());

console.log('\n' + '-'.repeat(52));
if (failures.length) {
  console.log('失败 ' + failures.length + ' 项 / 通过 ' + passed + ' 项：');
  failures.forEach(function (f) { console.log('  - ' + f); });
  process.exit(1);
}
console.log('全部通过（' + passed + ' 项断言）');