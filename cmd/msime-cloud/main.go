// Command msime-cloud lets an AI assistant, or a person, use the 水杉云 API from a shell: list and describe its operations, sign in with an email or SMS code or with Google, and call any operation with the session attached and refreshed. Responses go to stdout; anything for a person goes to stderr.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	msimebackend "github.com/metasequoiaime/MSIME-Backend"
	"github.com/metasequoiaime/MSIME-Backend/internal/contract"
)

const usage = `usage: msime-cloud <command> [arguments]

  tools                                  list the shared public query tools as JSON
  tool NAME [json|-]                     run one read-only dictionary or public-data tool
  mcp                                    serve the same tools over MCP stdio
  routes [filter]                        list the operations: method, path, auth, summary
  describe METHOD PATH                   print one operation: parameters, request body and responses
  call METHOD PATH [json|-] [-q name=value]... [-F name=value|name=@file]... [-o file]
                                         call an operation and print its response
  login start --email ADDRESS | --phone +8613800138000
                                         send a sign-in code and print the challenge_id
  login finish --challenge ID --code CODE
                                         sign in with the code the user received and keep the session
  login google [--browser false]         sign in with Google in the browser, or print the address to open
                                         with --browser false; waits up to 5 minutes for the browser
  login admin [--browser false]          sign in to the admin site with an administrator's Google account
                                         the same way; the session lasts 8 hours
  whoami [--admin]                       print the signed-in user, or the signed-in administrator
  logout [--all | --admin]               end the kept session, every session of its user, or the kept admin session
  version                                print the version

PATH may be a template from routes or a concrete path. /v1 paths go to the API with the kept session (or MSIME_CLOUD_TOKEN) when the operation needs one; /api paths go to the admin site with MSIME_ADMIN_TOKEN when set, otherwise with the session "login admin" kept; describe shows what docs/admin.md says about an /api route. logout always uses the kept session, ignoring environment tokens. A request body is a JSON object given inline, or read from stdin with -; -F sends multipart form fields instead, @ reading a file. A response that is not JSON or text needs -o. A 429 that asks to wait at most a minute is waited out and retried once.

Environment:
  MSIME_CLOUD_URL         API base URL (default https://api.msime.app)
  MSIME_CLOUD_TOKEN       a device token or access token to send instead of the kept session
  MSIME_ADMIN_URL         admin site base URL (default https://admin.msime.app)
  MSIME_ADMIN_TOKEN       the admin key or a personal access token, used instead of "login admin"
  MSIME_CLOUD_CONFIG_DIR  where the session is kept (default <user config dir>/msime-cloud)

Exit status: 0 when the command and output succeed, 1 when the call or output fails or the server answers with an error (its body is printed when possible), 2 for a usage error.`

const (
	defaultServer      = "https://api.msime.app"
	defaultAdminServer = "https://admin.msime.app"
	requestTimeout     = 5 * time.Minute
	// 自动等待 Retry-After 的最长时长；超过上限时直接返回响应。
	maxRetryWait = time.Minute
)

type usageError struct{ message string }

func (e usageError) Error() string { return e.message }

// statusError is a response outside 2xx, already printed.
type statusError struct{ status string }

func (e statusError) Error() string { return "the server answered " + e.status }

type cli struct {
	env    func(string) string
	stdin  io.Reader
	stdout io.Writer
	stderr io.Writer
	client *http.Client
	now    func() time.Time
	// browse opens a URL in the user's browser.
	browse func(string) error
}

func main() {
	c := cli{env: os.Getenv, stdin: os.Stdin, stdout: os.Stdout, stderr: os.Stderr, client: &http.Client{Timeout: requestTimeout}, now: time.Now, browse: openBrowser}
	os.Exit(c.run(os.Args[1:]))
}

