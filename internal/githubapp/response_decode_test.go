package githubapp

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestResponseDecodeErrorsDoNotExposeBody(t *testing.T) {
	for _, failure := range []string{"time", "number", "syntax"} {
		t.Run(failure, func(t *testing.T) {
			var out struct {
				CreatedAt time.Time `json:"created_at"`
				Number    int64     `json:"number"`
			}
			body := `{"created_at":"synthetic-private-input"}`
			if failure == "number" {
				body = `{"number":987654321098765432109876543210}`
			} else if failure == "syntax" {
				body = `{"created_at":"synthetic-private-input",`
			}
			err := (Response{Body: []byte(body)}).Decode(&out)
			if err == nil {
				t.Fatal("无效正文没有返回解码错误")
			}
			switch failure {
			case "time":
				var original *time.ParseError
				if !errors.As(err, &original) || original.Value != "synthetic-private-input" {
					t.Fatal("丢失了原始时间解析错误", err)
				}
			case "number":
				var original *json.UnmarshalTypeError
				if !errors.As(err, &original) || !strings.Contains(original.Value, "987654321098765432109876543210") {
					t.Fatal("丢失了原始数字解码错误", err)
				}
			case "syntax":
				var original *json.SyntaxError
				if !errors.As(err, &original) {
					t.Fatal("丢失了原始 JSON 语法错误", err)
				}
			}
			if err.Error() != "invalid GitHub response" {
				t.Errorf("GitHub 解码错误文本没有脱敏：%s", err)
			}
		})
	}
}
