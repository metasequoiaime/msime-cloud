package account

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"
)

func TestCandidateAcceptedPathsCanBeInstalledOnWindows(t *testing.T) {
	if runtime.GOOS != "windows" {
		// Windows 的设备名和目录尾点规范化只在真实 Windows 文件系统上验证。
		t.Skip("需要真实 Windows 文件系统")
	}
	for _, tc := range []struct{ preview, decoration string }{
		{"preview.png", "assets/deco.jpg"},
		{"conifer.png", "com0/deco.jpg"},
		{"CON.png", "assets/deco.jpg"},
		{"preview.png", "assets/NUL.jpg"},
		{"AUX/preview.png", "assets/deco.jpg"},
		{"COM1/preview.png", "assets/deco.jpg"},
		{"preview.png", "LPT9.JPG"},
		{"images/preview.png", "images./preview.png"},
		{".../preview.png", "assets/deco.jpg"},
	} {
		t.Run(tc.preview+"+"+tc.decoration, func(t *testing.T) {
			manifest, files := candidatePathFixture(t, tc.preview, tc.decoration)
			if tc.decoration == "images./preview.png" {
				files[tc.decoration] = files[tc.preview]
			}
			if code := candidatePipeline(manifest, files); code != "" {
				if code != "candidate_skin_file_path" {
					t.Fatal("路径在无关阶段失败", code)
				}
				return
			}
			// 短目录模拟客户端通常使用的绝对安装路径，避免长路径前缀改变设备名语义。
			root, err := os.MkdirTemp("", "msime-cp-")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := os.RemoveAll(root); err != nil {
					t.Error(err)
				}
			})
			files["skin.toml"] = []byte(manifest)
			paths := make([]string, 0, len(files))
			for path := range files {
				paths = append(paths, path)
			}
			slices.Sort(paths)
			for _, path := range paths {
				destination := filepath.Join(root, filepath.FromSlash(path))
				if len(destination) >= 248 {
					t.Fatal("测试路径过长，无法模拟短绝对路径", destination)
				}
				if err := os.MkdirAll(filepath.Dir(destination), 0700); err != nil {
					t.Fatal("服务器接受的路径无法创建父目录", path, err)
				}
				// 与客户端 OpenOptions::create_new(true) 一样，不能覆盖同名文件。
				file, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
				if err != nil {
					t.Fatal("服务器接受的路径无法安装", path, err)
				}
				_, writeErr := file.Write(files[path])
				closeErr := file.Close()
				if writeErr != nil || closeErr != nil {
					t.Fatal("服务器接受的文件无法写入", path, writeErr, closeErr)
				}
				data, err := os.ReadFile(destination)
				if err != nil || !bytes.Equal(data, files[path]) {
					t.Fatal("安装后的文件与包不一致", path, err)
				}
			}
			var installed []string
			if err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
				if err != nil {
					return err
				}
				if !entry.IsDir() {
					relative, err := filepath.Rel(root, path)
					if err != nil {
						return err
					}
					installed = append(installed, filepath.ToSlash(relative))
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			slices.Sort(installed)
			if !slices.Equal(installed, paths) {
				t.Fatal("文件系统改变了服务器接受的路径", installed, paths)
			}
		})
	}
}