func (c cli) run(args []string) int {
	err := c.dispatch(args)
	var bad usageError
	switch {
	case err == nil:
		return 0
	case errors.As(err, &bad):
		fmt.Fprintf(c.stderr, "msime-cloud: %s\n\n%s\n", err, usage)
		return 2
	default:
		fmt.Fprintf(c.stderr, "msime-cloud: %s\n", err)
		return 1
	}
}

func (c cli) dispatch(args []string) error {
	if len(args) == 0 {
		return usageError{"no command"}
	}
	command, args := args[0], args[1:]
	switch command {
	case "version", "--version":
		_, err := fmt.Fprintln(c.stdout, "msime-cloud "+msimebackend.Version())
		return err
	case "help", "-h", "--help":
		_, err := fmt.Fprintln(c.stdout, usage)
		return err
	case "routes":
		if len(args) > 1 {
			return usageError{"routes takes at most one filter"}
		}
		return listRoutes(c.stdout, strings.Join(args, ""))
	case "describe":
		if len(args) != 2 {
			return usageError{"describe needs METHOD and PATH"}
		}
		return describe(c.stdout, args[0], args[1])
	case "tools", "tool", "mcp":
		return c.publicTools(append([]string{command}, args...))
	case "call":
		return c.call(args)
	case "login":
		return c.login(args)
	case "whoami":
		switch {
		case len(args) == 0:
			return c.call([]string{"GET", "/v1/users/me"})
		case len(args) == 1 && args[0] == "--admin":
			return c.call([]string{"GET", "/api/me"})
		}
		return usageError{"whoami takes only --admin"}
	case "logout":
		return c.logout(args)
	}
	return usageError{"unknown command " + command}
}

// request is one call as the command line describes it.
type request struct {
	method, path string
	query        url.Values
	body         []byte
	contentType  string
	output       string
}

func (c cli) parseCall(args []string) (request, error) {
	r := request{query: url.Values{}}
	var positional []string
	var fields [][2]string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch arg {
		case "-q", "-F", "-o":
			if i+1 >= len(args) {
				return r, usageError{arg + " needs a value"}
			}
			i++
			value := args[i]
			switch arg {
			case "-o":
				r.output = value
			default:
				name, text, ok := strings.Cut(value, "=")
				if !ok || name == "" {
					return r, usageError{arg + " takes name=value"}
				}
				if arg == "-q" {
					r.query.Add(name, text)
				} else {
					fields = append(fields, [2]string{name, text})
				}
			}
		default:
			positional = append(positional, arg)
		}
	}
	if len(positional) < 2 || len(positional) > 3 {
		return r, usageError{"call needs METHOD, PATH and optionally a JSON body"}
	}
	r.method, r.path = strings.ToUpper(positional[0]), positional[1]
	if !strings.HasPrefix(r.path, "/") {
		return r, usageError{"PATH starts with /, such as /v1/users/me"}
	}
	if len(positional) == 3 {
		if len(fields) > 0 {
			return r, usageError{"send either a JSON body or -F fields, not both"}
		}
		text := []byte(positional[2])
		if positional[2] == "-" {
			var err error
			if text, err = io.ReadAll(c.stdin); err != nil {
				return r, fmt.Errorf("cannot read the body from stdin: %w", err)
			}
		}
		if !json.Valid(text) {
			return r, usageError{"the body is not valid JSON"}
		}
		r.body, r.contentType = text, "application/json"
	}
	if len(fields) > 0 {
		var buffer bytes.Buffer
		form := multipart.NewWriter(&buffer)
		for _, field := range fields {
			name, value := field[0], field[1]
			path, isFile := strings.CutPrefix(value, "@")
			if !isFile {
				if err := form.WriteField(name, value); err != nil {
					return r, err
				}
				continue
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return r, fmt.Errorf("cannot read %s: %w", path, err)
			}
			part, err := form.CreateFormFile(name, filepath.Base(path))
			if err != nil {
				return r, err
			}
			if _, err = part.Write(data); err != nil {
				return r, err
			}
		}
		if err := form.Close(); err != nil {
			return r, err
		}
		r.body, r.contentType = buffer.Bytes(), form.FormDataContentType()
	}
	return r, nil
}

