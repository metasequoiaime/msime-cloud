package account

import (
	"bytes"
	"encoding/json"
	"strconv"
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
	value := raw
	negative := value[0] == '-'
	if negative {
		value = value[1:]
	}
	mantissa := value
	var exponent int64
	if i := bytes.IndexAny(value, "eE"); i >= 0 {
		var err error
		exponent, err = strconv.ParseInt(string(value[i+1:]), 10, 32)
		if err != nil || exponent > maximumPreferenceNumericExponent || exponent < -maximumPreferenceNumericExponent {
			return false
		}
		mantissa = value[:i]
	}
	var fractionDigits int64
	if i := bytes.IndexByte(mantissa, '.'); i >= 0 {
		fractionDigits = int64(len(mantissa) - i - 1)
	}
	if fractionDigits-exponent > maximumPreferenceNumericScale {
		return false
	}
	// JSON 语法已保证最多一个小数点；保留子切片，计数和尾零检查时跳过它。
	digits := bytes.TrimLeft(mantissa, "0.")
	if len(digits) == 0 {
		return true
	}
	if negative {
		return false
	}
	digitCount := len(digits)
	if bytes.IndexByte(digits, '.') >= 0 {
		digitCount--
	}
	integerDigits := int64(digitCount) + exponent - fractionDigits
	if integerDigits < 7 {
		return true
	}
	// 恰有七位整数时，只有 1000000 本身合法；小数尾部也不能有非零数字。
	return integerDigits == 7 && digits[0] == '1' && len(bytes.Trim(digits[1:], "0.")) == 0
}

// validPreferenceString 检查 JSON 字符串、数据库编码限制及解码后的 UTF-8 字节数，不分配解码副本。
func validPreferenceString(raw json.RawMessage, max int) bool {
	raw = bytes.Trim(raw, " \t\r\n")
	if len(raw) < 2 || raw[0] != '"' || raw[len(raw)-1] != '"' || !json.Valid(raw) || !utf8.Valid(raw) {
		return false
	}
	// 从原文长度扣除转义增加的字节，保持原来 len(decoded) 的上限语义。
	length := len(raw) - 2
	for i := 1; i < len(raw)-1; i++ {
		if raw[i] != '\\' {
			continue
		}
		i++
		if raw[i] != 'u' {
			length--
			continue
		}
		code, _ := strconv.ParseUint(string(raw[i+1:i+5]), 16, 16)
		i += 4
		if code == 0 || (code >= 0xdc00 && code <= 0xdfff) {
			return false
		}
		if code < 0xd800 || code > 0xdbff {
			length += utf8.RuneLen(rune(code)) - 6
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
		// 两个六字节转义合成一个四字节 UTF-8 字符。
		length -= 8
	}
	return length <= max
}
