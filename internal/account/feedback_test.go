package account

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"hash/crc32"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"strings"
	"testing"
	"time"
)

// feedbackPNG 是一张带 tEXt 元数据块的 PNG，用来确认重新编码会去掉它。
func feedbackPNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for x := 0; x < w; x++ {
		img.Set(x, x%h, color.RGBA{200, 80, 40, 255})
	}
	var out bytes.Buffer
	if err := png.Encode(&out, img); err != nil {
		t.Fatal(err)
	}
	raw := out.Bytes()
	chunk := []byte("Comment\x00secret-location")
	var text bytes.Buffer
	binary.Write(&text, binary.BigEndian, uint32(len(chunk)))
	text.WriteString("tEXt")
	text.Write(chunk)
	binary.Write(&text, binary.BigEndian, crc32.ChecksumIEEE(append([]byte("tEXt"), chunk...)))
	// IHDR 是签名之后的第一个块（8 字节签名 + 25 字节 IHDR），元数据插在它后面。
	return append(append(append([]byte{}, raw[:33]...), text.Bytes()...), raw[33:]...)
}

// feedbackJPEG 是一张带 EXIF（APP1）段的 JPEG。
func feedbackJPEG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 32, 64))
	var out bytes.Buffer
	if err := jpeg.Encode(&out, img, nil); err != nil {
		t.Fatal(err)
	}
	raw := out.Bytes()
	exif := append([]byte("Exif\x00\x00"), []byte("GPS-secret-location")...)
	segment := []byte{0xFF, 0xE1, byte((len(exif) + 2) >> 8), byte(len(exif) + 2)}
	segment = append(segment, exif...)
	return append(append(append([]byte{}, raw[:2]...), segment...), raw[2:]...)
}

type feedbackPart struct {
	name, filename, contentType string
	data                        []byte
}

