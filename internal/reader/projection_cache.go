// 中文说明：校验导航缓存的来源和字节边界，损坏缓存只能触发重建，不能扩展阅读范围。
package reader

import "codex-chronicle.local/internal/domain"

func referenceBefore(plan Plan, a, b domain.EventReference) bool {
	if a.Path == b.Path {
		return a.Start < b.Start && a.End <= b.Start
	}
	for _, s := range plan.Segments {
		if s.Relative == a.Path {
			return true
		}
		if s.Relative == b.Path {
			return false
		}
	}
	return false
}

func (p *Projection) validFor(plan Plan) bool {
	if p.Version != projectionVersion || p.Stamp != Stamp(plan) || len(p.Ranges)+len(p.Turns)+len(p.Tools)+len(p.Files) > maxProjectionEntries || len(p.Diagnostics) > 20 {
		return false
	}
	segments := map[string]Segment{}
	var total int64
	for _, s := range plan.Segments {
		segments[s.Relative] = s
		total += s.End
	}
	if p.BytesRead != total {
		return false
	}
	validRef := func(r domain.EventReference) bool {
		s, ok := segments[r.Path]
		return ok && r.Start >= 0 && r.End > r.Start && r.End <= s.End && r.End-r.Start <= MaxLineBytes && r.Line > 0 && r.Line <= uint64(s.End)
	}
	for _, r := range p.Ranges {
		s, ok := segments[r.Path]
		if !ok || r.Start < 0 || r.End <= r.Start || r.End > s.End || r.EndLine == 0 || r.EndLine > uint64(s.End) {
			return false
		}
		switch r.Reason {
		case "rollback", "mirror", "paired", "marker":
		default:
			return false
		}
	}
	for i, t := range p.Turns {
		if !validRef(t.Start) || t.ID == "" || len([]rune(t.Caption)) > 101 || t.UserMessages < 0 || t.AssistantMessages < 0 {
			return false
		}
		if i > 0 && !referenceBefore(plan, p.Turns[i-1].Start, t.Start) {
			return false
		}
		if t.End != (domain.EventReference{}) && (!validRef(t.End) || !referenceBefore(plan, t.Start, t.End)) {
			return false
		}
		if t.Message != nil && (!validRef(*t.Message) || *t.Message != t.Start && !referenceBefore(plan, t.Start, *t.Message)) {
			return false
		}
	}
	for _, t := range p.Tools {
		if t.ID == "" || !validRef(t.Call) || t.Result != nil && (!validRef(*t.Result) || !referenceBefore(plan, t.Call, *t.Result)) {
			return false
		}
	}
	for _, f := range p.Files {
		if !validRef(f.Source) || f.Path == "" || len(f.Path) > 4096 || f.Added < 0 || f.Removed < 0 || f.Diff != "" {
			return false
		}
	}
	return true
}
