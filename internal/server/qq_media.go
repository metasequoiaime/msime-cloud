package server

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/metasequoiaime/MSIME-Backend/internal/httpclient"
)

// 管理员提 issue 时附带的图片和视频。GitHub 的 issue 接口不能上传附件，所以服务端从 QQ 下载附件，按文件头核实类型后写进公开的 R2 bucket（auth.avatars 的 bucket，前缀 qq-feedback/，经它的 public_base_url 提供），再把链接写进 issue 正文或补充评论。公开域名是我们自己的，所以对象的 Content-Type 一律取服务端认出的类型，只放行常见的图片和视频格式：HTML、SVG 之类能在浏览器里执行脚本的内容不会被托管。

const (
	// qqMediaMaxFiles 是一条消息最多处理的附件数，超出的算作没能保存。
	qqMediaMaxFiles = 9
	// qqImageBytes、qqVideoBytes 是单个图片、视频的大小上限。附件逐个下载，内存里同时只有一个。
	qqImageBytes = 20 << 20
	qqVideoBytes = 50 << 20
)

// qqMediaHosts 是允许下载附件的主机后缀。附件地址来自签过名的回调，仍只从 QQ 的 CDN 下载，免得服务端被当成访问内网的跳板。
var qqMediaHosts = []string{".qq.com", ".qq.com.cn", ".qpic.cn", ".gtimg.cn"}

// qqAttachment 是 GROUP_AT_MESSAGE_CREATE 里的一个附件。content_type 由 QQ 给出（image/*、video/*、voice），只用来跳过语音和选择大小上限，真正的类型看文件头。
type qqAttachment struct {
	URL         string `json:"url"`
	ContentType string `json:"content_type"`
}

// qqContentTypePrefix 按 HTTP 媒体类型的 ASCII 大小写规则检查前缀。
func qqContentTypePrefix(contentType, prefix string) bool {
	if len(contentType) < len(prefix) {
		return false
	}
	for i := range prefix {
		c := contentType[i]
		if c >= utf8.RuneSelf {
			return strings.HasPrefix(strings.ToLower(contentType), prefix)
		}
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		if c != prefix[i] {
			return false
		}
	}
	return true
}

// qqImageContentType 判断附件声明是否为图片，避免为每个附件创建小写副本。
func qqImageContentType(contentType string) bool {
	return qqContentTypePrefix(contentType, "image/")
}

func qqVideoContentType(contentType string) bool {
	return qqContentTypePrefix(contentType, "video/")
}

// qqMedia 是一个已经存好的附件。
type qqMedia struct {
	URL   string
	Video bool
}

// mediaPublisher 是 account.Service.PublishMedia。
type mediaPublisher interface {
	PublishMedia(ctx context.Context, key string, body []byte, contentType string) (string, error)
}

// qqMediaURL 校验附件地址并补上缺省的 https://，主机必须匹配 hosts 里的某个后缀（后缀去掉开头的点时也算匹配）。
func qqMediaURL(raw string, hosts []string) (string, error) {
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil || u.Hostname() == "" {
		return "", errors.New("invalid attachment url")
	}
	if !qqMediaHostAllowed(u.Hostname(), hosts) {
		return "", errors.New("attachment host not allowed")
	}
	return u.String(), nil
}

func qqMediaHostAllowed(host string, hosts []string) bool {
	for _, suffix := range hosts {
		bare := strings.TrimPrefix(suffix, ".")
		if len(host) == len(bare) && strings.EqualFold(host, bare) {
			return true
		}
		if len(host) >= len(suffix) && strings.EqualFold(host[len(host)-len(suffix):], suffix) {
			return true
		}
	}
	return false
}

// qqMediaKind 按文件头认出附件类型，返回存储用的 Content-Type、扩展名和是否是视频；认不出或不在白名单里时 ok 为 false。
func qqMediaKind(body []byte) (contentType, ext string, video, ok bool) {
	if len(body) >= 8 && string(body[4:8]) == "ftyp" {
		if len(body) < 16 {
			return "", "", false, false
		}
		boxSize := binary.BigEndian.Uint32(body[:4])
		if boxSize < 16 || boxSize%4 != 0 || uint64(boxSize) > uint64(len(body)) {
			return "", "", false, false
		}
		if string(body[8:12]) == "qt  " {
			return "video/quicktime", ".mov", true, true
		}
		// ftyp 也用于 AVIF、HEIC 和音频；按 MIME 嗅探规则核对 MP4 品牌，跳过版本字段，不读取盒外数据。
		for offset := 8; offset < int(boxSize); offset += 4 {
			if offset != 12 && string(body[offset:offset+3]) == "mp4" {
				return "video/mp4", ".mp4", true, true
			}
		}
		return "", "", false, false
	}
	switch http.DetectContentType(body) {
	case "image/png":
		return "image/png", ".png", false, true
	case "image/jpeg":
		return "image/jpeg", ".jpg", false, true
	case "image/gif":
		return "image/gif", ".gif", false, true
	case "image/webp":
		return "image/webp", ".webp", false, true
	case "video/webm":
		return "video/webm", ".webm", true, true
	}
	return "", "", false, false
}