func (c cli) call(args []string) error {
	r, err := c.parseCall(args)
	if err != nil {
		return err
	}
	response, err := c.send(r)
	if err != nil {
		return err
	}
	return c.print(response, r.output)
}

// send makes the request with the credentials its operation takes, refreshing a kept session that has expired or that the server no longer accepts, once.
func (c cli) send(r request) (*http.Response, error) {
	if strings.HasPrefix(r.path, adminPrefix) {
		return c.sendAdmin(r)
	}
	if path, _, _ := strings.Cut(r.path, "?"); path == contract.StreamingTranscriptionPath {
		return nil, errors.New(path + " is a WebSocket for live audio; this command speaks HTTP only")
	}
	auth := authAny
	if operations, err := catalog(); err == nil {
		if op, ok := find(operations, r.method, r.path); ok {
			auth = op.Auth
		} else {
			fmt.Fprintf(c.stderr, "msime-cloud: %s %s is not in the API description; sending it anyway\n", r.method, r.path)
		}
	}
	server := c.server()
	if auth == authNone {
		return c.do(server, r, "")
	}
	if token := c.env("MSIME_CLOUD_TOKEN"); token != "" {
		return c.do(server, r, token)
	}
	return c.sendKeptSession(r)
}

// sendKeptSession 使用本地保存的用户会话，必要时刷新；退出不能改用环境令牌的身份。
func (c cli) sendKeptSession(r request) (*http.Response, error) {
	server := c.server()
	sessions := c.store()
	current, ok, err := sessions.get(server)
	if err != nil {
		return nil, err
	}
	if !ok {
		response, err := c.do(server, r, "")
		if err == nil && response.StatusCode == http.StatusUnauthorized {
			fmt.Fprintln(c.stderr, "msime-cloud: not signed in; run `msime-cloud login start --email ...`, or set MSIME_CLOUD_TOKEN")
		}
		return response, err
	}
	return c.sendSession(server, r, current)
}

// sendSession 固定请求选中的登录；刷新只能沿用同一次登录的凭据。
func (c cli) sendSession(server string, r request, current session) (*http.Response, error) {
	var err error
	refreshed := false
	if c.now().Add(refreshMargin).After(current.ExpiresAt) {
		if current, err = c.refresh(server, current); err != nil {
			return nil, err
		}
		refreshed = true
	}
	response, err := c.do(server, r, current.AccessToken)
	if err != nil || response.StatusCode != http.StatusUnauthorized || refreshed {
		return response, err
	}
	response.Body.Close()
	if current, err = c.refresh(server, current); err != nil {
		return nil, err
	}
	return c.do(server, r, current.AccessToken)
}

// sendAdmin sends an /api request to the admin site with MSIME_ADMIN_TOKEN (the admin key or a personal access token) when set, otherwise with the session `login admin` kept. That session lasts eight hours and is not refreshed; one the site no longer accepts is forgotten.
func (c cli) sendAdmin(r request) (*http.Response, error) {
	admin := c.adminServer()
	if token := c.env("MSIME_ADMIN_TOKEN"); token != "" {
		return c.do(admin, r, token)
	}
	sessions := c.store()
	kept, ok, err := sessions.get(adminKey(admin))
	if err != nil {
		return nil, err
	}
	if !ok || !c.now().Before(kept.ExpiresAt) {
		if ok {
			if err = sessions.forget(adminKey(admin), kept.AccessToken); err != nil {
				return nil, err
			}
		}
		return nil, errors.New("not signed in to the admin site; run `msime-cloud login admin`, or set MSIME_ADMIN_TOKEN to the admin key or a personal access token")
	}
	response, err := c.do(admin, r, kept.AccessToken)
	if err == nil && response.StatusCode == http.StatusUnauthorized {
		if err = sessions.forget(adminKey(admin), kept.AccessToken); err != nil {
			response.Body.Close()
			return nil, err
		}
		fmt.Fprintln(c.stderr, "msime-cloud: the admin session has ended; run `msime-cloud login admin` again")
	}
	return response, err
}

