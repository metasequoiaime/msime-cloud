package account

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"image"
	"image/jpeg"
	"image/png"
	"io"
	"mime"
	"net/http"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// FeedbackPath 是 App 内反馈的提交接口。它不在 /v1/users/me 下，由 IsPath 放行全局 Bearer 检查，处理函数自己校验用户会话（匿名账号也可以提交）。
const FeedbackPath = "/v1/feedback"

const (
	// feedbackTimeout 覆盖最多 3 张 1 MiB 截图的上传和重新编码。
	feedbackTimeout = 60 * time.Second
	// maxFeedbackPayload 是 payload 部分的 JSON 上限；正文最多 500 个字符，诊断最多 9 项各 256 字节。
	maxFeedbackPayload = 16 << 10
	// maxFeedbackScreenshot 是单张截图的上传上限，maxFeedbackScreenshots 是张数上限。
	maxFeedbackScreenshot  = 1 << 20
	maxFeedbackScreenshots = 3
	// maxFeedbackBody 是整个 multipart 请求体的上限：payload、三张截图，再留出分隔线和部分头的余量。
	maxFeedbackBody = maxFeedbackScreenshots*maxFeedbackScreenshot + maxFeedbackPayload + 64<<10
	// 截图解码前先看声明的尺寸：单边最多 8192，总像素最多 8 MP，和候选窗皮肤图片的上限一致。
	maxFeedbackScreenshotSide   = 8192
	maxFeedbackScreenshotPixels = 8 << 20
	// feedbackText 是正文的 UTF-16 单元上限，与客户端的 0/500 计数一致。
	maxFeedbackText = 500
	// feedbackRetentionDays 是反馈的保留期，到期由 Store.Prune 删除。
	feedbackRetentionDays = 180
)

// 反馈的限流：每个用户每小时 5 次、每天 20 次，每个地址每天 50 次，计数在 PostgreSQL 的 auth_rates，所有副本共享。
const (
	feedbackPerUserHour   = 5
	feedbackPerUserDay    = 20
	feedbackPerAddressDay = 50
)

var feedbackTypes = map[string]bool{"bug": true, "suggestion": true, "dictionary": true}

// feedbackDiagnosticKeys 是 diagnostics 允许的键。值都是不超过 256 字节的单行字符串，描述设备与配置，不能包含输入内容。
var feedbackDiagnosticKeys = map[string]bool{"device": true, "os": true, "app_version": true, "edition": true, "scheme": true, "keyboard_layout": true, "skin": true, "ime_enabled": true, "ime_default": true}

type feedbackPayload struct {
	Type        string            `json:"type"`
	Text        string            `json:"text"`
	Platform    string            `json:"platform"`
	AppVersion  string            `json:"app_version"`
	Edition     string            `json:"edition"`
	Diagnostics map[string]string `json:"diagnostics"`
}

// feedbackScreenshot 是重新编码后的一张截图。
type feedbackScreenshot struct {
	mime  string
	bytes []byte
}

