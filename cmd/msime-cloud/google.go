package main

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"runtime"
	"time"
)

// Google sign-in for a desktop: the RFC 8252 loopback flow the desktop app uses. The server holds the client secret and the PKCE verifier; this side listens on 127.0.0.1, checks the authorization URL the server built for it, and relays the code the browser brings back with the matching state.
const (
	googleSignInWait = 5 * time.Minute
	// Kept back from the challenge lifetime so a code that arrives at the end of the wait still finds the challenge alive.
	googleLoginMargin = 30 * time.Second
	googleCallback    = "/callback"
	callbackTimeout   = 2 * time.Second
)

const callbackPage = `<!doctype html><meta charset="utf-8"><title>水杉输入法</title><p>%s</p>`

// postJSON 不带凭据发送 JSON 并解码 2xx 响应；编码失败时不发送，其他状态输出正文并返回 statusError。
func (c cli) postJSON(server, path string, body any, v any) error {
	req, err := newJSONPostRequest(path, body)
	if err != nil {
		return err
	}
	response, err := c.do(server, req, "")
	if err != nil {
		return err
	}
	data, err := readCLIResponse(response)
	response.Body.Close()
	if err != nil {
		return err
	}
	if response.StatusCode < 200 || response.StatusCode > 299 {
		if err = c.writeLine(bytes.TrimSpace(data)); err != nil {
			return err
		}
		return statusError{response.Status}
	}
	if err = json.Unmarshal(data, v); err != nil {
		return fmt.Errorf("the answer from %s is unreadable: %w", path, err)
	}
	return nil
}

// googleLoopback listens on 127.0.0.1, lets begin ask the server for an authorization URL redirecting there, opens it, and returns the redirect and the code the browser brings back. begin returns the URL, how long the server keeps the sign-in open, and the state it expects, or "" to take the state from the URL.
func (c cli) googleLoopback(open bool, begin func(target string) (string, int, string, error)) (string, string, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", "", fmt.Errorf("cannot listen for the browser: %w", err)
	}
	defer listener.Close()
	port := listener.Addr().(*net.TCPAddr).Port
	if port < 1024 {
		return "", "", fmt.Errorf("the system gave a privileged port %d for the browser to return to", port)
	}
	target := fmt.Sprintf("http://127.0.0.1:%d%s", port, googleCallback)
	address, expiresIn, expected, err := begin(target)
	if err != nil {
		return "", "", err
	}
	state, err := googleState(address, target)
	if err != nil {
		return "", "", err
	}
	if expected != "" && subtle.ConstantTimeCompare([]byte(state), []byte(expected)) != 1 {
		return "", "", errors.New("the server's Google sign-in address does not carry the state it gave")
	}
	marginSeconds := int(googleLoginMargin / time.Second)
	if expiresIn <= marginSeconds {
		return "", "", errors.New("the sign-in expires too soon to wait for the browser")
	}
	// 先按秒检查并限制等待时间，再换算纳秒，避免有效期乘法或减去安全余量时溢出。
	wait := time.Duration(min(expiresIn-marginSeconds, int(googleSignInWait/time.Second))) * time.Second
	fmt.Fprintf(c.stderr, "msime-cloud: sign in with Google at this address; waiting %s for the browser to come back:\n%s\n", wait.Round(time.Second), address)
	if open {
		if err = c.browse(address); err != nil {
			fmt.Fprintf(c.stderr, "msime-cloud: cannot open a browser (%s); open the address yourself\n", err)
		}
	}
	code, err := receiveGoogleCode(listener, state, wait)
	return target, code, err
}

func (c cli) googleSignIn(server string, open bool) error {
	var challenge struct {
		ChallengeID      string `json:"challenge_id"`
		ExpiresIn        int    `json:"expires_in"`
		AuthorizationURL string `json:"authorization_url"`
	}
	_, code, err := c.googleLoopback(open, func(target string) (string, int, string, error) {
		err := c.postJSON(server, "/v1/auth/challenges", map[string]string{"provider": "google", "target": target, "purpose": "login"}, &challenge)
		if err == nil && challenge.ChallengeID == "" {
			err = errors.New("the server answered the Google sign-in without a challenge")
		}
		return challenge.AuthorizationURL, challenge.ExpiresIn, "", err
	})
	if err != nil {
		return err
	}
	return c.signIn(server, challenge.ChallengeID, code)
}

