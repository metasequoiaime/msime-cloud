package account

import (
	"archive/zip"
	"bytes"
	"encoding/binary"
	"runtime"
	"strings"
	"testing"
)

func TestPluginArchiveDirectoryAllocationBound(t *testing.T) {
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	for range 80_000 {
		if _, err := writer.CreateRaw(&zip.FileHeader{Name: ".a", Method: zip.Store}); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	archive := buffer.Bytes()
	if len(archive) > maxPluginArchiveBytes {
		t.Fatal("测试归档超出上传大小上限", len(archive))
	}
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	_, code := readPluginArchive(archive)
	runtime.ReadMemStats(&after)
	allocated := after.TotalAlloc - before.TotalAlloc
	t.Logf("归档 %d 字节，解析分配 %d 字节", len(archive), allocated)
	if code != "plugin_too_large" {
		t.Fatal("大量成员未按大小限制拒绝", code)
	}
	// 只统计解析阶段；宽松的上限允许标准库与平台差异，但不能先为全部成员分配内存。
	if allocated > 8<<20 {
		t.Fatal("成员数检查之前分配了过多内存", allocated)
	}
}

func TestPluginArchiveDirectoryReadBudget(t *testing.T) {
	for _, scenario := range []struct {
		name, memberComment string
		members             int
		extra               []byte
		code                string
	}{
		{"64 个成员和最长归档注释", "", 64, nil, ""},
		{"64 个成员和额外元数据", "", 64, pluginDirectoryExtra(4096), ""},
		{"少量成员的大目录", strings.Repeat("c", 65_000), 7, nil, "plugin_too_large"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			var buffer bytes.Buffer
			writer := zip.NewWriter(&buffer)
			if err := writer.SetComment(strings.Repeat("c", 65_535)); err != nil {
				t.Fatal(err)
			}
			for i := 0; i < scenario.members; i++ {
				header := &zip.FileHeader{Name: ".note", Method: zip.Store, Comment: scenario.memberComment, Extra: scenario.extra}
				data := ""
				if i == 0 {
					header.Name = "plugin.toml"
					data = pluginEffectManifest
				}
				out, err := writer.CreateHeader(header)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := out.Write([]byte(data)); err != nil {
					t.Fatal(err)
				}
			}
			if err := writer.Close(); err != nil {
				t.Fatal(err)
			}
			if _, code := validPluginArchive(buffer.Bytes()); code != scenario.code {
				t.Fatal("目录大小校验结果不符", code, scenario.code)
			}
		})
	}
}

func pluginDirectoryExtra(size int) []byte {
	extra := make([]byte, size)
	binary.LittleEndian.PutUint16(extra, 0xffff)
	binary.LittleEndian.PutUint16(extra[2:], uint16(size-4))
	return extra
}

func TestPluginArchiveDirectoryBudgetDoesNotLimitFileData(t *testing.T) {
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	for _, file := range []pluginFile{
		{name: "plugin.toml", data: pluginSoundManifest},
		{name: "click.wav", data: pluginWAV + strings.Repeat("a", (512<<10)-len(pluginWAV))},
		{name: "enter.wav", data: pluginWAV + strings.Repeat("a", (512<<10)-len(pluginWAV))},
	} {
		out, err := writer.CreateHeader(&zip.FileHeader{Name: file.name, Method: zip.Store})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := out.Write([]byte(file.data)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if _, code := validPluginArchive(buffer.Bytes()); code != "" {
		t.Fatal("合法音频被目录读取预算限制", code)
	}
}