// validFeedbackEdition 只接受 0–32 个小写字母、数字和 `.`、`_`、`-`。
func validFeedbackEdition(edition string) bool {
	if len(edition) > 32 {
		return false
	}
	for _, c := range edition {
		if !(c == '.' || c == '_' || c == '-' || (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z')) {
			return false
		}
	}
	return true
}

// validate 校验 payload，返回给客户端的错误码；正文去掉首尾空白后保存。
func (p *feedbackPayload) validate() string {
	p.Text = strings.TrimSpace(p.Text)
	switch {
	case !feedbackTypes[p.Type]:
		return "invalid_feedback_type"
	case !resourceText(p.Text, 1, maxFeedbackText, true) || utf16Length(p.Text) > maxFeedbackText:
		return "invalid_feedback_text"
	case !sessionPlatforms[p.Platform]:
		return "invalid_platform"
	case !resourceText(p.AppVersion, 1, 64, false):
		return "invalid_app_version"
	case !validFeedbackEdition(p.Edition):
		return "invalid_edition"
	}
	for key, value := range p.Diagnostics {
		if !feedbackDiagnosticKeys[key] || len(value) > 256 || !utf8.ValidString(value) || strings.ContainsFunc(value, unicode.IsControl) {
			return "invalid_diagnostics"
		}
	}
	return ""
}

var errInvalidScreenshot = errors.New("invalid_screenshot")

// normalizeFeedbackScreenshots 在一个图片槽内清理整次提交的截图，避免编码器引用的上一张解码图片在等待下一槽时仍占用内存。
func normalizeFeedbackScreenshots(ctx context.Context, uploads [][]byte) ([]feedbackScreenshot, error) {
	screenshots := make([]feedbackScreenshot, 0, len(uploads))
	if len(uploads) == 0 {
		return screenshots, nil
	}
	var err error
	if !withCandidateImageSlot(ctx, func() {
		var buffers candidatePNGBufferPool
		for _, data := range uploads {
			if err = ctx.Err(); err != nil {
				return
			}
			var shot feedbackScreenshot
			shot, err = normalizeScreenshot(data, &buffers)
			if err != nil {
				return
			}
			screenshots = append(screenshots, shot)
		}
	}) {
		return nil, ctx.Err()
	}
	if err != nil {
		return nil, err
	}
	return screenshots, nil
}

// normalizeScreenshot 在完整解码前校验尺寸和 JPEG 扫描数，再按原格式重新编码 PNG 或 JPEG，去掉元数据；pool 允许同次提交的 PNG 共享压缩工作区，nil 表示独立分配。
func normalizeScreenshot(data []byte, pool png.EncoderBufferPool) (feedbackScreenshot, error) {
	config, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || (format != "png" && format != "jpeg") || config.Width < 1 || config.Height < 1 || config.Width > maxFeedbackScreenshotSide || config.Height > maxFeedbackScreenshotSide || config.Width*config.Height > maxFeedbackScreenshotPixels {
		return feedbackScreenshot{}, errInvalidScreenshot
	}
	if format == "jpeg" && jpegScans(data) > maxJPEGScans {
		return feedbackScreenshot{}, errInvalidScreenshot
	}
	source, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return feedbackScreenshot{}, errInvalidScreenshot
	}
	var out bytes.Buffer
	if format == "png" {
		err = (&png.Encoder{CompressionLevel: png.BestCompression, BufferPool: pool}).Encode(&out, source)
		return feedbackScreenshot{"image/png", out.Bytes()}, err
	}
	err = jpeg.Encode(&out, source, &jpeg.Options{Quality: 90})
	return feedbackScreenshot{"image/jpeg", out.Bytes()}, err
}

// newFeedbackID 返回随机的 UUID v4 字符串。
func newFeedbackID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	h := hex.EncodeToString(b)
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

// readFeedbackForm 读取 multipart 请求：恰好一个 payload 部分和最多 3 个 screenshots 部分，其他部分一律拒绝。失败时已经写好错误响应并返回 false。
func readFeedbackForm(w http.ResponseWriter, r *http.Request) (feedbackPayload, [][]byte, bool) {
	var payload feedbackPayload
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "multipart/form-data" {
		writeError(w, 415, "multipart_required")
		return payload, nil, false
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxFeedbackBody)
	reader, err := r.MultipartReader()
	if err != nil {
		writeError(w, 400, "invalid_feedback")
		return payload, nil, false
	}
	var screenshots [][]byte
	seenPayload := false
	tooLarge := func(err error) bool {
		var limit *http.MaxBytesError
		return errors.As(err, &limit)
	}
	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			if tooLarge(err) {
				writeError(w, 413, "feedback_too_large")
			} else {
				writeError(w, 400, "invalid_feedback")
			}
			return payload, nil, false
		}
		switch part.FormName() {
		case "payload":
			if seenPayload {
				writeError(w, 400, "invalid_feedback")
				return payload, nil, false
			}
			seenPayload = true
			raw, err := io.ReadAll(io.LimitReader(part, maxFeedbackPayload+1))
			if err != nil && tooLarge(err) {
				writeError(w, 413, "feedback_too_large")
				return payload, nil, false
			}
			if err != nil || len(raw) > maxFeedbackPayload {
				writeError(w, 400, "invalid_json")
				return payload, nil, false
			}
			d := json.NewDecoder(bytes.NewReader(raw))
			d.DisallowUnknownFields()
			if d.Decode(&payload) != nil || d.Decode(new(any)) != io.EOF {
				writeError(w, 400, "invalid_json")
				return payload, nil, false
			}
		case "screenshots":
			if len(screenshots) == maxFeedbackScreenshots {
				writeError(w, 400, "too_many_screenshots")
				return payload, nil, false
			}
			data, err := io.ReadAll(io.LimitReader(part, maxFeedbackScreenshot+1))
			if err != nil {
				if tooLarge(err) {
					writeError(w, 413, "feedback_too_large")
				} else {
					writeError(w, 400, "invalid_feedback")
				}
				return payload, nil, false
			}
			if len(data) > maxFeedbackScreenshot {
				writeError(w, 413, "screenshot_too_large")
				return payload, nil, false
			}
			screenshots = append(screenshots, data)
		default:
			writeError(w, 400, "invalid_feedback")
			return payload, nil, false
		}
	}
	if !seenPayload {
		writeError(w, 400, "invalid_feedback")
		return payload, nil, false
	}
	return payload, screenshots, true
}

