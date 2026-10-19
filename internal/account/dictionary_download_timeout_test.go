package account

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// delayedDictionaryDownload 在首次写出时跨过全局写期限，验证下载入口会延长真实连接的期限。
type delayedDictionaryDownload struct {
	http.ResponseWriter
	resume  time.Time
	delayed bool
}

func (w *delayedDictionaryDownload) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *delayedDictionaryDownload) Write(data []byte) (int, error) {
	if !w.delayed {
		w.delayed = true
		// 这里特意等待旧的写期限失效，以复现慢速传输；不依赖调度器猜测请求是否开始。
		if remaining := time.Until(w.resume); remaining > 0 {
			timer := time.NewTimer(remaining)
			defer timer.Stop()
			<-timer.C
		}
	}
	return w.ResponseWriter.Write(data)
}

func TestDictionaryDownloadsOutliveDefaultWriteTimeout(t *testing.T) {
	store := testStore(t)
	user := complete(t, store, Identity{"email", "dictionary-download-timeout@example.test"})
	if _, err := store.EditDictionary(t.Context(), user.User.ID, "english", "", 0, &DictionaryEntry{Code: "synthetic", Word: "Synthetic", Weight: 10}); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	Mount(mux, &Service{store: store})
	for _, test := range []struct {
		name, path, pattern, body string
	}{
		{"snapshot", "/v1/users/me/dictionary/snapshot", "GET /v1/users/me/dictionary/snapshot", ""},
		{"standard", "/v1/users/me/dictionaries/english/export", "GET /v1/users/me/dictionaries/{kind}/export", "Synthetic\tsynthetic\t10\n"},
		{"windows", "/v1/users/me/dictionaries/english/export?format=windows", "GET /v1/users/me/dictionaries/{kind}/export", "synthetic\tSynthetic\t10\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if budget := accountRouteTimeout(test.pattern); budget < time.Minute || budget > 3*time.Minute {
				t.Errorf("完整词库下载的路由时限不足或无界：%v", budget)
			}
			server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mux.ServeHTTP(&delayedDictionaryDownload{ResponseWriter: w, resume: time.Now().Add(2 * time.Second)}, r)
			}))
			server.Config.WriteTimeout = time.Second
			server.Start()
			defer server.Close()
			client := server.Client()
			client.Timeout = 10 * time.Second
			request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL+test.path, nil)
			if err != nil {
				t.Fatal(err)
			}
			request.Header.Set("Authorization", "Bearer "+user.AccessToken)
			response, err := client.Do(request)
			if err != nil {
				t.Fatalf("下载被全局写期限截断：%v", err)
			}
			defer response.Body.Close()
			body, err := io.ReadAll(response.Body)
			if err != nil || response.StatusCode != http.StatusOK {
				t.Fatalf("下载不完整：状态=%d，错误=%v", response.StatusCode, err)
			}
			if test.name == "snapshot" {
				if err = decodeDictionarySnapshot(bytes.NewReader(body), func(snapshotRecord) error { return nil }); err != nil {
					t.Fatalf("快照的记录或完成校验损坏：%v", err)
				}
			} else if string(body) != test.body {
				t.Fatalf("TSV 内容改变：%q", body)
			}
		})
	}
}
