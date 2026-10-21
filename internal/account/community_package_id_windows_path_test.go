package account

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestCommunityPackageIDsKeepWindowsDirectoryNames(t *testing.T) {
	for _, kind := range []string{"candidate-skins", "plugins"} {
		for _, id := range []string{"shared", "shared.v1", "shared-v1", "shared_v1", "con", "nul", "nul.v1", "null", "nul0", "con.v1", "aux.v1", "com1.v1", "lpt9.v1", "aux", "com1", "lpt9", "shared.", "shared..", "default.", "fluent."} {
			t.Run(kind+"/"+id, func(t *testing.T) {
				var manifest, code string
				if kind == "candidate-skins" {
					var files map[string][]byte
					manifest, files = candidateFixture(t, id)
					code = candidatePipeline(manifest, files)
				} else {
					manifest = strings.Replace(pluginCommandManifest, `id = "community-dates"`, `id = "`+id+`"`, 1)
					_, code = validPluginArchive(pluginZip(t, pluginFile{name: "plugin.toml", data: manifest}))
				}
				if code != "" {
					if code != "invalid_candidate_skin_package" && code != "invalid_plugin_manifest" {
						t.Fatal("包在无关阶段被拒绝", code)
					}
					return
				}
				if runtime.GOOS != "windows" {
					// 目录名规范化只在真实 Windows 上验证；其他平台不能证明可安装性。
					t.Skip("服务器已接受该 ID，需要真实 Windows 文件系统验证")
				}
				root, err := os.MkdirTemp("", "msime-id-")
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if err := os.RemoveAll(root); err != nil {
						t.Error(err)
					}
				})
				target := filepath.Join(root, id)
				if len(target) >= 248 {
					t.Fatal("测试需要短绝对目录")
				}
				if kind == "candidate-skins" {
					// 候选皮肤先以包 ID 创建暂存目录，安装后目录扫描必须能读回同一 ID。
					if err := os.Mkdir(target, 0700); err != nil {
						t.Fatal("服务器接受的 ID 无法创建目录", err)
					}
				} else {
					// 插件以唯一暂存目录校验，最后重命名为清单 ID。
					staging := filepath.Join(root, ".staging")
					if err := os.Mkdir(staging, 0700); err != nil {
						t.Fatal(err)
					}
					if err := os.Rename(staging, target); err != nil {
						t.Fatal("服务器接受的 ID 无法重命名安装", err)
					}
				}
				entries, err := os.ReadDir(root)
				if err != nil || len(entries) != 1 {
					t.Fatal("读取安装目录失败", entries, err)
				}
				if entries[0].Name() != id {
					t.Fatalf("Windows 将服务器接受的包 ID %q 变为目录 %q，目录扫描无法匹配清单", id, entries[0].Name())
				}
			})
		}
	}
}
