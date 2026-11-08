package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/yetone/magpie/internal/access"
	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/recoverystore"
)

type nativeRecoveryKey struct{}

type nativeRecovery struct {
	scope, branch    string
	original         []byte
	body             []byte
	compact          bool
	restored         bool
	retry            bool
	err              error
	pending          *recoverystore.Pending
	source           recoverystore.Record
	capturePrefix    json.RawMessage
	parents          []recoverystore.Record
	views            []recoverystore.Record
	writer           candidate
	window           int
	nativeOnly       bool
	choiceGeneration uint64
	adopted          bool
	currentWindow    int
	currentOutput    int
	currentModel     string
	currentImages    *[32]byte
	capacityReason   string
	excerptInput     json.RawMessage
	excerptOmitted   int
	capacity         bool
	state            string
	calls            []Call
	required         bool
	changedInput     bool
}

func nativeRecoveryOf(r *http.Request) *nativeRecovery {
	x, _ := r.Context().Value(nativeRecoveryKey{}).(*nativeRecovery)
	return x
}

func recoveryProtected(r *http.Request) bool {
	x := nativeRecoveryOf(r)
	return x != nil && !x.restored && len(sealedCompactions(x.body)) > 0
}

func recoveryCheckpoint(r *http.Request, body []byte) (checkpointWriter, bool, error) {
	if cp, ok := checkpointOf(body); ok {
		return cp, true, nil
	}
	x := nativeRecoveryOf(r)
	if x == nil || x.scope == "" || x.restored {
		return checkpointWriter{}, false, nil
	}
	// The small legacy writer cache can expire before an opted-in copy does.
	for _, enc := range sealedCompactions(body) {
		record, err := recoverystore.Load(x.scope, checkpointKey(enc))
		if errors.Is(err, recoverystore.ErrNotFound) {
			continue
		}
		if err != nil {
			return checkpointWriter{}, false, err
		}
		return checkpointWriter{Who: record.Writer, Provider: record.Provider, Model: record.Model, At: record.Created}, true, nil
	}
	return checkpointWriter{}, false, nil
}

func recoveryIdentity(r *http.Request, body []byte) (scope, branch string) {
	if who := access.Caller(r.Context()); who.KeyID != "" {
		scope = "key:" + who.KeyID
	} else if local(r) {
		scope = "local"
	} else {
		return "", ""
	}
	valid := func(v string) bool {
		return v != "" && len(v) <= 128 && strings.TrimSpace(v) == v &&
			!strings.ContainsAny(v, "\r\n\t") && !strings.EqualFold(v, "no session id") &&
			!slices.Contains([]string{"null", "undefined", "unknown"}, strings.ToLower(v))
	}
	m := requestSessionMetadata(r.Header, body)
	branch = m.Thread
	if flat := gjson.GetBytes(body, "client_metadata.thread_id").String(); flat != "" {
		if branch != "" && branch != flat {
			return scope, ""
		}
		branch = flat
	}
	if thread := r.Header.Get("thread-id"); thread != "" {
		if branch != "" && branch != thread {
			return scope, ""
		}
		branch = thread
	}
	if branch != "" && !valid(branch) {
		return scope, ""
	}
	for _, h := range append([]string{SessionHeader}, sessionHeaders...) {
		if branch != "" {
			break
		}
		if v := r.Header.Get(h); v != "" {
			if !valid(v) {
				return scope, ""
			}
			branch = v
			parent := m.Parent
			if parent == "" {
				parent = r.Header.Get("x-codex-parent-thread-id")
			}
			if branch == parent || (strings.Contains(m.Source, "subagent") && parent == "") {
				return scope, ""
			}
			break
		}
	}
	if branch == "" {
		return scope, ""
	}
	if sub := r.Header.Get("x-claude-code-agent-id"); sub != "" {
		if !valid(sub) {
			return scope, ""
		}
		branch += "\x00" + sub
	}
	return scope, checkpointKey(branch)
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request, from provider.Protocol, body []byte) {
	if from != provider.Responses {
		if nativeRecoveryOf(r) != nil {
			r = r.WithContext(context.WithValue(r.Context(), nativeRecoveryKey{}, (*nativeRecovery)(nil)))
		}
		s.serveCore(w, r, from, body)
		return
	}
	s.withNativeRecovery(w, r, body, func(w http.ResponseWriter, r *http.Request, body []byte) {
		s.serveCore(w, r, from, body)
	})
}

