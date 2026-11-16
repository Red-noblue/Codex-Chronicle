// 中文说明：验证全会话模式须明确启用，移除固定清单后仍拒绝路径目标和不合法操作组合。
package authorizedlive

import "testing"

func TestExplicitAllConversationsScope(t *testing.T) {
	newID := "11111111-2222-4333-8444-555555555555"
	legacy := Config{}
	general := Config{AllConversations: true}
	if legacy.Allows(newID) || !general.Allows(newID) {
		t.Fatal("全会话开关未隔离旧授权范围")
	}
	for _, id := range []string{"../auth.json", "", "thread-id", "11111111-2222-4333-8444-555555555555/../other"} {
		if general.Allows(id) {
			t.Fatalf("非法 ID 通过：%s", id)
		}
	}
	if legacy.AllowsAction(false, true) || !legacy.AllowsAction(true, false) || !general.AllowsAction(false, true) {
		t.Fatal("永久删除未经明确模式启用")
	}
	if general.AllowsAction(false, false) || general.AllowsAction(true, true) {
		t.Fatal("无动作或冲突动作被接受")
	}
}
