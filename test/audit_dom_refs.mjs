// 静态审计：ui.js 里查找的每个元素，是否真的存在于 index.html？
//
//   node test/audit_dom_refs.mjs
//
// 为什么值得查：`document.getElementById('x')` 拿到 null 之后，
// 常见写法是 `if (!el) return;` 或直接 addEventListener 抛错被吞掉 ——
// 结果就是**控件完全不响应、也不报错**（「啥也没有」）。
// 外观页已经出过 3 个「静默无效」类故障，所以把整页引用一次性对一遍。
//
// 也顺带检查：html 里定义了 id 但 ui.js 从没引用过的元素（可能是废弃残留）。

import fs from 'node:fs';
import path from 'node:path';

const DIST = path.join(import.meta.dirname, '..', 'web', 'dist');
const ui = fs.readFileSync(path.join(DIST, 'ui.js'), 'utf8');
const html = fs.readFileSync(path.join(DIST, 'index.html'), 'utf8');

// ---- ui.js 引用的 id ----
// 四类写法，**必须分两组**，否则 `querySelectorAll('button')` 这种标签选择器
// 会被当成 id 抓进来（`#` 若写成可选就会误报 #button）。
// 后两条**不要求选择器以 id 结尾**：`'#skin-seg button'`（取子元素）同样是引用。
const referenced = new Map(); // id -> [行号]
const PATTERNS = [
  /getElementById\(\s*['"]([A-Za-z][\w-]*)['"]/g,        // getElementById('x')
  /\$\(\s*['"]#([A-Za-z][\w-]*)/g,                        // $('#x')
  /querySelector(?:All)?\(\s*['"]#([A-Za-z][\w-]*)/g,     // querySelector('#x'), '#x button'
];
const lines = ui.split('\n');
lines.forEach((ln, i) => {
  PATTERNS.forEach((re) => {
    re.lastIndex = 0;   // 带 g 的正则会被 test/exec 残留 lastIndex，逐行复用前必须复位
    let m;
    while ((m = re.exec(ln))) {
      const id = m[1];
      if (!id) continue;
      if (!referenced.has(id)) referenced.set(id, []);
      referenced.get(id).push(i + 1);
    }
  });
});

// ---- index.html 定义的 id ----
const defined = new Set();
{
  const re = /\bid="([^"]+)"/g;
  let m;
  while ((m = re.exec(html))) defined.add(m[1]);
}

// ---- 运行时动态创建的 id（这些不该算缺失）----
// ui.js 里 el.id = 'xxx' 形式创建的元素
const dynamic = new Set();
{
  const re = /\.id\s*=\s*['"]([^'"]+)['"]/g;
  let m;
  while ((m = re.exec(ui))) dynamic.add(m[1]);
}
// 模板字符串里内联的 id（如弹窗 HTML）
{
  const re = /\bid=\\?["']([A-Za-z][\w-]*)["']/g;
  let m;
  while ((m = re.exec(ui))) dynamic.add(m[1]);
}

const missing = [];
for (const [id, at] of referenced) {
  if (!defined.has(id) && !dynamic.has(id)) missing.push({ id, at });
}

// 第三种引用形态：id 作为**字符串参数**传给辅助函数，
// 例如 `toggle('mcp-f-cmd-field', ...)` / `setMcpPane('mcp-pane-list')`。
// 这类没法用选择器语法识别，退化成「这个字面量在 ui.js 里出现过吗」。
function usedAsLiteral(id) {
  return ui.includes("'" + id + "'") || ui.includes('"' + id + '"');
}

// 布局容器与设置页是**只给 CSS / 计算式选择器**用的，不算废弃：
//   #app/#sidebar/#main/#drag-handle/#input-wrap → 只有 CSS 引用
//   #page-*  → 由 `'page-' + item.dataset.page` 计算得出
const CSS_ONLY = /^(app|sidebar|main|drag-handle|input-wrap|page-)/;

const unused = [...defined].filter(
  (id) => !referenced.has(id) && !dynamic.has(id) && !usedAsLiteral(id) && !CSS_ONLY.test(id)
);

console.log('ui.js 引用的 id 数: ' + referenced.size);
console.log('index.html 定义的 id 数: ' + defined.size);
console.log('ui.js 动态创建的 id 数: ' + dynamic.size);

console.log('\n--- 引用了但找不到的元素（会导致静默无效）---');
if (!missing.length) {
  console.log('  无 ✓');
} else {
  missing.forEach((m) => console.log('  ✗ #' + m.id + '  (ui.js:' + m.at.join(',') + ')'));
}

console.log('\n--- index.html 定义但 ui.js 从未引用（疑似废弃）---');
console.log('    （已排除：CSS 专用容器、#page-* 计算式选择器、作为字符串参数传入的 id）');
if (!unused.length) {
  console.log('  无 ✓');
} else {
  unused.forEach((id) => console.log('  · #' + id));
}

console.log('\n' + '-'.repeat(52));
if (missing.length) {
  console.log('发现 ' + missing.length + ' 个悬空引用');
  process.exitCode = 1;
} else {
  console.log('全部通过：没有悬空引用');
}
