package skins

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func previousSkinReadFile(fsys fs.FS, name string) ([]byte, error) {
	f, err := fsys.Open(name)
	if err != nil {
		return nil, ErrNotFound
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > 4<<20 {
		return nil, ErrInvalid
	}
	raw, err := io.ReadAll(io.LimitReader(f, (4<<20)+1))
	if err != nil || len(raw) > 4<<20 {
		return nil, ErrInvalid
	}
	return raw, nil
}

func TestSkinReadFileCompatibility(t *testing.T) {
	root := t.TempDir()
	for _, tc := range []struct {
		name string
		data []byte
	}{
		{"空文件", nil},
		{"短文件", []byte("合成皮肤资源")},
		{"大资源", bytes.Repeat([]byte("x"), 1536<<10)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(root, tc.name)
			if err := os.WriteFile(path, tc.data, 0600); err != nil {
				t.Fatal(err)
			}
			fsys := os.DirFS(root)
			got, gotErr := readFile(fsys, tc.name)
			want, wantErr := previousSkinReadFile(fsys, tc.name)
			if !reflect.DeepEqual(got, want) || (gotErr == nil) != (wantErr == nil) {
				t.Fatalf("皮肤资源读取变化：结果长度=%d/%d，错误=%v/%v", len(got), len(want), gotErr, wantErr)
			}
		})
	}
}

func benchmarkSkinReadFile(b *testing.B, data []byte, read func(fs.FS, string) ([]byte, error)) {
	root := b.TempDir()
	if err := os.WriteFile(filepath.Join(root, "resource.png"), data, 0600); err != nil {
		b.Fatal(err)
	}
	fsys := os.DirFS(root)
	b.ReportAllocs()
	b.SetBytes(int64(len(data)))
	for b.Loop() {
		got, err := read(fsys, "resource.png")
		if err != nil || !bytes.Equal(got, data) {
			b.Fatalf("皮肤资源读取失败：长度=%d，错误=%v", len(got), err)
		}
	}
}

func TestSkinReadFileAllocationBudget(t *testing.T) {
	data := bytes.Repeat([]byte("x"), 1536<<10)
	result := testing.Benchmark(func(b *testing.B) { benchmarkSkinReadFile(b, data, readFile) })
	// 已知大小的资源只需正文缓冲和文件操作开销。
	if got, budget := result.AllocedBytesPerOp(), int64(len(data))*3/2+64<<10; got > budget {
		t.Fatalf("皮肤资源读取分配 %d B，预算为 %d B", got, budget)
	}
}

func BenchmarkSkinReadFile(b *testing.B) {
	benchmarkSkinReadFileSizes(b, readFile)
}

func BenchmarkPreviousSkinReadFile(b *testing.B) {
	benchmarkSkinReadFileSizes(b, previousSkinReadFile)
}

func benchmarkSkinReadFileSizes(b *testing.B, read func(fs.FS, string) ([]byte, error)) {
	for _, tc := range []struct {
		name string
		size int
	}{
		{"短资源", 512},
		{"1536KiB资源", 1536 << 10},
		{"4MiB资源", 4 << 20},
	} {
		b.Run(tc.name, func(b *testing.B) {
			benchmarkSkinReadFile(b, bytes.Repeat([]byte("x"), tc.size), read)
		})
	}
}

type skinReadTestFS struct {
	file    *skinReadTestFile
	openErr error
}

func (f skinReadTestFS) Open(string) (fs.File, error) { return f.file, f.openErr }

type skinReadTestInfo struct {
	fs.FileInfo
	size int64
	mode fs.FileMode
}

func (i skinReadTestInfo) Size() int64       { return i.size }
func (i skinReadTestInfo) Mode() fs.FileMode { return i.mode }

type skinReadTestFile struct {
	reader    *bytes.Reader
	info      skinReadTestInfo
	statErr   error
	readErr   error
	readBytes int
	closed    bool
}

func (f *skinReadTestFile) Stat() (fs.FileInfo, error) { return f.info, f.statErr }
func (f *skinReadTestFile) Close() error               { f.closed = true; return nil }
func (f *skinReadTestFile) Read(p []byte) (int, error) {
	n, err := f.reader.Read(p)
	f.readBytes += n
	if f.readErr != nil {
		return n, f.readErr
	}
	return n, err
}

func TestSkinReadFileBoundaries(t *testing.T) {
	failure := errors.New("合成读取错误")
	for _, tc := range []struct {
		name                               string
		data                               []byte
		size                               int64
		mode                               fs.FileMode
		openErr, statErr, readErr, wantErr error
		wantRead                           int
	}{
		{name: "准确大小", data: []byte("abc"), size: 3, wantRead: 3},
		{name: "读取前增长", data: []byte("abcdef"), size: 1, wantRead: 6},
		{name: "读取前缩短", data: []byte("a"), size: 10, wantRead: 1},
		{name: "零大小回退", data: []byte("abc"), wantRead: 3},
		{name: "负大小回退", data: []byte("abc"), size: -1024, wantRead: 3},
		{name: "空文件"},
		{name: "缩短为空文件", size: 10},
		{name: "大小超限直接拒绝", data: []byte("abc"), size: (4 << 20) + 1, wantErr: ErrInvalid},
		{name: "实际超限限制读取", data: bytes.Repeat([]byte("x"), (4<<20)+10), size: 1, wantErr: ErrInvalid, wantRead: (4 << 20) + 1},
		{name: "目录拒绝读取", size: 1, mode: fs.ModeDir, wantErr: ErrInvalid},
		{name: "打开失败", openErr: failure, wantErr: ErrNotFound},
		{name: "属性读取失败", statErr: failure, wantErr: ErrInvalid},
		{name: "读取失败", size: 1, readErr: failure, wantErr: ErrInvalid},
		{name: "部分数据伴随错误", data: []byte("abc"), size: 3, readErr: failure, wantErr: ErrInvalid, wantRead: 3},
		{name: "回退读取失败", data: []byte("abc"), readErr: failure, wantErr: ErrInvalid, wantRead: 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			file := &skinReadTestFile{reader: bytes.NewReader(tc.data), info: skinReadTestInfo{size: tc.size, mode: tc.mode}, statErr: tc.statErr, readErr: tc.readErr}
			got, err := readFile(skinReadTestFS{file: file, openErr: tc.openErr}, "resource.png")
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("错误=%v，预期=%v", err, tc.wantErr)
			}
			if tc.wantErr == nil {
				if got == nil || !bytes.Equal(got, tc.data) {
					t.Fatalf("读取结果不符：长度=%d，预期=%d", len(got), len(tc.data))
				}
			} else if got != nil {
				t.Fatal("读取失败应返回 nil 正文")
			}
			if file.readBytes != tc.wantRead {
				t.Fatalf("实际读取 %d 字节，预期 %d", file.readBytes, tc.wantRead)
			}
			if file.closed != (tc.openErr == nil) {
				t.Fatalf("文件关闭状态=%v", file.closed)
			}
		})
	}
}
