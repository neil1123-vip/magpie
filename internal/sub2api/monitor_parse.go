package sub2api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// Protocol reference: CCX 0504411e3643 sub2api.go monitor helpers. Unknown
// fields stay unknown here; failed observations never erase successful ones.
func monitorJSON(body []byte) (any, error) {
	var value any
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return nil, fmt.Errorf("invalid Sub2API response")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, fmt.Errorf("invalid trailing Sub2API response")
	}
	return value, nil
}

func monitorObject(body []byte) (map[string]any, error) {
	value, err := monitorJSON(body)
	if err != nil {
		return nil, err
	}
	object, ok := value.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("Sub2API response is not an object")
	}
	if data, ok := object["data"].(map[string]any); ok {
		return data, nil
	}
	return object, nil
}

func parseGroups(body []byte) ([]Group, bool, error) {
	value, err := monitorJSON(body)
	if err != nil {
		return nil, false, err
	}
	complete := true
	if envelope, ok := value.(map[string]any); ok {
		value = envelope["data"]
		for _, key := range []string{"pagination", "page", "page_size", "pageSize", "page_count", "total_pages", "limit", "offset", "cursor", "next", "next_page", "nextPage", "next_cursor", "nextCursor"} {
			if _, exists := envelope[key]; exists {
				complete = false
			}
		}
		for _, key := range []string{"has_more", "hasMore"} {
			if value, exists := envelope[key]; exists {
				if more, valid := value.(bool); !valid || more {
					complete = false
				}
			}
		}
		if value, exists := envelope["complete"]; exists {
			if confirmed, valid := value.(bool); !valid || !confirmed {
				complete = false
			}
		}
		if list, ok := value.([]any); ok {
			for _, key := range []string{"total", "total_count", "totalCount"} {
				if _, exists := envelope[key]; exists {
					if total := monitorNumber(envelope, key); total == nil || *total != float64(len(list)) {
						complete = false
					}
				}
			}
		}
	}
	list, ok := value.([]any)
	if !ok {
		return nil, false, fmt.Errorf("unknown Sub2API groups shape")
	}
	groups := make([]Group, 0, len(list))
	seen := make(map[string]bool)
	for _, item := range list {
		row, ok := item.(map[string]any)
		if !ok {
			return nil, false, fmt.Errorf("invalid Sub2API group")
		}
		id := monitorID(row["id"])
		if id == "" {
			id = monitorGroupID(row)
		}
		name := monitorName(row)
		if id == "" || name == "" || seen[id] {
			return nil, false, fmt.Errorf("ambiguous Sub2API group identity")
		}
		seen[id] = true
		visible := true
		group := Group{ID: id, Name: name, Visible: &visible}
		// Only explicit, typed fields can describe panel state.
		for _, field := range []string{"enabled", "is_enabled"} {
			if enabled, ok := row[field].(bool); ok {
				group.Enabled = &enabled
				break
			}
		}
		group.Rate = monitorNumber(row, "group_rate_multiplier", "rate_multiplier")
		groups = append(groups, group)
	}
	return groups, complete, nil
}

// BillingRate keeps monitoring and explicit price checks on the same response interpretation.
func BillingRate(body []byte) (*float64, error) {
	row, err := monitorObject(body)
	if err != nil {
		return nil, err
	}
	rate := monitorNumber(row, "effective_rate_multiplier", "resolved_rate_multiplier", "group_rate_multiplier")
	if rate == nil {
		return nil, fmt.Errorf("Sub2API billing omitted an effective rate")
	}
	return rate, nil
}

func monitorNumber(row map[string]any, keys ...string) *float64 {
	for _, key := range keys {
		var n float64
		var err error
		switch value := row[key].(type) {
		case json.Number:
			n, err = value.Float64()
		case float64:
			n = value
		case string:
			n, err = strconv.ParseFloat(strings.TrimSpace(value), 64)
		default:
			continue
		}
		if err == nil && !math.IsNaN(n) && !math.IsInf(n, 0) && n >= 0 {
			return &n
		}
	}
	return nil
}

