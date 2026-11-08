package gateway

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"image"
	"io"
	"strings"
	"sync"

	"github.com/tidwall/gjson"
	"github.com/yetone/magpie/internal/recoverystore"
)

// These local limits bound validation work as well as the saved request.
const recoveryImageBytes = 20 << 20
const recoveryImageMemory = 64 << 20
const recoveryImageCount = 128

var recoveryImageDecode sync.Mutex
var recoveryImageConfigs = make(map[[32]byte]image.Config)

func recoveryReplaying(ctx context.Context) bool {
	x, _ := ctx.Value(nativeRecoveryKey{}).(*nativeRecovery)
	return x != nil && x.restored
}

// Sol can refuse valid restored images without naming a size or capability limit.
func recoveryImageRefused(ctx context.Context, status int, body []byte) bool {
	x, _ := ctx.Value(nativeRecoveryKey{}).(*nativeRecovery)
	if x == nil || !x.restored || status != 400 || (x.currentModel != "gpt-6-sol" && x.currentModel != "gpt-6.1-sol") {
		return false
	}
	if !gjson.ValidBytes(body) {
		return false
	}
	e := gjson.ParseBytes(body)
	if nested := e.Get("error"); nested.IsObject() {
		e = nested
	}
	return e.Get("type").Str == "invalid_request_error" && e.Get("code").Str == "invalid_request" &&
		e.Get("message").Str == "The request could not be processed. Please check the request parameters." &&
		e.Get("param").Type == gjson.Null && recoveryHasImages(x.body)
}

func recoveryImage(url string) (image.Config, error) {
	var empty image.Config
	header, data, ok := strings.Cut(url, ",")
	if !ok || !strings.HasPrefix(header, "data:image/") || !strings.HasSuffix(header, ";base64") || len(data) == 0 {
		return empty, fmt.Errorf("local recovery requires an inline base64 image")
	}
	if len(data) > base64.StdEncoding.EncodedLen(recoveryImageBytes) {
		return empty, fmt.Errorf("local recovery image exceeds 20 MiB")
	}
	// A few concurrent requests must not each hold a fully decoded screenshot.
	recoveryImageDecode.Lock()
	defer recoveryImageDecode.Unlock()
	key := sha256.Sum256([]byte(url))
	if cfg, ok := recoveryImageConfigs[key]; ok {
		return cfg, nil
	}
	if standard, changed := stdBase64(data); changed {
		data = standard
	}
	raw, err := base64.StdEncoding.Strict().DecodeString(data)
	if err != nil || len(raw) > recoveryImageBytes {
		return empty, fmt.Errorf("invalid or oversized local recovery image encoding")
	}
	cfg, format, err := imageConfig(bytes.NewReader(raw))
	if err != nil {
		return empty, fmt.Errorf("invalid local recovery image header")
	}
	// shortcut: screenshots use PNG, JPEG or WebP; add animated formats only with bounded frame validation.
	if (format != "png" && format != "jpeg" && format != "webp") || header != "data:image/"+format+";base64" {
		return empty, fmt.Errorf("local recovery supports matching PNG, JPEG and WebP image data only")
	}
	if cfg.Width > 65535 || cfg.Height > 65535 {
		return empty, fmt.Errorf("local recovery image dimensions exceed the validation limit")
	}
	holds, ok := imageHolds(cfg, format)
	if !ok || holds > recoveryImageMemory {
		return empty, fmt.Errorf("local recovery image dimensions exceed the validation limit")
	}
	if _, _, err := image.Decode(bytes.NewReader(raw)); err != nil {
		return empty, fmt.Errorf("damaged local recovery image")
	}
	// Keep only bounded validated metadata, never the image or conversation bytes.
	if len(recoveryImageConfigs) >= recoveryImageCount {
		clear(recoveryImageConfigs)
	}
	recoveryImageConfigs[key] = cfg
	return cfg, nil
}