// feedback 处理 POST /v1/feedback。校验通过后先计限流，再重新编码截图，最后在一个事务里写反馈和截图。正文、诊断和截图都不写日志。
func (a *Service) feedback(w http.ResponseWriter, r *http.Request) {
	p, ok := a.principal(w, r, false)
	if !ok {
		return
	}
	// 慢速截图上传使用反馈路由的连接期限，不能只延长上下文而仍被全局读写超时截断。
	controller := http.NewResponseController(w)
	_ = controller.SetReadDeadline(time.Now().Add(feedbackTimeout))
	_ = controller.SetWriteDeadline(time.Now().Add(feedbackTimeout + 5*time.Second))
	payload, uploads, ok := readFeedbackForm(w, r)
	if !ok {
		return
	}
	if code := payload.validate(); code != "" {
		writeError(w, 400, code)
		return
	}
	ctx := r.Context()
	for _, limit := range []struct {
		key    string
		n      int
		window time.Duration
	}{{"feedback-hour:" + p.UserID, feedbackPerUserHour, time.Hour}, {"feedback-day:" + p.UserID, feedbackPerUserDay, 24 * time.Hour}, {"feedback-ip:" + hash(a.clientAddress(r)), feedbackPerAddressDay, 24 * time.Hour}} {
		if err := a.store.Rate(ctx, limit.key, limit.n, limit.window); err != nil {
			a.error(w, err)
			return
		}
	}
	screenshots, err := normalizeFeedbackScreenshots(ctx, uploads)
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		writeError(w, 503, "server_busy")
		return
	}
	if errors.Is(err, errInvalidScreenshot) {
		writeError(w, 400, "invalid_screenshot")
		return
	}
	if err != nil {
		a.error(w, err)
		return
	}
	diagnostics := payload.Diagnostics
	if diagnostics == nil {
		diagnostics = map[string]string{}
	}
	raw, err := json.Marshal(diagnostics)
	if err != nil {
		a.error(w, err)
		return
	}
	tx, err := a.store.userDataTransaction(ctx, p.UserID)
	if err != nil {
		a.error(w, err)
		return
	}
	defer tx.Rollback(ctx)
	id := newFeedbackID()
	if _, err = tx.Exec(ctx, `INSERT INTO feedback(id,user_id,type,text,platform,app_version,edition,diagnostics) VALUES($1,$2,$3,$4,$5,$6,$7,$8::jsonb)`, id, p.UserID, payload.Type, payload.Text, payload.Platform, payload.AppVersion, payload.Edition, raw); err != nil {
		a.error(w, err)
		return
	}
	if len(screenshots) > 0 {
		positions := make([]int32, len(screenshots))
		mimes := make([]string, len(screenshots))
		contents := make([][]byte, len(screenshots))
		for i, shot := range screenshots {
			positions[i], mimes[i], contents[i] = int32(i), shot.mime, shot.bytes
		}
		if _, err = tx.Exec(ctx, `INSERT INTO feedback_screenshots(feedback_id,position,mime,bytes)
SELECT $1,shots.position,shots.mime,shots.bytes
FROM unnest($2::integer[],$3::text[],$4::bytea[]) AS shots(position,mime,bytes)`, id, positions, mimes, contents); err != nil {
			a.error(w, err)
			return
		}
	}
	if err = tx.Commit(ctx); err != nil {
		a.error(w, err)
		return
	}
	write(w, 201, map[string]string{"id": id, "status": "received"})
}