func feedbackForm(t *testing.T, parts ...feedbackPart) (*bytes.Buffer, string) {
	t.Helper()
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	for _, p := range parts {
		header := textproto.MIMEHeader{}
		disposition := `form-data; name="` + p.name + `"`
		if p.filename != "" {
			disposition += `; filename="` + p.filename + `"`
		}
		header.Set("Content-Disposition", disposition)
		if p.contentType != "" {
			header.Set("Content-Type", p.contentType)
		}
		part, err := w.CreatePart(header)
		if err != nil {
			t.Fatal(err)
		}
		part.Write(p.data)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return &body, w.FormDataContentType()
}

func payloadPart(payload string) feedbackPart {
	return feedbackPart{name: "payload", contentType: "application/json", data: []byte(payload)}
}

func screenshotPart(data []byte, contentType string) feedbackPart {
	return feedbackPart{name: "screenshots", filename: "shot", contentType: contentType, data: data}
}

const validFeedbackPayload = `{"type":"bug","text":"  候选栏偶尔不显示\n重启后恢复  ","platform":"android","app_version":"1.0.0","edition":"full","diagnostics":{"device":"Pixel 8","os":"Android 15","scheme":"quanpin","ime_enabled":"true"}}`

func postFeedback(t *testing.T, handler http.Handler, token string, status int, parts ...feedbackPart) *httptest.ResponseRecorder {
	t.Helper()
	body, contentType := feedbackForm(t, parts...)
	r := httptest.NewRequest("POST", FeedbackPath, body)
	r.Header.Set("Content-Type", contentType)
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != status {
		t.Fatalf("feedback: got %d want %d: %s", w.Code, status, w.Body.String())
	}
	return w
}

func TestFeedbackPayloadValidation(t *testing.T) {
	base := func() feedbackPayload {
		return feedbackPayload{Type: "suggestion", Text: "希望加入九宫格", Platform: "android", AppVersion: "1.0.0", Edition: "wubi"}
	}
	if p := base(); p.validate() != "" {
		t.Fatal("valid payload rejected")
	}
	p := base()
	p.Text = strings.Repeat("😀", 250)
	if p.validate() != "" {
		t.Fatal("500 UTF-16 units rejected")
	}
	for code, change := range map[string][]func(*feedbackPayload){
		"invalid_feedback_type": {func(p *feedbackPayload) { p.Type = "praise" }},
		"invalid_feedback_text": {func(p *feedbackPayload) { p.Text = "   " }, func(p *feedbackPayload) { p.Text = strings.Repeat("字", 501) }, func(p *feedbackPayload) { p.Text = strings.Repeat("😀", 250) + "a" }, func(p *feedbackPayload) { p.Text = "a\x00b" }},
		"invalid_platform":      {func(p *feedbackPayload) { p.Platform = "web" }},
		"invalid_app_version":   {func(p *feedbackPayload) { p.AppVersion = "" }, func(p *feedbackPayload) { p.AppVersion = strings.Repeat("1", 65) }, func(p *feedbackPayload) { p.AppVersion = "1\n0" }},
		"invalid_edition":       {func(p *feedbackPayload) { p.Edition = "Full" }, func(p *feedbackPayload) { p.Edition = strings.Repeat("a", 33) }},
		"invalid_diagnostics": {
			func(p *feedbackPayload) { p.Diagnostics = map[string]string{"typed_text": "hello"} },
			func(p *feedbackPayload) { p.Diagnostics = map[string]string{"device": strings.Repeat("x", 257)} },
			func(p *feedbackPayload) { p.Diagnostics = map[string]string{"os": "a\nb"} },
			func(p *feedbackPayload) { p.Diagnostics = map[string]string{"skin": "\xff"} },
		},
	} {
		for _, f := range change {
			p := base()
			f(&p)
			if got := p.validate(); got != code {
				t.Errorf("%+v: got %q want %q", p, got, code)
			}
		}
	}
	every := base()
	every.Diagnostics = map[string]string{}
	for key := range feedbackDiagnosticKeys {
		every.Diagnostics[key] = strings.Repeat("x", 256)
	}
	if every.validate() != "" {
		t.Fatal("whitelisted diagnostics rejected")
	}
}

func TestNormalizeScreenshot(t *testing.T) {
	shot, err := normalizeScreenshot(feedbackPNG(t, 40, 80))
	if err != nil || shot.mime != "image/png" || bytes.Contains(shot.bytes, []byte("secret-location")) || bytes.Contains(shot.bytes, []byte("tEXt")) {
		t.Fatal("png metadata kept", err)
	}
	if config, format, err := image.DecodeConfig(bytes.NewReader(shot.bytes)); err != nil || format != "png" || config.Width != 40 || config.Height != 80 {
		t.Fatal("png re-encode", config, format, err)
	}
	shot, err = normalizeScreenshot(feedbackJPEG(t))
	if err != nil || shot.mime != "image/jpeg" || bytes.Contains(shot.bytes, []byte("Exif")) || bytes.Contains(shot.bytes, []byte("secret-location")) {
		t.Fatal("jpeg metadata kept", err)
	}
	var animated bytes.Buffer
	if err = gif.Encode(&animated, image.NewPaletted(image.Rect(0, 0, 4, 4), []color.Color{color.Black, color.White}), nil); err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string][]byte{
		"gif":       animated.Bytes(),
		"garbage":   []byte("not an image"),
		"truncated": feedbackPNG(t, 40, 80)[:60],
		"too wide":  feedbackPNG(t, maxFeedbackScreenshotSide+1, 1),
		"too many pixels": func() []byte {
			var out bytes.Buffer
			png.Encode(&out, image.NewGray(image.Rect(0, 0, 4096, 2049)))
			return out.Bytes()
		}(),
	} {
		if _, err := normalizeScreenshot(data); err != errInvalidScreenshot {
			t.Errorf("%s accepted: %v", name, err)
		}
	}
}

