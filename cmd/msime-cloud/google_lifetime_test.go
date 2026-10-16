package main

import (
	"bytes"
	"fmt"
	"io"
	"net/url"
	"testing"
)

func TestGoogleLoopbackLifetimeDoesNotOverflow(t *testing.T) {
	for _, test := range []struct {
		seconds int64
		wait    string
	}{
		{0, ""},
		{-1, ""},
		{30, ""},
		{-9223372036, ""},
		{-1<<63 + 600, ""},
		{60, "30s"},
		{329, "4m59s"},
		{330, "5m0s"},
		{600, "5m0s"},
		{9223372037, "5m0s"},
		{1<<63 - 1, "5m0s"},
	} {
		t.Run(fmt.Sprint(test.seconds), func(t *testing.T) {
			seconds := int(test.seconds)
			if int64(seconds) != test.seconds {
				t.Skip("该整数无法由当前平台的 int 表示")
			}
			var stderr bytes.Buffer
			opened := false
			callback := browser(t, url.Values{"code": {"google-code"}})
			c := cli{stdout: io.Discard, stderr: &stderr, browse: func(address string) error {
				opened = true
				return callback(address)
			}}
			_, code, err := c.googleLoopback(true, func(target string) (string, int, string, error) {
				address := "https://accounts.google.com/o/oauth2/auth?" + url.Values{"redirect_uri": {target}, "state": {"synthetic"}}.Encode()
				return address, seconds, "synthetic", nil
			})
			if test.wait == "" {
				if err == nil || opened {
					t.Fatalf("已过期的挑战仍打开浏览器：opened=%v code=%q err=%v", opened, code, err)
				}
				return
			}
			if err != nil || code != "google-code" || !opened || !bytes.Contains(stderr.Bytes(), []byte("waiting "+test.wait+" for")) {
				t.Fatalf("有效挑战的等待时间错误：opened=%v code=%q err=%v stderr=%q", opened, code, err, stderr.String())
			}
		})
	}
}