func (s *Server) withNativeRecovery(w http.ResponseWriter, r *http.Request, body []byte, next func(http.ResponseWriter, *http.Request, []byte)) {
	if nativeRecoveryOf(r) != nil || (!compactionTriggerIn(body) && len(sealedCompactions(body)) == 0) {
		next(w, r, body)
		return
	}
	scope, branch := recoveryIdentity(r, body)
	x := &nativeRecovery{scope: scope, branch: branch, original: body, body: body, compact: compactionTriggerIn(body)}
	r = r.WithContext(context.WithValue(r.Context(), nativeRecoveryKey{}, x))
	defer func() {
		if x.pending != nil {
			x.pending.Cancel()
		}
		for i, call := range x.calls {
			call.recovery = nil
			call.Recovery = x.state
			if i == len(x.calls)-1 && x.err != nil {
				call.Status, call.Error = 409, x.err.Error()
			}
			s.record(call)
		}
	}()
	// A previously adopted view survives restarts and the writer coming back.
	if scope != "" {
		b, records, err := recoveryExpand(scope, branch, body, true)
		if err != nil {
			writeError(w, provider.Responses, 409, "Local recovery: "+err.Error())
			return
		}
		if len(records) > 0 {
			x.body, x.parents, x.views, x.restored, x.adopted = b, records, records, true, true
		}
	}
	for round := 0; round < 3; round++ {
		capture := &recoveryWriter{w: w, header: w.Header().Clone(), compact: x.compact, recovery: x}
		if x.restored {
			state := "restored"
			if x.branch == "" {
				state = "replayed-this-request"
			}
			for _, view := range x.views {
				if view.Lossy {
					state = "excerpt"
				}
			}
			capture.header.Set("X-Magpie-Recovery-State", state)
			x.state = state
		}
		x.retry, x.err = false, nil
		x.capacity, x.window, x.capacityReason = false, 0, ""
		x.excerptInput, x.excerptOmitted = nil, 0
		next(capture, r, x.body)
		if capture.over {
			x.err = errors.New("compaction output exceeds the local recovery buffer limit; original history must be kept")
		}
		if capture.passed || r.Context().Err() != nil {
			return
		}
		if x.nativeOnly {
			if err := x.checkChoice(); err != nil {
				x.err = err
			}
		}
		if x.err != nil {
			writeError(w, provider.Responses, 409, "Local recovery: "+x.err.Error())
			return
		}
		if !x.restored && len(sealedCompactions(x.body)) > 0 &&
			(capture.status == 429 || capture.status >= 500 || bytes.Contains(capture.buf.Bytes(), []byte("compacted natively"))) {
			x.retry = true
		}
		if x.retry && !x.restored && scope != "" {
			b, records, err := recoveryExpand(scope, branch, x.original, false)
			if err == nil && len(records) > 0 {
				x.body, x.parents, x.views, x.restored = b, records, records, true
				continue
			}
			// Keep the upstream error when no copy was promised for this seal.
			if err != nil {
				x.state = "unavailable"
			}
			if err != nil && !errors.Is(err, recoverystore.ErrNotFound) {
				writeError(w, provider.Responses, 409, "Local recovery: "+err.Error())
				return
			}
		}
		if x.restored && x.capacity && !capture.passed {
			ok, err := consumeRecoveryChoice(scope, branch, x.original, "excerpt")
			if err == nil && ok {
				err = x.excerpt()
				if err == nil {
					continue
				}
			}
			if err != nil {
				writeError(w, provider.Responses, 409, "Local recovery: "+err.Error())
				return
			}
			why := "The restored history cannot be replayed by an allowed model."
			if x.capacityReason != "" {
				why += " " + x.capacityReason
			}
			if branch != "" && x.window > 0 {
				id, offerErr := offerRecoveryChoice(scope, branch, x.original, why, "excerpt")
				if offerErr != nil {
					why += " " + offerErr.Error()
				} else {
					why += " Local excerpt requires confirmation: " + id
				}
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"type": "local_recovery_capacity", "message": why}})
			return
		}
		if x.pending != nil && capture.status < 400 {
			output, enc, err := recoveryOutput(capture.buf.Bytes())
			if capture.over {
				err = errors.New("compaction output exceeds the local recovery limit")
			}
			for _, parent := range x.parents {
				if err == nil {
					err = recoverystore.Check(parent)
				}
			}
			if err == nil {
				if len(x.capturePrefix) > 0 {
					prefix, _ := recoveryItems(x.capturePrefix)
					items, _ := recoveryItems(output)
					output, err = json.Marshal(append(prefix, items...))
					if len(output) > recoverystore.MaxOutputBytes {
						err = errors.New("compaction output and retained client prefix exceed the local recovery limit")
					}
				}
			}
			if err == nil {
				err = x.pending.Commit(recoverystore.Record{Hash: checkpointKey(enc), Output: output, Writer: x.writer.who(), Model: x.writer.model})
			}
			if err != nil {
				x.err, x.state = err, "unavailable"
				writeError(w, provider.Responses, 409, "Local recovery copy was not committed; keep the original history: "+err.Error())
				return
			}
			// Re-read after commit, including the generation, before publishing.
			if _, err = recoverystore.Load(scope, checkpointKey(enc)); err != nil {
				x.err, x.state = err, "unavailable"
				writeError(w, provider.Responses, 409, "Local recovery copy is no longer available: "+err.Error())
				return
			}
			if err = x.pending.Check(); err != nil {
				x.err, x.state = err, "unavailable"
				writeError(w, provider.Responses, 409, "Local recovery was changed before publication: "+err.Error())
				return
			}
			capture.header.Set("X-Magpie-Recovery-State", "native-backed")
			x.state = "native-backed"
		} else if x.compact {
			capture.header.Set("X-Magpie-Recovery-State", "native-only")
			x.state = "native-only"
		}
		capture.publish()
		return
	}
	writeError(w, provider.Responses, 409, "Local recovery could not complete this request")
}