func monitorID(value any) string {
	switch value := value.(type) {
	case string:
		return strings.TrimSpace(value)
	case json.Number:
		if n, err := value.Int64(); err == nil && n >= 0 {
			return value.String()
		}
	case float64:
		if value >= 0 && !math.IsInf(value, 0) && math.Trunc(value) == value {
			return strconv.FormatFloat(value, 'f', -1, 64)
		}
	}
	return ""
}

func monitorGroupID(row map[string]any) string {
	for _, key := range []string{"group_id", "groupId", "groupID"} {
		if id := monitorID(row[key]); id != "" {
			return id
		}
	}
	if group, ok := row["group"].(map[string]any); ok {
		if id := monitorID(group["id"]); id != "" {
			return id
		}
		return monitorGroupID(group)
	}
	return ""
}

func monitorName(row map[string]any) string {
	for _, key := range []string{"group_name", "groupName", "channel_name", "channelName", "name"} {
		if name, ok := row[key].(string); ok && strings.TrimSpace(name) != "" {
			return strings.TrimSpace(name)
		}
	}
	return ""
}

func parseMatrix(body []byte) ([]map[string]any, error) {
	row, err := monitorObject(body)
	if err != nil {
		return nil, err
	}
	items, ok := row["items"].([]any)
	if !ok || len(items) == 0 {
		return nil, fmt.Errorf("Sub2API matrix omitted group rows")
	}
	rows := make([]map[string]any, 0, len(items))
	seen := make(map[string]bool)
	for _, item := range items {
		group, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("invalid Sub2API matrix row")
		}
		id := monitorGroupID(group)
		if id == "" || seen[id] {
			return nil, fmt.Errorf("Sub2API matrix has ambiguous group IDs")
		}
		seen[id] = true
		rows = append(rows, group)
	}
	return rows, nil
}

func selectMatrix(rows []map[string]any, group Group) (map[string]any, error) {
	var found map[string]any
	for _, row := range rows {
		match := group.ID != "" && monitorGroupID(row) == group.ID
		if group.ID == "" && group.Name != "" {
			match = normalizeMonitorName(monitorName(row)) == normalizeMonitorName(group.Name)
		}
		if !match {
			continue
		}
		if found != nil {
			return nil, fmt.Errorf("Sub2API matrix group is ambiguous")
		}
		found = row
	}
	if found == nil {
		return nil, fmt.Errorf("Sub2API matrix did not include the group")
	}
	return found, nil
}

func parseV2(row map[string]any, source, scope, match string) *Observation {
	metrics := row
	if nested, ok := row["metrics"].(map[string]any); ok {
		metrics = nested
	}
	var sampledAt time.Time
	trend, ok := row["trend"].([]any)
	if !ok {
		trend, _ = row["buckets"].([]any)
	}
	for _, item := range trend {
		bucket, ok := item.(map[string]any)
		if !ok {
			continue
		}
		stamp, _ := bucket["bucket_start"].(string)
		at, err := time.Parse(time.RFC3339Nano, stamp)
		current, valid := bucket["metrics"].(map[string]any)
		if err == nil && valid && (sampledAt.IsZero() || at.After(sampledAt)) {
			metrics, sampledAt = current, at
		}
	}
	observation := &Observation{
		Source: source, Scope: scope, Window: "90m", SampledAt: sampledAt, Match: match,
		MatchedName: monitorName(row),
	}
	if value := monitorNumber(metrics, "error_rate"); value != nil && *value <= 1 {
		availability := 1 - *value
		observation.Availability = &availability
	} else if value := monitorNumber(metrics, "success_rate"); value != nil && *value <= 1 {
		observation.Availability = value
	}
	observation.CacheRate = monitorNumber(metrics, "cache_rate", "cache_rate_trimmed")
	if observation.CacheRate != nil && *observation.CacheRate > 1 {
		observation.CacheRate = nil
	}
	ttft := metrics
	if nested, ok := metrics["ttft"].(map[string]any); ok {
		ttft = nested
	}
	observation.TTFTP50MS = monitorNumber(ttft, "p50_ms", "ttft_p50_ms")
	return observation
}

