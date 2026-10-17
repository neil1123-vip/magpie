package sub2api

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

// ReadKeyGroups matches complete panel keys without exposing them to its caller.
func ReadKeyGroups(ctx context.Context, read func(context.Context, string) ([]byte, error), wanted []Key) (map[string]Group, error) {
	ids := make(map[string]string)
	values := make(map[string][]string)
	for _, key := range wanted {
		if key.ID == "" || key.Value == "" {
			continue
		}
		if previous, exists := ids[key.ID]; exists {
			if previous != key.Value {
				return nil, fmt.Errorf("ambiguous local Sub2API key identity")
			}
			continue
		}
		ids[key.ID] = key.Value
		values[key.Value] = append(values[key.Value], key.ID)
	}
	result := make(map[string]Group)
	if len(values) == 0 {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return result, nil
	}
	seen := make(map[string]Group)
	found := make(map[string]bool)
	total, pageSize := -1, 100
	for page := 1; page <= 100; page++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		body, err := read(ctx, fmt.Sprintf("/api/v1/keys?page=%d&page_size=%d", page, pageSize))
		if err != nil {
			return nil, err
		}
		items, count, size, err := parseKeyGroupPage(body, page, pageSize)
		if err != nil {
			return nil, err
		}
		if page > 1 && (count != total || size != pageSize) {
			return nil, fmt.Errorf("Sub2API key pagination changed during reading")
		}
		total, pageSize = count, size
		for _, item := range items {
			key, group, err := parseKeyGroup(item)
			if err != nil {
				return nil, err
			}
			if key == "" {
				continue
			}
			if previous, exists := seen[key]; exists {
				if previous.ID != group.ID || previous.Error != group.Error ||
					previous.Name != "" && group.Name != "" && previous.Name != group.Name {
					return nil, fmt.Errorf("conflicting Sub2API key groups")
				}
				if group.Name == "" {
					group.Name = previous.Name
				}
			}
			seen[key] = group
			if localIDs, match := values[key]; match {
				found[key] = true
				for _, id := range localIDs {
					if group.ID != "" {
						result[id] = group
					}
				}
			}
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if len(found) == len(values) || total < 0 || page*pageSize >= total {
			return result, nil
		}
	}
	return nil, fmt.Errorf("Sub2API key pagination exceeded its limit")
}

