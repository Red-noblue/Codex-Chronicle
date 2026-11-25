// 中文说明：定义索引内容树，并按所选类别提取 rollout 中的可读文字。
package searchtext

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"codex-chronicle.local/internal/catalog"
	"codex-chronicle.local/internal/compat"
	"codex-chronicle.local/internal/domain"
)

type ContentNode struct {
	ID       string        `json:"id"`
	Label    string        `json:"label"`
	Hint     string        `json:"hint,omitempty"`
	Children []ContentNode `json:"children,omitempty"`
}

func leaf(id, label, hint string) ContentNode {
	return ContentNode{ID: id, Label: label, Hint: hint}
}

func branch(id, label string, children ...ContentNode) ContentNode {
	return ContentNode{ID: id, Label: label, Children: children}
}

func toolBranch(id, label, inputLabel, outputLabel string) ContentNode {
	return branch(id, label,
		leaf(id+".input", inputLabel, "工具调用中可读的目标与参数"),
		leaf(id+".output", outputLabel, "与调用关联的返回内容"))
}

var ContentTree = []ContentNode{
	branch("dialogue", "对话正文",
		leaf("user", "用户提问", "用户输入和表单答复"),
		branch("assistant", "助手消息",
			leaf("final", "每轮最终回复", "日志中明确标记为最终回复的助手正文"),
			leaf("progress", "中间进展", "执行过程中发出的说明与进度"),
			leaf("unmarked", "未标记阶段的回复", "部分旧会话没有区分最终回复和进展")),
		leaf("summary", "压缩摘要", "每轮压缩留下的可读摘要，可单独索引，不受最近窗口限制")),
	branch("tools", "执行记录",
		branch("commands", "命令与进程",
			toolBranch("command", "命令执行", "命令和参数", "执行输出与报错"),
			toolBranch("process", "进程交互", "输入与等待参数", "后续输出与报错")),
		branch("files", "本地文件",
			toolBranch("read", "读取文件", "读取目标", "读取结果"),
			toolBranch("list", "文件与目录查找", "查找目标", "匹配的文件和目录"),
			toolBranch("text", "文本查找", "搜索词与范围", "命中的文本"),
			branch("edit", "文件改动",
				leaf("edit.paths", "改动路径", "包括原路径和移动后的路径"),
				leaf("edit.create", "新增内容", "从结构化文件改动提取"),
				leaf("edit.modify", "编辑差异", "从结构化文件改动提取"),
				leaf("edit.delete", "删除内容", "日志可见的移除内容"),
				leaf("edit.rename", "移动或重命名差异", "日志可见的移动及差异"),
				leaf("edit.output", "写入结果与报错", "与改动工具调用关联的返回内容"))),
		toolBranch("network", "网络查询与读取", "关键词和网址", "网页与检索结果"),
		toolBranch("agent", "子代理协作", "任务和协作消息", "子代理操作的返回内容"),
		toolBranch("other", "其他工具", "调用参数", "返回内容")),
}

var CompactContent = []string{"final", "summary", "unmarked", "user"}

type ContentScope map[string]bool

func ValidateContent(ids []string) ([]string, error) {
	available := make(map[string]bool)
	var walk func([]ContentNode)
	walk = func(nodes []ContentNode) {
		for _, node := range nodes {
			if len(node.Children) == 0 {
				available[node.ID] = true
			} else {
				walk(node.Children)
			}
		}
	}
	walk(ContentTree)
	if len(ids) == 0 || len(ids) > len(available) {
		return nil, fmt.Errorf("请选择至少一项有效的索引内容")
	}
	ids = slices.Clone(ids)
	slices.Sort(ids)
	for i, id := range ids {
		if !available[id] || (i > 0 && id == ids[i-1]) {
			return nil, fmt.Errorf("索引内容类别无效或重复")
		}
	}
	return ids, nil
}

// ContentScopeFor 将保存的类别转成投影范围；空范围沿用旧索引规则。
func ContentScopeFor(ids []string) ContentScope {
	if ids == nil {
		return nil
	}
	scope := make(ContentScope, len(ids))
	for _, id := range ids {
		scope[id] = true
	}
	return scope
}

type scopeRecord struct {
	Type    string `json:"type"`
	Payload struct {
		Type   string `json:"type"`
		Phase  string `json:"phase"`
		CallID string `json:"call_id"`
		Item   struct {
			Phase  string `json:"phase"`
			Parsed []struct {
				Kind string `json:"type"`
			} `json:"parsed_cmd"`
		} `json:"item"`
		Message json.RawMessage `json:"message"`
	} `json:"payload"`
}

