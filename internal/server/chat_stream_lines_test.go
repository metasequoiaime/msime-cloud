package server

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"testing/iotest"
	"time"
)

func TestChatStreamAcceptsSSELineEndingsAndBOM(t *testing.T) {
	for _, tc := range []struct {
		name, prefix, first, second string
	}{
		{"LF", "", "\n", "\n"},
		{"CRLF", "", "\r\n", "\r\n"},
		{"CR", "", "\r", "\r"},
		{"混合换行", "", "\r", "\n"},
		{"BOM与LF", "\ufeff", "\n", "\n"},
		{"BOM与CR", "\ufeff", "\r", "\r"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := fixture(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				sse(w, tc.prefix+strings.ReplaceAll(contentFrame("首段"), "\n", tc.first), strings.ReplaceAll(contentFrame("尾段"), "\n", tc.second), "data: [DONE]"+tc.second+tc.second)
			})
			s.config.Admin.Enabled = true
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			live := httptest.NewServer(s)
			defer live.Close()
			resp := postStream(t, ctx, live, streamBody)
			body, err := io.ReadAll(resp.Body)
			want := contentPrefix(t, "首段") + contentPrefix(t, "尾段") + "data: [DONE]\n\n"
			if err != nil || resp.StatusCode != 200 || string(body) != want {
				t.Fatalf("合法 SSE 未完整转发：status=%d body=%q err=%v", resp.StatusCode, body, err)
			}
			if m := chatMetrics(s); m.Calls != 1 || m.Errors != 0 {
				t.Fatalf("合法 SSE 被记为失败：%+v", m)
			}
		})
	}
}

func TestChatStreamDispatchesCRBeforeMoreUpstreamBytes(t *testing.T) {
	for _, splitCRLF := range []bool{false, true} {
		name, ending, continuation := "单CR", "\r\r", ""
		if splitCRLF {
			name, ending, continuation = "跨读取CRLF", "\r\n\r", "\n"
		}
		t.Run(name, func(t *testing.T) {
			next := make(chan struct{})
			s := fixture(t, func(w http.ResponseWriter, r *http.Request) {
				// 最后的 CR 已经结束空行；上游等待客户端确认，不能再等下一个字节才转发。
				sse(w, strings.TrimSuffix(contentFrame("首段"), "\n\n")+ending)
				select {
				case <-next:
				case <-r.Context().Done():
					return
				}
				sse(w, continuation+contentFrame("尾段"), "data: [DONE]\n\n")
			})
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			live := httptest.NewServer(s)
			defer func() { cancel(); live.Close() }()
			resp := postStream(t, ctx, live, streamBody)
			reader := bufio.NewReader(resp.Body)
			if got := eventContent(t, readEvent(t, reader)); got != "首段" {
				t.Fatalf("首段被延迟或丢失：%q", got)
			}
			close(next)
			if got := eventContent(t, readEvent(t, reader)); got != "尾段" {
				t.Fatalf("跨读取的换行破坏后续事件：%q", got)
			}
			if got := readEvent(t, reader); got != "[DONE]" {
				t.Fatalf("流未正常结束：%q", got)
			}
		})
	}
}

func TestSSELineBoundariesAcrossReads(t *testing.T) {
	for _, tc := range []struct {
		name, input string
		want        []string
	}{
		{"空流", "", nil},
		{"连续空行", "\n\r\r\n", []string{"", "", ""}},
		{"混合分隔和末行", "一\r\n二\r三\n末行", []string{"一", "二", "三", "末行"}},
		{"完整事件", "data: first\r\rdata: second\r\n\r\n", []string{"data: first", "", "data: second", ""}},
		{"CR结尾", "末行\r", []string{"末行"}},
	} {
		for _, oneByte := range []bool{false, true} {
			name := tc.name + "/整段读取"
			if oneByte {
				name = tc.name + "/逐字节读取"
			}
			t.Run(name, func(t *testing.T) {
				var reader io.Reader = strings.NewReader(tc.input)
				if oneByte {
					reader = iotest.OneByteReader(reader)
				}
				scanner := bufio.NewScanner(reader)
				scanner.Split(splitSSELines())
				var got []string
				for scanner.Scan() {
					got = append(got, scanner.Text())
				}
				if err := scanner.Err(); err != nil || !slices.Equal(got, tc.want) {
					t.Fatalf("SSE 行边界不正确：got=%q want=%q err=%v", got, tc.want, err)
				}
			})
		}
	}
}

func TestChatStreamBOMIsOnlyStrippedAtStart(t *testing.T) {
	for _, tc := range []struct {
		name, stream, want string
	}{
		{"首行注释", "\ufeff: comment\r" + contentFrame("正文"), contentPrefix(t, "正文")},
		{"首行空行", "\ufeff\r" + contentFrame("正文"), contentPrefix(t, "正文")},
		{"首行只去一个", "\ufeff\ufeff" + contentFrame("忽略字段") + contentFrame("正文"), contentPrefix(t, "正文")},
		{"后续字段不去", contentFrame("正文") + "\ufeff" + contentFrame("忽略字段"), contentPrefix(t, "正文")},
		{"正文保留BOM", contentFrame("\ufeff正文"), contentPrefix(t, "\ufeff正文")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := fixture(t, func(w http.ResponseWriter, r *http.Request) {
				sse(w, tc.stream, "data: [DONE]\n\n")
			})
			w := call(s, "POST", "/v1/chat/completions", streamBody)
			if w.Code != 200 || w.Body.String() != tc.want+"data: [DONE]\n\n" {
				t.Fatalf("BOM 处理范围不正确：status=%d body=%q", w.Code, w.Body.String())
			}
		})
	}
}
