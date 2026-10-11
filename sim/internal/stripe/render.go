package stripe

import (
	"encoding/json"
	"strings"
)

const maxExpandDepth = 4

func validateExpand(paths []string) error {
	for _, path := range paths {
		segments := strings.Split(path, ".")
		if len(segments) > maxExpandDepth {
			return invalidRequest("expand", "You cannot expand more than %d levels of a property. Property: %s", maxExpandDepth, path)
		}
		for _, seg := range segments {
			if seg == "" {
				return invalidRequest("expand", "Invalid expand path: %s", path)
			}
		}
	}
	return nil
}

func renderExpanded(a *account, obj any, paths []string) (json.RawMessage, error) {
	raw, err := json.Marshal(obj)
	if err != nil || len(paths) == 0 {
		return raw, err
	}
	var tree any
	if err := json.Unmarshal(raw, &tree); err != nil {
		return nil, err
	}
	for _, path := range paths {
		tree = expandPath(a, tree, strings.Split(path, "."))
	}
	return json.Marshal(tree)
}

func expandPath(a *account, node any, path []string) any {
	switch n := node.(type) {
	case []any:
		for i := range n {
			n[i] = expandPath(a, n[i], path)
		}
		return n
	case map[string]any:
		if len(path) == 0 {
			return n
		}
		child, ok := n[path[0]]
		if !ok {
			return n
		}
		if id, isID := child.(string); isID {
			obj, found := a.objectByID(id)
			if !found {
				return n
			}
			child = snapshot(obj)
		}
		n[path[0]] = expandPath(a, child, path[1:])
		return n
	}
	return node
}

type listPage struct {
	Object  string `json:"object"`
	Data    []any  `json:"data"`
	HasMore bool   `json:"has_more"`
	URL     string `json:"url"`
}

func paginate[P identified](items []P, keep func(P) bool, p *params, url string) (listPage, error) {
	limit, set, err := p.integer("limit")
	if err != nil {
		return listPage{}, err
	}
	if !set {
		limit = 10
	}
	if limit < 1 || limit > 100 {
		return listPage{}, invalidRequest("limit", "Invalid limit: must be between 1 and 100")
	}
	after, before := p.str("starting_after"), p.str("ending_before")
	if after != "" && before != "" {
		return listPage{}, exclusiveParams("ending_before", "starting_after")
	}

	newestFirst := make([]P, 0, len(items))
	cursor := -1
	for i := len(items) - 1; i >= 0; i-- {
		if id := items[i].objectID(); id == after || id == before {
			cursor = len(newestFirst)
			continue
		}
		if keep == nil || keep(items[i]) {
			newestFirst = append(newestFirst, items[i])
		}
	}
	if (after != "" || before != "") && cursor < 0 {
		param, id := "starting_after", after
		if before != "" {
			param, id = "ending_before", before
		}
		return listPage{}, noSuch("object", id, param)
	}

	window := newestFirst
	switch {
	case after != "":
		window = newestFirst[cursor:]
	case before != "":
		window = newestFirst[:cursor]
	}
	page := listPage{Object: "list", Data: []any{}, URL: url}
	if before != "" {
		start := max(0, len(window)-int(limit))
		page.HasMore = start > 0
		window = window[start:]
	} else if len(window) > int(limit) {
		page.HasMore = true
		window = window[:limit]
	}
	for _, item := range window {
		page.Data = append(page.Data, item)
	}
	return page, nil
}
