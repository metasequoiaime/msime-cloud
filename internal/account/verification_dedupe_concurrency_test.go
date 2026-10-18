package account

import (
	"context"
	"errors"
	"sync"
	"testing"
)

// 同一目标的并发验证码请求只能有一个新种子；其他挑战复用这个种子，避免并发窗口发送多封邮件。
func TestVerificationChallengeDeduplicatesConcurrentSeeds(t *testing.T) {
	db := testStore(t)
	ctx := context.Background()
	const provider, target = "email", "concurrent-code@example.test"
	makeChallenge := func(id string) (bool, string, error) {
		c := Challenge{IDHash: hash(id), Provider: provider, Subject: target}
		seed := "code-v1:" + randomToken()
		return db.PutVerificationChallenge(ctx, c, provider, target, seed, func(actual string) string {
			return hash("verification:" + actual)
		})
	}

	var wg sync.WaitGroup
	results := make(chan struct {
		reused bool
		seed   string
		err    error
	}, 2)
	for i := 0; i < 2; i++ {
		id := randomToken()
		wg.Add(1)
		go func() {
			defer wg.Done()
			reused, seed, err := makeChallenge(id)
			results <- struct {
				reused bool
				seed   string
				err    error
			}{reused, seed, err}
		}()
	}
	wg.Wait()
	close(results)

	var got []struct {
		reused bool
		seed   string
		err    error
	}
	for result := range results {
		if result.err != nil {
			t.Fatal(result.err)
		}
		got = append(got, result)
	}
	if len(got) != 2 || (got[0].reused == got[1].reused) || got[0].seed == "" || got[0].seed != got[1].seed {
		t.Fatalf("并发验证码种子未去重：%+v", got)
	}
	var challenges int
	if err := db.pool.QueryRow(ctx, `SELECT count(*) FROM auth_challenges WHERE provider=$1 AND subject=$2 AND nonce=$3`, provider, target, got[0].seed).Scan(&challenges); err != nil {
		t.Fatal(err)
	}
	if challenges != 2 {
		t.Fatalf("两个挑战应复用同一种子：挑战数=%d", challenges)
	}
	var activeSeed string
	if err := db.pool.QueryRow(ctx, `SELECT nonce FROM auth_challenges WHERE provider=$1 AND subject=$2 AND expires_at>clock_timestamp() LIMIT 1`, provider, target).Scan(&activeSeed); err != nil || activeSeed != got[0].seed {
		t.Fatalf("有效验证码种子不一致：%q，错误=%v", activeSeed, err)
	}
	if errors.Is(got[0].err, context.Canceled) || errors.Is(got[1].err, context.Canceled) {
		t.Fatal("并发请求不应被取消")
	}
}
