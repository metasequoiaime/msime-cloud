package account

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

func TestPluginArchiveAudioHeaderLength(t *testing.T) {
	for _, extension := range []string{"ogg", "wav"} {
		for size := 0; size <= 12; size++ {
			t.Run(fmt.Sprintf("%s-%d", extension, size), func(t *testing.T) {
				data := "OggS" + strings.Repeat("\x00", 8)
				if extension == "wav" {
					data = "RIFF\x00\x00\x00\x00WAVE"
				}
				manifest := strings.ReplaceAll(pluginMusicManifest, "rain.ogg", "rain."+extension)
				archive := pluginZip(t, pluginFile{name: "plugin.toml", data: manifest}, pluginFile{name: "rain." + extension, data: data[:size]}, pluginFile{name: "night.wav", data: pluginWAV})
				want := "invalid_plugin_manifest"
				if size == 12 {
					want = ""
				}
				if _, code := validPluginArchive(archive); code != want {
					t.Fatalf("音频文件头长度 %d：错误码 %q，期望 %q", size, code, want)
				}
			})
		}
	}
}

func TestCommunityPluginPublishRejectsTruncatedAudioHeaders(t *testing.T) {
	store := testStore(t)
	user := complete(t, store, Identity{"apple", "plugin-audio-headers"})
	c := pluginClient{t, &Service{store: store}}
	for index, size := range []int{4, 11} {
		archive := pluginZip(t, pluginFile{name: "plugin.toml", data: pluginMusicManifest}, pluginFile{name: "rain.ogg", data: "OggS" + strings.Repeat("\x00", size-4)}, pluginFile{name: "night.wav", data: pluginWAV})
		id := fmt.Sprintf("ab334455-1234-1234-1234-%012d", index+1)
		w := c.do("POST", "/v1/community/plugins", pluginPublishBody(id, "合成音乐", "music", "community-rain", "2.1", archive), user.AccessToken)
		if w.Code != 400 || !strings.Contains(w.Body.String(), "invalid_plugin_manifest") {
			t.Errorf("音乐发布接受了截断的音频文件头：%d %s", w.Code, w.Body.String())
		}
	}
	var stored int
	if err := store.pool.QueryRow(context.Background(), `SELECT count(*) FROM community_plugins WHERE owner_id=$1`, user.User.ID).Scan(&stored); err != nil || stored != 0 {
		t.Fatal("被拒绝的归档仍被存储", stored, err)
	}
}