type ContentProjector struct {
	scope                ContentScope
	limitDefaultDialogue bool
	calls                map[string]string
}

func NewContentProjector(scope ContentScope, recentContext bool) *ContentProjector {
	p := &ContentProjector{scope: scope, limitDefaultDialogue: recentContext && scope == nil}
	for id := range scope {
		if strings.HasSuffix(id, ".output") {
			p.calls = make(map[string]string)
			break
		}
	}
	return p
}

// IndexRecord 标记受时间范围限制的正文，执行记录和压缩摘要独立保留。
func (p *ContentProjector) IndexRecord(frame domain.JSONLFrame, content []byte, inWindow bool) (text string, windowLimited bool) {
	return p.record(frame, content, inWindow, false)
}

func (scope ContentScope) CheckRecord(frame domain.JSONLFrame, content []byte) string {
	text, _ := (&ContentProjector{scope: scope}).record(frame, content, true, true)
	return text
}

func (p *ContentProjector) record(frame domain.JSONLFrame, raw []byte, inWindow, checking bool) (text string, windowLimited bool) {
	if p.scope == nil {
		if !p.limitDefaultDialogue {
			return Record(frame, raw), false
		}
		// 默认范围沿用原有投影；时间设置只裁剪普通正文，不把范围改成省空间预设。
		event, parsed := compat.ParseFullReaderEvent(frame, raw)
		dialogue := parsed && (event.Role == "assistant" ||
			event.Role == "user" && event.Kind != "injected_context" && event.Kind != "agent_task")
		if dialogue && !inWindow {
			return "", true
		}
		return recordWithEvent(raw, event, parsed), dialogue
	}
	var record scopeRecord
	if json.Unmarshal(raw, &record) != nil {
		return "", false
	}
	switch record.Type {
	case "session_meta", "turn_context", "token_usage_record", "world_state":
		return "", false
	}
	if record.Payload.Type == "token_count" || record.Payload.Type == "reasoning" {
		return "", false
	}
	if record.Type == "compacted" || record.Payload.Type == "context_compacted" {
		if p.scope["summary"] {
			var message string
			if json.Unmarshal(record.Payload.Message, &message) == nil {
				return Normalize(message), false
			}
		}
		return "", false
	}
	event, ok := compat.ParseFullReaderEvent(frame, raw)
	if !ok || event.Kind == "injected_context" {
		return "", false
	}
	switch event.Role {
	case "user":
		id := "user"
		if event.Kind == "agent_task" {
			id = "agent.input"
		}
		if p.scope[id] && (inWindow || id != "user") {
			return Normalize(event.Text), id == "user"
		}
	case "assistant":
		if !inWindow {
			return "", false
		}
		phase := record.Payload.Phase
		if phase == "" {
			phase = record.Payload.Item.Phase
		}
		id := "unmarked"
		switch phase {
		case "final_answer":
			id = "final"
		case "commentary":
			id = "progress"
		}
		if p.scope[id] {
			return Normalize(event.Text), true
		}
	case "tool":
		return p.toolText(event, record, checking), false
	}
	return "", false
}

func (p *ContentProjector) toolText(event domain.ConversationEvent, record scopeRecord, checking bool) string {
	if event.Tool == nil {
		return ""
	}
	tool := event.Tool
	if isToolOutput(event.Kind) {
		group := p.calls[event.CallID]
		if group == "" {
			group = "other"
		}
		// 独立输出的类别在构建时确定，回读候选只复核对应输出文字。
		if checking || p.scope[group+".output"] {
			return Normalize(tool.Output)
		}
		return ""
	}
	group, target := classifyTool(event, record)
	p.rememberCall(event, group)
	var parts []string
	if group == "edit" {
		if p.scope["edit.paths"] {
			parts = append(parts, target)
		}
		for _, change := range event.Changes {
			if p.scope["edit.paths"] {
				parts = append(parts, change.Path, change.MovePath)
			}
			if p.scope["edit."+change.Kind] {
				parts = append(parts, change.Diff)
			}
		}
	} else if p.scope[group+".input"] {
		if target == "" && event.Kind != "command_execution" {
			target = readableArguments(event.Text)
		}
		parts = append(parts, target)
	}
	if p.scope[group+".output"] {
		parts = append(parts, tool.Output)
	}
	return Normalize(strings.Join(parts, "\n"))
}

func (p *ContentProjector) rememberCall(event domain.ConversationEvent, group string) {
	if p.calls != nil && event.CallID != "" && group != "other" &&
		(event.Kind == "function_call" || event.Kind == "custom_tool_call") {
		p.calls[event.CallID] = group
	}
}

