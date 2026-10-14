package account

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"testing"
)

func TestIndentJSONPreservesNumbers(t *testing.T) {
	for _, raw := range []string{
		`{"revision":9007199254740993,"settings":{"scale":0.12345678901234567890123456789}}`,
		`[9223372036854775807,-9223372036854775808,1.234567890123456789e+20]`,
	} {
		formatted := indentJSON([]byte(raw))
		var compact bytes.Buffer
		if err := json.Compact(&compact, formatted); err != nil {
			t.Fatal(err)
		}
		if compact.String() != raw {
			t.Errorf("导出排版改变了数值：输入=%s，输出=%s", raw, compact.String())
		}
		if !bytes.HasSuffix(formatted, []byte{'\n'}) {
			t.Error("导出 JSON 缺少末尾换行")
		}
	}
	invalid := []byte(`{"revision":`)
	if got := indentJSON(invalid); !bytes.Equal(got, invalid) {
		t.Fatalf("无效 JSON 的回退改变了原数据：%q", got)
	}
}

func TestUserDataExportPreservesPreferenceNumbers(t *testing.T) {
	s := testStore(t)
	user := complete(t, s, Identity{"email", "export-number@example.test"})
	mux := http.NewServeMux()
	Mount(mux, &Service{store: s})
	const number = "0.12345678901234567890123456789"
	body := `{"revision":0,"settings":{"general.floating_toolbar_scale":` + number + `}}`
	w := userDataRequest(mux, "PUT", "/v1/users/me/preferences", user.AccessToken, body)
	if w.Code != http.StatusOK {
		t.Fatal(w.Code, w.Body.String())
	}
	w = userDataRequest(mux, "GET", "/v1/users/me/preferences", user.AccessToken, "")
	var original Preferences
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &original) != nil || string(original.Settings["general.floating_toolbar_scale"]) != number {
		t.Fatal("同步接口未保留输入数值", w.Code, w.Body.String())
	}
	w = userDataRequest(mux, "GET", "/v1/users/me/data/export", user.AccessToken, "")
	if w.Code != http.StatusOK {
		t.Fatal(w.Code, w.Body.String())
	}
	archive, err := zip.NewReader(bytes.NewReader(w.Body.Bytes()), int64(w.Body.Len()))
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range archive.File {
		if file.Name != "preferences.json" {
			continue
		}
		reader, err := file.Open()
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(reader)
		reader.Close()
		if err != nil {
			t.Fatal(err)
		}
		var exported Preferences
		if err := json.Unmarshal(data, &exported); err != nil {
			t.Fatal(err)
		}
		if got := string(exported.Settings["general.floating_toolbar_scale"]); got != number || exported.Revision != original.Revision {
			t.Fatalf("ZIP 导出改变了已保存的偏好数值：输入=%s，输出=%s，revision=%d", number, got, exported.Revision)
		}
		return
	}
	t.Fatal("导出缺少 preferences.json")
}
