package server

import "testing"

func TestQQFeedbackTextPreservesPlainAngleBrackets(t *testing.T) {
	for _, input := range []string{"候选 1 < 2 > 0", "代码使用 <T> 类型参数"} {
		if got := qqFeedbackText(input); got != input {
			t.Errorf("qqFeedbackText(%q) = %q，普通尖括号文本被误删", input, got)
		}
	}
}

func TestQQFeedbackTextRemovesKnownQQMarkup(t *testing.T) {
	const input = `<@!123> 问题 <faceType=1,faceId="178"> <qqbot-at-user id="456" />`
	const want = "问题"
	if got := qqFeedbackText(input); got != want {
		t.Fatalf("qqFeedbackText(%q) = %q，预期 %q", input, got, want)
	}
}