// downloadMedia 下载一个附件，最多 limit 字节。重定向也只能去允许的主机。
func (b *qqBot) downloadMedia(ctx context.Context, raw string, limit int64) ([]byte, error) {
	target, err := qqMediaURL(raw, b.mediaHosts)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, errors.New("invalid attachment request")
	}
	resp, err := b.mediaClient.Do(req)
	if err != nil {
		return nil, errors.New("attachment download failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("attachment download answered %d", resp.StatusCode)
	}
	if resp.ContentLength > limit {
		return nil, errors.New("attachment too large")
	}
	body, err := httpclient.ReadResponseBody(resp, limit)
	if err != nil {
		return nil, errors.New("attachment download failed")
	}
	if int64(len(body)) > limit {
		return nil, errors.New("attachment too large")
	}
	return body, nil
}

// publishQQMedia 把消息里的图片和视频存进公开 bucket，返回存好的附件和没能保存的个数。语音等其他附件不算在内；没有配置 bucket 时图片和视频全部算作没能保存。只记录失败原因，不记录附件地址（里面带着 QQ 的访问签名）。
func (s *Server) publishQQMedia(ctx context.Context, attachments []qqAttachment) (media []qqMedia, failed int) {
	for _, a := range attachments {
		videoDeclared := qqVideoContentType(a.ContentType)
		if !videoDeclared && !qqImageContentType(a.ContentType) {
			continue
		}
		if s.media == nil || len(media)+failed >= qqMediaMaxFiles {
			failed++
			continue
		}
		limit := int64(qqImageBytes)
		if videoDeclared {
			limit = qqVideoBytes
		}
		body, err := s.qq.downloadMedia(ctx, a.URL, limit)
		if err != nil {
			slog.Warn("qq bot: attachment skipped", "reason", err.Error())
			failed++
			continue
		}
		contentType, ext, video, ok := qqMediaKind(body)
		if !ok || !video && len(body) > qqImageBytes {
			slog.Warn("qq bot: attachment skipped", "reason", "unsupported content")
			failed++
			continue
		}
		key := "qq-feedback/" + s.qq.now().UTC().Format("2006/01/") + qqMediaName() + ext
		published, err := s.media.PublishMedia(ctx, key, body, contentType)
		if err != nil {
			// 存储 SDK 的错误可能带有上游响应正文和请求凭据，不写入附件错误日志。
			slog.Warn("qq bot: attachment not stored", "reason", "storage_write_failed")
			failed++
			continue
		}
		media = append(media, qqMedia{URL: published, Video: video})
	}
	return media, failed
}

// qqMediaName 是对象名的随机部分，32 个十六进制字符，猜不出也不会重复。
func qqMediaName() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// qqMediaSection 是写进 issue 正文或评论的附件一节：图片直接显示，视频给链接（GitHub 只内嵌它自己托管的视频）；有没能保存的附件时注明个数。没有附件时为空。存好的附件是原图原视频，开头注明没有脱敏，维护者看到个人信息时知道要删。
func qqMediaSection(media []qqMedia, failed int) string {
	if len(media) == 0 && failed == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\n### 截图与视频\n")
	if len(media) > 0 {
		b.WriteString("\n附件为原图，未做脱敏。\n")
	}
	images, videos := 0, 0
	for _, m := range media {
		if m.Video {
			videos++
			fmt.Fprintf(&b, "\n[视频 %d](%s)\n", videos, m.URL)
		} else {
			images++
			fmt.Fprintf(&b, "\n![截图 %d](%s)\n", images, m.URL)
		}
	}
	if failed > 0 {
		fmt.Fprintf(&b, "\n另有 %d 个附件没能保存。\n", failed)
	}
	return strings.TrimRight(b.String(), "\n")
}

// newQQMediaClient 是下载附件用的客户端：视频较大，时限比开放平台接口宽；重定向只跟到允许的主机。
func newQQMediaClient(hosts *[]string) *http.Client {
	return &http.Client{Timeout: 90 * time.Second, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 || !qqMediaHostAllowed(req.URL.Hostname(), *hosts) {
			return errors.New("attachment redirect not allowed")
		}
		return nil
	}}
}
