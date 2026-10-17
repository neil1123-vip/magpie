package sub2api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestReadKeyGroupsPagesAndMatchesOnlyCompleteKeys(t *testing.T) {
	first := make([]map[string]any, 100)
	for i := range first {
		first[i] = map[string]any{"key": fmt.Sprintf("other-%d", i), "group_id": 1}
	}
	first[3] = map[string]any{"key": "sk-exact", "group_id": 142, "group": map[string]any{"id": 142, "name": "Actual group"}}
	first[4] = map[string]any{"key": "sk-***masked", "group_id": 151}
	pages := []any{
		map[string]any{"data": map[string]any{"items": first, "total": 102, "page": 1, "page_size": 100, "total_pages": 2}},
		map[string]any{"data": map[string]any{"items": []any{
			map[string]any{"api_key": "sk-last", "group_id": "67"},
			map[string]any{"key": "sk-without-group", "group_id": nil},
		}, "total": 102, "page": 2, "page_size": 100}},
	}
	calls := 0
	result, err := ReadKeyGroups(context.Background(), func(_ context.Context, path string) ([]byte, error) {
		calls++
		if path != fmt.Sprintf("/api/v1/keys?page=%d&page_size=100", calls) || calls > len(pages) {
			t.Fatalf("unexpected query: %s", path)
		}
		return json.Marshal(pages[calls-1])
	}, []Key{
		{ID: "first", Value: "sk-exact"},
		{ID: "last", Value: "sk-last"},
		{ID: "masked", Value: "sk-complete-masked"},
		{ID: "ungrouped", Value: "sk-without-group"},
	})
	if err != nil || calls != 2 || len(result) != 2 || result["first"].ID != "142" ||
		result["first"].Name != "Actual group" || result["last"].ID != "67" {
		t.Fatalf("result=%v calls=%d err=%v", result, calls, err)
	}
}

func TestReadKeyGroupsRejectsAmbiguousOrIncompleteResponses(t *testing.T) {
	for _, body := range []string{
		`{"data":{"items":[],"total":1,"page":1,"page_size":100}}`,
		`{"data":{"items":[]}}`,
		`{"data":{"items":[],"total":0}}`,
		`{"data":{"items":[],"page":1,"page_size":100}}`,
		`{"data":{"items":[],"total":0,"page":2,"page_size":100}}`,
		`{"data":{"items":[],"total":0,"page":1,"page_size":0}}`,
		`{"data":{"items":[],"total":0,"page":1,"page_size":101}}`,
		`{"data":{"items":[],"total":0,"page":1,"page_size":100,"total_pages":2}}`,
		`{"data":{"items":[],"total":0,"page":1,"page_size":100,"pages":2}}`,
		`{"data":[],"pages":1}`,
		`{"data":[],"total_pages":1}`,
		`{"data":{"items":[],"total":10001,"page":1,"page_size":100}}`,
		`{"code":1,"data":[]}`,
		`{"success":false,"data":[]}`,
		`{"data":[{"key":"secret","group_id":1},{"key":"secret","group_id":2}]}`,
		`{"data":[{"key":"secret","group_id":1,"group":{"id":2}}]}`,
		`{"data":[{"key":"secret","api_key":"another-secret","group_id":1}]}`,
		`{"data":[{"key":"secret","group_id":1}]} {"data":[]}`,
		`{"data":null}`,
		`{"data":[],"has_more":true}`,
		`{"data":[],"next_page":2}`,
		`{"data":[],"complete":false}`,
		`{"data":[],"total_count":1}`,
	} {
		t.Run(body, func(t *testing.T) {
			result, err := ReadKeyGroups(context.Background(), func(context.Context, string) ([]byte, error) {
				return []byte(body), nil
			}, []Key{{ID: "local", Value: "secret"}})
			if err == nil || result != nil || strings.Contains(err.Error(), "secret") {
				t.Fatalf("result=%v err=%v", result, err)
			}
		})
	}
}

