package account

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

func TestPluginArchiveRejectsReservedFileNames(t *testing.T) {
	names := []string{"CON.txt", "prn.md", "aUx.txt", "NuL.md"}
	for digit := 0; digit <= 9; digit++ {
		names = append(names, fmt.Sprintf("COM%d.txt", digit), fmt.Sprintf("lpt%d.md", digit))
	}
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			archive := pluginZip(t, pluginFile{name: "plugin.toml", data: pluginCommandManifest}, pluginFile{name: name, data: "合成说明"})
			if _, code := validPluginArchive(archive); code != "invalid_plugin_archive" {
				t.Fatal("客户端拒绝的保留文件名被服务端接受", name, code)
			}
		})
	}
	manifest := strings.ReplaceAll(pluginSoundManifest, "click.wav", "NUL.wav")
	archive := pluginZip(t, pluginFile{name: "plugin.toml", data: manifest}, pluginFile{name: "NUL.wav", data: pluginWAV}, pluginFile{name: "enter.wav", data: pluginWAV})
	if _, code := validPluginArchive(archive); code != "invalid_plugin_archive" {
		t.Fatal("清单引用的保留音频文件名被服务端接受", code)
	}
}

func TestPluginArchiveAcceptsNamesSimilarToReservedNames(t *testing.T) {
	for _, name := range []string{"CONSOLE.txt", "null.txt", "auxiliary.md", "LPT10.txt", "com10.txt", "COM1-note.md"} {
		t.Run(name, func(t *testing.T) {
			archive := pluginZip(t, pluginFile{name: "plugin.toml", data: pluginCommandManifest}, pluginFile{name: name, data: "合成说明"})
			if _, code := validPluginArchive(archive); code != "" {
				t.Fatal("普通文件名被误判为设备保留名", name, code)
			}
		})
	}
}

func TestCommunityPluginPublishRejectsReservedFileNames(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	user := complete(t, store, Identity{"apple", "plugin-device-names"})
	c := pluginClient{t, &Service{store: store}}
	manifest := strings.ReplaceAll(pluginSoundManifest, "click.wav", "NUL.wav")
	for index, scenario := range []struct {
		kind, pluginID, version string
		archive                 []byte
	}{
		{"sound", "community-clicks", "1.0.0", pluginZip(t, pluginFile{name: "plugin.toml", data: manifest}, pluginFile{name: "NUL.wav", data: pluginWAV}, pluginFile{name: "enter.wav", data: pluginWAV})},
		{"command_table", "community-dates", "1", pluginZip(t, pluginFile{name: "plugin.toml", data: pluginCommandManifest}, pluginFile{name: "CON.txt", data: "合成说明"})},
	} {
		id := fmt.Sprintf("ab334455-1234-1234-1234-%012d", index+1)
		w := c.do("POST", "/v1/community/plugins", pluginPublishBody(id, "合成插件", scenario.kind, scenario.pluginID, scenario.version, scenario.archive), user.AccessToken)
		if w.Code != 400 || !strings.Contains(w.Body.String(), "invalid_plugin_archive") {
			t.Errorf("插件发布接受了客户端不能安装的文件名：%d %s", w.Code, w.Body.String())
		}
	}
	var stored int
	if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM community_plugins WHERE owner_id=$1`, user.User.ID).Scan(&stored); err != nil || stored != 0 {
		t.Fatal("被拒绝的归档仍被存储", stored, err)
	}
}