func recoveryExpand(scope, branch string, body []byte, viewsOnly bool) ([]byte, []recoverystore.Record, error) {
	seals := sealedCompactions(body)
	if len(seals) > 32 {
		return nil, nil, errors.New("too many compaction checkpoints")
	}
	var records []recoverystore.Record
	for _, enc := range seals {
		hash := checkpointKey(enc)
		var record recoverystore.Record
		err := recoverystore.ErrNotFound
		if branch != "" {
			record, err = recoverystore.View(scope, branch, hash)
		}
		if errors.Is(err, recoverystore.ErrNotFound) {
			record, err = recoverystore.Load(scope, hash)
			if viewsOnly && (errors.Is(err, recoverystore.ErrNotFound) || errors.Is(err, recoverystore.ErrStale) || err == nil && (!record.Lossy || record.PrefixItems == 0)) {
				continue
			}
		}
		if err != nil {
			return nil, nil, err
		}
		if len(sealedCompactions(append(append([]byte(`{"input":`), record.Input...), '}'))) != 0 {
			return nil, nil, errors.New("recovery source has an unresolved parent checkpoint")
		}
		previousOutput := record.Output
		body, record.Output, err = recoveryReplacement(body, previousOutput, record.Input, record.Input)
		if err != nil {
			return nil, nil, err
		}
		record.PrefixItems += len(gjson.ParseBytes(record.Output).Array()) - len(gjson.ParseBytes(previousOutput).Array())
		if len(record.Output) > recoverystore.MaxOutputBytes {
			return nil, nil, errors.New("retained client prefix exceeds the local recovery output limit")
		}
		if len(body) > recoverystore.MaxInputBytes {
			return nil, nil, errors.New("expanded history exceeds the local recovery limit")
		}
		records = append(records, record)
	}
	if len(records) > 0 {
		input, err := recoveryInput(body)
		if err != nil {
			return nil, nil, err
		}
		var request map[string]json.RawMessage
		_ = json.Unmarshal(body, &request)
		if compactionTriggerIn(body) {
			items, _ := recoveryItems(input)
			items = append(items, json.RawMessage(`{"type":"compaction_trigger"}`))
			input, _ = json.Marshal(items)
		}
		request["input"] = input
		// Bound the output reservation as well as the restored input.
		if _, ok := request["max_output_tokens"]; !ok {
			request["max_output_tokens"] = json.RawMessage("8192")
		}
		body, _ = json.Marshal(request)
	}
	return body, records, nil
}