func TestReadKeyGroupsDiscardsPartialResultsAfterLaterFailure(t *testing.T) {
	items := make([]map[string]any, 100)
	for i := range items {
		items[i] = map[string]any{"key": fmt.Sprintf("key-%d", i), "group_id": 1}
	}
	body, err := json.Marshal(map[string]any{"data": map[string]any{
		"items": items, "total": 101, "page": 1, "page_size": 100,
	}})
	if err != nil {
		t.Fatal(err)
	}
	failure := errors.New("query failed")
	calls := 0
	result, err := ReadKeyGroups(context.Background(), func(context.Context, string) ([]byte, error) {
		calls++
		if calls == 1 {
			return body, nil
		}
		return nil, failure
	}, []Key{{ID: "found", Value: "key-0"}, {ID: "missing", Value: "key-later"}})
	if !errors.Is(err, failure) || result != nil || calls != 2 {
		t.Fatalf("result=%v calls=%d err=%v", result, calls, err)
	}
}

func TestReadKeyGroupsStopsWhenAllWantedKeysAreKnown(t *testing.T) {
	items := make([]map[string]any, 100)
	for i := range items {
		items[i] = map[string]any{"key": fmt.Sprintf("key-%d", i), "group_id": 1}
	}
	body, err := json.Marshal(map[string]any{"data": map[string]any{
		"items": items, "total": 20000, "page": 1, "page_size": 100,
	}})
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	result, err := ReadKeyGroups(context.Background(), func(context.Context, string) ([]byte, error) {
		calls++
		return body, nil
	}, []Key{{ID: "found", Value: "key-0"}})
	if err != nil || calls != 1 || result["found"].ID != "1" {
		t.Fatalf("result=%v calls=%d err=%v", result, calls, err)
	}
}

func TestReadKeyGroupsRejectsLaterPageInconsistency(t *testing.T) {
	items := make([]map[string]any, 100)
	for i := range items {
		items[i] = map[string]any{"key": fmt.Sprintf("key-%d", i), "group_id": 1}
	}
	for _, last := range []map[string]any{
		{"items": []any{map[string]any{"key": "key-0", "group_id": 2}}, "total": 101, "page": 2, "page_size": 100},
		{"items": []any{map[string]any{"key": "key-last", "group_id": 2}}, "total": 102, "page": 2, "page_size": 100},
	} {
		calls := 0
		result, err := ReadKeyGroups(context.Background(), func(context.Context, string) ([]byte, error) {
			calls++
			if calls == 1 {
				return json.Marshal(map[string]any{"data": map[string]any{
					"items": items, "total": 101, "page": 1, "page_size": 100,
				}})
			}
			return json.Marshal(map[string]any{"data": last})
		}, []Key{{ID: "found", Value: "key-0"}, {ID: "missing", Value: "key-last"}})
		if err == nil || result != nil || calls != 2 {
			t.Fatalf("result=%v calls=%d err=%v", result, calls, err)
		}
	}
}

func TestReadKeyGroupsUngroupedAndArrayShapes(t *testing.T) {
	for _, body := range []string{
		`[{"key":"secret","group_id":null}]`,
		`{"data":[{"key":"secret","group_id":0,"group":null}]}`,
		`{"code":0,"success":true,"data":[{"key":"secret","group":{}}]}`,
	} {
		result, err := ReadKeyGroups(context.Background(), func(context.Context, string) ([]byte, error) {
			return []byte(body), nil
		}, []Key{{ID: "local", Value: "secret"}})
		if err != nil || len(result) != 0 {
			t.Fatalf("result=%v err=%v", result, err)
		}
	}
}