func parseKeyGroupPage(body []byte, requested, requestedSize int) ([]any, int, int, error) {
	value, err := monitorJSON(body)
	if err != nil {
		return nil, 0, 0, err
	}
	var envelopes []map[string]any
	paginated := false
	if envelope, ok := value.(map[string]any); ok {
		if success, exists := envelope["success"]; exists && success != true {
			return nil, 0, 0, fmt.Errorf("Sub2API key query was not successful")
		}
		if code, exists := envelope["code"]; exists && monitorID(code) != "0" {
			return nil, 0, 0, fmt.Errorf("Sub2API key query was not successful")
		}
		envelopes = append(envelopes, envelope)
		if data, exists := envelope["data"]; exists {
			value = data
		}
	}
	if object, ok := value.(map[string]any); ok {
		paginated = true
		if len(envelopes) == 0 || envelopes[0]["data"] != nil {
			envelopes = append(envelopes, object)
		}
		value = object["items"]
	}
	items, ok := value.([]any)
	if !ok {
		return nil, 0, 0, fmt.Errorf("unknown Sub2API key list shape")
	}
	total, pageSize := -1, requestedSize
	for _, envelope := range envelopes {
		for _, field := range []string{"pagination", "pageSize", "total_count", "totalCount", "limit", "offset", "cursor", "next", "next_page", "nextPage", "next_cursor", "nextCursor"} {
			if value, exists := envelope[field]; exists && value != nil {
				return nil, 0, 0, fmt.Errorf("unknown Sub2API key pagination")
			}
		}
		for _, field := range []string{"has_more", "hasMore"} {
			if value, exists := envelope[field]; exists && value != false {
				return nil, 0, 0, fmt.Errorf("incomplete Sub2API key pagination")
			}
		}
		if complete, exists := envelope["complete"]; exists && complete != true {
			return nil, 0, 0, fmt.Errorf("incomplete Sub2API key list")
		}
		_, hasTotal := envelope["total"]
		_, hasPage := envelope["page"]
		_, hasSize := envelope["page_size"]
		_, hasPageCount := envelope["page_count"]
		_, hasTotalPages := envelope["total_pages"]
		_, hasPages := envelope["pages"]
		if !hasTotal && !hasPage && !hasSize && !hasPageCount && !hasTotalPages && !hasPages {
			continue
		}
		count, countErr := strconv.Atoi(monitorID(envelope["total"]))
		page, pageErr := strconv.Atoi(monitorID(envelope["page"]))
		size, sizeErr := strconv.Atoi(monitorID(envelope["page_size"]))
		if !hasTotal || !hasPage || !hasSize || countErr != nil || pageErr != nil || sizeErr != nil ||
			count < 0 || page != requested || size < 1 || size > requestedSize ||
			total >= 0 && (total != count || pageSize != size) {
			return nil, 0, 0, fmt.Errorf("invalid Sub2API key pagination")
		}
		for _, field := range []string{"page_count", "total_pages", "pages"} {
			if value, exists := envelope[field]; exists {
				pages, err := strconv.Atoi(monitorID(value))
				expected := count / size
				if count%size != 0 {
					expected++
				}
				if err != nil || pages != expected && !(count == 0 && pages == 1) {
					return nil, 0, 0, fmt.Errorf("invalid Sub2API key page count")
				}
			}
		}
		total, pageSize = count, size
	}
	if total >= 0 {
		expected := min(pageSize, total-(requested-1)*pageSize)
		if expected < 0 || len(items) != expected {
			return nil, 0, 0, fmt.Errorf("incomplete Sub2API key page")
		}
	} else if paginated || requested > 1 {
		return nil, 0, 0, fmt.Errorf("Sub2API key pagination disappeared")
	}
	return items, total, pageSize, nil
}

func parseKeyGroup(item any) (string, Group, error) {
	row, ok := item.(map[string]any)
	if !ok {
		return "", Group{}, fmt.Errorf("invalid Sub2API key row")
	}
	var key string
	for _, field := range []string{"key", "api_key"} {
		if value, exists := row[field]; exists && value != nil {
			current, ok := value.(string)
			if !ok || key != "" && current != "" && key != current {
				return "", Group{}, fmt.Errorf("ambiguous Sub2API key value")
			}
			if current != "" {
				key = current
			}
		}
	}
	group := Group{ID: monitorID(row["group_id"])}
	if nested, exists := row["group"]; exists && nested != nil {
		object, ok := nested.(map[string]any)
		if !ok {
			return "", Group{}, fmt.Errorf("invalid Sub2API key group")
		}
		id := monitorID(object["id"])
		if group.ID != "" && id != "" && group.ID != id {
			return "", Group{}, fmt.Errorf("conflicting Sub2API key group identity")
		}
		if group.ID == "" {
			group.ID = id
		}
		group.Name, _ = object["name"].(string)
		group.Name = strings.TrimSpace(group.Name)
	}
	if group.ID == "0" {
		group.ID = ""
	}
	if routes, exists := row["group_ids"]; exists && routes != nil {
		list, ok := routes.([]any)
		if !ok {
			return "", Group{}, fmt.Errorf("invalid Sub2API key route groups")
		}
		seen := make(map[string]bool)
		for i, value := range list {
			id := monitorID(value)
			number, err := strconv.ParseInt(id, 10, 64)
			id = strconv.FormatInt(number, 10)
			if err != nil || number <= 0 || seen[id] {
				return "", Group{}, fmt.Errorf("invalid Sub2API key route group identity")
			}
			seen[id] = true
			if i == 0 {
				if group.ID != "" && group.ID != id {
					return "", Group{}, fmt.Errorf("conflicting Sub2API key route group identity")
				}
				group.ID = id
			}
		}
		if len(list) > 1 {
			// The billing endpoint reports the primary group, not each request's route.
			group.Error = "Sub2API key uses multiple routing groups; automatic group and price synchronization is unavailable"
		}
	}
	if group.ID == "" {
		group.Name = ""
	}
	return key, group, nil
}
