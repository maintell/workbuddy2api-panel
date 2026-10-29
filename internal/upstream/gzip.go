// gzip.go 出站 chat body 压缩。
//
// 依据（2026-10-29 桌面端 TLS 中间人实抓，WorkBuddy 5.9.3 / CLI 2.165.0）：
// 主对话的 `/v2/chat/completions` 请求恒带 `Content-Encoding: gzip`，
// body 55 618 字节在线上是压缩体（解压后 133 729 字节）。上游按头解压，
// 明文 body 属于可识别的指纹差异。
package upstream

import (
	"bytes"
	"compress/gzip"
)

// gzipBody 返回压缩后的 body 与是否压缩成功。
// 空 body / 压缩失败一律返回 (src, false)：宁可退回明文，也不因为压缩问题中断请求。
func gzipBody(src []byte) ([]byte, bool) {
	if len(src) == 0 {
		return src, false
	}
	var buf bytes.Buffer
	zw, err := gzip.NewWriterLevel(&buf, gzip.BestSpeed)
	if err != nil {
		return src, false
	}
	if _, err := zw.Write(src); err != nil {
		_ = zw.Close()
		return src, false
	}
	if err := zw.Close(); err != nil {
		return src, false
	}
	return buf.Bytes(), true
}