func readableArguments(text string) string {
	text = strings.TrimSpace(text)
	var decoded any
	if len(text) == 0 || (text[0] != '{' && text[0] != '[') || json.Unmarshal([]byte(text), &decoded) != nil {
		return text
	}
	var values strings.Builder
	appendStrings(&values, decoded)
	return values.String()
}

func isToolOutput(kind string) bool {
	return kind == "function_call_output" || kind == "custom_tool_call_output"
}

// ReadCommitted 恢复断点之前的调用类型，续建时仍能识别随后追加的工具输出。
func (p *ContentProjector) ReadCommitted(input io.Reader) error {
	if p.calls == nil {
		_, err := io.Copy(io.Discard, input)
		return err
	}
	_, err := catalog.ScanReaderContent(input, func(frame domain.JSONLFrame, raw []byte) error {
		p.rememberRecordCall(frame, raw)
		return nil
	})
	return err
}

// rememberRecordCall 恢复已提交记录中的调用类别，以识别随后追加的工具输出。
func (p *ContentProjector) rememberRecordCall(frame domain.JSONLFrame, raw []byte) {
	if p.calls == nil || !frame.ValidJSON || frame.Pending || !bytes.Contains(raw, []byte(`"call_id"`)) {
		return
	}
	var record scopeRecord
	if json.Unmarshal(raw, &record) != nil || record.Payload.CallID == "" ||
		(record.Payload.Type != "function_call" && record.Payload.Type != "custom_tool_call") {
		return
	}
	event, ok := compat.ParseFullReaderEvent(frame, raw)
	if ok && event.Tool != nil {
		group, _ := classifyTool(event, record)
		p.rememberCall(event, group)
	}
}

func classifyTool(event domain.ConversationEvent, record scopeRecord) (group, target string) {
	target = event.Tool.Target
	if event.Tool.Kind == "command" {
		// 优先采用 Codex 对完成记录的分类。混合命令保持为一条记录。
		if parsed := record.Payload.Item.Parsed; len(parsed) == 1 {
			switch parsed[0].Kind {
			case "read":
				return "read", target
			case "list_files":
				return "list", target
			case "search":
				return "text", target
			}
		}
	}
	if event.Tool.Kind == "execution" {
		name := strings.TrimPrefix(event.ToolName, "functions_")
		if name == "exec" || strings.HasSuffix(name, ".exec") {
			return classifyWrappedTool(event.Text)
		}
		return "process", target
	}
	return toolGroup(event.Tool.Kind, target), target
}

var wrappedTool = regexp.MustCompile(`\btools\s*\.\s*([a-zA-Z0-9_]+)\s*\(`)
var wrappedCommand = regexp.MustCompile(`\btools\s*\.\s*exec_command\s*\(\s*\{\s*cmd\s*:\s*("(?:[^"\\]|\\.)*"|'[^'\\\r\n]*')`)

func classifyWrappedTool(code string) (string, string) {
	// 只读取可识别的调用形式。有多条调用时无法拆分其汇总输出。
	calls := wrappedTool.FindAllStringSubmatch(code, 2)
	if len(calls) != 1 {
		return "other", ""
	}
	switch calls[0][1] {
	case "exec_command":
		if match := wrappedCommand.FindStringSubmatch(code); match != nil {
			var target string
			if match[1][0] == '\'' {
				target = match[1][1 : len(match[1])-1]
			} else {
				target, _ = strconv.Unquote(match[1])
			}
			if target != "" {
				return toolGroup("command", target), target
			}
		}
		return "command", ""
	case "apply_patch":
		return "edit", ""
	case "write_stdin", "wait":
		return "process", ""
	case "view_image":
		return "read", ""
	default:
		return "other", ""
	}
}

func toolGroup(kind, target string) string {
	switch kind {
	case "search":
		return "network"
	case "read", "edit", "agent":
		return kind
	case "command":
		// 仅对单条、明确的读取命令细分。复合脚本归入命令执行。
		words := strings.Fields(target)
		if len(words) == 0 || strings.ContainsAny(target, ";|&<>$\n\x60()") {
			return "command"
		}
		executable := strings.TrimSuffix(strings.ToLower(path.Base(strings.ReplaceAll(words[0], `\`, "/"))), ".exe")
		switch executable {
		case "cat", "head", "tail", "get-content":
			return "read"
		case "ls", "dir", "get-childitem", "find":
			return "list"
		case "rg":
			if slices.Contains(words[1:], "--files") {
				return "list"
			}
			return "text"
		case "grep", "select-string":
			return "text"
		}
		return "command"
	default:
		return "other"
	}
}