// refresh 在文件锁下轮换令牌，或复用其他进程对同一次登录的刷新结果。
func (c cli) refresh(server string, used session) (session, error) {
	sessions := c.store()
	unlock, err := sessions.lock()
	if err != nil {
		return session{}, err
	}
	defer unlock()
	latest, ok, err := sessions.get(server)
	if err != nil {
		return session{}, err
	}
	if !ok {
		return session{}, errors.New("signed out by another command; sign in again")
	}
	if latest.loginID() != used.loginID() {
		return session{}, errors.New("another command signed in again; retry this command with the current login")
	}
	if latest.RefreshToken != used.RefreshToken {
		return latest, nil
	}
	body, _ := json.Marshal(map[string]string{"refresh_token": latest.RefreshToken})
	response, err := c.do(server, request{method: "POST", path: "/v1/auth/refresh", body: body, contentType: "application/json"}, "")
	if err != nil {
		return session{}, err
	}
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	if err != nil {
		return session{}, err
	}
	if response.StatusCode == http.StatusUnauthorized {
		if err = sessions.putLocked(server, nil); err != nil {
			return session{}, err
		}
		return session{}, errors.New("the session has ended; sign in again with `msime-cloud login start`")
	}
	if response.StatusCode != http.StatusOK {
		return session{}, fmt.Errorf("refreshing the session failed with %s: %s", response.Status, strings.TrimSpace(string(data)))
	}
	var tokens tokenResponse
	if err = json.Unmarshal(data, &tokens); err != nil || !tokens.valid() {
		return session{}, errors.New("the refresh response carries an invalid session")
	}
	next := tokens.session(c.now())
	next.LoginID = latest.loginID()
	if len(next.User) == 0 {
		next.User = latest.User
	}
	return next, sessions.putLocked(server, &next)
}

// do 发送请求；GET、HEAD、OPTIONS 收到 429 且等待不超过 maxRetryWait 时重试一次。
func (c cli) do(server string, r request, token string) (*http.Response, error) {
	response, err := c.doOnce(server, r, token)
	if err != nil || response.StatusCode != http.StatusTooManyRequests || (r.method != http.MethodGet && r.method != http.MethodHead && r.method != http.MethodOptions) {
		return response, err
	}
	seconds, parseErr := strconv.Atoi(strings.TrimSpace(response.Header.Get("Retry-After")))
	// 先检查秒数再转换时长，避免大整数乘以 time.Second 后溢出。
	if parseErr != nil || seconds < 0 || seconds > int(maxRetryWait/time.Second) {
		return response, nil
	}
	wait := time.Duration(seconds) * time.Second
	response.Body.Close()
	fmt.Fprintf(c.stderr, "msime-cloud: rate limited; retrying in %s\n", wait)
	time.Sleep(wait)
	return c.doOnce(server, r, token)
}

func (c cli) doOnce(server string, r request, token string) (*http.Response, error) {
	target := strings.TrimRight(server, "/") + r.path
	if len(r.query) > 0 {
		separator := "?"
		if strings.Contains(target, "?") {
			separator = "&"
		}
		target += separator + r.query.Encode()
	}
	req, err := http.NewRequest(r.method, target, bytes.NewReader(r.body))
	if err != nil {
		return nil, usageError{err.Error()}
	}
	if r.contentType != "" {
		req.Header.Set("Content-Type", r.contentType)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "msime-cloud/"+msimebackend.Version())
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return c.client.Do(req)
}

