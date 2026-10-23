package account

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestWriteAdminImage(t *testing.T) {
	for _, tt := range []struct {
		name string
		mime string
		data []byte
	}{
		{"png", "image/png", []byte("\x89PNG\r\n\x1a\n\x00\xff")},
		{"jpeg", "image/jpeg", []byte("\xff\xd8\xff\x00\xfe")},
		{"webp", "image/webp", []byte("RIFF\x00\x00\x00\x00WEBP")},
		{"empty", "image/png", nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			w.Header().Set("Vary", "Origin")
			writeAdminImage(w, tt.mime, tt.data)
			response := w.Result()
			defer response.Body.Close()
			if response.StatusCode != http.StatusOK {
				t.Fatalf("status = %d", response.StatusCode)
			}
			for header, want := range map[string]string{
				"Content-Type":            tt.mime,
				"Cache-Control":           "no-store",
				"X-Content-Type-Options":  "nosniff",
				"Content-Security-Policy": "sandbox; default-src 'none'",
				"Vary":                    "Origin",
			} {
				if got := response.Header.Get(header); got != want {
					t.Errorf("%s = %q, want %q", header, got, want)
				}
			}
			data, err := io.ReadAll(response.Body)
			if err != nil || !bytes.Equal(data, tt.data) {
				t.Fatalf("body = %x, error = %v", data, err)
			}
		})
	}
}
