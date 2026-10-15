package account

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"
	"unicode/utf8"
)

const (
	// PostgreSQL 的 `numeric` 存储小数位和输入指数上限；`jsonb` 沿用这两个限制。
	maximumPreferenceNumericScale    = 16383
	maximumPreferenceNumericExponent = 1<<30 - 1
)

// validPreferenceNumber 按十进制原文检查 0..1000000 和数据库存储限制，不经过浮点舍入或展开巨大指数。
func validPreferenceNumber(raw json.RawMessage) bool {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || (raw[0] != '-' && (raw[0] < '0' || raw[0] > '9')) || !json.Valid(raw) {
		return false
	}
	value := string(raw)
	negative := value[0] == '-'
	if negative {
		value = value[1:]
	}
	mantissa := value
	var exponent int64
	if i := strings.IndexAny(value, "eE"); i >= 0 {
		var err error
		exponent, err = strconv.ParseInt(value[i+1:], 10, 32)
		if err != nil || exponent > maximumPreferenceNumericExponent || exponent < -maximumPreferenceNumericExponent {
			return false
		}
		mantissa = value[:i]
	}
	var fractionDigits int64
	if i := strings.IndexByte(mantissa, '.'); i >= 0 {
		fractionDigits = int64(len(mantissa) - i - 1)
	}
	if fractionDigits-exponent > maximumPreferenceNumericScale {
		return false
	}
	digits := strings.TrimLeft(strings.ReplaceAll(mantissa, ".", ""), "0")
	if digits == "" {
		return true
	}
	if negative {
		return false
	}
	integerDigits := int64(len(digits)) + exponent - fractionDigits
	if integerDigits < 7 {
		return true
	}
	// 恰有七位整数时，只有 1000000 本身合法；小数尾部也不能有非零数字。
	return integerDigits == 7 && digits[0] == '1' && strings.Trim(digits[1:], "0") == ""
}

// validPreferenceUnicode 检查已通过 JSON 语法校验的字符串原文，拒绝 Go 解码时会替换、但 `jsonb` 无法保存的编码。
func validPreferenceUnicode(raw json.RawMessage) bool {
	if !utf8.Valid(raw) {
		return false
	}
	for i := 0; i < len(raw); i++ {
		if raw[i] != '\\' {
			continue
		}
		i++
		if raw[i] != 'u' {
			continue
		}
		code, _ := strconv.ParseUint(string(raw[i+1:i+5]), 16, 16)
		i += 4
		if code >= 0xdc00 && code <= 0xdfff {
			return false
		}
		if code < 0xd800 || code > 0xdbff {
			continue
		}
		if len(raw)-i-1 < 6 || raw[i+1] != '\\' || raw[i+2] != 'u' {
			return false
		}
		low, _ := strconv.ParseUint(string(raw[i+3:i+7]), 16, 16)
		if low < 0xdc00 || low > 0xdfff {
			return false
		}
		i += 6
	}
	return true
}