// print 把响应写入文件或标准输出；文件下载完整接收并关闭成功后才替换目标。
func (c cli) print(response *http.Response, output string) error {
	defer response.Body.Close()
	failed := response.StatusCode < 200 || response.StatusCode > 299
	if output != "" && !failed {
		if info, err := os.Lstat(output); err == nil {
			if !info.Mode().IsRegular() {
				return errors.New("output must be a regular file")
			}
		} else if !os.IsNotExist(err) {
			return err
		}
		file, err := os.CreateTemp(filepath.Dir(output), ".msime-cloud-download-*")
		if err != nil {
			return err
		}
		defer os.Remove(file.Name())
		n, err := io.Copy(file, response.Body)
		if closeErr := file.Close(); err == nil {
			err = closeErr
		}
		if err != nil {
			return err
		}
		if err = os.Rename(file.Name(), output); err != nil {
			return err
		}
		saved, _ := json.Marshal(map[string]any{"saved": output, "bytes": n, "content_type": response.Header.Get("Content-Type")})
		_, err = fmt.Fprintln(c.stdout, string(saved))
		return err
	}
	data, err := io.ReadAll(response.Body)
	if err != nil {
		return err
	}
	kind, _, _ := mime.ParseMediaType(response.Header.Get("Content-Type"))
	switch {
	case len(data) == 0:
	case kind == "application/json" || strings.HasSuffix(kind, "+json"):
		var indented bytes.Buffer
		if json.Indent(&indented, data, "", "  ") == nil {
			data = indented.Bytes()
		}
		_, err = fmt.Fprintln(c.stdout, strings.TrimRight(string(data), "\n"))
	case strings.HasPrefix(kind, "text/"):
		_, err = fmt.Fprintln(c.stdout, strings.TrimRight(string(data), "\n"))
	default:
		if !failed {
			return fmt.Errorf("the response is %s (%d bytes); pass -o <file> to save it", kind, len(data))
		}
	}
	if err != nil {
		return err
	}
	if failed {
		return statusError{response.Status}
	}
	return nil
}

func (c cli) login(args []string) error {
	if len(args) == 0 {
		return usageError{"login needs start, finish, google or admin"}
	}
	step, flags := args[0], map[string]string{}
	for i := 1; i < len(args); i += 2 {
		name := strings.TrimPrefix(args[i], "--")
		if name == args[i] || i+1 >= len(args) {
			return usageError{"login takes --name value pairs"}
		}
		flags[name] = args[i+1]
	}
	server := c.server()
	switch step {
	case "start":
		provider, target := "email", flags["email"]
		if phone := flags["phone"]; phone != "" {
			provider, target = "phone", phone
		}
		if len(flags) != 1 || target == "" {
			return usageError{"login start takes --email ADDRESS or --phone +8613800138000"}
		}
		body, _ := json.Marshal(map[string]string{"provider": provider, "target": target, "purpose": "login"})
		response, err := c.do(server, request{method: "POST", path: "/v1/auth/challenges", body: body, contentType: "application/json"}, "")
		if err != nil {
			return err
		}
		if err = c.print(response, ""); err == nil {
			fmt.Fprintf(c.stderr, "msime-cloud: a code was sent to %s; once the user reads it out, run `msime-cloud login finish --challenge <challenge_id> --code <code>`\n", target)
		}
		return err
	case "finish":
		challenge, code := flags["challenge"], flags["code"]
		if len(flags) != 2 || challenge == "" || code == "" {
			return usageError{"login finish takes --challenge ID --code CODE"}
		}
		return c.signIn(server, challenge, code)
	case "google", "admin":
		open := true
		switch {
		case len(flags) == 0:
		case len(flags) == 1 && flags["browser"] == "false":
			open = false
		default:
			return usageError{"login " + step + " takes only --browser false"}
		}
		if step == "admin" {
			return c.adminSignIn(open)
		}
		return c.googleSignIn(server, open)
	}
	return usageError{"login needs start, finish, google or admin"}
}

