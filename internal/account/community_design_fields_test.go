package account

import (
	"bytes"
	"encoding/json"
	"fmt"
	"image/jpeg"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

// previousParseCommunityDesignFields 保留完整的旧解析器，对照字段校验、照片清理和分配开销。
func previousParseCommunityDesignFields(raw json.RawMessage) (CommunityDesign, error) {
	var v CommunityDesign
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&v); err != nil {
		return v, err
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil {
		return v, ErrInvalid
	}
	for _, key := range []string{"background", "keyBackground", "keyForeground", "accent", "actionBackground", "cornerRadius", "borderWidth", "shadow", "pattern", "monospaced"} {
		if len(fields[key]) == 0 || string(fields[key]) == "null" {
			return v, ErrInvalid
		}
	}
	switch v.KeyShape {
	case "", "rounded", "capsule", "ticket", "pebble":
	default:
		return v, ErrInvalid
	}
	switch v.KeyMaterial {
	case "", "flat", "raised", "glass", "paper":
	default:
		return v, ErrInvalid
	}
	for _, c := range []uint32{v.Background, v.KeyBackground, v.KeyForeground, v.Accent, v.ActionBackground} {
		if c > 0xffffff {
			return v, ErrInvalid
		}
	}
	for _, c := range []*uint32{v.GradientEnd, v.CustomBorderColor} {
		if c != nil && *c > 0xffffff {
			return v, ErrInvalid
		}
	}
	if !validRange(v.CornerRadius, 0, 20) || !validRange(v.BorderWidth, 0, 2) || !validRange(v.Shadow, 0, 0.4) || v.Pattern < 0 || v.Pattern > 3 {
		return v, ErrInvalid
	}
	for _, r := range []struct {
		p        *float64
		min, max float64
	}{{v.KeyOpacity, .25, 1}, {v.PatternOpacity, 0, .5}, {v.PhotoShade, 0, .8}, {v.PhotoPosition, 0, 1}} {
		if r.p != nil && !validRange(*r.p, r.min, r.max) {
			return v, ErrInvalid
		}
	}
	if len(v.Photo) > 0 {
		if len(v.Photo) > 512000 || jpegScans(v.Photo) > maxJPEGScans {
			return v, ErrInvalid
		}
		config, err := jpeg.DecodeConfig(bytes.NewReader(v.Photo))
		if err != nil || config.Width < 1 || config.Height < 1 || config.Width > 1024 || config.Height > 1024 {
			return v, ErrInvalid
		}
		image, err := jpeg.Decode(bytes.NewReader(v.Photo))
		if err != nil {
			return v, ErrInvalid
		}
		var clean bytes.Buffer
		if jpeg.Encode(&clean, image, &jpeg.Options{Quality: 80}) != nil || clean.Len() > 512000 {
			return v, ErrInvalid
		}
		v.Photo = clean.Bytes()
	}
	return v, nil
}

func communityDesignFieldsPhoto(tb testing.TB, photo []byte) json.RawMessage {
	tb.Helper()
	fields := map[string]json.RawMessage{}
	if err := json.Unmarshal([]byte(communityFixture), &fields); err != nil {
		tb.Fatal(err)
	}
	rawPhoto, err := json.Marshal(photo)
	if err != nil {
		tb.Fatal(err)
	}
	fields["photo"] = rawPhoto
	raw, err := json.Marshal(fields)
	if err != nil {
		tb.Fatal(err)
	}
	return raw
}

func benchmarkCommunityDesignFields(b *testing.B, parse func(json.RawMessage) (CommunityDesign, error), raw json.RawMessage) {
	b.ReportAllocs()
	for b.Loop() {
		design, err := parse(raw)
		if err != nil || design.Background != 15266027 {
			b.Fatalf("社区皮肤解析失败：%v", err)
		}
	}
}

func TestCommunityDesignFieldsAllocationBudget(t *testing.T) {
	photo := make([]byte, 512000)
	copy(photo, avatarBufferImage(t, "jpeg", 8, 8))
	raw := communityDesignFieldsPhoto(t, photo)
	previous := testing.Benchmark(func(b *testing.B) { benchmarkCommunityDesignFields(b, previousParseCommunityDesignFields, raw) })
	current := testing.Benchmark(func(b *testing.B) { benchmarkCommunityDesignFields(b, parseCommunityDesign, raw) })
	t.Logf("完整解析原实现：%s；当前：%s", previous.MemString(), current.MemString())
	// 必填字段检查只需要存在性与 null 状态，不应再复制大照片的 Base64 文本。
	if saved := previous.AllocedBytesPerOp() - current.AllocedBytesPerOp(); saved < int64(len(raw)/2) {
		t.Fatalf("省下的分配 %d B，未达到 %d B", saved, len(raw)/2)
	}
}

func TestCommunityDesignFieldsCompatibility(t *testing.T) {
	check := func(raw json.RawMessage) {
		t.Helper()
		want, wantErr := previousParseCommunityDesignFields(raw)
		got, gotErr := parseCommunityDesign(raw)
		errorText := func(err error) string {
			if err == nil {
				return ""
			}
			return err.Error()
		}
		if !reflect.DeepEqual(got, want) || errorText(gotErr) != errorText(wantErr) {
			t.Fatalf("设计校验或清理结果改变：长度=%d，原错误=%v，当前错误=%v", len(raw), wantErr, gotErr)
		}
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(communityFixture), &fields); err != nil {
		t.Fatal(err)
	}
	for key, value := range fields {
		t.Run(key, func(t *testing.T) {
			for _, replacement := range []string{"null", " null ", `"null"`, "[]", "{}", "true", "false", "0", "1", "1e999", `"invalid"`} {
				raw := strings.Replace(communityFixture, `"`+key+`":`+string(value), `"`+key+`":`+replacement, 1)
				check(json.RawMessage(raw))
				check(json.RawMessage(strings.TrimSuffix(communityFixture, "}") + `,"` + key + `":` + replacement + `}`))
				check(json.RawMessage(strings.TrimSuffix(raw, "}") + `,"` + key + `":` + string(value) + `}`))
			}
			for _, variant := range []string{strings.ToUpper(key), fmt.Sprintf(`\u%04x%s`, key[0], key[1:])} {
				check(json.RawMessage(strings.Replace(communityFixture, `"`+key+`"`, `"`+variant+`"`, 1)))
			}
			clone := map[string]json.RawMessage{}
			for k, v := range fields {
				if k != key {
					clone[k] = v
				}
			}
			raw, err := json.Marshal(clone)
			if err != nil {
				t.Fatal(err)
			}
			check(raw)
		})
	}
	for _, raw := range []string{
		communityFixture, `{}`, `null`, `[]`, `1`, `"text"`, "", `{`, communityFixture + ` {}`, communityFixture + ` true`,
		strings.TrimSuffix(communityFixture, "}") + `,"unknown":"value"}`,
		strings.TrimSuffix(communityFixture, "}") + `,"background":null,"BACKGROUND":1}`,
		strings.TrimSuffix(communityFixture, "}") + `,"BACKGROUND":null}`,
		strings.TrimSuffix(communityFixture, "}") + `,"photo":null}`,
		strings.TrimSuffix(communityFixture, "}") + `,"photo":""}`,
		strings.TrimSuffix(communityFixture, "}") + `,"photo":"invalid"}`,
		strings.TrimSuffix(communityFixture, "}") + `,"photo":123}`,
	} {
		check(json.RawMessage(raw))
	}
	for _, size := range []int{0, 512000, 512001} {
		photo := avatarBufferImage(t, "jpeg", 8, 8)
		if size > 0 {
			padded := make([]byte, size)
			copy(padded, photo)
			photo = padded
		}
		check(communityDesignFieldsPhoto(t, photo))
	}
	check(communityDesignFieldsPhoto(t, avatarBufferImage(t, "jpeg", 512, 512)))
	check(communityDesignFieldsPhoto(t, avatarBufferImage(t, "png", 8, 8)))
}

func TestCommunityDesignFieldsPublishPhoto(t *testing.T) {
	db := testStore(t)
	user := complete(t, db, Identity{"email", "design-fields@example.test"})
	mux := http.NewServeMux()
	Mount(mux, &Service{store: db})
	photo := make([]byte, 512000)
	copy(photo, avatarBufferImage(t, "jpeg", 8, 8))
	raw := communityDesignFieldsPhoto(t, photo)
	want, err := previousParseCommunityDesignFields(raw)
	if err != nil {
		t.Fatal(err)
	}
	wantJSON, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	id := "d50edadd-83d9-4e13-ab7e-12a00d0ae112"
	body, err := json.Marshal(map[string]any{"id": id, "name": "字段检查测试", "description": "合成图片", "design": raw})
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, "/v1/community/skins", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+user.AccessToken)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != http.StatusCreated {
		t.Fatalf("发布失败：状态=%d，正文=%s", w.Code, w.Body.String())
	}
	r = httptest.NewRequest(http.MethodGet, "/v1/community/skins/"+id, nil)
	r.Header.Set("Authorization", "Bearer "+user.AccessToken)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	var got CommunitySkin
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || w.Code != http.StatusOK {
		t.Fatalf("详情读取失败：状态=%d，错误=%v", w.Code, err)
	}
	var saved, expected any
	if json.Unmarshal(got.Design, &saved) != nil || json.Unmarshal(wantJSON, &expected) != nil || !reflect.DeepEqual(saved, expected) {
		t.Fatal("持久化后的设计或照片清理字节改变")
	}
}

func BenchmarkCommunityDesignFields(b *testing.B) {
	jpegPhoto := avatarBufferImage(b, "jpeg", 512, 512)
	padded := make([]byte, 512000)
	copy(padded, avatarBufferImage(b, "jpeg", 8, 8))
	for _, tc := range []struct {
		name string
		raw  json.RawMessage
	}{
		{"noPhoto", json.RawMessage(communityFixture)},
		{"jpeg512", communityDesignFieldsPhoto(b, jpegPhoto)},
		{"paddedPhotoLimit", communityDesignFieldsPhoto(b, padded)},
	} {
		for _, impl := range []struct {
			name  string
			parse func(json.RawMessage) (CommunityDesign, error)
		}{
			{"previous", previousParseCommunityDesignFields}, {"current", parseCommunityDesign},
		} {
			b.Run(tc.name+"/"+impl.name, func(b *testing.B) { benchmarkCommunityDesignFields(b, impl.parse, tc.raw) })
		}
	}
}
