package server

import (
	"bytes"
	"io"
	"net/http"
	"testing"
)

type qqVisionDownloadBody struct {
	reader io.Reader
	read   int64
	closed bool
}

func (b *qqVisionDownloadBody) Read(p []byte) (int, error) {
	n, err := b.reader.Read(p)
	b.read += int64(n)
	return n, err
}

func (b *qqVisionDownloadBody) Close() error {
	b.closed = true
	return nil
}

func TestQQVisionDownloadUsesModelLimit(t *testing.T) {
	for _, tc := range []struct {
		name                  string
		size, length, maxRead int64
		valid                 bool
	}{
		{name: "已知长度超限", size: 2 * qqVisionBytes, length: 2 * qqVisionBytes},
		{name: "未知长度超限", size: 2 * qqVisionBytes, length: -1, maxRead: qqVisionBytes + 1},
		{name: "已知长度恰好上限", size: qqVisionBytes, length: qqVisionBytes, maxRead: qqVisionBytes, valid: true},
		{name: "未知长度恰好上限", size: qqVisionBytes, length: -1, maxRead: qqVisionBytes, valid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, fake, _ := qqServer(t)
			data := make([]byte, tc.size)
			copy(data, qqTestMedia["shot.png"])
			body := &qqVisionDownloadBody{reader: bytes.NewReader(data)}
			s.qq.mediaClient = &http.Client{Transport: qqTestTransport(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: http.StatusOK, ContentLength: tc.length, Body: body, Header: make(http.Header)}, nil
			})}
			got, err := s.qqDescribeImage(t.Context(), qqAttachment{ContentType: "image/png", URL: "https://gchat.qpic.cn/synthetic.png"})
			if tc.valid {
				if err != nil || got == "" || len(fake.visionCalls) != 1 {
					t.Fatal("上限内的截图没有交给模型描述", got, err, len(fake.visionCalls))
				}
			} else if err == nil || got != "" || len(fake.visionCalls) != 0 {
				t.Fatal("超限截图仍被交给模型", got, err, len(fake.visionCalls))
			}
			if body.read != tc.maxRead {
				t.Errorf("截图下载没有按模型上限停止：读取=%d，期望=%d", body.read, tc.maxRead)
			}
			if !body.closed {
				t.Error("截图下载响应没有关闭")
			}
		})
	}
}

func TestQQPublishedImageKeepsLargerDownloadLimit(t *testing.T) {
	s, _, _ := qqServer(t)
	data := make([]byte, qqVisionBytes+1)
	copy(data, qqTestMedia["shot.png"])
	body := &qqVisionDownloadBody{reader: bytes.NewReader(data)}
	s.qq.mediaClient = &http.Client{Transport: qqTestTransport(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, ContentLength: int64(len(data)), Body: body, Header: make(http.Header)}, nil
	})}
	media := &fakeMedia{}
	s.media = media
	stored, failed := s.publishQQMedia(t.Context(), []qqAttachment{{ContentType: "image/png", URL: "https://gchat.qpic.cn/synthetic.png"}})
	if len(stored) != 1 || failed != 0 || len(media.objects) != 1 || body.read != int64(len(data)) || !body.closed {
		t.Fatal("原图附件的下载上限被模型上限影响", len(stored), failed, len(media.objects), body.read, body.closed)
	}
}
