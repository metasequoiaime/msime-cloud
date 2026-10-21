package server

import (
	"encoding/hex"
	"testing"
)

func TestNiuTransTaskMACCompatibility(t *testing.T) {
	// 固定答案由 Python hashlib/hmac 独立计算，锁定协议标识、字段顺序和 JSON 编码。
	e := NiuTransEndpoint{appID: "synthetic-app", apiKey: "synthetic-key"}
	want := "1ee84705b8b6978308ac4c6e051de6633a2f67d85c341294fae3a64badba0f89"
	if got := hex.EncodeToString(niuTransTaskMAC(e, "niutrans_document", "synthetic-owner", "synthetic-task")); got != want {
		t.Fatalf("小牛任务签名不兼容：得到 %q，期望 %q", got, want)
	}
}
