package account

import (
	"encoding/json"
	"testing"
)

func TestStrictAdminValue(t *testing.T) {
	decoders := map[string]func(json.RawMessage) error{
		"敏感词": func(raw json.RawMessage) error {
			var value sensitiveWordInput
			return decodeSensitiveValue(raw, &value)
		},
		"故障": func(raw json.RawMessage) error {
			_, err := decodeIncidentValue(raw)
			return err
		},
	}
	for name, decode := range decoders {
		t.Run(name, func(t *testing.T) {
			for _, raw := range []string{`{}]`, `{}}`, `{} null`, `{} synthetic`, `{"unknown":true}`, ``, `{`} {
				t.Run(raw, func(t *testing.T) {
					if err := decode(json.RawMessage(raw)); err == nil || err.Error() != "invalid_value" {
						t.Fatalf("应拒绝非法值并返回 invalid_value，实际 %v", err)
					}
				})
			}
			for _, raw := range []string{`{}`, "{} \r\n\t", `null`} {
				if err := decode(json.RawMessage(raw)); err != nil {
					t.Fatalf("合法值 %q 应保留原有语义：%v", raw, err)
				}
			}
		})
	}
}