// One UTF-8 byte per token is a conservative bound for readable text. Media
// needs model-specific tokenization, so unknown media budgets fail closed.
func recoveryBudget(body []byte, model string) (int, error) {
	var request map[string]json.RawMessage
	if json.Unmarshal(body, &request) != nil {
		return 0, errors.New("cannot budget an invalid restored request")
	}
	_, err := recoveryInput(body)
	if err != nil {
		return 0, err
	}
	cost, err := recoveryRequestCost(body, model)
	if err != nil {
		return 0, err
	}
	n, err := recoveryOutputLimit(body, provider.Responses, 8192)
	if err != nil {
		return 0, err
	}
	return cost + n + 4096, nil
}

var errRecoveryWindow = errors.New("restored history exceeds the model context window")

func recoveryOutputLimit(body []byte, to provider.Protocol, fallback int) (int, error) {
	var fields []string
	switch to {
	case provider.Responses:
		fields = []string{"max_output_tokens"}
	case provider.Chat:
		fields = []string{"max_tokens", "max_completion_tokens"}
	case provider.Anthropic:
		fields = []string{"max_tokens"}
	case provider.Gemini:
		fields = []string{"generationConfig.maxOutputTokens"}
	}
	n := 0
	for _, field := range fields {
		v := gjson.GetBytes(body, field)
		if !v.Exists() {
			continue
		}
		limit := v.Int()
		if v.Type != gjson.Number || limit <= 0 || limit > 1<<30 || v.Num != float64(limit) {
			return 0, errors.New("invalid restored output token reservation")
		}
		n = max(n, int(limit))
	}
	if n == 0 {
		n = fallback
	}
	if n <= 0 || n > 1<<30 {
		return 0, fmt.Errorf("%w: model output limit is unknown", errRecoveryWindow)
	}
	return n, nil
}

