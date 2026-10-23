package account

import (
	"archive/zip"
	"bytes"
	"testing"
)

var pluginManifestArchiveKeysSink pluginPack

func BenchmarkPluginManifestArchiveKeys(b *testing.B) {
	for _, tc := range []struct{ name, manifest string }{
		{"effect", pluginEffectManifest},
		{"command_table", pluginCommandManifest},
	} {
		b.Run(tc.name, func(b *testing.B) {
			var data bytes.Buffer
			writer := zip.NewWriter(&data)
			member, err := writer.Create("plugin.toml")
			if err != nil {
				b.Fatal(err)
			}
			if _, err = member.Write([]byte(tc.manifest)); err != nil {
				b.Fatal(err)
			}
			if err = writer.Close(); err != nil {
				b.Fatal(err)
			}
			archive := data.Bytes()
			b.ReportAllocs()
			for b.Loop() {
				var code string
				pluginManifestArchiveKeysSink, code = validPluginArchive(archive)
				if code != "" || pluginManifestArchiveKeysSink.Kind != tc.name {
					b.Fatalf("合法清单被拒绝：%q，类型 %q", code, pluginManifestArchiveKeysSink.Kind)
				}
			}
		})
	}
}
