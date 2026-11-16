// 中文说明：规范化记录中的工具、问答与文件改动；只解释历史证据，不执行命令或访问项目路径。
package compat

import (
	"codex-chronicle.local/internal/domain"
	"encoding/json"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

var recordedExit = regexp.MustCompile(`(?m)^(?:Process exited with code |Exit code: |Process exit code: )(-?\d+)\s*$`)

func extractRecordedText(raw json.RawMessage) string {
	if text := recordedString(raw); text != "" {
		return text
	}
	var items []json.RawMessage
	if json.Unmarshal(raw, &items) != nil {
		return ""
	}
	var out strings.Builder
	for i, item := range items {
		if i >= 256 || out.Len() >= 8<<20 {
			break
		}
		if text := recordedString(item); text != "" {
			out.WriteString(text)
			continue
		}
		o := rawObject(item)
		kind := rawString(o["type"])
		if kind == "input_text" || kind == "output_text" || kind == "text" || kind == "Text" || kind == "summary_text" || kind == "" {
			out.WriteString(recordedString(o["text"]))
		}
	}
	return out.String()
}

func recordedString(raw json.RawMessage) string {
	var value string
	_ = json.Unmarshal(raw, &value)
	return value
}

func questionReplyText(text string) (string, bool) {
	const open, close = "<send_user_message_question_reply>", "</send_user_message_question_reply>"
	t := strings.TrimSpace(text)
	if len(t) > 256<<10 || !strings.HasPrefix(t, open) || !strings.HasSuffix(t, close) {
		return "", false
	}
	var replies []struct {
		Question string `json:"question"`
		Answer   string `json:"answer"`
		ID       string `json:"questionItemId"`
	}
	if json.Unmarshal([]byte(t[len(open):len(t)-len(close)]), &replies) != nil || len(replies) == 0 || len(replies) > 32 {
		return "", false
	}
	parts := []string{}
	for _, r := range replies {
		if r.ID == "" || r.Question == "" {
			return "", false
		}
		parts = append(parts, r.Question+"\n"+r.Answer)
	}
	return strings.Join(parts, "\n\n"), true
}

func executionStatus(value string) string {
	switch strings.ToLower(value) {
	case "completed", "success", "succeeded":
		return "completed"
	case "failed", "failure", "error", "declined":
		return "failed"
	case "cancelled", "canceled", "interrupted":
		return "interrupted"
	case "in_progress", "running", "queued":
		return "running"
	default:
		return ""
	}
}

func presentTool(event *domain.ConversationEvent, input string, full bool) {
	info := &domain.ToolPresentation{Kind: "other", Title: firstNonEmpty(event.ToolName, "工具结果")}
	args := rawObject(json.RawMessage(input))
	name := event.ToolName
	for _, prefix := range []string{"functions.", "tools.", "functions_"} {
		name = strings.TrimPrefix(name, prefix)
	}
	get := func(keys ...string) string {
		for _, key := range keys {
			if v := rawString(args[key]); v != "" {
				return v
			}
		}
		return ""
	}
	switch name {
	case "exec_command", "shell_command", "bash", "powershell", "run_command":
		info.Kind, info.Title = "command", "命令执行"
		info.Target = get("cmd", "command", "script")
		if info.Target == "" {
			var commands []string
			if json.Unmarshal(args["command"], &commands) == nil {
				info.Target = strings.Join(commands, " ")
			}
		}
	case "apply_patch", "edit", "write_file":
		info.Kind, info.Title = "edit", "文件改动"
		info.Target = get("path", "file_path")
	case "read_file", "view_image", "open_file":
		info.Kind, info.Title = "read", "读取文件"
		info.Target = get("path", "file_path")
	case "web_search", "search_query", "web_fetch":
		info.Kind, info.Title = "search", "网络检索"
		info.Target = get("query", "q", "url")
	case "spawn_agent", "send_message", "wait_agent":
		info.Kind, info.Title = "agent", "子代理"
		info.Target = get("message", "target", "prompt")
	case "exec", "wait", "write_stdin":
		info.Kind, info.Title = "execution", "工具执行"
	}
	info.WorkingDirectory = get("workdir", "cwd", "working_directory")
	info.Target, _ = limitEventText(info.Target)
	if len(info.Target) > 2048 {
		info.Target = string([]rune(info.Target)[:min(512, len([]rune(info.Target)))]) + "…"
	}
	if event.Kind == "function_call_output" || event.Kind == "custom_tool_call_output" {
		info.Output = input
		if !full {
			info.Output, info.OutputTruncated = limitEventText(input)
		}
		if codeRaw := args["exit_code"]; len(codeRaw) > 0 && string(codeRaw) != "null" {
			var code int
			if json.Unmarshal(codeRaw, &code) == nil {
				event.ExitCode = &code
			}
		}
		if match := recordedExit.FindStringSubmatch(input); len(match) > 1 {
			if code, err := strconv.Atoi(match[1]); err == nil {
				event.ExitCode = &code
			}
		}
		if event.ExitCode != nil {
			if *event.ExitCode == 0 {
				event.Status = "completed"
			} else {
				event.Status = "failed"
			}
		}
	}
	if event.Kind == "command_execution" {
		info.Output = firstNonEmpty(get("formatted_output", "aggregated_output"), get("stdout")+get("stderr"))
		if !full {
			info.Output, info.OutputTruncated = limitEventText(info.Output)
		}
	}
	if event.Status == "" {
		event.Status = "unconfirmed"
	}
	if event.ExitCode != nil {
		if *event.ExitCode == 0 {
			event.Status = "completed"
		} else {
			event.Status = "failed"
		}
	}
	if info.Kind == "edit" && len(event.Changes) == 0 && strings.Contains(input, "*** Begin Patch") {
		event.Changes = parseRecordedPatch(input, event.Status)
	}
	event.Tool = info
}

func recordedChanges(raw json.RawMessage, status string) []domain.RecordedFileChange {
	var values map[string]json.RawMessage
	if json.Unmarshal(raw, &values) != nil {
		var items []map[string]json.RawMessage
		if json.Unmarshal(raw, &items) != nil {
			return nil
		}
		values = map[string]json.RawMessage{}
		for _, item := range items[:min(len(items), 2000)] {
			path := rawString(item["path"])
			if path == "" {
				continue
			}
			if len(item["type"]) == 0 {
				item["type"] = item["kind"]
			}
			if len(item["unified_diff"]) == 0 {
				item["unified_diff"] = item["diff"]
			}
			b, _ := json.Marshal(item)
			values[path] = b
		}
	}
	paths := make([]string, 0, len(values))
	for path := range values {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	out := make([]domain.RecordedFileChange, 0, min(len(paths), 2000))
	for _, path := range paths[:min(len(paths), 2000)] {
		if len(path) > 4096 {
			continue
		}
		v := rawObject(values[path])
		kind := strings.ToLower(rawString(v["type"]))
		switch kind {
		case "add", "create":
			kind = "create"
		case "delete":
			kind = "delete"
		default:
			kind = "modify"
		}
		diff := firstNonEmpty(rawString(v["unified_diff"]), rawString(v["content"]))
		change := domain.RecordedFileChange{Path: path, MovePath: rawString(v["move_path"]), Kind: kind, Status: firstNonEmpty(status, "unconfirmed")}
		if change.MovePath != "" {
			change.Kind = "rename"
		}
		for _, line := range strings.Split(diff, "\n") {
			if strings.HasPrefix(line, "+") && !strings.HasPrefix(line, "+++") {
				change.Added++
			}
			if strings.HasPrefix(line, "-") && !strings.HasPrefix(line, "---") {
				change.Removed++
			}
		}
		if kind == "create" && diff != "" && !strings.HasPrefix(diff, "@@") {
			change.Added = strings.Count(strings.TrimSuffix(diff, "\n"), "\n") + 1
		}
		change.Diff, change.Truncated = limitEventText(diff)
		out = append(out, change)
	}
	return out
}

func parseRecordedPatch(text, status string) []domain.RecordedFileChange {
	if len(text) > 8<<20 {
		return nil
	}
	out := []domain.RecordedFileChange{}
	for _, line := range strings.Split(text, "\n") {
		if line == "*** End Patch" {
			break
		}
		kind, path := "", ""
		for _, header := range []struct{ prefix, kind string }{{"*** Add File: ", "create"}, {"*** Update File: ", "modify"}, {"*** Delete File: ", "delete"}} {
			if strings.HasPrefix(line, header.prefix) {
				kind, path = header.kind, strings.TrimPrefix(line, header.prefix)
			}
		}
		if kind != "" && len(out) < 2000 && len(path) <= 4096 {
			out = append(out, domain.RecordedFileChange{Path: path, Kind: kind, Status: status})
		} else if len(out) > 0 {
			c := &out[len(out)-1]
			if strings.HasPrefix(line, "*** Move to: ") {
				c.MovePath = strings.TrimPrefix(line, "*** Move to: ")
				c.Kind = "rename"
			}
			if strings.HasPrefix(line, "+") {
				c.Added++
			}
			if strings.HasPrefix(line, "-") {
				c.Removed++
			}
			if len(c.Diff) < 64<<10 {
				c.Diff += line + "\n"
			} else {
				c.Truncated = true
			}
		}
	}
	return out
}