func (s *Server) prepareNativeRecovery(r *http.Request, c candidate, body []byte) error {
	x := nativeRecoveryOf(r)
	if x == nil {
		return nil
	}
	if x.restored {
		window := windowOf(c.p, c.model)
		x.currentWindow = window
		x.currentOutput = 0
		x.currentModel = provider.UpstreamNameIn(wiresOf(r.Context()), c.p.ID, c.model)
		x.currentImages = nil
		if recoveryHasImages(x.body) {
			// The target's own prompt may insert a developer item before the images.
			images := recoveryImageHistory(withModelSystem(r, c.p, c.model, provider.Responses, x.body))
			x.currentImages = &images
		}
		images, known := false, (*bool)(nil)
		// A backend may remove the request's cap; only its model limit then bounds output.
		for _, m := range c.p.Available() {
			if m.ID == c.model {
				x.currentOutput = m.Output
				images, known = m.Images, m.ImageInput
				break
			}
		}
		_, known = provider.ApplyImage(c.p.ID, c.model, images, known)
		if recoveryHasImages(x.body) && !slices.Contains(s.usable(c.p, c.model), provider.Responses) {
			return x.cannotReplay(errors.New("image recovery currently requires a Responses endpoint"))
		}
		if recoveryHasImages(x.body) && (known != nil && !*known || s.sightRefused(c.p.ID, c.model)) {
			return x.cannotReplay(fmt.Errorf("%s/%s does not support image input", c.p.ID, c.model))
		}
		if recoveryHasImages(x.body) {
			if n := s.imageCap(c.p, withFields(body, map[string]any{"model": x.currentModel})); n > 0 && recoveryCountImages(x.body) > n {
				return x.cannotReplay(fmt.Errorf("%s/%s takes at most %d images", c.p.ID, c.model, n))
			}
		}
		needed, err := recoveryBudget(body, x.currentModel)
		if err != nil {
			return x.cannotReplay(err)
		}
		canonicalNeeded, err := recoveryBudget(x.body, x.currentModel)
		if err != nil {
			return x.cannotReplay(err)
		}
		needed = max(needed, canonicalNeeded)
		if window <= 0 || needed > window {
			x.considerExcerpt(x.currentModel, window-max(0, needed-canonicalNeeded))
			return x.cannotReplay(fmt.Errorf("%s/%s needs at most %d tokens, window %d", c.p.ID, c.model, needed, window))
		}
		x.capacity = false
		for _, record := range x.parents {
			if err := recoverystore.Check(record); err != nil {
				return err
			}
		}
		r.Header.Del("X-Magpie-Recovery")
	}
	if !x.compact {
		return nil
	}
	if x.pending != nil {
		x.pending.Cancel()
		x.pending = nil
	}
	x.capturePrefix = nil
	x.writer = c
	enabled, err := recoverystore.Enabled(c.p.ID)
	if err != nil {
		return err
	}
	if !enabled && !x.required {
		return nil
	}
	x.required = true
	if x.scope == "" {
		return errors.New("local recovery requires an authenticated caller or a local request")
	}
	if x.nativeOnly {
		return x.checkChoice()
	}
	var allowed bool
	if token := r.Header.Get("X-Magpie-Recovery-Token"); token != "" && x.branch == "" {
		allowed, x.choiceGeneration, err = consumeRecoveryRequestAt(x.scope, x.original, token)
	} else {
		allowed, x.choiceGeneration, err = consumeRecoveryChoiceAt(x.scope, x.branch, x.original, "native-only")
	}
	if err != nil {
		return err
	} else if allowed {
		x.nativeOnly = true
		return x.checkChoice()
	}
	if !enabled {
		if x.branch == "" {
			id, err := offerRecoveryRequest(x.scope, x.original, "The fallback provider has no local recovery copy enabled")
			if err != nil {
				return err
			}
			return fmt.Errorf("fallback %s has no local recovery copy enabled; enable it or confirm request %s and retry with X-Magpie-Recovery-Token: %s", c.p.ID, id, id)
		}
		id, err := offerRecoveryChoice(x.scope, x.branch, x.original, "The fallback provider has no local recovery copy enabled", "native-only")
		if err != nil {
			return err
		}
		return fmt.Errorf("fallback %s has no local recovery copy enabled; enable it or confirm native-only request %s", c.p.ID, id)
	}
	if x.changedInput {
		return errors.New("provider middleware changed compaction history; no exact local recovery source can be promised")
	}
	// Save the canonical input, before this target's masking and translation.
	canonical, parents, err := recoveryExpand(x.scope, x.branch, x.body, false)
	if err == nil {
		var input json.RawMessage
		input, err = recoveryInput(canonical)
		if err == nil {
			// Tool declarations belong to the current request, not the saved history.
			items, _ := recoveryItems(input)
			items = slices.DeleteFunc(items, func(item json.RawMessage) bool {
				return gjson.GetBytes(item, "type").Str == "additional_tools"
			})
			input, _ = json.Marshal(items)
			x.source = recoverystore.Record{Scope: x.scope, Provider: c.p.ID, Writer: c.who(), Model: c.model, Input: input}
			unique := make([]recoverystore.Record, 0, len(x.parents)+len(parents))
			for _, parent := range append(slices.Clone(x.parents), parents...) {
				if slices.ContainsFunc(unique, func(r recoverystore.Record) bool { return r.Hash == parent.Hash }) {
					continue
				}
				unique = append(unique, parent)
				x.source.Lossy = x.source.Lossy || parent.Lossy
				x.source.Omitted += parent.Omitted
				if err = recoverystore.Check(parent); err != nil {
					break
				}
			}
			if err == nil {
				x.parents = unique
				x.capturePrefix, err = recoveryCarryPrefix(x.original, unique)
				if err == nil {
					x.source.PrefixItems = len(gjson.ParseBytes(x.capturePrefix).Array())
					x.pending, err = recoverystore.Begin(x.source)
				}
			}
		}
	}
	if err != nil {
		if x.branch == "" {
			id, offerErr := offerRecoveryRequest(x.scope, x.original, err.Error())
			if offerErr == nil {
				return fmt.Errorf("%w; adjust storage or confirm native-only request %s, then retry with X-Magpie-Recovery-Token: %s", err, id, id)
			}
		}
		id, offerErr := offerRecoveryChoice(x.scope, x.branch, x.original, err.Error(), "native-only")
		if offerErr == nil && id != "" {
			return fmt.Errorf("%w; adjust storage or confirm this request as native-only: %s", err, id)
		}
		return fmt.Errorf("%w; adjust local recovery storage before retrying", err)
	}
	return nil
}