// adminSignIn signs an administrator in through the admin site's command-line flow and keeps the admin session, which lasts eight hours and is not refreshed.
func (c cli) adminSignIn(open bool) error {
	admin := c.adminServer()
	var started struct {
		State            string `json:"state"`
		ExpiresIn        int    `json:"expires_in"`
		AuthorizationURL string `json:"authorization_url"`
	}
	target, code, err := c.googleLoopback(open, func(target string) (string, int, string, error) {
		err := c.postJSON(admin, "/api/auth/cli/start", map[string]string{"redirect_uri": target}, &started)
		if err == nil && started.State == "" {
			err = errors.New("the admin site answered the sign-in without a state")
		}
		return started.AuthorizationURL, started.ExpiresIn, started.State, err
	})
	if err != nil {
		return err
	}
	var finished struct {
		Token     string `json:"token"`
		ExpiresIn int    `json:"expires_in"`
		Email     string `json:"email"`
	}
	if err = c.postJSON(admin, "/api/auth/cli/finish", map[string]string{"state": started.State, "code": code, "redirect_uri": target}, &finished); err != nil {
		return err
	}
	if finished.Token == "" || !validSessionLifetime(finished.ExpiresIn) {
		return errors.New("the admin site answered the sign-in without a valid session")
	}
	user, _ := json.Marshal(map[string]string{"email": finished.Email})
	kept := session{AccessToken: finished.Token, ExpiresAt: c.now().Add(time.Duration(finished.ExpiresIn) * time.Second), User: user}
	if err = c.store().put(adminKey(admin), &kept); err != nil {
		return err
	}
	_, err = fmt.Fprintf(c.stdout, "{\n  \"email\": %q\n}\n", finished.Email)
	return err
}

// adminKey is where the admin session for admin is kept, apart from user sessions for the same host.
func adminKey(admin string) string { return "admin " + admin }

// googleState 只接受 Google OAuth 授权端点，并完整解析查询参数、核对回调地址后返回 state。
func googleState(raw, target string) (string, error) {
	refused := errors.New("the server's Google sign-in address is not one this command opens")
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "https" || parsed.Host != "accounts.google.com" || parsed.User != nil || parsed.Fragment != "" || len(raw) > 4096 {
		return "", refused
	}
	// 同时兼容新旧授权端点，按转义路径匹配，拒绝其他页面及编码后的路径别名。
	switch parsed.EscapedPath() {
	case "/o/oauth2/auth", "/o/oauth2/v2/auth":
	default:
		return "", refused
	}
	query, err := url.ParseQuery(parsed.RawQuery)
	if err != nil || len(query["redirect_uri"]) != 1 || query.Get("redirect_uri") != target || len(query["state"]) != 1 {
		return "", refused
	}
	state := query.Get("state")
	if state == "" || len(state) > 512 {
		return "", refused
	}
	return state, nil
}

// receiveGoogleCode answers requests on listener until the browser returns with state, then gives the authorization code. Requests for other paths or with another state are answered and ignored.
func receiveGoogleCode(listener net.Listener, state string, wait time.Duration) (string, error) {
	type outcome struct {
		code string
		err  error
	}
	done := make(chan outcome, 1)
	finish := func(result outcome) {
		select {
		case done <- result:
		default:
		}
	}
	page := func(w http.ResponseWriter, status int, text string) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(status)
		fmt.Fprintf(w, callbackPage, text)
	}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		if r.Method != http.MethodGet || r.URL.Path != googleCallback || subtle.ConstantTimeCompare([]byte(query.Get("state")), []byte(state)) != 1 {
			page(w, http.StatusNotFound, "这不是本次登录的回调。")
			return
		}
		if query.Has("error") {
			page(w, http.StatusOK, "登录已取消，可以关闭此页面。")
			finish(outcome{err: errors.New("the Google sign-in was cancelled in the browser")})
			return
		}
		code := query.Get("code")
		if code == "" || len(code) > 2048 {
			page(w, http.StatusBadRequest, "登录失败，请回到终端重试。")
			finish(outcome{err: errors.New("the browser came back without a Google authorization code")})
			return
		}
		page(w, http.StatusOK, "登录完成，可以关闭此页面并回到终端。")
		finish(outcome{code: code})
	})
	server := &http.Server{Handler: handler, ReadHeaderTimeout: callbackTimeout, ReadTimeout: callbackTimeout, WriteTimeout: callbackTimeout, MaxHeaderBytes: 8 << 10}
	go server.Serve(listener)
	// The handler reports its outcome before its page is sent, so Close here would cut the browser off mid-response; Shutdown lets the page finish, bounded by the write timeout.
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), callbackTimeout)
		defer cancel()
		if server.Shutdown(ctx) != nil {
			server.Close()
		}
	}()
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case result := <-done:
		return result.code, result.err
	case <-timer.C:
		return "", errors.New("the browser did not come back in time; run `msime-cloud login google` again")
	}
}

// openBrowser opens a URL googleState has already checked, so it holds nothing a shell or the opener could read as an option.
func openBrowser(address string) error {
	var command *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		command = exec.Command("open", address)
	case "windows":
		command = exec.Command("rundll32", "url.dll,FileProtocolHandler", address)
	default:
		command = exec.Command("xdg-open", address)
	}
	if err := command.Start(); err != nil {
		return err
	}
	go command.Wait()
	return nil
}
