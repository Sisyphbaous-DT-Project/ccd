package main

// 按“帧”读写 SSE：一个帧是到空行为止的一整块原始字节。
// 不改写的帧整段原样写出，所以 event: 行、注释行、字段顺序、行尾都不会被动到。

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
)

type sseFrame struct {
	Raw     []byte // 原始字节（含末尾空行）
	Event   []byte // 原始的 "event: ...\n" 行（可能有多行），没有则为 nil
	Data    []byte // data 行的载荷，多行用 \n 连接
	HasData bool
	Err     error // 读上游出错时放在最后一帧
}

// readFrames 逐帧读取上游 SSE，读到 ctx 取消、EOF 或出错为止。
func readFrames(ctx context.Context, r io.Reader, out chan<- sseFrame) {
	defer close(out)
	br := bufio.NewReaderSize(r, 32<<10)
	var cur, ev, data []byte
	var hasData bool

	emit := func() bool {
		if len(cur) == 0 {
			return true
		}
		f := sseFrame{Raw: cur, Event: ev, Data: data, HasData: hasData}
		cur, ev, data, hasData = nil, nil, nil, false
		select {
		case out <- f:
			return true
		case <-ctx.Done():
			return false
		}
	}

	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			cur = append(cur, line...)
			trimmed := bytes.TrimRight(line, "\r\n")
			switch {
			case len(trimmed) == 0:
				if !emit() {
					return
				}
			case bytes.HasPrefix(trimmed, []byte("event:")):
				ev = append(ev, line...)
			case bytes.HasPrefix(trimmed, []byte("data:")):
				v := bytes.TrimSpace(trimmed[5:])
				if hasData {
					data = append(data, '\n')
				}
				data = append(data, v...)
				hasData = true
			}
		}
		if err != nil {
			if len(cur) > 0 && !emit() {
				return
			}
			if err != nil && err != io.EOF && ctx.Err() == nil {
				select {
				case out <- sseFrame{Err: err}:
				case <-ctx.Done():
				}
			}
			return
		}
	}
}

// writeFrame 原样写出一帧。
func writeFrame(w io.Writer, f sseFrame) error {
	if len(f.Raw) == 0 {
		return nil // 空写会让 io.Pipe 卡住，兜一层
	}
	_, err := w.Write(f.Raw)
	return err
}

// writeDataFrame 用原帧的 event: 行重新拼一帧（data 部分换成 v）。
func writeDataFrame(w io.Writer, ev []byte, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	buf := make([]byte, 0, len(ev)+len(b)+8)
	buf = append(buf, ev...)
	buf = append(buf, "data: "...)
	buf = append(buf, b...)
	buf = append(buf, '\n', '\n')
	_, err = w.Write(buf)
	return err
}

// writeDataRaw 同 writeDataFrame，但 data 是现成的 JSON 字节。
func writeDataRaw(w io.Writer, ev, body []byte) error {
	buf := make([]byte, 0, len(ev)+len(body)+8)
	buf = append(buf, ev...)
	buf = append(buf, "data: "...)
	buf = append(buf, body...)
	buf = append(buf, '\n', '\n')
	_, err := w.Write(buf)
	return err
}