func TestReadKeyGroupsUsesServerPageSize(t *testing.T) {
	for _, changedSize := range []bool{false, true} {
		t.Run(fmt.Sprint(changedSize), func(t *testing.T) {
			calls := 0
			result, err := ReadKeyGroups(t.Context(), func(_ context.Context, path string) ([]byte, error) {
				calls++
				if calls > 2 {
					t.Fatalf("unexpected extra page: %s", path)
				}
				size := 100
				if calls == 2 {
					size = 5
				}
				if path != fmt.Sprintf("/api/v1/keys?page=%d&page_size=%d", calls, size) {
					t.Fatalf("server page size was not used: %s", path)
				}
				items := []any{map[string]any{"key": "sk-last", "group_id": 7}}
				if calls == 1 {
					items = nil
					for i := 0; i < 5; i++ {
						items = append(items, map[string]any{"key": fmt.Sprintf("other-%d", i), "group_id": 5})
					}
				}
				size = 5
				if calls == 2 && changedSize {
					size = 4
					items = append(items, map[string]any{"key": "other-last", "group_id": 7})
				}
				return json.Marshal(map[string]any{"code": 0, "data": map[string]any{
					"items": items, "total": 6, "page": calls, "page_size": size, "pages": 2,
				}})
			}, []Key{{ID: "last", Value: "sk-last"}})
			if calls != 2 || changedSize && (err == nil || result != nil) ||
				!changedSize && (err != nil || result["last"].ID != "7") {
				t.Fatalf("server pagination: result=%+v calls=%d err=%v", result, calls, err)
			}
		})
	}
}

func TestReadKeyGroupsKedayaRoutes(t *testing.T) {
	result, err := ReadKeyGroups(t.Context(), func(context.Context, string) ([]byte, error) {
		// kiss-kedaya/sub2api 0557a72e: dto.APIKey and response.PaginatedData.
		return []byte(`{"code":0,"data":{"items":[
			{"key":"sk-single","group_id":5,"group_ids":[5],"group":{"id":5,"name":"Single"}},
			{"key":"sk-only-route","group_ids":[6]},
			{"key":"sk-routed","group_id":5,"group_ids":[5,6],"group":{"id":5,"name":"Primary"}}
		],"total":3,"page":1,"page_size":100,"pages":1}}`), nil
	}, []Key{{ID: "single", Value: "sk-single"}, {ID: "only-route", Value: "sk-only-route"}, {ID: "routed", Value: "sk-routed"}})
	if err != nil || result["single"].ID != "5" || result["single"].Error != "" ||
		result["only-route"].ID != "6" || result["only-route"].Error != "" ||
		result["routed"].Error == "" || strings.Contains(result["routed"].Error, "sk-routed") {
		t.Fatalf("dynamic routes treated as a single billing group: %+v, %v", result, err)
	}
	for _, row := range []string{
		`{"key":"secret","group_id":5,"group_ids":"5,6"}`,
		`{"key":"secret","group_id":5,"group_ids":[6]}`,
		`{"key":"secret","group_id":5,"group_ids":[5,null]}`,
		`{"key":"secret","group_id":5,"group_ids":[5,0]}`,
		`{"key":"secret","group_id":5,"group_ids":[5,5]}`,
		`{"key":"secret","group_id":5,"group_ids":[5,"05"]}`,
		`{"key":"secret","group_id":5,"group_ids":[5,{}]}`,
		`{"key":"secret","group_id":5,"group_ids":[5,"-1"]}`,
		`{"key":"secret","group_ids":["unknown"]}`,
		`{"key":"secret","group_ids":["9223372036854775808"]}`,
	} {
		t.Run(row, func(t *testing.T) {
			result, err := ReadKeyGroups(t.Context(), func(context.Context, string) ([]byte, error) {
				return []byte("[" + row + "]"), nil
			}, []Key{{ID: "local", Value: "secret"}})
			if err == nil || result != nil || strings.Contains(err.Error(), "secret") {
				t.Fatalf("invalid route identity accepted: %+v, %v", result, err)
			}
		})
	}
}
