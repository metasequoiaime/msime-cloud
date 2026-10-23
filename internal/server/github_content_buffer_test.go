package server

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/metasequoiaime/MSIME-Backend/internal/githubapp"
)

type githubContentFixture struct {
	server          *Server
	client          *githubapp.Client
	pull            dictPull
	files           []dictFile
	check           bool
	requests        int
	responsesClosed int
}

type githubContentResponseBody struct {
	io.Reader
	closed *int
}

func (body *githubContentResponseBody) Close() error {
	*body.closed++
	return nil
}

func newGitHubContentFixture(tb testing.TB, content string) *githubContentFixture {
	tb.Helper()
	fixture := &githubContentFixture{check: true,
		server: &Server{config: Config{Admin: AdminConfig{GitHub: AdminGitHubConfig{DictionaryRepo: "synthetic/dictionary"}}}},
		files:  []dictFile{{kind: kindEnglish, head: content + "\nremoved\tRemoved\t1", blobSHA: "synthetic-blob", added: []int{strings.Count(content, "\n") + 1}}},
	}
	fixture.pull.Head.SHA = "synthetic-before"
	fixture.pull.Head.Ref = "community-words/synthetic"
	fixture.pull.Title = submissionTitle(map[string]int{})
	// 按旧实现独立固定完整请求字节，包含 base64、字段顺序和空文件的字符串语义。
	want, err := json.Marshal(map[string]string{
		"message": "chore(custom): drop 1 entries during review",
		"content": base64.StdEncoding.EncodeToString([]byte(content)),
		"sha":     "synthetic-blob",
		"branch":  fixture.pull.Head.Ref,
	})
	if err != nil {
		tb.Fatal(err)
	}
	fixture.client = &githubapp.Client{HTTP: &http.Client{Transport: qqTestTransport(func(r *http.Request) (*http.Response, error) {
		defer r.Body.Close()
		if r.Method != http.MethodPut || r.URL.String() != "https://api.github.com/repos/synthetic/dictionary/contents/custom/english.txt" ||
			r.Header.Get("Content-Type") != "application/json" || r.Header.Get("Authorization") != "Bearer synthetic-token" || r.ContentLength != int64(len(want)) {
			return nil, fmt.Errorf("文件写入的方法、目标、请求头或长度发生变化")
		}
		if fixture.check {
			raw, err := io.ReadAll(r.Body)
			if err != nil || !bytes.Equal(raw, want) {
				return nil, fmt.Errorf("文件写入的 JSON 字节发生变化：%v", err)
			}
		} else if n, err := io.Copy(io.Discard, r.Body); err != nil || n != int64(len(want)) {
			return nil, fmt.Errorf("文件写入未完整传输：长度=%d，错误=%v", n, err)
		}
		fixture.requests++
		const response = `{"commit":{"sha":"synthetic-after","parents":[{"sha":"synthetic-before"}]}}`
		return &http.Response{StatusCode: http.StatusOK, ContentLength: int64(len(response)), Header: make(http.Header),
			Body: &githubContentResponseBody{Reader: strings.NewReader(response), closed: &fixture.responsesClosed}}, nil
	})}}
	return fixture
}

func (fixture *githubContentFixture) run(tb testing.TB) {
	tb.Helper()
	head, title, removed, err := fixture.server.trimFiles(tb.Context(), fixture.client, "synthetic-token", fixture.pull, fixture.files, nil)
	if err != nil || head != "synthetic-after" || title != fixture.pull.Title || removed != 1 {
		tb.Fatalf("文件修剪结果发生变化：head=%q title=%q removed=%d err=%v", head, title, removed, err)
	}
}

func TestGitHubContentRequestBytes(t *testing.T) {
	for _, content := range []string{"", "a", "ab", "abc", "abcd", "中文\t示例\t1\r\n", "\x00\xff\xfe<&>\"\\", strings.Repeat("x", 64<<10)} {
		t.Run(fmt.Sprintf("bytes=%d", len(content)), func(t *testing.T) {
			fixture := newGitHubContentFixture(t, content)
			fixture.run(t)
			if fixture.requests != 1 || fixture.responsesClosed != 1 {
				t.Fatal("文件写入或响应关闭次数不正确")
			}
		})
	}
}

func benchmarkGitHubContentBuffer(b *testing.B, size int) {
	fixture := newGitHubContentFixture(b, strings.Repeat("x", size))
	fixture.run(b)
	fixture.check = false
	fixture.requests, fixture.responsesClosed = 0, 0
	b.ReportAllocs()
	b.SetBytes(int64(size))
	for b.Loop() {
		fixture.run(b)
	}
	if fixture.requests != b.N || fixture.responsesClosed != b.N {
		b.Fatal("文件写入或响应关闭次数不正确")
	}
}

func BenchmarkGitHubContentBuffer(b *testing.B) {
	for _, size := range []int{0, 64 << 10, 1 << 20, 4 << 20} {
		b.Run(fmt.Sprintf("bytes=%d", size), func(b *testing.B) { benchmarkGitHubContentBuffer(b, size) })
	}
}

func TestGitHubContentBufferBudget(t *testing.T) {
	const size = 1 << 20
	content := strings.Repeat("x", size)
	var body map[string]any
	result := testing.Benchmark(func(b *testing.B) {
		for b.Loop() {
			body = githubContentBody(content, "合成提交说明", "synthetic-blob", "synthetic-branch")
		}
	})
	if body == nil {
		t.Fatal("文件写入正文为空")
	}
	// 只量三处生产调用共用的请求体构造，排除 JSON 缓冲池在竞态模式下随机丢弃条目的影响。
	// 允许原文到字节切片的一次安全复制，以及字段装箱等固定开销。
	const budget = size + 16<<10
	t.Logf("文件写入请求体构造分配 %d 字节，预算 %d 字节", result.AllocedBytesPerOp(), budget)
	if got := result.AllocedBytesPerOp(); got > budget {
		t.Fatalf("文件写入请求体构造分配 %d 字节，超过 %d 字节预算", got, budget)
	}
}
