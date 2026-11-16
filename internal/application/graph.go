// 中文说明：从只读 inventory 组装 canonical conversation asset graph；关系证据不足时显式标记为 partial，不授权写入。
package application

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"codex-chronicle.local/internal/compat"
	"codex-chronicle.local/internal/domain"
)

const (
	graphComplete = "complete"
	graphPartial  = "partial"
	graphUnknown  = "unknown"
)

// BuildConversationAssetGraph 为指定逻辑对话构建 canonical 资产图；调用方可以先用无图扫描避免为其他对话构建派生图。
func BuildConversationAssetGraph(root string, files []domain.FileInventory, conversation domain.ConversationSummary) domain.ConversationAssetGraph {
	graph := domain.ConversationAssetGraph{
		Version: 1, ConversationID: conversation.ID, Completeness: graphComplete,
		Nodes: make([]domain.AssetGraphNode, 0), Edges: make([]domain.AssetGraphEdge, 0), Diagnostics: make([]domain.Diagnostic, 0),
	}
	nodes := make(map[string]domain.AssetGraphNode)
	edges := make(map[string]domain.AssetGraphEdge)
	fileByPath := make(map[string]domain.FileInventory, len(files))
	for _, file := range files {
		fileByPath[file.RelativePath] = file
	}

	addNode := func(node domain.AssetGraphNode) string {
		if node.ID == "" {
			node.ID = graphNodeID(node.Kind, node.RelativePath+"\x00"+node.ThreadID+"\x00"+node.Table+"\x00"+node.Column+"\x00"+node.RowKey+"\x00"+node.Label)
		}
		if existing, ok := nodes[node.ID]; ok {
			if existing.Status == domain.StatusUnknown && node.Status != domain.StatusUnknown {
				nodes[node.ID] = node
			}
			return node.ID
		}
		nodes[node.ID] = node
		return node.ID
	}
	addEdge := func(from, to, relation, evidence, confidence string) {
		if from == "" || to == "" || from == to {
			return
		}
		edge := domain.AssetGraphEdge{From: from, To: to, Relation: relation, Evidence: evidence, Confidence: confidence}
		encoded, _ := json.Marshal(edge)
		edges[hex.EncodeToString(hashBytes(encoded))] = edge
	}

	conversationNode := addNode(domain.AssetGraphNode{Kind: domain.GraphNodeConversation, Label: conversation.Title, Status: conversation.Status, Source: "conversation projection"})
	threadNodes := make(map[string]string)
	addThread := func(threadID, source string, status domain.AssetStatus) string {
		threadID = strings.TrimSpace(threadID)
		if threadID == "" {
			return ""
		}
		if nodeID, ok := threadNodes[threadID]; ok {
			return nodeID
		}
		nodeID := addNode(domain.AssetGraphNode{Kind: domain.GraphNodeThread, ThreadID: threadID, Label: threadID, Status: status, Source: source})
		threadNodes[threadID] = nodeID
		return nodeID
	}
	addFile := func(path string, relation, evidence, confidence string) string {
		file, ok := fileByPath[path]
		if !ok {
			graph.Completeness = graphPartial
			graph.Diagnostics = appendGraphDiagnostic(graph.Diagnostics, domain.Diagnostic{Code: "GRAPH_ASSET_MISSING", Severity: domain.SeverityBlock, Message: "对话关系引用的物理资产未出现在本次扫描中", Evidence: path, ReadOnly: true})
			return ""
		}
		kind := domain.GraphNodeFile
		if file.Kind == domain.AssetKindJSONL {
			kind = domain.GraphNodeRollout
		}
		nodeID := addNode(domain.AssetGraphNode{Kind: kind, Label: path, RelativePath: path, AssetKind: file.Kind, Status: file.Status, SizeBytes: file.SizeBytes, ModTime: file.ModTime, Source: "inventory"})
		addEdge(conversationNode, nodeID, relation, evidence, confidence)
		return nodeID
	}

	for _, path := range conversation.RolloutPaths {
		fileNode := addFile(path, "contains", "rollout path and conversation ID", "high")
		if file, ok := fileByPath[path]; ok && file.Rollout != nil {
			threadNode := addThread(firstNonEmpty(file.Rollout.ThreadID, file.Rollout.SourceID), "rollout session metadata", file.Status)
			addEdge(conversationNode, threadNode, "contains", "rollout thread_id", "high")
			addEdge(threadNode, fileNode, "has_rollout", "rollout path", "high")
			if parent := addThread(file.Rollout.ParentThreadID, "rollout spawn metadata", file.Status); parent != "" {
				addEdge(threadNode, parent, "spawned_from", "rollout parent_thread_id", "high")
			}
		}
	}

	for _, path := range conversation.RelatedAssetPaths {
		addFile(path, "contains", "SQLite/index relationship projection", "high")
	}

	for _, file := range files {
		// 旧备份的控制记录只属于快照，不是当前会话的可操作关系。
		if isBackupRolloutPath(file.RelativePath) && (file.SQLite != nil || file.SessionIndex != nil) {
			continue
		}
		if file.SQLite != nil {
			for _, thread := range file.SQLite.Threads {
				if compat.StableConversationID(thread.ID) != conversation.ID {
					continue
				}
				fileNode := addFile(file.RelativePath, "contains", "SQLite threads.id", "high")
				threadNode := addThread(thread.ID, "SQLite threads row", file.Status)
				addEdge(conversationNode, threadNode, "contains", "SQLite threads.id", "high")
				addEdge(threadNode, fileNode, "stored_in", "SQLite table threads", "high")
				if parent := addThread(thread.ParentThreadID, "SQLite threads.source", file.Status); parent != "" {
					addEdge(threadNode, parent, "spawned_from", "SQLite threads.source", "high")
				}
				if rolloutPath, safe := normalizeRolloutPath(root, thread.RolloutPath); safe && rolloutPath != "" {
					rolloutNode := addFile(rolloutPath, "references", "SQLite threads.rollout_path", "high")
					addEdge(threadNode, rolloutNode, "has_rollout", "SQLite threads.rollout_path", "high")
				}
			}
			for _, relationship := range file.SQLite.Relationships {
				if compat.StableConversationID(relationship.ThreadID) != conversation.ID && compat.StableConversationID(relationship.RelatedThreadID) != conversation.ID {
					continue
				}
				fileNode := addFile(file.RelativePath, "contains", fmt.Sprintf("SQLite %s.%s", relationship.Table, relationship.Column), "high")
				relationKey := relationship.Table + "\x00" + relationship.Column + "\x00" + relationship.ThreadID
				relationNode := addNode(domain.AssetGraphNode{Kind: domain.GraphNodeSQLiteRelation, Label: relationship.Table + "." + relationship.Column, Table: relationship.Table, Column: relationship.Column, ThreadID: relationship.ThreadID, Status: file.Status, Source: "SQLite relationship projection"})
				addEdge(fileNode, relationNode, "contains", "SQLite relation column", "high")
				addEdge(conversationNode, relationNode, "references", relationKey, "high")
				threadNode := addThread(relationship.ThreadID, "SQLite relationship", file.Status)
				addEdge(threadNode, relationNode, relationship.Relation, "SQLite relation thread_id", "high")
				relatedIDs := relationship.RelatedThreadIDs
				if len(relatedIDs) == 0 {
					relatedIDs = []string{relationship.RelatedThreadID}
				}
				for _, id := range relatedIDs {
					if related := addThread(id, "SQLite relationship", file.Status); related != "" {
						addEdge(threadNode, related, relationship.Relation, "SQLite parent/child columns", "high")
					}
				}
				if !relationship.RowsComplete {
					graph.Completeness = graphPartial
					graph.Diagnostics = appendGraphDiagnostic(graph.Diagnostics, domain.Diagnostic{Code: "GRAPH_SQLITE_ROWS_INCOMPLETE", Severity: domain.SeverityWarning, Message: "SQLite 关系行无法完整取得稳定主键，写操作必须降级为只读", Evidence: relationKey, ReadOnly: true})
				}
				for _, row := range relationship.RowKeys {
					rowNode := addNode(domain.AssetGraphNode{Kind: domain.GraphNodeSQLiteRow, Label: relationship.Table + "." + row.Key, Table: relationship.Table, Column: relationship.Column, ThreadID: relationship.ThreadID, RowKey: row.Key, RowKeyKind: row.KeyKind, Status: file.Status, Source: "SQLite row key"})
					addEdge(relationNode, rowNode, "contains", "SQLite primary key/rowid", "high")
				}
			}
		}
		if file.SessionIndex != nil {
			for _, entry := range file.SessionIndex.Entries {
				if compat.StableConversationID(entry.ID) != conversation.ID {
					continue
				}
				fileNode := addFile(file.RelativePath, "indexed_by", "session index ID", "high")
				threadNode := addThread(entry.ID, "session index entry", file.Status)
				rowNode := addNode(domain.AssetGraphNode{Kind: domain.GraphNodeIndexEntry, Label: file.RelativePath + ":" + fmt.Sprint(entry.Line), RelativePath: file.RelativePath, Line: entry.Line, ThreadID: entry.ID, Status: file.Status, Source: "session index line"})
				addEdge(fileNode, rowNode, "contains", "session index line", "high")
				addEdge(rowNode, threadNode, "indexes", "session index ID", "high")
			}
		}
	}

	graph.Nodes = make([]domain.AssetGraphNode, 0, len(nodes))
	for _, node := range nodes {
		graph.Nodes = append(graph.Nodes, node)
	}
	sort.Slice(graph.Nodes, func(i, j int) bool { return graph.Nodes[i].ID < graph.Nodes[j].ID })
	graph.Edges = make([]domain.AssetGraphEdge, 0, len(edges))
	for _, edge := range edges {
		graph.Edges = append(graph.Edges, edge)
	}
	sort.Slice(graph.Edges, func(i, j int) bool {
		if graph.Edges[i].From == graph.Edges[j].From {
			if graph.Edges[i].To == graph.Edges[j].To {
				return graph.Edges[i].Relation < graph.Edges[j].Relation
			}
			return graph.Edges[i].To < graph.Edges[j].To
		}
		return graph.Edges[i].From < graph.Edges[j].From
	})
	if conversation.Status == domain.StatusUnknown || conversation.Status == domain.StatusConflict || conversation.Status == domain.StatusUnsupported {
		graph.Completeness = graphUnknown
	}
	canonical := struct {
		Version        int
		ConversationID string
		Completeness   string
		Nodes          []domain.AssetGraphNode
		Edges          []domain.AssetGraphEdge
	}{graph.Version, graph.ConversationID, graph.Completeness, graph.Nodes, graph.Edges}
	encoded, _ := json.Marshal(canonical)
	graph.Hash = hex.EncodeToString(hashBytes(encoded))
	return graph
}

func buildConversationAssetGraph(root string, files []domain.FileInventory, conversation domain.ConversationSummary) domain.ConversationAssetGraph {
	return BuildConversationAssetGraph(root, files, conversation)
}

func appendGraphDiagnostic(items []domain.Diagnostic, item domain.Diagnostic) []domain.Diagnostic {
	if len(items) >= 32 {
		return items
	}
	return append(items, item)
}

func graphNodeID(kind domain.GraphNodeKind, key string) string {
	digest := hashBytes([]byte(string(kind) + "\x00" + key))
	return "node-" + hex.EncodeToString(digest[:16])
}

func hashBytes(value []byte) []byte {
	digest := sha256.Sum256(value)
	return digest[:]
}
