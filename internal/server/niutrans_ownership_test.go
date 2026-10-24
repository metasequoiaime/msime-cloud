package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/metasequoiaime/MSIME-Backend/internal/account"
)

func TestNiuTransUploadRejectsBanRace(t *testing.T) {
	admin, schema := disposableSchema(t)
	observer, err := pgx.Connect(context.Background(), os.Getenv("MSIME_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = observer.Close(context.Background()) })
	t.Setenv("TEST_AUTH_PEPPER", strings.Repeat("p", 64))

	db, err := account.Open(context.Background(), os.Getenv("MSIME_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	challengeID := "niutrans-upload-ban-race"
	sum := sha256.Sum256([]byte(challengeID))
	challenge := account.Challenge{IDHash: hex.EncodeToString(sum[:]), Provider: "email", Subject: challengeID + "@example.test"}
	if err := db.PutChallenge(context.Background(), challenge); err != nil {
		t.Fatal(err)
	}
	tokens, err := db.Complete(context.Background(), challenge, account.Identity{Provider: "email", Subject: challenge.Subject})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.DeleteUser(context.Background(), tokens.User.ID) })

	upstreamEntered := make(chan struct{}, 1)
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamEntered <- struct{}{}
		_, _ = io.WriteString(w, `{"code":200,"data":{"fileNo":"synthetic-task"}}`)
	}))
	t.Cleanup(upstream.Close)
	s, err := New(Config{Auth: account.Config{Enabled: true, DatabaseEnv: "MSIME_TEST_DATABASE_URL", PepperEnv: "TEST_AUTH_PEPPER"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close(); s.CloseAccounts() })
	s.client = upstream.Client()
	s.config.NiuTrans.Document = NiuTransEndpoint{URL: upstream.URL + "/upload", appID: "synthetic-app", apiKey: "synthetic-key"}

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	_ = mw.WriteField("from", "zh")
	_ = mw.WriteField("to", "en")
	file, _ := mw.CreateFormFile("file", "synthetic.txt")
	_, _ = file.Write([]byte("synthetic media"))
	_ = mw.Close()

	ban, err := admin.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	quoted := pgx.Identifier{schema}.Sanitize()
	if _, err := ban.Exec(context.Background(), "UPDATE "+quoted+".auth_users SET banned_at=now() WHERE id=$1", tokens.User.ID); err != nil {
		t.Fatal(err)
	}
	response := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		r := httptest.NewRequest(http.MethodPost, "/v1/niutrans/documents", bytes.NewReader(body.Bytes()))
		r.Header.Set("Authorization", "Bearer "+tokens.AccessToken)
		r.Header.Set("Content-Type", mw.FormDataContentType())
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		response <- w
	}()

	deadline := time.Now().Add(5 * time.Second)
	for {
		var waiting bool
		if err := observer.QueryRow(context.Background(), `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE wait_event_type='Lock' AND query LIKE 'SELECT banned_at IS NOT NULL FROM auth_users WHERE id=$1 FOR UPDATE%')`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		select {
		case <-upstreamEntered:
			t.Fatal("封禁提交前 NiuTrans 上传仍调用了上游")
		default:
		}
		if waiting {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("上传没有等待用户封禁行锁")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := ban.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	w := <-response
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), `"account_banned"`) {
		t.Fatalf("封禁后的 NiuTrans 上传请求: %d %s", w.Code, w.Body.String())
	}
	select {
	case <-upstreamEntered:
		t.Fatal("封禁后的 NiuTrans 上传仍调用了上游")
	default:
	}
}

