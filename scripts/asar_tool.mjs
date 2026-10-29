// Electron app.asar 只读工具（不依赖 @electron/asar）：列目录 / 取单个文件 / 按前缀导出。
// 用途：WorkBuddy 每次更新后复核出站请求（版本号、UA、头构造、端点、payload）。
//
// 用法：
//   node scripts/asar_tool.mjs list    <app.asar> [pathPrefix] [limit]
//   node scripts/asar_tool.mjs cat     <app.asar> <internal/path>       # 输出到 stdout
//   node scripts/asar_tool.mjs extract <app.asar> <outDir> <pathPrefix...>
//
// 例：
//   node scripts/asar_tool.mjs cat "C:/Users/<u>/AppData/Local/Programs/WorkBuddy/resources/app.asar" package.json
//   node scripts/asar_tool.mjs extract "<...>/app.asar" tmp_wb "main" "cli/dist" "cli/product.json"
//
// 关键出站逻辑通常在 main/server.js 里搜这些符号：
//   buildVlmGatewayHeaders / applyGatewayHeaders / applyTraceHeaders / VlmUpstream / zF(denylist)
import fs from 'node:fs';
import path from 'node:path';

const [, , cmd, asarPath, ...rest] = process.argv;
if (!cmd || !asarPath) {
  console.error('usage: list|cat|extract <app.asar> ...');
  process.exit(2);
}

const fd = fs.openSync(asarPath, 'r');
const head = Buffer.alloc(16);
fs.readSync(fd, head, 0, 16, 0);
const headerSize = head.readUInt32LE(12);
const hb = Buffer.alloc(headerSize);
fs.readSync(fd, hb, 0, headerSize, 16);
const header = JSON.parse(hb.toString('utf8'));
const baseOffset = 16 + headerSize;
const unpackedRoot = asarPath.replace(/app\.asar$/, 'app.asar.unpacked');

function walk(node, prefix = '', out = []) {
  for (const [name, entry] of Object.entries(node.files || {})) {
    const p = prefix ? prefix + '/' + name : name;
    if (entry.files) walk(entry, p, out);
    else out.push({ path: p, size: entry.size, offset: Number(entry.offset), unpacked: !!entry.unpacked });
  }
  return out;
}
const all = walk(header);

function readEntry(e) {
  const onDisk = path.join(unpackedRoot, e.path);
  if (e.unpacked && fs.existsSync(onDisk)) return fs.readFileSync(onDisk);
  const buf = Buffer.alloc(e.size);
  if (e.size > 0) fs.readSync(fd, buf, 0, e.size, baseOffset + e.offset);
  return buf;
}

if (cmd === 'list') {
  const [prefix = '', limit = '60'] = rest;
  const hits = all.filter((e) => e.path.startsWith(prefix));
  for (const h of hits.slice(0, Number(limit))) console.log(String(h.size).padStart(10), h.unpacked ? 'U' : ' ', h.path);
  console.log(`# total ${all.length}, matched ${hits.length}`);
} else if (cmd === 'cat') {
  const want = rest[0];
  const e = all.find((x) => x.path === want);
  if (!e) { console.error('not found: ' + want); process.exit(3); }
  process.stdout.write(readEntry(e));
} else if (cmd === 'extract') {
  const [outDir, ...prefixes] = rest;
  let n = 0, bytes = 0;
  for (const prefix of prefixes) {
    const exact = all.find((e) => e.path === prefix);
    const list = exact ? [exact] : all.filter((e) => e.path.startsWith(prefix));
    for (const e of list) {
      const dest = path.join(outDir, e.path);
      fs.mkdirSync(path.dirname(dest), { recursive: true });
      fs.writeFileSync(dest, readEntry(e));
      n++; bytes += e.size;
    }
  }
  console.log(`extracted ${n} files, ${(bytes / 1048576).toFixed(1)} MB -> ${outDir}`);
} else {
  console.error('unknown command: ' + cmd);
  process.exit(2);
}
