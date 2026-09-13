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
  const code = [
    extractFunction(src, 'escapeHtml'),
    extractFunction(src, 'renderMD'),
    extractFunction(src, 'toolLabel'),
    extractFunction(src, 'countLines'),
    'module.exports = { renderMD: renderMD, toolLabel: toolLabel, countLines: countLines };',
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
let renderMD, toolLabel, countLines;
try {
  const api = loadRenderer();
  renderMD = api.renderMD;
  toolLabel = api.toolLabel;
  countLines = api.countLines;
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

// ---------- 6.5 新建项目：清空工作区必须先于新建会话 ----------
// 易回归点：setWorkspace 走 HTTP、doNewSession 走 WS，不等待就会抢跑，
// 新会话被挂到**上一个项目**下（而不是形成空工作区的「新项目」分组）。
group('新建项目：清空工作区与建会话的先后');
check('setWorkspace 返回 Promise 供调用方等待',
  /const done = fetch\('\/api\/workspace'/.test(extractFunction(uiSrc, 'setWorkspace')) &&
  /return done;/.test(extractFunction(uiSrc, 'setWorkspace')));
check('「新建项目」按钮先清空工作区再建会话',
  /setWorkspace\(''\)\s*\.then\(doNewSession/.test(uiSrc));

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
  /composerSnap\s*=\s*false;[\s\S]{0,200}?addUser\(text\)/.test(uiSrc));

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
  /addUser\(text\);[\s\S]{0,180}?showThinking\(\)/.test(uiSrc));
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
console.log('\n' + '-'.repeat(52));
if (failures.length) {
  console.log('失败 ' + failures.length + ' 项 / 通过 ' + passed + ' 项：');
  failures.forEach(function (f) { console.log('  - ' + f); });
  process.exit(1);
}
console.log('全部通过（' + passed + ' 项断言）');
