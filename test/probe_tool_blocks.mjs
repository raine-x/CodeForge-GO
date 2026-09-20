// 检查会话历史里有没有「缺 name 的 tool_use 块」—— 这正是 replayHistory 抛异常的原因。
//
//   node test/probe_tool_blocks.mjs

const BASE = 'http://127.0.0.1:8420';
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

const res = await fetch(BASE + '/', { redirect: 'manual' });
const raw = res.headers.getSetCookie ? res.headers.getSetCookie() : [res.headers.get('set-cookie')];
const cookie = (raw || []).filter(Boolean).map((c) => c.split(';')[0]).join('; ');

const listRes = await fetch(BASE + '/api/sessions', { headers: { Cookie: cookie } });
const items = (await listRes.json()).items || [];

const ws = new WebSocket(BASE.replace('http', 'ws') + '/ws', { headers: { Cookie: cookie } });

let done = false;
ws.onopen = async () => {
  for (const s of items) {
    const got = await new Promise((resolve) => {
      const h = (ev) => {
        let m; try { m = JSON.parse(ev.data); } catch (e) { return; }
        if (m.type === 'history' && m.session_id === s.id) {
          ws.removeEventListener('message', h);
          resolve(m.messages || []);
        }
      };
      ws.addEventListener('message', h);
      ws.send(JSON.stringify({ type: 'load_session', session_id: s.id }));
      setTimeout(() => resolve(null), 3000);
    });

    if (!got) { console.log('  ' + s.id + '  "' + s.title + '"  → 未取到历史'); continue; }
    const bad = [];
    got.forEach((m, mi) => {
      (m.content || []).forEach((b, bi) => {
        if (b.type === 'tool_use' && (typeof b.name !== 'string' || !b.name)) {
          bad.push({ mi, bi, block: b, msgRole: m.role });
        }
      });
    });
    console.log('  ' + s.id + '  "' + s.title + '"  消息=' + got.length +
      '  缺 name 的 tool_use 块: ' + bad.length);
    // 把坏块**原样**打出来：要看它到底缺了什么、有没有别的字段能推出工具名
    bad.slice(0, 5).forEach((x) => {
      console.log('      msg[' + x.mi + '](' + x.msgRole + ').block[' + x.bi + '] = ' +
        JSON.stringify(x.block).slice(0, 400));
    });
  }
  done = true;
  ws.close();
};

await sleep(12000);
if (!done) console.log('（超时）');
