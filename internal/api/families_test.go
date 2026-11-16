// 中文说明：验证父子去重、后代优先、实际线程上限及关系冲突不会绕过执行确认。
package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"codex-chronicle.local/internal/domain"
)

func TestExpandFamilyTargetsUsesActualThreadCount(t *testing.T) {
	cs := []domain.ConversationSummary{{ID: "root", ChildConversationIDs: []string{"a", "b"}}, {ID: "a", ChildConversationIDs: []string{"c"}}, {ID: "b"}, {ID: "c"}, {ID: "fork"}}
	ids, err := expandFamilyTargets(cs, []string{"root", "a"})
	if err != nil || !reflect.DeepEqual(ids, []string{"c", "a", "b", "root"}) {
		t.Fatalf("%v %v", ids, err)
	}
	cs[3].FamilyConflict = true
	if _, err := expandFamilyTargets(cs, []string{"root"}); err == nil {
		t.Fatal("conflict allowed")
	}
	cs = []domain.ConversationSummary{{ID: "root"}}
	for i := 0; i < 100; i++ {
		id := fmt.Sprint(i)
		cs[0].ChildConversationIDs = append(cs[0].ChildConversationIDs, id)
		cs = append(cs, domain.ConversationSummary{ID: id})
	}
	if _, err := expandFamilyTargets(cs, []string{"root"}); err == nil {
		t.Fatal("actual count exceeded 100")
	}
}

func TestSingleFamilyPreviewConfirmsAllDescendants(t *testing.T) {
	server, ids := executionFixture(t, 12)
	for i := 1; i < len(ids); i++ {
		raw := fmt.Sprintf("execution-%02d", i)
		data := fmt.Sprintf("{\"type\":\"session_meta\",\"payload\":{\"id\":%q,\"session_id\":\"execution-00\",\"parent_thread_id\":\"execution-00\",\"thread_source\":\"subagent\"}}\n", raw)
		if err := os.WriteFile(filepath.Join(server.root, raw+".jsonl"), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	handler := server.Handler()
	res := executionJSON(t, handler, "/api/v1/operations/preview", map[string]any{"conversation_id": ids[0], "permanent_delete": true})
	if res.Code != http.StatusOK {
		t.Fatalf("preview: %d %s", res.Code, res.Body.String())
	}
	var preview struct {
		PreviewID string               `json:"preview_id"`
		Items     []batchOperationItem `json:"items"`
		Plan      struct {
			ConversationID string `json:"conversation_id"`
		} `json:"plan"`
	}
	if err := json.Unmarshal(res.Body.Bytes(), &preview); err != nil {
		t.Fatal(err)
	}
	if len(preview.Items) != 12 || preview.Plan.ConversationID != ids[0] {
		t.Fatalf("single preview hid descendants: %s", res.Body.String())
	}
	for _, item := range preview.Items {
		if item.AutomaticallyIncluded != (item.ConversationID != ids[0]) {
			t.Fatal("explicit selection confused with automatic inclusion")
		}
	}
	res = executionJSON(t, handler, "/api/v1/operations/confirmations", map[string]string{"preview_id": preview.PreviewID})
	var confirmation executionConfirmation
	if res.Code != http.StatusOK || json.Unmarshal(res.Body.Bytes(), &confirmation) != nil || confirmation.Total != 12 || confirmation.AdditionalCode == "" {
		t.Fatalf("actual thread count not confirmed: %d %s", res.Code, res.Body.String())
	}
}
