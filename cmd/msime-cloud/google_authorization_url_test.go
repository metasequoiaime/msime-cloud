package main

import (
	"bytes"
	"io"
	"net/url"
	"testing"
)

func TestGoogleAuthorizationURLRejectsOtherPagesAndMalformedQuery(t *testing.T) {
	target := "http://127.0.0.1:50000/callback"
	query := url.Values{"redirect_uri": {target}, "state": {"synthetic-state"}}.Encode()
	for _, path := range []string{"/o/oauth2/auth", "/o/oauth2/v2/auth"} {
		if state, err := googleState("https://accounts.google.com"+path+"?"+query, target); err != nil || state != "synthetic-state" {
			t.Fatalf("拒绝合法授权端点 %q：state=%q err=%v", path, state, err)
		}
	}
	for _, address := range []string{
		"https://accounts.google.com?" + query,
		"https://accounts.google.com/Logout?" + query,
		"https://accounts.google.com/ServiceLogin?" + query,
		"https://accounts.google.com/o/oauth2/v2/auth/other?" + query,
		"https://accounts.google.com/o%2Foauth2/v2/auth?" + query,
		"https://accounts.google.com/o/oauth2/v2/auth?" + query + "&scope=%zz",
		"https://accounts.google.com/o/oauth2/v2/auth?" + query + "&scope=openid;email",
	} {
		t.Run(address, func(t *testing.T) {
			if _, err := googleState(address, target); err == nil {
				t.Fatal("接受了非授权页面或无法完整解析的查询参数")
			}
		})
	}
}

func TestGoogleLoopbackRejectsOtherPagesBeforeOpeningBrowser(t *testing.T) {
	var stderr bytes.Buffer
	opened := false
	callback := browser(t, url.Values{"code": {"synthetic-code"}})
	c := cli{stdout: io.Discard, stderr: &stderr, browse: func(address string) error {
		opened = true
		return callback(address)
	}}
	_, _, err := c.googleLoopback(true, func(target string) (string, int, string, error) {
		query := url.Values{"redirect_uri": {target}, "state": {"synthetic-state"}}
		return "https://accounts.google.com/Logout?" + query.Encode(), 60, "synthetic-state", nil
	})
	if err == nil || opened || stderr.Len() != 0 {
		t.Fatalf("校验前已展示或打开非授权地址：opened=%v stderr=%q err=%v", opened, stderr.String(), err)
	}
}
