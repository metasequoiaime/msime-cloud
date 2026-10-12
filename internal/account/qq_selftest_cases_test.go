package account

import (
	"context"
	"testing"
)

func TestQQSelfTestCases(t *testing.T) {
	db := testStore(t)
	a := &Service{store: db}
	ctx := context.Background()
	if _, err := db.pool.Exec(ctx, `TRUNCATE qq_selftest_cases`); err != nil {
		t.Fatal(err)
	}
	for _, c := range [][2]string{{"鸿蒙版在哪下载？", "还没上架"}, {"搜狗词库怎么导入？", "转成 txt 再导入"}, {"鸿蒙版在哪下载？", "华为应用市场搜水杉输入法"}} {
		if err := a.AddQQSelfTestCase(ctx, c[0], c[1]); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.pool.Exec(ctx, `INSERT INTO qq_selftest_cases(question,answer,created_at) VALUES('很久以前的问题','答',now()-interval '200 days')`); err != nil {
		t.Fatal(err)
	}
	cases, err := a.QQSelfTestCases(ctx, 10)
	if err != nil || len(cases) != 3 || cases[0].Question != "鸿蒙版在哪下载？" || cases[0].Answer != "华为应用市场搜水杉输入法" || cases[1].Question != "搜狗词库怎么导入？" {
		t.Fatal(cases, err)
	}
	if cases, _ := a.QQSelfTestCases(ctx, 1); len(cases) != 1 {
		t.Fatal(cases)
	}
	db.Prune(ctx)
	if cases, _ := a.QQSelfTestCases(ctx, 10); len(cases) != 2 {
		t.Fatal("prune must delete cases older than 180 days", cases)
	}
}
