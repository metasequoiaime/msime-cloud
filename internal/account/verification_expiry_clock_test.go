package account

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestVerificationChallengeAfterTargetLockWait(t *testing.T) {
	for _, kind := range []string{"new", "active", "expired"} {
		t.Run(kind, func(t *testing.T) {
			db := testStore(t)
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			t.Setenv("AUTH_TEST_PEPPER", strings.Repeat("p", 32))
			sender := &captureSender{}
			a := &Service{store: db, config: Config{PepperEnv: "AUTH_TEST_PEPPER", Email: MailConfig{From: "login@example.test"}}, sender: sender}
			const provider, target = "email", "lock-expiry@example.test"
			oldID, oldSeed := randomToken(), "code-v1:"+randomToken()
			if kind != "new" {
				challenge := Challenge{IDHash: hash(oldID), Provider: provider, Subject: target, Nonce: oldSeed, CodeHash: a.codeHash(oldID, a.verificationCode(provider, target, oldSeed))}
				if err := db.PutChallenge(ctx, challenge); err != nil {
					t.Fatal(err)
				}
			}
			mux := http.NewServeMux()
			Mount(mux, a)
			request := func(path string, input any) *httptest.ResponseRecorder {
				raw, err := json.Marshal(input)
				if err != nil {
					panic(err)
				}
				r := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(raw)).WithContext(ctx)
				r.Header.Set("Content-Type", "application/json")
				w := httptest.NewRecorder()
				mux.ServeHTTP(w, r)
				return w
			}
			blocker, err := db.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer blocker.Rollback(context.Background())
			if _, err := blocker.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "verification-code:"+provider+":"+target); err != nil {
				t.Fatal(err)
			}
			done := make(chan *httptest.ResponseRecorder, 1)
			go func() {
				done <- request("/v1/auth/challenges", map[string]string{"provider": provider, "target": target})
			}()
			waitForAdminDatabaseLock(t, db, blocker.Conn().PgConn().PID())
			if kind == "expired" {
				// 初查已经见到旧种子，在事务重查前使它过期，验证不能把过期种子重新签发。
				if _, err := blocker.Exec(ctx, `UPDATE auth_challenges SET expires_at=clock_timestamp() WHERE id_hash=$1`, hash(oldID)); err != nil {
					t.Fatal(err)
				}
			}
			var unlockedAt time.Time
			if err := blocker.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&unlockedAt); err != nil {
				t.Fatal(err)
			}
			if err := blocker.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			w := <-done
			if w.Code != http.StatusCreated {
				t.Fatal("创建挑战失败", w.Code, w.Body.String())
			}
			var response struct {
				ID        string `json:"challenge_id"`
				Reused    bool   `json:"reused"`
				ExpiresIn int    `json:"expires_in"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil || response.ID == "" {
				t.Fatal("挑战响应无效", err, w.Body.String())
			}
			wantReused := kind == "active"
			if response.Reused != wantReused || response.ExpiresIn != 300 {
				t.Errorf("复用状态或声明有效期错误：reused=%v expires_in=%d", response.Reused, response.ExpiresIn)
			}
			var seed string
			var expiresAt time.Time
			if err := db.pool.QueryRow(ctx, `SELECT nonce,expires_at FROM auth_challenges WHERE id_hash=$1`, hash(response.ID)).Scan(&seed, &expiresAt); err != nil {
				t.Fatal(err)
			}
			if expiresAt.Before(unlockedAt.Add(5 * time.Minute)) {
				t.Errorf("新挑战被锁等待缩短了有效期：expires_at=%s earliest=%s", expiresAt, unlockedAt.Add(5*time.Minute))
			}
			if (seed == oldSeed) != wantReused {
				t.Errorf("验证码种子选择错误：kind=%s reused_old_seed=%v", kind, seed == oldSeed)
			}
			wantSent := 1
			if wantReused {
				wantSent = 0
			}
			var deliveries int
			if err := db.pool.QueryRow(ctx, `SELECT COALESCE((SELECT count FROM auth_rates WHERE key='delivery-global'),0)`).Scan(&deliveries); err != nil {
				t.Fatal(err)
			}
			if sender.sent != wantSent || deliveries != wantSent {
				t.Errorf("发信或额度次数错误：sent=%d deliveries=%d want=%d", sender.sent, deliveries, wantSent)
			}
			code := a.verificationCode(provider, target, oldSeed)
			if !wantReused {
				code = sender.code
			}
			if w := request("/v1/auth/login", map[string]string{"challenge_id": response.ID, "credential": code}); w.Code != http.StatusOK {
				t.Errorf("所选验证码无法完成新挑战：status=%d", w.Code)
			}
		})
	}
}
