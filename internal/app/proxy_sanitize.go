package app

import (
	"io"
)

// controlSanitizingReader 在读取层做位置感知的控制字符清洗。
// 状态跨 Read 调用保持(中继为单读者串行使用, 无需加锁)。
type controlSanitizingReader struct {
	src      io.Reader
	inString bool
	escaped  bool
}

func (r *controlSanitizingReader) Read(p []byte) (int, error) {
	n, err := r.src.Read(p)
	for i := 0; i < n; i++ {
		c := p[i]
		// 按行重置状态机(P3-35): 状态机逐字节翻转 inString 却不校验所在行
		// 是否为合法 JSON —— 含奇数个双引号的非 JSON 数据行会让 inString 卡死,
		// 其后所有 \n 都被当成"字符串内控制字符"替换为空格, 帧边界全部粘连,
		// 直到流结束。行边界必须永远保留: 遇到 \n 先归零状态再原样放行。
		if c == '\n' {
			r.inString = false
			r.escaped = false
			continue
		}
		if r.inString {
			if r.escaped {
				r.escaped = false
				continue
			}
			switch c {
			case '\\':
				r.escaped = true
			case '"':
				r.inString = false
			default:
				if c < 0x20 {
					p[i] = ' '
				}
			}
			continue
		}
		if c == '"' {
			r.inString = true
			continue
		}
		if c < 0x20 && c != '\t' && c != '\n' && c != '\r' {
			p[i] = ' '
		}
	}
	return n, err
}

// sanitizeJSONControlChars 同上, 作用于已切好的一段文本(兜底路径)。
func sanitizeJSONControlChars(b []byte) ([]byte, bool) {
	inStr, esc, dirty := false, false, false
	for _, c := range b {
		if inStr {
			if esc {
				esc = false
				continue
			}
			switch c {
			case '\\':
				esc = true
			case '"':
				inStr = false
			default:
				if c < 0x20 {
					dirty = true
				}
			}
			continue
		}
		if c == '"' {
			inStr = true
			continue
		}
		if c < 0x20 && c != '\t' && c != '\n' && c != '\r' {
			dirty = true
		}
	}
	if !dirty {
		return b, false
	}
	out := make([]byte, len(b))
	inStr, esc = false, false
	for i, c := range b {
		if inStr {
			if esc {
				out[i] = c
				esc = false
				continue
			}
			switch c {
			case '\\':
				out[i] = c
				esc = true
			case '"':
				out[i] = c
				inStr = false
			default:
				if c < 0x20 {
					out[i] = ' '
				} else {
					out[i] = c
				}
			}
			continue
		}
		if c == '"' {
			out[i] = c
			inStr = true
			continue
		}
		if c < 0x20 && c != '\t' && c != '\n' && c != '\r' {
			out[i] = ' '
			continue
		}
		out[i] = c
	}
	return out, true
}