func parseV1(body []byte, group Group) (*Observation, error) {
	value, err := monitorJSON(body)
	if err != nil {
		return nil, err
	}
	var records []map[string]any
	var walk func(any, int)
	walk = func(value any, depth int) {
		if depth > 12 {
			return
		}
		switch value := value.(type) {
		case map[string]any:
			if monitorName(value) != "" && (monitorNumber(value, "availability_7d", "availability", "latency_ms", "primary_latency_ms") != nil) {
				records = append(records, value)
			}
			for _, child := range value {
				walk(child, depth+1)
			}
		case []any:
			for _, child := range value {
				walk(child, depth+1)
			}
		}
	}
	walk(value, 0)
	var best map[string]any
	match := "id"
	for _, row := range records {
		if group.ID == "" || monitorGroupID(row) != group.ID {
			continue
		}
		if best != nil {
			return nil, fmt.Errorf("Sub2API V1 group ID is ambiguous")
		}
		best = row
	}
	if best == nil {
		score, second := 0, 0
		for _, row := range records {
			if id := monitorGroupID(row); group.ID != "" && id != "" && id != group.ID {
				continue
			}
			candidate := monitorNameScore(normalizeMonitorName(group.Name), normalizeMonitorName(monitorName(row)))
			if candidate > score {
				second, score, best = score, candidate, row
			} else if candidate > second {
				second = candidate
			}
		}
		if score < 70 || score-second < 15 {
			return nil, fmt.Errorf("Sub2API V1 group name is ambiguous or unknown")
		}
		match = "approximate"
	}
	observation := &Observation{
		Source: "v1", Scope: "group", Window: "7d", Approximate: true,
		Match: match, MatchedName: monitorName(best),
		ProbeLatencyMS: monitorNumber(best, "latency_ms", "primary_latency_ms"),
	}
	observation.Availability = monitorNumber(best, "availability_7d", "availability")
	if observation.Availability != nil {
		value := *observation.Availability
		if value > 1 {
			value /= 100
		}
		if value > 1 {
			observation.Availability = nil
		} else {
			observation.Availability = &value
		}
	}
	return observation, nil
}

func normalizeMonitorName(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	name = strings.Map(func(r rune) rune {
		if strings.ContainsRune("-_/.:|()[]", r) {
			return ' '
		}
		return r
	}, name)
	return strings.Join(strings.Fields(name), " ")
}

func monitorNameScore(wanted, candidate string) int {
	if wanted == "" || candidate == "" {
		return 0
	}
	if wanted == candidate {
		return 100
	}
	compact := func(value string) string {
		return strings.Map(func(r rune) rune {
			if unicode.IsSpace(r) {
				return -1
			}
			return r
		}, value)
	}
	left, right := compact(wanted), compact(candidate)
	if left == right {
		return 98
	}
	if strings.Contains(candidate, wanted) || strings.Contains(wanted, candidate) {
		return 85
	}
	if strings.Contains(right, left) || strings.Contains(left, right) {
		return 83
	}
	wantedTokens, candidateTokens := strings.Fields(wanted), strings.Fields(candidate)
	matched := 0
	used := make([]bool, len(candidateTokens))
	for _, token := range wantedTokens {
		best, index := 0, -1
		for i, other := range candidateTokens {
			if used[i] {
				continue
			}
			if token == other {
				best, index = 2, i
				break
			}
			if best < 1 && len([]rune(token)) >= 3 && len([]rune(other)) >= 3 && (strings.HasPrefix(token, other) || strings.HasPrefix(other, token)) {
				best, index = 1, i
			}
		}
		if index >= 0 {
			used[index] = true
			matched += best
		}
	}
	return matched * 100 / (2 * len(wantedTokens))
}
