// 客户端出站请求录制器（MITM 的「配置重定向」实现，无需改 hosts、无需装 CA）。
//
// 原理：WorkBuddy 客户端（Electron 主进程与内置 CLI）都走 ProductEndpointHttpInterceptor，
// 它会把请求的 baseURL 改成 ACC_PRODUCT_CONFIG_V3 / ACC_PRODUCT_CONFIG_PATH 提供的 endpoint。
// 于是把端点指向本地录制器，就能拿到与真实上游逐字节相同的请求（方法/路径/头顺序与大小写/body）。
//
// 用法：
//   1) 起录制器
//        node scripts/capture_client.mjs 18899
//   2) 用内置 CLI 发一次真实请求（把 <ver> 换成当前安装里的版本目录）
//        $env:ACC_PRODUCT_CONFIG_V3 = '{"endpoint":"http://127.0.0.1:18899","deploymentType":"SaaS"}'
//        $env:CODEBUDDY_API_KEY = 'dummy'      # 无登录态时用它过鉴权闸门，上游会 401 但请求已抓到
//        node "<install>/resources/app.asar.unpacked/cli/dist/codebuddy-lite-wb.mjs" -p "1+1" --model fast-model --tools ""
//   3) 看结果（JSONL 每条一次请求，body 以 base64 保存以便解 gzip）
//        node scripts/capture_client.mjs --dump <capture.jsonl> [urlSubstr]
//
// 注意：body 可能含真实提示词，属于敏感数据，只留在本机、勿提交（.gitignore 已忽略 scripts/_captures/）。
import http from 'node:http';
import fs from 'node:fs';
import path from 'node:path';
import zlib from 'node:zlib';

const outDir = path.resolve('scripts/_captures');

function sse(res) {
  res.writeHead(200, {
    'Content-Type': 'text/event-stream; charset=utf-8',
    'Cache-Control': 'no-cache',
    Connection: 'keep-alive',
  });
  const base = { id: 'chatcmpl-capture', object: 'chat.completion.chunk', created: Math.floor(Date.now() / 1000), model: 'fast-model' };
  res.write(`data: ${JSON.stringify({ ...base, choices: [{ index: 0, delta: { role: 'assistant', content: 'ok' }, finish_reason: null }] })}\n\n`);
  res.write(`data: ${JSON.stringify({ ...base, choices: [{ index: 0, delta: {}, finish_reason: 'stop' }] })}\n\n`);
  res.write('data: [DONE]\n\n');
  res.end();
}

function serve(port) {
  fs.mkdirSync(outDir, { recursive: true });
  const stamp = new Date().toISOString().replace(/[:.]/g, '-');
  const jsonl = path.join(outDir, `capture-${stamp}.jsonl`);
  const server = http.createServer((req, res) => {
    const chunks = [];
    req.on('data', (c) => chunks.push(c));
    req.on('end', () => {
      const body = Buffer.concat(chunks);
      const raw = [];
      for (let i = 0; i < req.rawHeaders.length; i += 2) raw.push([req.rawHeaders[i], req.rawHeaders[i + 1]]);
      fs.appendFileSync(jsonl, JSON.stringify({
        ts: new Date().toISOString(),
        method: req.method,
        url: req.url,
        httpVersion: req.httpVersion,
        headers: raw,
        bodyLength: body.length,
        contentEncoding: req.headers['content-encoding'] || '',
        bodyBase64: body.subarray(0, 400000).toString('base64'),
      }) + '\n');
      if ((req.url || '').includes('chat/completions')) return sse(res);
      res.writeHead(200, { 'Content-Type': 'application/json; charset=utf-8' });
      res.end(JSON.stringify({ code: 0, msg: 'capture', data: {} }));
    });
  });
  server.listen(port, '127.0.0.1', () => console.log(`capturing on http://127.0.0.1:${port}\nlog: ${jsonl}`));
}

function dump(file, filter) {
  const recs = fs.readFileSync(file, 'utf8').trim().split('\n').map((l) => JSON.parse(l));
  for (const r of recs) {
    if (filter && !(r.url || '').includes(filter)) continue;
    let body = Buffer.from(r.bodyBase64 || '', 'base64');
    const enc = (r.contentEncoding || '').toLowerCase();
    try {
      if (enc.includes('gzip')) body = zlib.gunzipSync(body);
      else if (enc.includes('deflate')) body = zlib.inflateSync(body);
      else if (enc.includes('br')) body = zlib.brotliDecompressSync(body);
    } catch { /* 保留原始字节 */ }
    console.log(`\n=== ${r.method} ${r.url}  (enc=${enc || 'none'} raw=${r.bodyLength}) ===`);
    for (const [k, v] of r.headers) console.log(`${k}: ${v}`);
    const text = body.toString('utf8');
    if (text) console.log(`--- body(${text.length}) ---\n${text.slice(0, 4000)}`);
  }
}

const [, , arg1, arg2] = process.argv;
if (arg1 === '--dump') dump(arg2, process.argv[4]);
else serve(Number(arg1 || 18899));