func (x *nativeRecovery) excerpt() error {
	if x.branch == "" || len(x.views) != 1 {
		return errors.New("a local excerpt requires one checkpoint and a reliable branch identity")
	}
	view := x.views[0]
	if x.excerptInput == nil {
		return errors.New("latest complete interaction does not fit an allowed recovery model")
	}
	input, omitted := x.excerptInput, x.excerptOmitted
	body, output, err := recoveryReplacement(x.original, view.Output, view.Input, input)
	if err != nil {
		return err
	}
	if len(output) > recoverystore.MaxOutputBytes {
		return errors.New("retained client prefix exceeds the local recovery output limit")
	}
	view.PrefixItems += len(gjson.ParseBytes(output).Array()) - len(gjson.ParseBytes(view.Output).Array())
	view.Input, view.Output, view.Lossy, view.Omitted = input, output, true, view.Omitted+omitted
	x.body, x.views, x.adopted = body, []recoverystore.Record{view}, false
	var request map[string]json.RawMessage
	_ = json.Unmarshal(x.body, &request)
	if _, ok := request["max_output_tokens"]; !ok {
		request["max_output_tokens"] = json.RawMessage("8192")
		x.body, _ = json.Marshal(request)
	}
	return nil
}

// A later checkpoint must also consume the proven client prefix, even after the
// source messages it represented have been discarded by an excerpt.
func recoveryCarryPrefix(body []byte, parents []recoverystore.Record) (json.RawMessage, error) {
	var carry []json.RawMessage
	for _, parent := range parents {
		if !parent.Lossy || parent.PrefixItems == 0 {
			continue
		}
		if _, _, err := recoveryReplacement(body, parent.Output, parent.Input, parent.Input); err != nil {
			return nil, err
		}
		output, _ := recoveryItems(parent.Output)
		if parent.PrefixItems < 0 || parent.PrefixItems >= len(output) {
			return nil, errors.New("invalid retained client prefix length")
		}
		if len(carry) > 0 {
			return nil, errors.New("cannot carry multiple retained client prefixes into one checkpoint")
		}
		carry = output[:parent.PrefixItems]
	}
	if len(carry) == 0 {
		return nil, nil
	}
	prefix, err := json.Marshal(carry)
	if len(prefix) > recoverystore.MaxOutputBytes {
		return nil, errors.New("retained client prefix exceeds the local recovery output limit")
	}
	return prefix, err
}

func (x *nativeRecovery) cannotReplay(err error) error {
	x.capacity, x.capacityReason = true, err.Error()
	return fmt.Errorf("%w: %s", errRecoveryWindow, err)
}

