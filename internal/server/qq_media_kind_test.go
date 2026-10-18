package server

import (
	"encoding/binary"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func qqTestFileTypeBox(major string, compatible ...string) []byte {
	box := make([]byte, 16+4*len(compatible))
	binary.BigEndian.PutUint32(box, uint32(len(box)))
	copy(box[4:8], "ftyp")
	copy(box[8:12], major)
	for i, brand := range compatible {
		copy(box[16+4*i:], brand)
	}
	return box
}

func TestQQMediaKindISOBaseMedia(t *testing.T) {
	for _, tc := range []struct {
		name, major, contentType, ext string
		compatible                    []string
	}{
		{name: "MP4 主品牌", major: "mp42", contentType: "video/mp4", ext: ".mp4"},
		{name: "MP4 兼容品牌", major: "isom", compatible: []string{"iso2", "mp41"}, contentType: "video/mp4", ext: ".mp4"},
		{name: "QuickTime", major: "qt  ", contentType: "video/quicktime", ext: ".mov"},
		{name: "AVIF 图片", major: "avif", compatible: []string{"avif", "mif1"}},
		{name: "AVIF 动图", major: "avis", compatible: []string{"avis", "msf1"}},
		{name: "HEIC 图片", major: "heic", compatible: []string{"mif1", "heic"}},
		{name: "HEIF 图片", major: "mif1", compatible: []string{"heix"}},
		{name: "M4A 音频", major: "M4A "},
		{name: "JPEG 2000", major: "jp2 "},
		{name: "通用容器", major: "isom"},
		{name: "未知品牌", major: "test"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			contentType, ext, video, ok := qqMediaKind(qqTestFileTypeBox(tc.major, tc.compatible...))
			want := tc.contentType != ""
			if contentType != tc.contentType || ext != tc.ext || video != want || ok != want {
				t.Fatal("文件类型识别不符合附件白名单", contentType, ext, video, ok)
			}
		})
	}
}

func TestQQMediaKindRejectsMalformedFileTypeBox(t *testing.T) {
	for _, major := range []string{"mp42", "qt  "} {
		for _, size := range []uint32{0, 1, 8, 12, 15, 17, 20, ^uint32(0)} {
			box := qqTestFileTypeBox(major)
			binary.BigEndian.PutUint32(box, size)
			if _, _, _, ok := qqMediaKind(box); ok {
				t.Errorf("接受了损坏的文件类型头：品牌=%q，声明长度=%d，实际长度=%d", major, size, len(box))
			}
		}
		if _, _, _, ok := qqMediaKind(qqTestFileTypeBox(major)[:12]); ok {
			t.Errorf("接受了缺少版本字段的文件类型头：品牌=%q", major)
		}
	}
	box := qqTestFileTypeBox("isom")
	copy(box[12:16], "mp42")
	if _, _, _, ok := qqMediaKind(box); ok {
		t.Error("把版本字段当成了 MP4 兼容品牌")
	}
	box = append(qqTestFileTypeBox("isom"), []byte("mp42")...)
	if _, _, _, ok := qqMediaKind(box); ok {
		t.Error("把文件类型盒以外的数据当成了 MP4 兼容品牌")
	}
}

func TestQQMediaKindReadsCompleteBrandList(t *testing.T) {
	brands := make([]string, 130)
	for i := range brands {
		brands[i] = "iso2"
	}
	brands = append(brands, "mp41")
	contentType, ext, video, ok := qqMediaKind(qqTestFileTypeBox("isom", brands...))
	if contentType != "video/mp4" || ext != ".mp4" || !video || !ok {
		t.Fatal("未识别较长品牌列表中的 MP4 兼容品牌", contentType, ext, video, ok)
	}
}

func TestQQMediaDoesNotPublishOtherISOBaseMedia(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(qqTestFileTypeBox(r.URL.Path[1:]))
	}))
	defer upstream.Close()
	media := &fakeMedia{}
	s := &Server{qq: &qqBot{mediaHosts: []string{"127.0.0.1"}, mediaClient: upstream.Client(), now: time.Now}, media: media}
	for _, brand := range []string{"avif", "heic", "M4A "} {
		stored, failed := s.publishQQMedia(t.Context(), []qqAttachment{{ContentType: "video/mp4", URL: upstream.URL + "/" + brand}})
		if len(stored) != 0 || failed != 1 || len(media.objects) != 0 {
			t.Fatal("非视频容器被发布为视频附件", brand, stored, failed, media.objects)
		}
	}
}