// 反馈提交：匿名账号也能提交，截图重新编码后存进数据库，诊断只收白名单键；格式不对的请求逐类拒绝，不写入任何记录。
func TestFeedbackSubmitAndValidationHTTP(t *testing.T) {
	db := testStore(t)
	ctx := context.Background()
	if _, err := db.pool.Exec(ctx, `TRUNCATE feedback CASCADE`); err != nil {
		t.Fatal(err)
	}
	anonymous := complete(t, db, Identity{"anonymous", "msime-device-0001:hmac"})
	mux := http.NewServeMux()
	Mount(mux, &Service{store: db})
	w := postFeedback(t, mux, anonymous.AccessToken, 201, payloadPart(validFeedbackPayload), screenshotPart(feedbackPNG(t, 40, 80), "image/png"), screenshotPart(feedbackJPEG(t), "image/jpeg"))
	var created struct{ ID, Status string }
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil || len(created.ID) != 36 || !validCommunityID(created.ID) || created.Status != "received" {
		t.Fatal(created, err, w.Body.String())
	}
	var kind, text, platform, version, edition, status string
	var diagnostics map[string]string
	if err := db.pool.QueryRow(ctx, `SELECT type,text,platform,app_version,edition,status,diagnostics FROM feedback WHERE id=$1 AND user_id=$2`, created.ID, anonymous.User.ID).Scan(&kind, &text, &platform, &version, &edition, &status, &diagnostics); err != nil {
		t.Fatal(err)
	}
	if kind != "bug" || text != "候选栏偶尔不显示\n重启后恢复" || platform != "android" || version != "1.0.0" || edition != "full" || status != "new" || diagnostics["device"] != "Pixel 8" || len(diagnostics) != 4 {
		t.Fatal("stored feedback", kind, text, platform, version, edition, status, diagnostics)
	}
	rows, err := db.pool.Query(ctx, `SELECT position,mime,bytes FROM feedback_screenshots WHERE feedback_id=$1 ORDER BY position`, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	var mimes []string
	for rows.Next() {
		var position int
		var mime string
		var data []byte
		if err = rows.Scan(&position, &mime, &data); err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(data, []byte("secret-location")) {
			t.Fatal("screenshot metadata stored")
		}
		mimes = append(mimes, mime)
	}
	rows.Close()
	if strings.Join(mimes, ",") != "image/png,image/jpeg" {
		t.Fatal("screenshots", mimes)
	}
	// 不带截图、不带诊断也可以。
	postFeedback(t, mux, anonymous.AccessToken, 201, payloadPart(`{"type":"dictionary","text":"「水杉」排在后面","platform":"ios","app_version":"2.0"}`))
	before := 0
	if err = db.pool.QueryRow(ctx, `SELECT count(*) FROM feedback`).Scan(&before); err != nil || before != 2 {
		t.Fatal(before, err)
	}
	png := feedbackPNG(t, 4, 4)
	for _, tc := range []struct {
		status int
		code   string
		parts  []feedbackPart
	}{
		{400, "invalid_feedback", nil},
		{400, "invalid_feedback", []feedbackPart{screenshotPart(png, "image/png")}},
		{400, "invalid_feedback", []feedbackPart{payloadPart(validFeedbackPayload), payloadPart(validFeedbackPayload)}},
		{400, "invalid_feedback", []feedbackPart{payloadPart(validFeedbackPayload), {name: "attachment", data: []byte("x")}}},
		{400, "invalid_json", []feedbackPart{payloadPart(`{"type":"bug"`)}},
		{400, "invalid_json", []feedbackPart{payloadPart(strings.Replace(validFeedbackPayload, `"edition":"full"`, `"edition":"full","contact":"me"`, 1))}},
		{400, "invalid_json", []feedbackPart{payloadPart(validFeedbackPayload + `{}`)}},
		{400, "invalid_json", []feedbackPart{payloadPart(strings.Replace(validFeedbackPayload, `"ime_enabled":"true"`, `"ime_enabled":true`, 1))}},
		{400, "invalid_json", []feedbackPart{payloadPart(`{"text":"` + strings.Repeat("x", maxFeedbackPayload) + `"}`)}},
		{400, "invalid_diagnostics", []feedbackPart{payloadPart(strings.Replace(validFeedbackPayload, `"scheme"`, `"last_input"`, 1))}},
		{400, "invalid_feedback_type", []feedbackPart{payloadPart(strings.Replace(validFeedbackPayload, `"bug"`, `"praise"`, 1))}},
		{400, "too_many_screenshots", []feedbackPart{payloadPart(validFeedbackPayload), screenshotPart(png, ""), screenshotPart(png, ""), screenshotPart(png, ""), screenshotPart(png, "")}},
		{400, "invalid_screenshot", []feedbackPart{payloadPart(validFeedbackPayload), screenshotPart([]byte("GIF89a"), "image/png")}},
		{413, "screenshot_too_large", []feedbackPart{payloadPart(validFeedbackPayload), screenshotPart(bytes.Repeat([]byte{1}, maxFeedbackScreenshot+1), "image/png")}},
	} {
		w := postFeedback(t, mux, anonymous.AccessToken, tc.status, tc.parts...)
		if !strings.Contains(w.Body.String(), tc.code) {
			t.Fatalf("want %s: %s", tc.code, w.Body.String())
		}
	}
	// 整个请求体超过上限。
	body, contentType := feedbackForm(t, payloadPart(validFeedbackPayload), screenshotPart(bytes.Repeat([]byte{1}, maxFeedbackScreenshot), ""), screenshotPart(bytes.Repeat([]byte{1}, maxFeedbackScreenshot), ""), screenshotPart(bytes.Repeat([]byte{1}, maxFeedbackScreenshot), ""), feedbackPart{name: "padding", data: bytes.Repeat([]byte{1}, 128<<10)})
	r := httptest.NewRequest("POST", FeedbackPath, body)
	r.Header.Set("Content-Type", contentType)
	r.Header.Set("Authorization", "Bearer "+anonymous.AccessToken)
	recorder := httptest.NewRecorder()
	mux.ServeHTTP(recorder, r)
	if recorder.Code != 413 && recorder.Code != 400 {
		t.Fatal("oversized body", recorder.Code, recorder.Body.String())
	}
	for _, contentType := range []string{"", "application/json", "multipart/form-data"} {
		r := httptest.NewRequest("POST", FeedbackPath, strings.NewReader(validFeedbackPayload))
		r.Header.Set("Content-Type", contentType)
		r.Header.Set("Authorization", "Bearer "+anonymous.AccessToken)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		if (contentType == "multipart/form-data" && w.Code != 400) || (contentType != "multipart/form-data" && w.Code != 415) {
			t.Fatal(contentType, w.Code, w.Body.String())
		}
	}
	// 没有会话不能提交。
	postFeedback(t, mux, "", 401, payloadPart(validFeedbackPayload))
	after := 0
	if err = db.pool.QueryRow(ctx, `SELECT count(*) FROM feedback`).Scan(&after); err != nil || after != before {
		t.Fatal("rejected feedback was stored", after, err)
	}
}

// 限流：每个用户每小时 5 次（超出 429，且不写入），每个地址每天 50 次；180 天后由 Prune 删除，注销账号时级联删除。
func TestFeedbackLimitsRetentionAndDeletion(t *testing.T) {
	db := testStore(t)
	ctx := context.Background()
	if _, err := db.pool.Exec(ctx, `TRUNCATE feedback CASCADE`); err != nil {
		t.Fatal(err)
	}
	user := complete(t, db, Identity{"email", "feedback-limit@example.test"})
	other := complete(t, db, Identity{"email", "feedback-other@example.test"})
	mux := http.NewServeMux()
	Mount(mux, &Service{store: db})
	for i := 0; i < feedbackPerUserHour; i++ {
		postFeedback(t, mux, user.AccessToken, 201, payloadPart(validFeedbackPayload), screenshotPart(feedbackPNG(t, 8, 8), "image/png"))
	}
	if w := postFeedback(t, mux, user.AccessToken, 429, payloadPart(validFeedbackPayload)); w.Header().Get("Retry-After") == "" {
		t.Fatal("missing Retry-After")
	}
	var count int
	if err := db.pool.QueryRow(ctx, `SELECT count(*) FROM feedback WHERE user_id=$1`, user.User.ID).Scan(&count); err != nil || count != feedbackPerUserHour {
		t.Fatal("limited feedback stored", count, err)
	}
	// 同一个地址当天已经用满 50 次时，换一个账号也不行。
	if _, err := db.pool.Exec(ctx, `UPDATE auth_rates SET count=$2 WHERE key=$1`, "feedback-ip:"+hash("192.0.2.1"), feedbackPerAddressDay); err != nil {
		t.Fatal(err)
	}
	postFeedback(t, mux, other.AccessToken, 429, payloadPart(validFeedbackPayload))
	if _, err := db.pool.Exec(ctx, `DELETE FROM auth_rates WHERE key LIKE 'feedback-%'`); err != nil {
		t.Fatal(err)
	}
	postFeedback(t, mux, other.AccessToken, 201, payloadPart(validFeedbackPayload))
	if _, err := db.pool.Exec(ctx, `UPDATE feedback SET created_at=now()-interval '181 days' WHERE user_id=$1`, other.User.ID); err != nil {
		t.Fatal(err)
	}
	db.Prune(ctx)
	if err := db.pool.QueryRow(ctx, `SELECT count(*) FROM feedback WHERE user_id=$1`, other.User.ID).Scan(&count); err != nil || count != 0 {
		t.Fatal("expired feedback kept", count, err)
	}
	if err := db.pool.QueryRow(ctx, `SELECT count(*) FROM feedback WHERE user_id=$1`, user.User.ID).Scan(&count); err != nil || count != feedbackPerUserHour {
		t.Fatal("recent feedback pruned", count, err)
	}
	if err := db.DeleteUser(ctx, user.User.ID); err != nil {
		t.Fatal(err)
	}
	if err := db.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM feedback)+(SELECT count(*) FROM feedback_screenshots)`).Scan(&count); err != nil || count != 0 {
		t.Fatal("deleted account kept feedback", count, err)
	}
}

func TestFeedbackUnavailable(t *testing.T) {
	db := testStore(t)
	user := complete(t, db, Identity{"email", "feedback-unavailable@example.test"})
	body, contentType := feedbackForm(t, payloadPart(validFeedbackPayload), screenshotPart(feedbackPNG(t, 8, 8), "image/png"))
	request := func() *http.Request {
		r := httptest.NewRequest("POST", FeedbackPath, bytes.NewReader(body.Bytes()))
		r.Header.Set("Content-Type", contentType)
		r.Header.Set("Authorization", "Bearer "+user.AccessToken)
		return r
	}
	// 只读库：限流计数写不进去，返回 503，不写入。
	w := httptest.NewRecorder()
	readOnlyService(t, db).feedback(w, request())
	if w.Code != 503 {
		t.Fatal(w.Code, w.Body.String())
	}
	// 截图解码名额被占满且请求期限已到：返回 503 server_busy。
	for range cap(candidateImageSlots) {
		candidateImageSlots <- struct{}{}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	w = httptest.NewRecorder()
	(&Service{store: db}).feedback(w, request().WithContext(ctx))
	cancel()
	for range cap(candidateImageSlots) {
		<-candidateImageSlots
	}
	if w.Code != 503 || !strings.Contains(w.Body.String(), "server_busy") {
		t.Fatal(w.Code, w.Body.String())
	}
	var count int
	if err := db.pool.QueryRow(t.Context(), `SELECT count(*) FROM feedback WHERE user_id=$1`, user.User.ID).Scan(&count); err != nil || count != 0 {
		t.Fatal("unavailable feedback stored", count, err)
	}
	// 被封禁的账号（封禁写在后台之外、会话还在）提交时返回 403。
	if _, err := db.pool.Exec(t.Context(), `UPDATE auth_users SET banned_at=now() WHERE id=$1`, user.User.ID); err != nil {
		t.Fatal(err)
	}
	w = httptest.NewRecorder()
	(&Service{store: db}).feedback(w, request())
	if w.Code != 403 {
		t.Fatal(w.Code, w.Body.String())
	}
}