func (x *nativeRecovery) considerExcerpt(model string, window int) {
	if x.branch == "" || len(x.views) != 1 || window <= 0 {
		return
	}
	needed, err := recoveryBudget(x.body, model)
	if err != nil {
		return
	}
	view := x.views[0]
	cost, _, err := recoveryInputCost(gjson.ParseBytes(view.Input), model)
	if err != nil {
		return
	}
	input, omitted, err := recoveryExcerpt(view.Input, window-(needed-cost)-1024, model)
	if err == nil && (x.excerptInput == nil || omitted < x.excerptOmitted) {
		x.window, x.excerptInput, x.excerptOmitted = window, input, omitted
	}
}

func recoveryWireCheck(ctx context.Context, to provider.Protocol, body []byte) error {
	x, _ := ctx.Value(nativeRecoveryKey{}).(*nativeRecovery)
	if x == nil {
		return nil
	}
	if x.pending != nil || x.restored {
		for _, field := range []string{"previous_response_id", "conversation"} {
			v := gjson.GetBytes(body, field)
			if v.Exists() && v.Type != gjson.Null && v.Raw != `""` {
				x.err = fmt.Errorf("local recovery cannot include an unmaterialized %s", field)
				return x.err
			}
		}
	}
	if err := x.checkChoice(); err != nil {
		x.err = err
		return err
	}
	if x.pending != nil {
		if err := x.pending.Check(); err != nil {
			x.err = err
			return err
		}
	}
	if !x.restored {
		return nil
	}
	reserve, err := recoveryOutputLimit(body, to, x.currentOutput)
	if err != nil {
		if errors.Is(err, errRecoveryWindow) {
			x.capacity = true
		} else {
			x.err = err
		}
		return err
	}
	cost := len(body)
	model := modelOf(body)
	if model == "" {
		model = x.currentModel
	}
	if recoveryHasImages(x.body) || to == provider.Responses && recoveryHasImages(body) || to != provider.Responses && hasImage(to, body) {
		if model != x.currentModel {
			return x.cannotReplay(errors.New("target changed the recovery model after its capacity check"))
		}
		if to != provider.Responses {
			return x.cannotReplay(errors.New("image recovery currently requires a Responses endpoint"))
		}
		expected := recoveryImageHistory(x.body)
		if x.currentImages != nil {
			expected = *x.currentImages
		}
		if expected != recoveryImageHistory(body) {
			return x.cannotReplay(errors.New("target changed restored images, their detail or their history positions"))
		}
	}
	if to == provider.Responses {
		cost, err = recoveryRequestCost(body, model)
		if err != nil {
			return x.cannotReplay(err)
		}
	} else if len(body) > recoverystore.MaxInputBytes {
		return x.cannotReplay(errors.New("restored request exceeds the 32 MiB local recovery limit"))
	}
	needed := cost + reserve + 4096
	if x.currentWindow <= 0 || needed > x.currentWindow {
		x.capacity = true
		if canonical, err := recoveryBudget(x.body, model); err == nil {
			x.considerExcerpt(model, x.currentWindow-max(0, needed-canonical))
		}
		return x.cannotReplay(fmt.Errorf("final request needs at most %d tokens, window %d", needed, x.currentWindow))
	}
	if !x.adopted && x.branch != "" {
		for i, record := range x.views {
			view, err := recoverystore.SaveView(x.scope, x.branch, record.Hash, record)
			if err != nil {
				x.err = err
				return err
			}
			x.views[i] = view
			for j, parent := range x.parents {
				if parent.Hash == record.Hash {
					x.parents[j] = view
				}
			}
		}
		x.adopted = true
	}
	for _, parent := range x.parents {
		if err := recoverystore.Check(parent); err != nil {
			x.err = err
			return err
		}
	}
	return nil
}

func recoveryHistoryChanged(before, after []byte) bool {
	for _, field := range []string{"input", "previous_response_id", "conversation"} {
		a, b := gjson.GetBytes(before, field), gjson.GetBytes(after, field)
		if a.Raw == b.Raw {
			continue
		}
		canonical := func(raw string) []byte {
			var value any
			d := json.NewDecoder(strings.NewReader(raw))
			d.UseNumber()
			if d.Decode(&value) != nil {
				return []byte(raw)
			}
			out, _ := json.Marshal(value)
			return out
		}
		if !bytes.Equal(canonical(a.Raw), canonical(b.Raw)) {
			return true
		}
	}
	return false
}

