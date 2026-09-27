// 用 agent-browser 驱动，验证 #1（工具失败/被拒 → 卡片加 ⛔；成功 → 保持原样）。
//
// 为什么需要它：这台机器连不上上游模型，真实 E2E（发消息→模型→工具）跑不了。
// 手法是 --init-script 在页面脚本之前挂钩 WebSocket，把 app 的 socket 与它
// addEventListener('message') 的处理器一并截获，再用**真实的监听器**派发合成帧
// —— 走的是 ui.js 里那条真实的事件分发路径，不是直接调内部函数。
//
// 用法：先起服务（bin/codeforge.exe -config config -no-open），再
//   node test/verify_tool_denied.mjs

import { execFileSync } from 'node:child_process';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const HERE = path.dirname(fileURLToPath(import.meta.url));
const APP = 'http://127.0.0.1:8420/';
// 必须用正斜杠：agent-browser 读这个 env 时会把反斜杠当转义符吃掉，
// 写成 C:\Program Files\... 会变成 C:Program Files...（实测踩过）。
const CHROME = 'C:/Program Files/Google/Chrome/Application/chrome.exe';
const HOOK = path.join(HERE, 'ab_hook_ws.js');
const SHOT_DIR = 'C:/Users/26536/AppData/Local/Temp/opencode';
// 直接 spawn agent-browser.cmd 在 Windows 上会 EINVAL（Node 不能执行 .cmd），
// 所以绕过 npm 的 .cmd 包装，直接用 node 跑它的真实入口。
const AB_JS = 'D:\\npm-global\\node_modules\\agent-browser\\bin\\agent-browser.js';

const env = {
  ...process.env,
  AGENT_BROWSER_EXECUTABLE_PATH: CHROME,
  AGENT_BROWSER_INIT_SCRIPTS: HOOK,
  // init script 只在**浏览器启动时**注册：浏览器已在跑时改 ab_hook_ws.js 不会生效
  // （实测踩过：拿到的是旧钩子）。用独立 namespace 强制每次都新开一个浏览器。
  //
  // ⚠️ 不要用 `close --all` 来重置：它会连带杀掉 daemon 启的进程，触发
  // ChildProcess.kill 报错把整次调用打断。namespace 隔离更干净。
  AGENT_BROWSER_NAMESPACE: 'cfverify',
  AGENT_BROWSER_SESSION: 'cfverify',
};

function ab(args) {
  try {
    return execFileSync(process.execPath, [AB_JS, ...args], {
      encoding: 'utf8', timeout: 120000, env, cwd: SHOT_DIR,
    }).trim();
  } catch (e) {
    return 'AB_ERROR: ' + (e.stdout || '') + (e.stderr || e.message);
  }
}

// --json 输出是 {success, data:{result}, error}，比裸 stdout 干净。
// data.result 是页面返回值的 JSON 编码（页面上已 JSON.stringify 过一次 → 双层）。
function evVal(js) {
  const out = ab(['--json', 'eval', js]);
  let outer;
  try { outer = JSON.parse(out); } catch (_) { return { __raw: out }; }
  if (!outer.success) return { __err: outer.error };
  let v = outer.data && outer.data.result;
  for (let i = 0; i < 3 && typeof v === 'string'; i++) {
    try { v = JSON.parse(v); } catch (_) { break; }
  }
  return v === undefined ? { __raw: out } : v;
}

const passed = [], failed = [];
function check(name, cond, detail) {
  (cond ? passed : failed).push(name);
  console.log((cond ? '  [OK] ' : '  [NG] ') + name + (detail ? '   ' + detail : ''));
}
const group = (n) => console.log('\n' + n);

// 独立 namespace 已经是全新浏览器，init script 一定是当前这份。
ab(['open', APP]);
ab(['--json', 'eval',
  "fetch('/api/workspace',{method:'POST',headers:{'Content-Type':'application/json'},"
  + "body:JSON.stringify({path:'C:/Users/26536/Desktop/Code/CodeForge-GO/CodeForge-go'})}).then(r=>r.status)"]);
ab(['reload']);
ab(['wait', '1600']);

group('0. 环境');
const e0 = evVal("JSON.stringify({hook:typeof window.__feed==='function', ws:!!window.__cfws,"
  + " composer:!!document.getElementById('composer'),"
  + " probe:window.__feed({type:'info', text:'probe'})})");
