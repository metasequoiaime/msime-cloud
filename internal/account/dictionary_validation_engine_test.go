package account

import (
	"encoding/json"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/metasequoiaime/MSIME-Backend/internal/engine"
)

const dictionaryValidationProcess = "MSIME_TEST_DICTIONARY_VALIDATION_PROCESS"
const dictionaryValidationCapture = "MSIME_TEST_DICTIONARY_VALIDATION_CAPTURE"
const dictionaryValidationMode = "MSIME_TEST_DICTIONARY_VALIDATION_MODE"

// 直接启动当前测试程序作为协议子进程，避免依赖平台 shell 或额外安装的解释器。
func dictionaryValidationEngine(t *testing.T, mode string) (engine.Config, string) {
	t.Helper()
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	capture := filepath.Join(t.TempDir(), "requests.jsonl")
	t.Setenv(dictionaryValidationProcess, "1")
	t.Setenv(dictionaryValidationCapture, capture)
	t.Setenv(dictionaryValidationMode, mode)
	return engine.Config{Binary: binary, Resources: "-test.run=^TestDictionaryValidationEngineProcess$"}, capture
}

func TestDictionaryValidationEngineProcess(t *testing.T) {
	if os.Getenv(dictionaryValidationProcess) != "1" {
		return
	}
	data, err := io.ReadAll(os.Stdin)
	if err != nil {
		os.Exit(2)
	}
	var request struct {
		Operation string                       `json:"operation"`
		Entries   []map[string]json.RawMessage `json:"entries"`
	}
	var envelope map[string]json.RawMessage
	if json.Unmarshal(data, &request) != nil || json.Unmarshal(data, &envelope) != nil || len(envelope) != 2 || request.Operation != "validate_dictionary_batch" || len(request.Entries) == 0 || len(request.Entries) > 50 {
		os.Exit(2)
	}
	response := struct {
		Entries []DictionaryEntry `json:"entries"`
	}{Entries: make([]DictionaryEntry, len(request.Entries))}
	for i, fields := range request.Entries {
		var entry struct {
			Kind   string `json:"kind"`
			Code   string `json:"code"`
			Text   string `json:"text"`
			Weight int64  `json:"weight"`
		}
		raw, _ := json.Marshal(fields)
		if len(fields) != 4 || json.Unmarshal(raw, &entry) != nil || fields["kind"] == nil || fields["code"] == nil || fields["text"] == nil || fields["weight"] == nil {
			os.Exit(2)
		}
		response.Entries[i] = DictionaryEntry{Kind: entry.Kind, Code: strings.ToLower(entry.Code), Word: strings.TrimSpace(entry.Text), Weight: entry.Weight}
	}
	capture := os.Getenv(dictionaryValidationCapture)
	existing, _ := os.Stat(capture)
	file, err := os.OpenFile(capture, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		os.Exit(2)
	}
	if _, err = file.Write(append(data, '\n')); err != nil {
		os.Exit(2)
	}
	if file.Close() != nil {
		os.Exit(2)
	}
	switch os.Getenv(dictionaryValidationMode) {
	case "invalid":
		io.WriteString(os.Stdout, `{"error":"invalid_dictionary_entry"}`)
	case "unavailable":
		io.WriteString(os.Stdout, `{"error":"resources_unavailable"}`)
	case "failure":
		io.WriteString(os.Stdout, `{"error":"unknown"}`)
	case "malformed":
		io.WriteString(os.Stdout, `[`)
	case "bad-entries":
		io.WriteString(os.Stdout, `{"entries":{}}`)
	case "wrong-count":
		response.Entries = response.Entries[:len(response.Entries)-1]
		json.NewEncoder(os.Stdout).Encode(response)
	case "fail-after-first":
		if existing != nil && existing.Size() > 0 {
			io.WriteString(os.Stdout, `{"error":"invalid_dictionary_entry"}`)
		} else {
			json.NewEncoder(os.Stdout).Encode(response)
		}
	default:
		json.NewEncoder(os.Stdout).Encode(response)
	}
	os.Exit(0)
}

func TestDictionaryValidationBatchWireCompatibility(t *testing.T) {
	config, capture := dictionaryValidationEngine(t, "")
	service := &Service{engine: config}
	entries := dictionaryValidationEntries(101, false)
	inserted := true
	for i := range entries {
		entries[i].Kind = "wubi"
		entries[i].ID = "不应转发的元数据"
		entries[i].UserInserted = &inserted
		entries[i].Code = strings.ToUpper(entries[i].Code)
		entries[i].Word = " " + entries[i].Word + "😀<&> "
	}
	entries[0].Code = ""
	entries[100].Weight = math.MaxInt64
	got, err := service.validateDictionary(t.Context(), "pinyin", entries)
	if err != nil || len(got) != len(entries) {
		t.Fatalf("结果=%v/%v", got, err)
	}
	for i, e := range got {
		if e.Kind != "pinyin" || e.Code != strings.ToLower(entries[i].Code) || e.Word != strings.TrimSpace(entries[i].Word) || e.Weight != entries[i].Weight || e.ID != "" || e.UserInserted != nil {
			t.Fatalf("第 %d 条未采用引擎规范化结果：%+v", i, e)
		}
	}
	data, err := os.ReadFile(capture)
	if err != nil {
		t.Fatal(err)
	}
	var sizes []int
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var request struct {
			Entries []json.RawMessage `json:"entries"`
		}
		if json.Unmarshal([]byte(line), &request) != nil {
			t.Fatal("捕获的批次 JSON 无效")
		}
		sizes = append(sizes, len(request.Entries))
	}
	if !slices.Equal(sizes, []int{50, 50, 1}) {
		t.Fatalf("请求分批=%v", sizes)
	}
}

func TestDictionaryValidationBatchEngineErrors(t *testing.T) {
	for mode, want := range map[string]error{
		"invalid":          engine.ErrInvalid,
		"unavailable":      engine.ErrUnavailable,
		"failure":          engine.ErrFailure,
		"malformed":        engine.ErrFailure,
		"bad-entries":      engine.ErrFailure,
		"wrong-count":      engine.ErrFailure,
		"fail-after-first": engine.ErrInvalid,
	} {
		t.Run(mode, func(t *testing.T) {
			config, _ := dictionaryValidationEngine(t, mode)
			service := &Service{engine: config}
			count := 1
			if mode == "fail-after-first" {
				count = 51
			}
			got, err := service.validateDictionary(t.Context(), "english", dictionaryValidationEntries(count, false))
			if got != nil || !errors.Is(err, want) {
				t.Fatalf("结果=%v，错误=%v，期望=%v", got, err, want)
			}
		})
	}
}
