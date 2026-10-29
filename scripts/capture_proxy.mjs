// 桌面端出站录制代理：把端点重定向到本机，再原样转发到真实上游，顺手把请求完整落盘。
// 这样应用功能不受影响（登录/配置/模型/对话照常），但每一条出站请求都能看到。
//
// 用法：
//   node scripts/capture_proxy.mjs 18899 [upstreamHost]
//   # 然后带环境变量重启桌面端：
//   #   WORKBUDDY_DEV_ENDPOINT_OVERRIDE=http://127.0.0.1:18899
//   #   ACC_PRODUCT_CONFIG_V3={"endpoint":"http://127.0.0.1:18899","deploymentType":"SaaS"}
//
// 回看：
//   node scripts/capture_proxy.mjs --dump <capture.jsonl> [urlSubstr]
import http from 'node:http';
import https from 'node:https';
import fs from 'node:fs';
import path from 'node:path';
import zlib from 'node:zlib';

const outDir = path.resolve('scripts/_captures');
const agent = new https.Agent({ keepAlive: true });

const SECRET_HEADERS = new Set(['authorization', 'x-api-key', 'cookie', 'x-refresh-token', 'x-device-token', 'set-cookie']);

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
    console.log(`\n=== ${r.method} ${r.url}  (enc=${enc || 'none'} raw=${r.bodyLength} status=${r.status ?? '?'}) ===`);
    for (const [k, v] of r.headers) {
      // 默认脱敏：token / cookie / 设备令牌不落终端（抓包文件仍在本机，勿提交）
      const shown = SECRET_HEADERS.has(k.toLowerCase()) ? `<redacted ${String(v).length}B>` : v;
      console.log(`${k}: ${shown}`);
    }
    const text = body.toString('utf8');
    if (text) console.log(`--- body(${text.length}) ---\n${text.slice(0, 6000)}`);
  }
}

function serve(port, upstreamHost) {
  fs.mkdirSync(outDir, { recursive: true });
  const stamp = new Date().toISOString().replace(/[:.]/g, '-');
  const jsonl = path.join(outDir, `desktop-${stamp}.jsonl`);
  let seq = 0;

  const server = http.createServer((req, res) => {
    const chunks = [];
    req.on('data', (c) => chunks.push(c));
    req.on('end', () => {
      const body = Buffer.concat(chunks);
      const id = ++seq;
      const raw = [];
      for (let i = 0; i < req.rawHeaders.length; i += 2) raw.push([req.rawHeaders[i], req.rawHeaders[i + 1]]);

      const fwdHeaders = { ...req.headers };
      delete fwdHeaders.host;
      delete fwdHeaders.connection;
      if (body.length) fwdHeaders['content-length'] = String(body.length);

      const up = https.request(
        { host: upstreamHost, port: 443, method: req.method, path: req.url, headers: fwdHeaders, agent },
        (upRes) => {
          const rec = {
            ts: new Date().toISOString(),
            seq: id,
            method: req.method,
            url: req.url,
            headers: raw,
            bodyLength: body.length,
            contentEncoding: req.headers['content-encoding'] || '',
            bodyBase64: body.subarray(0, 400000).toString('base64'),
            status: upRes.statusCode,
            respContentType: upRes.headers['content-type'] || '',
          };
          fs.appendFileSync(jsonl, JSON.stringify(rec) + '\n');
          const mark = (req.url || '').includes('chat/completions') ? '  <== CHAT' : '';
          console.log(`#${id} ${req.method} ${req.url} -> ${upRes.statusCode}${mark}`);
          res.writeHead(upRes.statusCode || 502, upRes.headers);
          upRes.pipe(res);
        },
      );
      up.on('error', (e) => {
        fs.appendFileSync(jsonl, JSON.stringify({
          ts: new Date().toISOString(), seq: id, method: req.method, url: req.url,
          headers: raw, bodyLength: body.length,
          contentEncoding: req.headers['content-encoding'] || '',
          bodyBase64: body.subarray(0, 400000).toString('base64'),
          status: `ERR ${e.message}`,
        }) + '\n');
        console.log(`#${id} ${req.method} ${req.url} -> ERR ${e.message}`);
        if (!res.headersSent) res.writeHead(502, { 'Content-Type': 'application/json' });
        res.end(JSON.stringify({ code: -1, msg: `capture proxy: ${e.message}` }));
      });
      if (body.length) up.write(body);
      up.end();
    });
  });

  server.listen(port, '127.0.0.1', () => {
    console.log(`capture proxy on http://127.0.0.1:${port} -> https://${upstreamHost}`);
    console.log(`log: ${jsonl}`);
  });
}

const [, , arg1, arg2, arg3] = process.argv;
if (arg1 === '--dump') dump(arg2, process.argv[4]);
else serve(Number(arg1 || 18899), arg2 || 'www.workbuddy.cn');