func TestNiuTransMediaTasksStayWithUploader(t *testing.T) {
	var requests atomic.Int64
	s := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Method == http.MethodPost {
			_, _ = io.WriteString(w, `{"code":200,"data":{"fileNo":"synthetic-task"}}`)
			return
		}
		_, _ = io.WriteString(w, `{"code":200,"data":{"fileNo":"synthetic-task","transStatus":105}}`)
	})
	const otherToken = "synthetic-other-client-token-0000000000000"
	s.config.Clients = append(s.config.Clients, Client{ID: "other", RequestsPerMinute: 100, tokenHash: sha256.Sum256([]byte(otherToken))})
	e := NiuTransEndpoint{URL: s.config.Chat.URL + "/upload", appID: "synthetic-app", apiKey: "synthetic-key"}
	s.config.NiuTrans.Document, s.config.NiuTrans.Image, s.config.NiuTrans.Voice = e, e, e
	for _, kind := range []string{"documents", "images", "voice"} {
		t.Run(kind, func(t *testing.T) {
			var body bytes.Buffer
			mw := multipart.NewWriter(&body)
			_ = mw.WriteField("from", "zh")
			_ = mw.WriteField("to", "en")
			file, _ := mw.CreateFormFile("file", "synthetic.bin")
			_, _ = file.Write([]byte("synthetic media"))
			_ = mw.Close()
			w := callMultipart(s, http.MethodPost, "/v1/niutrans/"+kind, mw.FormDataContentType(), body.Bytes())
			var uploaded struct {
				Data struct {
					FileNo string `json:"fileNo"`
				} `json:"data"`
			}
			if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &uploaded) != nil || uploaded.Data.FileNo == "" {
				t.Fatal(w.Code, w.Body.String())
			}
			path := "/v1/niutrans/" + kind + "/" + uploaded.Data.FileNo
			for _, operation := range []struct{ method, suffix string }{
				{http.MethodGet, ""}, {http.MethodGet, "/download"}, {http.MethodPut, "/interrupt"},
				{http.MethodDelete, ""},
			} {
				if kind != "documents" && operation.method == http.MethodDelete {
					continue
				}
				before := requests.Load()
				r := httptest.NewRequest(operation.method, path+operation.suffix, nil)
				r.Header.Set("Authorization", "Bearer "+otherToken)
				w := httptest.NewRecorder()
				s.ServeHTTP(w, r)
				if w.Code != http.StatusNotFound || requests.Load() != before {
					t.Errorf("其他身份不应访问上传者的任务：%s %s status=%d upstream=%d body=%s", operation.method, kind+operation.suffix, w.Code, requests.Load()-before, w.Body.String())
				}
			}
			if w := call(s, http.MethodGet, path, ""); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), uploaded.Data.FileNo) {
				t.Fatal("上传者无法查询自己的任务", w.Code, w.Body.String())
			}
		})
	}
}

// niuTransTestTask 为旧有转发测试构造所属测试客户端的编号；跨身份回归仍从真实上传响应取得编号。
func niuTransTestTask(s *Server, kind, fileNo string) string {
	e := s.config.NiuTrans.Voice
	service := "niutrans_voice"
	switch kind {
	case "documents":
		e, service = s.config.NiuTrans.Document, "niutrans_document"
	case "images":
		e, service = s.config.NiuTrans.Image, "niutrans_image"
	}
	return niuTransTaskID(e, service, "test", fileNo)
}

func TestNiuTransTaskRejectsTamperingAndOtherServices(t *testing.T) {
	var requests atomic.Int64
	upstream := func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		_, _ = io.WriteString(w, `{"code":200}`)
	}
	s := fixture(t, upstream)
	e := NiuTransEndpoint{URL: s.config.Chat.URL + "/upload", appID: "synthetic-app", apiKey: "synthetic-key"}
	s.config.NiuTrans.Document, s.config.NiuTrans.Image, s.config.NiuTrans.Voice = e, e, e
	task := niuTransTestTask(s, "documents", "synthetic-task")
	parts := strings.Split(task, ".")
	for _, tc := range []struct{ name, kind, task string }{
		{"原始编号", "documents", "synthetic-task"},
		{"缺少签名", "documents", "nt1." + parts[1]},
		{"修改编号", "documents", "nt1." + base64.RawURLEncoding.EncodeToString([]byte("other-task")) + "." + parts[2]},
		{"修改签名", "documents", "nt1." + parts[1] + "." + strings.Repeat("A", 43)},
		{"无效编码", "documents", "nt1.!." + parts[2]},
		{"无效签名编码", "documents", "nt1." + parts[1] + ".!"},
		{"非法上游编号", "documents", niuTransTestTask(s, "documents", "file/other")},
		{"不同图片类型", "images", task},
		{"不同语音类型", "voice", task},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := requests.Load()
			w := call(s, http.MethodGet, "/v1/niutrans/"+tc.kind+"/"+tc.task, "")
			if w.Code != http.StatusNotFound || requests.Load() != before {
				t.Fatal(w.Code, w.Body.String())
			}
		})
	}
	replica := fixture(t, upstream)
	replica.config.NiuTrans.Document = e
	if w := call(replica, http.MethodGet, "/v1/niutrans/documents/"+task, ""); w.Code != http.StatusOK {
		t.Fatal(w.Code, w.Body.String())
	}
	for _, changed := range []NiuTransEndpoint{
		{URL: e.URL, appID: "other-app", apiKey: e.apiKey},
		{URL: e.URL, appID: e.appID, apiKey: "other-key"},
	} {
		replica.config.NiuTrans.Document = changed
		before := requests.Load()
		if w := call(replica, http.MethodGet, "/v1/niutrans/documents/"+task, ""); w.Code != http.StatusNotFound || requests.Load() != before {
			t.Fatal(w.Code, w.Body.String())
		}
	}
}

