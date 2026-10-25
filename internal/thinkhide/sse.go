package thinkhide

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
)

// StreamContentDeltas 读取 OpenAI 兼容的 SSE 流，把每个 choices[0].delta.content 的文字交给 fn；
// 遇到 [DONE]、EOF、ctx 取消，或 fn 返回 false（消费方不想要更多）时结束。
func StreamContentDeltas(ctx context.Context, body io.Reader, fn func(string) bool) error {
	br := bufio.NewReaderSize(body, 32<<10)
	for {
		line, err := br.ReadBytes('\n')
		line = bytes.TrimSpace(line)
		if bytes.HasPrefix(line, []byte("data:")) {
			payload := bytes.TrimSpace(line[5:])
			if string(payload) == "[DONE]" {
				return nil
			}
			var chunk struct {
				Choices []struct {
					Delta struct {
						Content string `json:"content"`
					} `json:"delta"`
				} `json:"choices"`
			}
			if json.Unmarshal(payload, &chunk) == nil && len(chunk.Choices) > 0 {
				if c := chunk.Choices[0].Delta.Content; c != "" && !fn(c) {
					return nil
				}
			}
		}
		if err != nil {
			if err == io.EOF || ctx.Err() != nil {
				return nil
			}
			return err
		}
	}
}
