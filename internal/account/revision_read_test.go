package account

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestReadRevisionJSON(t *testing.T) {
	db := testStore(t)
	for _, tc := range []struct {
		name, query string
		revision    int64
		want        map[string]bool
		failure     string
	}{
		{"document", `SELECT 7::bigint, '{"ready":true}'::jsonb`, 7, map[string]bool{"ready": true}, ""},
		{"empty", `SELECT 7::bigint, '{}'::jsonb`, 7, map[string]bool{}, ""},
		{"missing", `SELECT 7::bigint, '{}'::jsonb WHERE false`, 0, map[string]bool{}, ""},
		{"null", `SELECT 7::bigint, 'null'::jsonb`, 7, nil, ""},
		{"wrong_shape", `SELECT 7::bigint, '[]'::jsonb`, 7, map[string]bool{}, "type"},
		{"partial", `SELECT 7::bigint, '{"ready":true,"bad":"wrong"}'::jsonb`, 7, map[string]bool{"ready": true, "bad": false}, "type"},
		{"syntax", `SELECT 7::bigint, '['::text`, 7, map[string]bool{}, "syntax"},
		{"sql_null", `SELECT 7::bigint, NULL::jsonb`, 7, map[string]bool{}, "syntax"},
		{"database", `SELECT 1/0, '{}'::jsonb`, 0, map[string]bool{}, "database"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var revision int64
			value := map[string]bool{}
			err := readRevisionJSON(db.pool.QueryRow(t.Context(), tc.query), &revision, &value)
			if revision != tc.revision || !reflect.DeepEqual(value, tc.want) {
				t.Fatalf("结果 = %d, %#v，期望 %d, %#v，错误 %v", revision, value, tc.revision, tc.want, err)
			}
			var typeError *json.UnmarshalTypeError
			var syntaxError *json.SyntaxError
			var databaseError *pgconn.PgError
			valid := false
			switch tc.failure {
			case "":
				valid = err == nil
			case "type":
				valid = errors.As(err, &typeError)
			case "syntax":
				valid = errors.As(err, &syntaxError)
			case "database":
				valid = errors.As(err, &databaseError) && databaseError.Code == "22012"
			}
			if !valid {
				t.Fatalf("错误 = %v，期望类别 %q", err, tc.failure)
			}
		})
	}
}

func TestReadRevisionJSONPhrases(t *testing.T) {
	db := testStore(t)
	for _, tc := range []struct {
		name, payload string
		want          []Phrase
		invalid       bool
	}{
		{"empty", `[]`, []Phrase{}, false},
		{"null", `null`, nil, false},
		{"document", `[{"id":"first","text":"收到","group":"工作","position":3}]`, []Phrase{{ID: "first", Text: "收到", Group: "工作", Position: 3}}, false},
		{"partial", `[{"id":"first","text":"收到"},{"id":13,"text":"稍后"}]`, []Phrase{{ID: "first", Text: "收到"}, {Text: "稍后"}}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var revision int64
			phrases := []Phrase{}
			err := readRevisionJSON(db.pool.QueryRow(t.Context(), `SELECT 9::bigint,$1::jsonb`, tc.payload), &revision, &phrases)
			if revision != 9 || !reflect.DeepEqual(phrases, tc.want) {
				t.Fatalf("结果 = %d, %#v，期望 9, %#v，错误 %v", revision, phrases, tc.want, err)
			}
			var typeError *json.UnmarshalTypeError
			if tc.invalid {
				if !errors.As(err, &typeError) {
					t.Fatalf("期望解码类型错误，得到 %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
		})
	}
}