check('已挂上 WebSocket 钩子', e0.hook === true, JSON.stringify(e0));
check('已捕获 app 的 socket', e0.ws === true, '');
check('页面骨架完好', e0.composer === true, '');
check('合成帧真的进了 app 的 handler（探针无错）', e0.probe && e0.probe.ok === true,
  JSON.stringify(e0.probe));

group('1. 失败：黑名单拒绝（decision=deny + result.success=false）');
const r1 = evVal(`(function(){
  window.__clearCol();
  window.__feed({type:'busy'});
  window.__feed({type:'tool_call', tool_call_id:'t1', tool_name:'delete_file',
    tool_input:{path:'a.txt'}, decision:'deny', reason:'命中危险命令黑名单',
    diff_stats:{added:0,removed:12}});
  var pre = window.__lastTool();
  window.__feed({type:'tool_result', tool_call_id:'t1', tool_name:'delete_file',
    result:{success:false, error:'命中危险命令黑名单：拒绝执行'}});
  return JSON.stringify({pre:pre, post:window.__lastTool()});
})()`);
check('tool_call 后处于 running 且有 spinner',
  r1.pre && r1.pre.running === true && r1.pre.spinner === true, JSON.stringify(r1.pre));
check('失败后卡片带 denied 类', r1.post && r1.post.denied === true, JSON.stringify(r1.post));
check('失败后末尾出现 ⛔', r1.post && r1.post.mark === '⛔', 'mark=' + (r1.post && r1.post.mark));
check('失败后 spinner 撤下、不再假装运行',
  r1.post && r1.post.spinner === false && r1.post.running === false, JSON.stringify(r1.post));

group('2. 成功：result.success=true（产品要求：什么都不改）');
const r2 = evVal(`(function(){
  window.__clearCol();
  window.__feed({type:'busy'});
  window.__feed({type:'tool_call', tool_call_id:'t2', tool_name:'edit_file',
    tool_input:{path:'b.go'}, decision:'allow', reason:'', diff_stats:{added:3,removed:1}});
  window.__feed({type:'tool_result', tool_call_id:'t2', tool_name:'edit_file',
    result:{success:true, data:{ok:true}}});
  return JSON.stringify(window.__lastTool());
})()`);
check('成功卡片没有 denied 类', r2 && r2.denied === false, JSON.stringify(r2));
check('成功卡片没有 ⛔', r2 && r2.mark === null, 'mark=' + (r2 && r2.mark));
check('成功卡片 spinner 已撤下', r2 && r2.spinner === false && r2.running === false, '');
check('成功卡片保留 +N 徽标（未被去色逻辑误伤）', r2 && r2.stats === true, '');

group('3. 历史回放：重建的卡片不误加 ⛔');
const r3 = evVal(`(async function(){
  window.__clearCol();
  window.__feed({type:'history', session_id:'probe1', title:'x', messages:[
    {role:'user', content:[{type:'text', text:'改一下'}]},
    {role:'assistant', content:[
      {type:'tool_use', id:'h1', name:'write_file', input:{path:'c.txt'}},
      {type:'tool_result', tool_use_id:'h1', content:[{type:'text', text:'ok'}]}
    ]}
  ]});
  await new Promise(function(r){setTimeout(r,400);});
  return JSON.stringify(window.__lastTool());
})()`);
check('回放重建的卡片不误加 ⛔（历史里的 tool_use 都已执行）',
  r3 && r3.count === 1 && r3.mark === null, JSON.stringify(r3));

// 留一张失败态截图
evVal(`(function(){
  window.__clearCol();
  window.__feed({type:'busy'});
  window.__feed({type:'tool_call', tool_call_id:'s1', tool_name:'delete_file',
    tool_input:{path:'a.txt'}, decision:'deny', reason:'命中危险命令黑名单',
    diff_stats:{added:0,removed:12}});
  window.__feed({type:'tool_result', tool_call_id:'s1', tool_name:'delete_file',
    result:{success:false, error:'命中危险命令黑名单：拒绝执行'}});
  return 'ok';
})()`);
ab(['screenshot', SHOT_DIR + '/cf-tool-denied.png']);

console.log('\n----------------------------------------------------');
console.log(failed.length
  ? '失败 ' + failed.length + ' 项：通过 ' + passed.length + ' 项：\n  - ' + failed.join('\n  - ')
  : '全部通过（' + passed.length + ' 项断言）');
process.exit(failed.length ? 1 : 0);
