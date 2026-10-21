package server

import (
	"crypto/hmac"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/metasequoiaime/MSIME-Backend/internal/digest"
	segmentjson "github.com/segmentio/encoding/json"
)

const niuTransTaskPrefix = "nt1."

// niuTransTaskMAC 用独立协议标识绑定上游应用、媒体类型、身份与原始编号；JSON 数组避免字段拼接歧义。
func niuTransTaskMAC(e NiuTransEndpoint, service, owner, fileNo string) []byte {
	body, _ := json.Marshal([]string{"msime-niutrans-task-v1", e.appID, service, owner, fileNo})
	return digest.HMACSHA256([]byte(e.apiKey), body)
}

func validNiuTransFileNo(fileNo string) bool {
	return fileNo != "" && len(fileNo) <= 256 && utf8.ValidString(fileNo) && !strings.ContainsAny(fileNo, "/\\\x00\r\n")
}

// niuTransTaskID 不保存进程内状态，同样凭据的其他副本可以验证；编号仅能由原身份用于同类媒体。
func niuTransTaskID(e NiuTransEndpoint, service, owner, fileNo string) string {
	return niuTransTaskPrefix + base64.RawURLEncoding.EncodeToString([]byte(fileNo)) + "." + base64.RawURLEncoding.EncodeToString(niuTransTaskMAC(e, service, owner, fileNo))
}

func niuTransTaskFileNo(e NiuTransEndpoint, service, owner, task string) (string, bool) {
	if owner == "" || !strings.HasPrefix(task, niuTransTaskPrefix) || len(task) > 512 {
		return "", false
	}
	encoded, signature, ok := strings.Cut(strings.TrimPrefix(task, niuTransTaskPrefix), ".")
	if !ok {
		return "", false
	}
	fileNo, err := base64.RawURLEncoding.Strict().DecodeString(encoded)
	if err != nil || !validNiuTransFileNo(string(fileNo)) {
		return "", false
	}
	mac, err := base64.RawURLEncoding.Strict().DecodeString(signature)
	if err != nil || !hmac.Equal(mac, niuTransTaskMAC(e, service, owner, string(fileNo))) {
		return "", false
	}
	return string(fileNo), true
}

// niuTransUploadResult 只替换成功上传的 fileNo，其他业务字段保持原样；业务错误由调用方统一转换，不透传上游详情。
func niuTransUploadResult(r *http.Request, e NiuTransEndpoint, service string, body []byte) ([]byte, error) {
	if !json.Valid(body) {
		return nil, errors.New("invalid upstream upload response")
	}
	reply, ok := niuTransResponseFields(body)
	if !ok || reply == nil {
		return nil, errors.New("invalid upstream upload response")
	}
	var code *int
	if json.Unmarshal(reply["code"], &code) != nil || code == nil {
		return nil, errors.New("invalid upstream upload response")
	}
	if *code != http.StatusOK {
		return body, nil
	}
	data, ok := niuTransResponseFields(reply["data"])
	var fileNo string
	owner, _ := r.Context().Value(skinJobOwnerKey{}).(string)
	if !ok || json.Unmarshal(data["fileNo"], &fileNo) != nil || !validNiuTransFileNo(fileNo) || owner == "" {
		return nil, errors.New("invalid upstream upload response")
	}
	data["fileNo"], _ = json.Marshal(niuTransTaskID(e, service, owner, fileNo))
	reply["data"], _ = json.Marshal(data)
	return json.Marshal(reply)
}

// niuTransFileResult 保留操作响应中的本服务编号，防止客户端用状态响应覆盖已加签的编号。
func niuTransFileResult(body []byte, task string) ([]byte, error) {
	if !json.Valid(body) {
		return nil, errors.New("invalid upstream JSON")
	}
	reply, ok := niuTransResponseFields(body)
	if !ok {
		return body, nil
	}
	data, ok := niuTransResponseFields(reply["data"])
	if !ok {
		return body, nil
	}
	if _, ok := data["fileNo"]; !ok {
		return body, nil
	}
	data["fileNo"], _ = json.Marshal(task)
	reply["data"], _ = json.Marshal(data)
	return json.Marshal(reply)
}

// niuTransResponseFields 解析已通过标准库语法校验的对象或其原始子值，字段值只在响应改写期间借用正文。
func niuTransResponseFields(body []byte) (map[string]json.RawMessage, bool) {
	// 预先创建小映射，避免解码器按大型对象预分配容量。
	fields := make(map[string]json.RawMessage)
	rest, err := segmentjson.Parse(body, &fields, segmentjson.DontCopyRawMessage)
	return fields, err == nil && len(rest) == 0
}
