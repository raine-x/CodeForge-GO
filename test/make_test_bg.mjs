// 生成背景图测试素材（零依赖，手写 PNG）
//
//   node test/make_test_bg.mjs <输出路径> [solid|gradient]
//
//   solid    （默认）纯品红 #FF00FF —— 用于**像素断言**：颜色极端好认，
//            且能扛住 blur/brightness（实测 brightness 62% 后仍是暗紫，一眼可辨）
//   gradient 深蓝→浅灰的斜向渐变 —— 用于**肉眼看可读性**：纯色判断不出
//            「文字压在图上看不看得清」，必须有明暗对比才能暴露问题
//
// ⚠️ Node 22 的 zlib **没有** crc32，得自己实现（PNG 每个 chunk 都要 CRC32）。
import zlib from 'node:zlib';
import fs from 'node:fs';

const W = 400, H = 300;
const mode = (process.argv[3] || 'solid').toLowerCase();

function pixelAt(x, y) {
  if (mode === 'gradient') {
    // 左上深蓝 → 右下浅灰：一半深底浅字、一半浅底深字，两边都测到
    const t = (x / W + y / H) / 2;
    const from = [26, 26, 46];      // #1a1a2e
    const to = [232, 232, 240];     // #e8e8f0
    return [
      Math.round(from[0] + (to[0] - from[0]) * t),
      Math.round(from[1] + (to[1] - from[1]) * t),
      Math.round(from[2] + (to[2] - from[2]) * t),
    ];
  }
  // 深/浅纯色：用于验证「深浅色自适应」两个极端（平均亮度分别约 0.1 / 0.9）
  if (mode === 'dark') return [26, 26, 46];       // #1a1a2e
  if (mode === 'light') return [232, 232, 240];   // #e8e8f0
  return [255, 0, 255];             // 品红
}

const rows = [];
for (let y = 0; y < H; y++) {
  const row = Buffer.alloc(1 + W * 3);   // 每行开头 1 字节 filter type(0)
  for (let x = 0; x < W; x++) {
    const [r, g, b] = pixelAt(x, y);
    row[1 + x * 3] = r;
    row[2 + x * 3] = g;
    row[3 + x * 3] = b;
  }
  rows.push(row);
}
const raw = Buffer.concat(rows);

function crc32(buf) {
  let c, crc = 0xffffffff;
  for (let i = 0; i < buf.length; i++) {
    c = (crc ^ buf[i]) & 0xff;
    for (let k = 0; k < 8; k++) c = c & 1 ? 0xedb88320 ^ (c >>> 1) : c >>> 1;
    crc = (crc >>> 8) ^ c;
  }
  return (crc ^ 0xffffffff) >>> 0;
}

function chunk(type, data) {
  const len = Buffer.alloc(4);
  len.writeUInt32BE(data.length);
  const body = Buffer.concat([Buffer.from(type, 'ascii'), data]);
  const crc = Buffer.alloc(4);
  crc.writeUInt32BE(crc32(body));
  return Buffer.concat([len, body, crc]);
}

const ihdr = Buffer.alloc(13);
ihdr.writeUInt32BE(W, 0);
ihdr.writeUInt32BE(H, 4);
ihdr[8] = 8;    // bit depth
ihdr[9] = 2;    // color type: truecolor
const png = Buffer.concat([
  Buffer.from([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a]),
  chunk('IHDR', ihdr),
  chunk('IDAT', zlib.deflateSync(raw)),
  chunk('IEND', Buffer.alloc(0)),
]);

const out = process.argv[2];
fs.writeFileSync(out, png);
const LABEL = {
  solid: '纯品红 #FF00FF', gradient: '深蓝→浅灰渐变',
  dark: '深色 #1a1a2e（平均亮度≈0.10）', light: '浅色 #e8e8f0（平均亮度≈0.90）',
};
console.log('已生成测试图: ' + out + '  (' + png.length + ' bytes, ' + (LABEL[mode] || mode) + ')');

