package main

import (
	"bytes"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCLIMultipartFileBufferCompatibility(t *testing.T) {
	for _, size := range []int{0, 32, 511, 512, 513, 1024, 64 << 10, 4 << 20} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			data := bytes.Repeat([]byte("x"), size)
			path := filepath.Join(t.TempDir(), "synthetic.bin")
			if err := os.WriteFile(path, data, 0o600); err != nil {
				t.Fatal(err)
			}
			r, err := (cli{}).parseCall([]string{"POST", "/v1/test", "-F", "kind=合成", "-F", "file=@" + path, "-F", "empty=", "-F", "file=@" + path, "-F", "kind=尾部"})
			if err != nil || len(r.body) > maxCLIRequestBytes {
				t.Fatalf("构造 multipart 失败：长度=%d，错误=%v", len(r.body), err)
			}
			mediaType, params, err := mime.ParseMediaType(r.contentType)
			if err != nil || mediaType != "multipart/form-data" {
				t.Fatalf("multipart 类型变化：%q，错误=%v", r.contentType, err)
			}
			reader := multipart.NewReader(bytes.NewReader(r.body), params["boundary"])
			for _, want := range []struct {
				name, filename string
				data           []byte
			}{
				{"kind", "", []byte("合成")}, {"file", filepath.Base(path), data},
				{"empty", "", nil}, {"file", filepath.Base(path), data}, {"kind", "", []byte("尾部")},
			} {
				part, err := reader.NextPart()
				if err != nil {
					t.Fatal(err)
				}
				got, err := io.ReadAll(part)
				if err != nil || part.FormName() != want.name || part.FileName() != want.filename || !bytes.Equal(got, want.data) {
					t.Fatalf("表单字段顺序或内容变化：字段=%q，文件名=%q，长度=%d，错误=%v", part.FormName(), part.FileName(), len(got), err)
				}
			}
			if _, err := reader.NextPart(); err != io.EOF {
				t.Fatalf("multipart 尾部变化：%v", err)
			}
		})
	}
}

func TestCLIMultipartFileBufferBounds(t *testing.T) {
	path := filepath.Join(t.TempDir(), "synthetic.bin")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	args := []string{"POST", "/v1/test", "-F", "file=@" + path}
	empty, err := (cli{}).parseCall(args)
	if err != nil {
		t.Fatal(err)
	}
	overhead := len(empty.body)
	for _, tc := range []struct {
		name string
		size int
		want string
	}{
		{"完整请求恰好上限", maxCLIRequestBytes - overhead, ""},
		{"闭合边界导致超限", maxCLIRequestBytes - overhead + 1, fmt.Sprintf("request exceeds %d bytes", maxCLIRequestBytes)},
		{"文件头导致超限", maxCLIRequestBytes, fmt.Sprintf("request exceeds %d bytes", maxCLIRequestBytes)},
		{"文件本身超限", maxCLIRequestBytes + 1, fmt.Sprintf("cannot read %s: request exceeds %d bytes", path, maxCLIRequestBytes)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := os.WriteFile(path, bytes.Repeat([]byte("x"), tc.size), 0o600); err != nil {
				t.Fatal(err)
			}
			r, err := (cli{}).parseCall(args)
			if tc.want == "" {
				if err != nil || len(r.body) != maxCLIRequestBytes {
					t.Fatalf("上限内请求变化：长度=%d，错误=%v", len(r.body), err)
				}
			} else if err == nil || err.Error() != tc.want || r.body != nil || r.contentType != "" {
				t.Fatalf("超限错误或部分正文变化：长度=%d，类型=%q，错误=%v", len(r.body), r.contentType, err)
			}
		})
	}
}

func TestCLIMultipartFileBufferReadFailures(t *testing.T) {
	for _, path := range []string{filepath.Join(t.TempDir(), "missing.bin"), t.TempDir()} {
		r, err := (cli{}).parseCall([]string{"POST", "/v1/test", "-F", "kind=合成", "-F", "file=@" + path})
		if err == nil || !strings.HasPrefix(err.Error(), "cannot read "+path+": ") || r.body != nil || r.contentType != "" {
			t.Fatalf("文件读取失败处理变化：长度=%d，类型=%q，错误=%v", len(r.body), r.contentType, err)
		}
	}
}

func benchmarkCLIMultipartFileBuffer(b *testing.B, size int, files int) {
	path := filepath.Join(b.TempDir(), "synthetic.bin")
	if err := os.WriteFile(path, bytes.Repeat([]byte("x"), size), 0o600); err != nil {
		b.Fatal(err)
	}
	args := []string{"POST", "/v1/test"}
	for range files {
		args = append(args, "-F", "file=@"+path)
	}
	b.SetBytes(int64(size * files))
	b.ReportAllocs()
	for b.Loop() {
		r, err := (cli{}).parseCall(args)
		if err != nil || len(r.body) <= size*files || len(r.body) > maxCLIRequestBytes {
			b.Fatalf("构造文件上传失败：长度=%d，错误=%v", len(r.body), err)
		}
	}
}

func TestCLIMultipartFileBufferAllocationBudget(t *testing.T) {
	const size = 8 << 20
	result := testing.Benchmark(func(b *testing.B) { benchmarkCLIMultipartFileBuffer(b, size, 1) })
	if got, budget := result.AllocedBytesPerOp(), int64(size)*3/2+128<<10; got > budget {
		t.Fatalf("文件上传构造分配 %d B，预算 %d B", got, budget)
	}
}

func BenchmarkCLIMultipartFileBuffer(b *testing.B) {
	for _, tc := range []struct {
		name        string
		size, files int
	}{
		{"32B", 32, 1}, {"512B", 512, 1}, {"513B", 513, 1}, {"1KiB", 1024, 1},
		{"8MiB", 8 << 20, 1}, {"15MiB", 15 << 20, 1}, {"两个4MiB文件", 4 << 20, 2},
	} {
		b.Run(tc.name, func(b *testing.B) { benchmarkCLIMultipartFileBuffer(b, tc.size, tc.files) })
	}
}