func TestNiuTransUploadResponseValidation(t *testing.T) {
	e := NiuTransEndpoint{appID: "synthetic-app", apiKey: "synthetic-key"}
	r := httptest.NewRequest(http.MethodPost, "/", nil)
	r = r.WithContext(context.WithValue(r.Context(), skinJobOwnerKey{}, "test"))
	for _, body := range []string{`null`, `[]`, `{}`, `{"code":null}`, `{"code":"200"}`, `{"code":200}`, `{"code":200,"data":null}`, `{"code":200,"data":{"fileNo":1}}`, `{"code":200,"data":{"fileNo":""}}`, `{"code":200,"data":{"fileNo":"file/other"}}`} {
		if _, err := niuTransUploadResult(r, e, "niutrans_document", []byte(body)); err == nil {
			t.Errorf("无效上传响应被接受：%s", body)
		}
	}
	failed := `{"code":10001,"msg":"synthetic failure"}`
	if got, err := niuTransUploadResult(r, e, "niutrans_document", []byte(failed)); err != nil || string(got) != failed {
		t.Fatal(string(got), err)
	}
	body := `{"code":200,"msg":"ok","data":{"fileNo":"synthetic-task","pages":2},"extra":true}`
	got, err := niuTransUploadResult(r, e, "niutrans_document", []byte(body))
	if err != nil {
		t.Fatal(err)
	}
	var reply struct {
		Code  int
		Msg   string
		Extra bool
		Data  struct {
			FileNo string
			Pages  int
		}
	}
	if json.Unmarshal(got, &reply) != nil || reply.Code != 200 || reply.Msg != "ok" || !reply.Extra || reply.Data.Pages != 2 {
		t.Fatal(string(got))
	}
	if raw, ok := niuTransTaskFileNo(e, "niutrans_document", "test", reply.Data.FileNo); !ok || raw != "synthetic-task" {
		t.Fatal(raw, ok)
	}
	if _, err := niuTransUploadResult(httptest.NewRequest(http.MethodPost, "/", nil), e, "niutrans_document", []byte(body)); err == nil {
		t.Fatal("没有上传身份仍然生成编号")
	}
}

func TestNiuTransTasksFollowUserAcrossSessions(t *testing.T) {
	disposableSchema(t)
	db, err := account.Open(t.Context(), os.Getenv("MSIME_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err = db.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	login := func(subject, challengeID string) account.Tokens {
		t.Helper()
		sum := sha256.Sum256([]byte(challengeID))
		challenge := account.Challenge{IDHash: hex.EncodeToString(sum[:]), Provider: "email", Subject: subject}
		if err := db.PutChallenge(t.Context(), challenge); err != nil {
			t.Fatal(err)
		}
		tokens, err := db.Complete(t.Context(), challenge, account.Identity{Provider: "email", Subject: subject})
		if err != nil {
			t.Fatal(err)
		}
		return tokens
	}
	owner := login("owner@example.test", "synthetic-owner")
	other := login("other@example.test", "synthetic-other")
	var requests atomic.Int64
	s := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		_, _ = io.WriteString(w, `{"code":200,"data":{"fileNo":"synthetic-user-task"}}`)
	})
	t.Setenv("TEST_AUTH_PEPPER", strings.Repeat("p", 64))
	s.accounts, err = account.New(t.Context(), account.Config{Enabled: true, DatabaseEnv: "MSIME_TEST_DATABASE_URL", PepperEnv: "TEST_AUTH_PEPPER"})
	if err != nil {
		t.Fatal(err)
	}
	defer s.CloseAccounts()
	s.config.NiuTrans.Document = NiuTransEndpoint{URL: s.config.Chat.URL + "/upload", appID: "synthetic-app", apiKey: "synthetic-key"}
	send := func(method, path, token string, body io.Reader, contentType string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, body)
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("Content-Type", contentType)
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		return w
	}
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	_ = mw.WriteField("from", "zh")
	_ = mw.WriteField("to", "en")
	file, _ := mw.CreateFormFile("file", "synthetic.txt")
	_, _ = file.Write([]byte("synthetic media"))
	_ = mw.Close()
	w := send(http.MethodPost, "/v1/niutrans/documents", owner.AccessToken, &body, mw.FormDataContentType())
	var uploaded struct{ Data struct{ FileNo string } }
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &uploaded) != nil || uploaded.Data.FileNo == "" {
		t.Fatal(w.Code, w.Body.String())
	}
	path := "/v1/niutrans/documents/" + uploaded.Data.FileNo
	before := requests.Load()
	for _, token := range []string{other.AccessToken, testToken} {
		if w := send(http.MethodGet, path, token, nil, ""); w.Code != http.StatusNotFound || requests.Load() != before {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	refreshed, err := db.Refresh(t.Context(), owner.RefreshToken)
	if err != nil {
		t.Fatal(err)
	}
	anotherSession := login("owner@example.test", "synthetic-owner-second-session")
	for _, token := range []string{refreshed.AccessToken, anotherSession.AccessToken} {
		if w := send(http.MethodGet, path, token, nil, ""); w.Code != http.StatusOK {
			t.Fatal(w.Code, w.Body.String())
		}
	}
}