// Only protocol image parts are discounted; base64 in text or tool arguments
// is still text. Raw JSON string lengths include any escaping on the wire.
func recoveryItemMedia(item gjson.Result, model string) (delta, count int, err error) {
	for _, field := range []string{"content", "output"} {
		parts := item.Get(field)
		if !parts.IsArray() {
			continue
		}
		for _, part := range parts.Array() {
			switch part.Get("type").Str {
			case "input_file", "input_audio":
				return 0, 0, fmt.Errorf("local recovery cannot budget files or audio")
			case "input_image", "image_url":
				url, detail := part.Get("image_url"), part.Get("detail")
				if url.IsObject() {
					if nested := url.Get("detail"); nested.Exists() {
						if detail.Exists() && detail.Str != nested.Str {
							return 0, 0, fmt.Errorf("conflicting local recovery image detail")
						}
						detail = nested
					}
					url = url.Get("url")
				}
				if url.Type != gjson.String || detail.Exists() && detail.Type != gjson.String || part.Get("file_id").Exists() {
					return 0, 0, fmt.Errorf("unsupported local recovery image reference or detail")
				}
				if _, err := recoveryImageTokens(model, detail.Str, 1, 1); err != nil {
					return 0, 0, err
				}
				cfg, err := recoveryImage(url.Str)
				if err != nil {
					return 0, 0, err
				}
				tokens, err := recoveryImageTokens(model, detail.Str, cfg.Width, cfg.Height)
				if err != nil {
					return 0, 0, err
				}
				delta += tokens - len(url.Raw) + 2 // retain the empty JSON string
				count++
			}
		}
	}
	return delta, count, nil
}

func recoveryInputCost(input gjson.Result, model string) (int, int, error) {
	cost, count := len(input.Raw), 0
	for _, item := range input.Array() {
		delta, images, err := recoveryItemMedia(item, model)
		if err != nil {
			return 0, 0, err
		}
		cost += delta
		count += images
		if count > recoveryImageCount {
			return 0, 0, fmt.Errorf("local recovery exceeds %d images", recoveryImageCount)
		}
	}
	return cost, count, nil
}

func recoveryRequestCost(body []byte, model string) (int, error) {
	if err := recoveryMediaBounds(body); err != nil {
		return 0, err
	}
	input := gjson.GetBytes(body, "input")
	cost, _, err := recoveryInputCost(input, model)
	return len(body) - len(input.Raw) + cost, err
}

func recoveryHasImages(body []byte) bool {
	return recoveryCountImages(body) > 0
}

func recoveryMediaBounds(body []byte) error {
	if len(body) > recoverystore.MaxInputBytes {
		return fmt.Errorf("restored request exceeds the 32 MiB local recovery limit")
	}
	if recoveryCountImages(body) > recoveryImageCount {
		return fmt.Errorf("local recovery exceeds %d images", recoveryImageCount)
	}
	return nil
}

func recoveryCountImages(body []byte) int {
	count := 0
	for _, item := range gjson.GetBytes(body, "input").Array() {
		for _, field := range []string{"content", "output"} {
			parts := item.Get(field)
			if parts.IsArray() {
				for _, part := range parts.Array() {
					if kind := part.Get("type").Str; kind == "input_image" || kind == "image_url" {
						count++
					}
				}
			}
		}
	}
	return count
}

// Bind each image to its message/tool result and part position after translation.
func recoveryImageHistory(body []byte) [32]byte {
	hash := sha256.New()
	i := 0
	for _, item := range gjson.GetBytes(body, "input").Array() {
		if kind := item.Get("type").Str; kind == "reasoning" || kind == "additional_tools" {
			continue // Transport cleanup can remove reasoning and lift tool declarations.
		}
		for _, field := range []string{"content", "output"} {
			parts := item.Get(field)
			if !parts.IsArray() {
				continue
			}
			for j, part := range parts.Array() {
				kind := part.Get("type").Str
				if kind != "input_image" && kind != "image_url" {
					continue
				}
				url, detail := part.Get("image_url"), part.Get("detail").Str
				if url.IsObject() {
					if nested := url.Get("detail"); nested.Exists() {
						detail = nested.Str
					}
					url = url.Get("url")
				}
				if detail == "" {
					detail = "auto"
				}
				itemType := item.Get("type").Str
				if itemType == "" && item.Get("role").Str != "" {
					itemType = "message"
				}
				imageURL := url.Str
				if header, data, ok := strings.Cut(imageURL, ","); ok {
					if standard, changed := stdBase64(data); changed {
						imageURL = header + "," + standard
					}
				}
				fmt.Fprintf(hash, "%d:%q:%d:%q:%q:%q:%q:%d:", i, field, j, item.Get("role").Str, itemType, item.Get("call_id").Str, detail, len(imageURL))
				_, _ = io.WriteString(hash, imageURL)
			}
		}
		i++
	}
	return [32]byte(hash.Sum(nil))
}
