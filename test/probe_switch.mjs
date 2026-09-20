// WS 探针：切换会话时，服务端回的 history 事件里的 session_id 是否与请求一致？
//
//   node test/probe_switch.mjs <session_id>
//
// 用途：定位「切过去后侧栏没有选中态」—— renderSessions 是按 `s.id === sessionID` 打
// .active 的，若服务端回的 session_id 与请求的不一致，高亮就会落到别的条目上（或没有）。

const BASE = 'http://127.0.0.1:8420';
const SID = process.argv[2] || '';
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

// 1) 先 GET / 拿 HttpOnly Cookie（/ws 需要鉴权）
const res = await fetch(BASE + '/', { redirect: 'manual' });
const raw = res.headers.getSetCookie ? res.headers.getSetCookie() : [res.headers.get('set-cookie')];
const cookie = (raw || []).filter(Boolean).map((c) => c.split(';')[0]).join('; ');
console.log('Cookie: ' + (cookie ? '已获取' : '（无）'));

// 2) 连 /ws
const wsUrl = BASE.replace('http', 'ws') + '/ws';
const ws = new WebSocket(wsUrl, { headers: { Cookie: cookie } });
const seen = [];
let closed = false;

ws.onopen = () => {
  console.log('WS 已连接，发送 load_session: ' + SID);
  ws.send(JSON.stringify({ type: 'load_session', session_id: SID }));
};
ws.onmessage = (ev) => {
  let m;
  try { m = JSON.parse(ev.data); } catch (e) { return; }
  const n = (m.messages || []).length;
  seen.push({ type: m.type, session_id: m.session_id, messages: n });
  if (m.type === 'history' || m.type === 'session' || m.type === 'sessions') {
    console.log('  ← ' + m.type +
      '  session_id=' + JSON.stringify(m.session_id) +
      (m.type === 'history' ? '  消息数=' + n : ''));
  }
};
ws.onerror = (e) => console.log('WS 错误');
ws.onclose = () => { closed = true; };

await sleep(2500);
ws.close();
await sleep(300);

console.log('\n--- 结论 ---');
const hist = seen.find((x) => x.type === 'history');
if (!hist) {
  console.log('未收到 history 事件');
} else if (hist.session_id === SID) {
  console.log('session_id 一致 ✓ → 高亮逻辑的输入没问题，问题在别处');
} else {
  console.log('❌ session_id 不一致！请求=' + JSON.stringify(SID) + ' 回包=' + JSON.stringify(hist.session_id));
  console.log('   → renderSessions 会按回包 id 打高亮，于是选中态落到别的条目 / 丢失');
}
console.log('收到的事件序列: ' + seen.map((s) => s.type).join(' → '));
