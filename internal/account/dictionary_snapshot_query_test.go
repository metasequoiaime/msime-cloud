package account

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/metasequoiaime/MSIME-Backend/internal/engine"
)

const dictionaryQueryProcess = "MSIME_TEST_DICTIONARY_QUERY_PROCESS"

// 使用当前测试程序检查查询信封和实际传入子进程的 JSONL，避免依赖平台 shell。
func TestDictionaryQueryEngineProcess(t *testing.T) {
	if os.Getenv(dictionaryQueryProcess) != "1" {
		return
	}
	var request struct {
		Operation string         `json:"operation"`
		Query     map[string]any `json:"query"`
	}
	if json.NewDecoder(os.Stdin).Decode(&request) != nil || request.Operation != "personal_query" {
		os.Exit(2)
	}
	raw, err := os.ReadFile(filepath.Join(os.Args[len(os.Args)-1], "snapshot.jsonl"))
	if err != nil || len(raw) == 0 || raw[len(raw)-1] != '\n' {
		os.Exit(2)
	}
	var rows []json.RawMessage
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	for {
		var row json.RawMessage
		err := decoder.Decode(&row)
		if err == io.EOF {
			break
		}
		if err != nil {
			os.Exit(2)
		}
		rows = append(rows, row)
	}
	json.NewEncoder(os.Stdout).Encode(map[string]any{"query": request.Query, "snapshot": rows})
	os.Exit(0)
}

func TestPersonalDictionaryQuerySnapshotHTTP(t *testing.T) {
	store := testStore(t)
	one := complete(t, store, Identity{"email", "query-wire-one@example.test"})
	two := complete(t, store, Identity{"email", "query-wire-two@example.test"})
	for _, item := range []struct{ user, word string }{{one.User.ID, "Own"}, {two.User.ID, "Other"}} {
		if _, err := store.EditDictionary(t.Context(), item.user, "english", "", 0, &DictionaryEntry{Code: "hello", Word: item.word, Weight: 10}); err != nil {
			t.Fatal(err)
		}
	}
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(dictionaryQueryProcess, "1")
	service := &Service{store: store, engine: engine.Config{Binary: binary, Resources: "-test.run=^TestDictionaryQueryEngineProcess$"}}
	mux := http.NewServeMux()
	Mount(mux, service)
	for _, tc := range []struct {
		method, path, body, operation string
		limit                         float64
	}{
		{"POST", "/v1/users/me/dictionary/candidates", `{"kind":"english","text":"HELLO","limit":5}`, "english", 5},
		{"GET", "/v1/users/me/dictionaries/english/catalog?q=HELLO&offset=2&limit=3", "", "dictionary", 3},
		{"GET", "/v1/users/me/dictionaries/quick/catalog?limit=4", "", "dictionary", 4},
	} {
		t.Run(tc.path, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
			req.Header.Set("Authorization", "Bearer "+one.AccessToken)
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, req)
			if w.Code != 200 {
				t.Fatalf("状态=%d，响应=%s", w.Code, w.Body.String())
			}
			var result struct {
				Query    map[string]any    `json:"query"`
				Snapshot []json.RawMessage `json:"snapshot"`
			}
			if json.Unmarshal(w.Body.Bytes(), &result) != nil || result.Query["operation"] != tc.operation || result.Query["limit"] != tc.limit || len(result.Snapshot) == 0 {
				t.Fatal(w.Body.String())
			}
			if tc.operation == "english" && result.Query["text"] != "hello" {
				t.Fatal(result.Query)
			}
			if strings.Contains(tc.path, "english/catalog") && (result.Query["offset"] != float64(2) || result.Query["kind"] != "english" || result.Query["text"] != "hello") {
				t.Fatal(result.Query)
			}
			if strings.Contains(tc.path, "quick") {
				if result.Query["text"] != "" || result.Query["kind"] != "quick" {
					t.Fatal(result.Query)
				}
			} else if !strings.Contains(w.Body.String(), "Own") {
				t.Fatal("丢失当前用户词条", w.Body.String())
			}
			if strings.Contains(w.Body.String(), "Other") {
				t.Fatal("快照包含其他用户词条")
			}
			service.engine = engine.Config{}
			unavailable := httptest.NewRecorder()
			retry := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
			retry.Header = req.Header.Clone()
			mux.ServeHTTP(unavailable, retry)
			service.engine = engine.Config{Binary: binary, Resources: "-test.run=^TestDictionaryQueryEngineProcess$"}
			if unavailable.Code != 503 || !strings.Contains(unavailable.Body.String(), "engine_unavailable") {
				t.Fatal(unavailable.Code, unavailable.Body.String())
			}
		})
	}
}
