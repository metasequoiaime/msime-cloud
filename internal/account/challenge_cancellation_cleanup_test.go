package account

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// cancelledChallengeSender 在投递尚未成功时取消请求，复现客户端断开后 SMTP 失败的边界。
type cancelledChallengeSender struct {
	cancel        context.CancelFunc
	calls         int
	deliveredCode string
}

func (s *cancelledChallengeSender) Send(ctx context.Context, _, _, code string) error {
	s.calls++
	if s.calls == 1 {
		s.cancel()
		<-ctx.Done()
		return &deliveryFailure{}
	}
	s.deliveredCode = code
	return nil
}

func TestVerificationDeliveryCancellationDoesNotLeaveReusableChallenge(t *testing.T) {
	db := testStore(t)
	t.Setenv("AUTH_TEST_PEPPER", strings.Repeat("p", 32))
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	sender := &cancelledChallengeSender{cancel: cancel}
	a := &Service{store: db, config: Config{PepperEnv: "AUTH_TEST_PEPPER", Email: MailConfig{From: "login@example.test"}}, sender: sender}
	mux := http.NewServeMux()
	Mount(mux, a)
	const target = "cancelled-verification@example.test"
	request := func(ctx context.Context, path, body string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequestWithContext(ctx, http.MethodPost, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		return w
	}
	body := `{"provider":"email","target":"` + target + `"}`
	if w := request(ctx, "/v1/auth/challenges", body); w.Code != http.StatusServiceUnavailable {
		t.Fatal("投递取消未返回失败", w.Code, w.Body.String())
	}
	var remaining int
	if err := db.pool.QueryRow(t.Context(), `SELECT count(*) FROM auth_challenges WHERE provider='email' AND subject=$1`, target).Scan(&remaining); err != nil || remaining != 0 {
		t.Errorf("取消后留下可被复用的失败挑战：count=%d err=%v", remaining, err)
	}
	// 只推进同一目标的分钟窗口，模拟允许重试；不靠真实等待或绕过生产鉴权流程。
	if _, err := db.pool.Exec(t.Context(), `UPDATE auth_rates SET expires_at=clock_timestamp()-interval '1 second' WHERE key=$1`, "target-minute:"+a.codeHash("email", target)); err != nil {
		t.Fatal(err)
	}
	w := request(t.Context(), "/v1/auth/challenges", body)
	var challenge struct {
		ID     string `json:"challenge_id"`
		Reused bool   `json:"reused"`
	}
	if w.Code != http.StatusCreated || json.Unmarshal(w.Body.Bytes(), &challenge) != nil || challenge.ID == "" {
		t.Fatal("重试未创建挑战", w.Code, w.Body.String())
	}
	if challenge.Reused || sender.calls != 2 || sender.deliveredCode == "" {
		t.Fatalf("重试没有投递新验证码：reused=%v attempts=%d delivered=%v", challenge.Reused, sender.calls, sender.deliveredCode != "")
	}
	var used int
	if err := db.pool.QueryRow(t.Context(), `SELECT count FROM auth_rates WHERE key='delivery-global'`).Scan(&used); err != nil || used != 1 {
		t.Fatal("失败投递与重试未正确计算全站额度", used, err)
	}
	login, err := json.Marshal(map[string]string{"challenge_id": challenge.ID, "credential": sender.deliveredCode})
	if err != nil {
		t.Fatal(err)
	}
	if w := request(t.Context(), "/v1/auth/login", string(login)); w.Code != http.StatusOK {
		t.Fatal("重试投递的验证码无法登录", w.Code, w.Body.String())
	}
}

func TestDropChallengeAfterRequestCancellation(t *testing.T) {
	for _, expired := range []bool{false, true} {
		name := "主动取消"
		if expired {
			name = "期限已过"
		}
		t.Run(name, func(t *testing.T) {
			db := testStore(t)
			id := hash(randomToken())
			if err := db.PutChallenge(t.Context(), Challenge{IDHash: id, Provider: "email", Subject: "cleanup@example.test"}); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			if expired {
				cancel()
				ctx, cancel = context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
			}
			cancel()
			db.DropChallenge(ctx, id)
			var remaining int
			if err := db.pool.QueryRow(t.Context(), `SELECT count(*) FROM auth_challenges WHERE id_hash=$1`, id).Scan(&remaining); err != nil || remaining != 0 {
				t.Fatal("请求结束后挑战清理失效", remaining, err)
			}
		})
	}
}
