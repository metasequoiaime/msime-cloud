package server

import (
	"crypto/md5"
	"encoding/hex"
	"maps"
	"math/rand"
	"sort"
	"strconv"
	"strings"
	"testing"
)

func previousNiuTransAuth(params map[string]string, apiKey string) string {
	keys := make([]string, 0, len(params)+1)
	for key, value := range params {
		if key != "authStr" && key != "apikey" && value != "" {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys)+1)
	parts = append(parts, "apikey="+apiKey)
	for _, key := range keys {
		parts = append(parts, key+"="+params[key])
	}
	sum := md5.Sum([]byte(strings.Join(parts, "&")))
	return hex.EncodeToString(sum[:])
}

func niuTransAuthParams(count int) map[string]string {
	params := map[string]string{"appId": "synthetic-app", "timestamp": "1700000000000"}
	if count > 2 {
		params["from"], params["to"] = "zh", "en"
	}
	for i := 4; i < count; i++ {
		params["option"+strconv.Itoa(i)] = strings.Repeat("synthetic-value", 8)
	}
	return params
}

func TestNiuTransAuthFixedVectors(t *testing.T) {
	for _, tc := range []struct {
		params map[string]string
		apiKey string
		want   string
	}{
		{nil, "", "d08776618db210cca1203f3d733d1d2f"},
		{niuTransAuthParams(2), "synthetic-key", "e15b217adb8fb882ca9104134ad554ac"},
		{niuTransAuthParams(4), "synthetic-key", "050a0b4a8c8264b2e2fe66834d2aad44"},
		{map[string]string{"appId": "synthetic-app", "timestamp": "1700000000000", "authStr": "ignored", "apikey": "ignored", "empty": ""}, "synthetic-key", "e15b217adb8fb882ca9104134ad554ac"},
	} {
		if got := niuTransAuth(tc.params, tc.apiKey); got != tc.want {
			t.Fatalf("固定合成向量签名变化：结果=%s，期望=%s", got, tc.want)
		}
	}
}

func TestNiuTransAuthBufferCompatibility(t *testing.T) {
	rng := rand.New(rand.NewSource(109))
	for count := 0; count < 33; count++ {
		params := map[string]string{"authStr": "ignored", "apikey": "ignored", "empty": ""}
		for i := 0; i < count; i++ {
			key, value := make([]byte, i+1), make([]byte, 3*i+1)
			_, _ = rng.Read(key)
			_, _ = rng.Read(value)
			params[string(key)] = string(value)
		}
		for _, apiKey := range []string{"", "synthetic-key", "合成密钥&=\x00\xff"} {
			before := maps.Clone(params)
			if got, want := niuTransAuth(params, apiKey), previousNiuTransAuth(params, apiKey); got != want {
				t.Fatalf("合成参数签名变化：参数数=%d，结果=%s，期望=%s", count, got, want)
			}
			if !maps.Equal(params, before) {
				t.Fatal("签名计算修改了输入参数")
			}
		}
	}
}

func TestNiuTransAuthBufferAllocationBudget(t *testing.T) {
	for _, count := range []int{2, 4, 9, 32} {
		params := niuTransAuthParams(count)
		var got string
		allocations := testing.AllocsPerRun(50, func() { got = niuTransAuth(params, "synthetic-key") })
		if got != previousNiuTransAuth(params, "synthetic-key") {
			t.Fatal("合成参数签名变化")
		}
		if allocations > 3 {
			t.Fatalf("%d 个签名参数分配 %.0f 次，期望只分配参数索引、签名字节缓冲区和摘要字符串", count, allocations)
		}
	}
}

func BenchmarkNiuTransAuthBuffer(b *testing.B) {
	for _, count := range []int{2, 4, 9, 32} {
		params := niuTransAuthParams(count)
		for _, impl := range []struct {
			name string
			auth func(map[string]string, string) string
		}{{"previous", previousNiuTransAuth}, {"current", niuTransAuth}} {
			b.Run(strconv.Itoa(count)+"/"+impl.name, func(b *testing.B) {
				b.ReportAllocs()
				var got string
				for b.Loop() {
					got = impl.auth(params, "synthetic-key")
				}
				if got != previousNiuTransAuth(params, "synthetic-key") {
					b.Fatal("合成参数签名变化")
				}
			})
		}
	}
}
