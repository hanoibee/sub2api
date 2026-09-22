package requestarchive

import (
	"bytes"
	"encoding/json"
)

// 一次网络读取不等于一个 SSE 事件。内存中只保留当前事件，
// 不受 Scanner Token 长度限制，也不将网络读取块误当作 JSON 事件。
func (c *capture) acceptSSE(data []byte) {
	for len(data) > 0 {
		pos := bytes.IndexByte(data, '\n')
		if pos < 0 {
			c.line = append(c.line, data...)
			return
		}
		c.line = append(c.line, data[:pos]...)
		line := c.line
		if len(line) > 0 && line[len(line)-1] == '\r' {
			line = line[:len(line)-1]
		}
		c.sseLine(line)
		c.line = c.line[:0]
		data = data[pos+1:]
	}
}
func (c *capture) sseLine(line []byte) {
	if len(line) == 0 {
		c.flushEvent()
		return
	}
	if !bytes.Equal(line, []byte("data")) && !bytes.HasPrefix(line, []byte("data:")) {
		return
	}
	var value []byte
	if len(line) > 4 {
		value = line[5:]
		if len(value) > 0 && value[0] == ' ' {
			value = value[1:]
		}
	}
	if c.hasData {
		c.event = append(c.event, '\n')
	}
	c.event = append(c.event, value...)
	c.hasData = true
}
func (c *capture) flushEvent() {
	if !c.hasData {
		return
	}
	raw := c.event
	switch {
	case bytes.Equal(raw, []byte("[DONE]")):
		c.write(raw)
		c.write([]byte{'\n'})
		c.terminal = true
	case json.Valid(raw) && !bytes.ContainsAny(raw, "\r\n"):
		c.observeUsage(raw)
		// 当前归档策略不做脱敏，每个事件按上游原始 JSON 保存一行。
		c.write(raw)
		c.write([]byte{'\n'})
	default:
		// 非 JSON 或多行 data 载荷不属于首版归档范围。不得擅自改写，
		// 也不得把这种情况误记为完整归档。
		c.invalid = true
	}
	c.hasData = false
	c.event = c.event[:0]
}