// signIn completes a challenge with credential, keeps the session and prints the user.
func (c cli) signIn(server, challenge, credential string) error {
	body, _ := json.Marshal(map[string]string{"challenge_id": challenge, "credential": credential})
	response, err := c.do(server, request{method: "POST", path: "/v1/auth/login", body: body, contentType: "application/json"}, "")
	if err != nil {
		return err
	}
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	if err != nil {
		return err
	}
	if response.StatusCode != http.StatusOK {
		if _, err = fmt.Fprintln(c.stdout, strings.TrimSpace(string(data))); err != nil {
			return err
		}
		return statusError{response.Status}
	}
	var tokens tokenResponse
	if err = json.Unmarshal(data, &tokens); err != nil || !tokens.valid() {
		return errors.New("the sign-in response carries an invalid session")
	}
	signedIn := tokens.session(c.now())
	if err = c.store().put(server, &signedIn); err != nil {
		return err
	}
	// The tokens stay in the credentials file; only the user is shown.
	user, _ := json.MarshalIndent(json.RawMessage(tokens.User), "", "  ")
	_, err = fmt.Fprintln(c.stdout, string(user))
	return err
}

func (c cli) logout(args []string) error {
	all := false
	switch {
	case len(args) == 1 && args[0] == "--admin":
		return c.adminLogout()
	case len(args) == 1 && args[0] == "--all":
		all = true
	case len(args) != 0:
		return usageError{"logout takes only --all or --admin"}
	}
	server := c.server()
	sessions := c.store()
	current, ok, err := sessions.get(server)
	if err != nil || !ok {
		if err == nil {
			err = errors.New("not signed in to " + server)
		}
		return err
	}
	body, _ := json.Marshal(map[string]bool{"all": all})
	response, err := c.sendSession(server, request{method: "POST", path: "/v1/auth/logout", body: body, contentType: "application/json"}, current)
	if err != nil {
		return err
	}
	response.Body.Close()
	// A session the server already ended is as good as ended here.
	if response.StatusCode != http.StatusOK && response.StatusCode != http.StatusNoContent && response.StatusCode != http.StatusUnauthorized {
		return statusError{response.Status}
	}
	if err = sessions.forgetLogin(server, current.loginID()); err != nil {
		return err
	}
	fmt.Fprintln(c.stderr, "msime-cloud: signed out of "+server)
	return nil
}

// adminLogout ends the session `login admin` kept, on the site and here. MSIME_ADMIN_TOKEN is left alone: keys and personal access tokens are revoked in the admin console.
func (c cli) adminLogout() error {
	admin := c.adminServer()
	sessions := c.store()
	kept, ok, err := sessions.get(adminKey(admin))
	if err != nil {
		return err
	}
	if !ok {
		return errors.New("not signed in to " + admin)
	}
	response, err := c.do(admin, request{method: "POST", path: "/api/auth/logout"}, kept.AccessToken)
	if err != nil {
		return err
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK && response.StatusCode != http.StatusUnauthorized {
		return statusError{response.Status}
	}
	if err = sessions.forget(adminKey(admin), kept.AccessToken); err != nil {
		return err
	}
	fmt.Fprintln(c.stderr, "msime-cloud: signed out of "+admin)
	return nil
}

func (c cli) server() string {
	if value := c.env("MSIME_CLOUD_URL"); value != "" {
		return strings.TrimRight(value, "/")
	}
	return defaultServer
}

func (c cli) adminServer() string {
	if value := c.env("MSIME_ADMIN_URL"); value != "" {
		return strings.TrimRight(value, "/")
	}
	return defaultAdminServer
}

func (c cli) store() store {
	if dir := c.env("MSIME_CLOUD_CONFIG_DIR"); dir != "" {
		return store{dir: dir}
	}
	// Without a config directory the store refuses rather than leave tokens somewhere shared such as the temp directory.
	base, err := os.UserConfigDir()
	if err != nil {
		return store{}
	}
	return store{dir: filepath.Join(base, "msime-cloud")}
}