func (x *nativeRecovery) checkChoice() error {
	if !x.nativeOnly {
		return nil
	}
	st, err := recoverystore.Stats()
	if err == nil && (x.choiceGeneration == 0 || st.Generation != x.choiceGeneration) {
		err = recoverystore.ErrStale
	}
	return err
}

func recoveryOutput(body []byte) (json.RawMessage, string, error) {
	var completed gjson.Result
	if json.Valid(body) {
		completed = gjson.ParseBytes(body)
	} else {
		for line := range bytes.SplitSeq(body, []byte("\n")) {
			data, ok := bytes.CutPrefix(bytes.TrimSpace(line), []byte("data:"))
			if !ok {
				continue
			}
			event := gjson.ParseBytes(bytes.TrimSpace(data))
			switch event.Get("type").Str {
			case "error", "response.failed", "response.incomplete":
				return nil, "", errors.New("compaction did not complete successfully")
			case "response.completed":
				if completed.Exists() {
					return nil, "", errors.New("ambiguous compaction completion")
				}
				completed = event.Get("response")
			}
		}
	}
	output := completed.Get("output")
	if completed.Get("status").Str != "completed" || !output.IsArray() || completed.Get("error").IsObject() {
		return nil, "", errors.New("compaction has no successful complete output window")
	}
	enc := ""
	for _, item := range output.Array() {
		if slices.Contains(compactionKinds, item.Get("type").Str) {
			if enc != "" {
				return nil, "", errors.New("multiple compaction items have no unique replacement mapping")
			}
			enc = item.Get("encrypted_content").Str
		}
	}
	if enc == "" || strings.HasPrefix(enc, magpieCompaction) {
		return nil, "", errors.New("native compaction produced no sealed checkpoint")
	}
	return json.RawMessage(output.Raw), enc, nil
}

// Successful ordinary replies stream immediately. Only compaction and errors
// are held, so neither a failed commit nor recovery can publish a partial seal.
type recoveryWriter struct {
	w        http.ResponseWriter
	header   http.Header
	status   int
	buf      bytes.Buffer
	compact  bool
	passed   bool
	over     bool
	recovery *nativeRecovery
}

func (w *recoveryWriter) Header() http.Header { return w.header }
func (w *recoveryWriter) WriteHeader(code int) {
	if w.status == 0 || !w.passed && w.buf.Len() == 0 {
		w.status = code
	}
}
func (w *recoveryWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = 200
	}
	if !w.passed && recoveryKeepalive(p) {
		return len(p), nil // hold transport keepalives until a real reply
	}
	buffered := w.compact && (w.recovery == nil || w.recovery.pending != nil || w.recovery.nativeOnly)
	if !buffered && w.status < 400 {
		if w.compact && w.recovery != nil {
			w.recovery.state = "native-only"
			w.header.Set("X-Magpie-Recovery-State", "native-only")
		}
		if !w.passed {
			w.publish()
		}
		return w.w.Write(p)
	}
	if w.buf.Len()+len(p) > recoverystore.MaxOutputBytes {
		w.over = true
		return 0, errors.New("local recovery response buffer is full")
	}
	return w.buf.Write(p)
}

func recoveryKeepalive(p []byte) bool {
	for line := range bytes.SplitSeq(p, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) > 0 && line[0] != ':' {
			return false
		}
	}
	return true
}
func (w *recoveryWriter) Flush() {
	if w.passed {
		if f, ok := w.w.(http.Flusher); ok {
			f.Flush()
		}
	}
}
func (w *recoveryWriter) publish() {
	for k := range w.w.Header() {
		w.w.Header().Del(k)
	}
	for k, values := range w.header {
		w.w.Header()[k] = slices.Clone(values)
	}
	w.w.WriteHeader(max(w.status, 200))
	w.passed = true
	_, _ = w.w.Write(w.buf.Bytes())
	w.buf.Reset()
}
