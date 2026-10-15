package server

import (
	"context"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/metasequoiaime/MSIME-Backend/internal/githubapp"
)

func TestQQDuplicateCandidateBodyMatchesPreviousPrefix(t *testing.T) {
	s, fake, _ := qqServer(t)
	for _, body := range []string{
		"", "合成正文", strings.Repeat("水", qqBotCandidateRunes-1),
		strings.Repeat("水", qqBotCandidateRunes), strings.Repeat("水", qqBotCandidateRunes+1),
		strings.Repeat("a", 65536), strings.Repeat("水😀a\u0301\x00", 16384),
		"\xff\xfe尾", strings.Repeat("水", qqBotCandidateRunes-1) + "\xff尾",
		strings.Repeat("水", qqBotCandidateRunes-1) + "\xe4\xb8",
	} {
		fake.searchItems = []map[string]any{{"number": 7, "title": "合成标题", "body": body}}
		fake.duplicate = `{"duplicate":7}`
		fake.chatCalls = nil
		fake.chatBodies = nil
		got, err := s.qqDuplicate(context.Background(), s.adminGitHub, "synthetic-token", "o/r", qqReport{Keywords: "合成"})
		if err != nil || got != 7 {
			t.Fatalf("去重结果=%d，错误=%v", got, err)
		}
		index := slices.Index(fake.chatCalls, "duplicate")
		if index < 0 {
			t.Fatal("没有调用去重模型")
		}
		runes := []rune(body)
		want := string(runes[:min(len(runes), qqBotCandidateRunes)])
		_, candidate, ok := strings.Cut(fake.chatBodies[index], "#7 合成标题\n")
		if !ok || candidate != want {
			t.Fatalf("候选正文不兼容：输入字节数=%d，结果字节数=%d，期望字节数=%d", len(body), len(candidate), len(want))
		}
	}
}

func TestQQDuplicateAvoidsFullRuneAllocation(t *testing.T) {
	// 预编码响应并使用同步 transport，避免 HTTP/TLS 在 race 模式下的额外分配干扰预算。
	measure := func(number int64) testing.BenchmarkResult {
		search := `{"items":[{"number":` + strconv.FormatInt(number, 10) + `,"title":"合成标题","body":"` + strings.Repeat("a", 65536) + `"}]}`
		chat := `{"choices":[{"message":{"role":"assistant","content":"{\"duplicate\":7}"},"finish_reason":"stop"}]}`
		client := &http.Client{Transport: qqTestTransport(func(r *http.Request) (*http.Response, error) {
			switch {
			case r.Method == http.MethodGet && r.URL.Path == "/search/issues":
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(search)), Header: make(http.Header)}, nil
			case number == 7 && r.Method == http.MethodPost && r.URL.Path == "/v1/chat/completions":
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(chat)), Header: make(http.Header)}, nil
			default:
				return &http.Response{StatusCode: http.StatusNotFound, Body: io.NopCloser(strings.NewReader("")), Header: make(http.Header)}, nil
			}
		})}
		s := &Server{
			config: Config{Chat: ChatEndpoint{Endpoint: Endpoint{URL: "https://chat.example.test/v1/chat/completions"}}},
			qq:     &qqBot{config: QQBotConfig{Model: "synthetic-model"}},
			client: client,
		}
		github := &githubapp.Client{APIURL: "https://github.example.test", HTTP: client}
		result := testing.Benchmark(func(b *testing.B) {
			for b.Loop() {
				got, err := s.qqDuplicate(context.Background(), github, "synthetic-token", "o/r", qqReport{Keywords: "合成"})
				if err != nil || got != number {
					t.Errorf("去重结果=%d，期望=%d，错误=%v", got, number, err)
					b.FailNow()
				}
			}
		})
		if result.N == 0 {
			t.Fatal("基准未执行去重调用")
		}
		return result
	}
	// 编号 0 的同尺寸响应仍完整读取和解码正文，扣除这部分必须的分配后约束候选准备开销。
	baseline, candidate := measure(0), measure(7)
	additional := candidate.AllocedBytesPerOp() - baseline.AllocedBytesPerOp()
	t.Logf("基线=%d B/op，候选=%d B/op，额外=%d B/op", baseline.AllocedBytesPerOp(), candidate.AllocedBytesPerOp(), additional)
	if additional > 64<<10 {
		t.Fatalf("候选准备额外分配 %d 字节，超过 64 KiB 预算", additional)
	}
}
