package gateway

import "fmt"

// Conservative bounds from the sizing tables and the Sol probes, checked 2026-11-09:
// https://developers.openai.com/api/docs/guides/images-vision
func recoveryImageTokens(model, detail string, width, height int) (int, error) {
	if width <= 0 || height <= 0 || width > 1<<30 || height > 1<<30 {
		return 0, fmt.Errorf("invalid image dimensions for local recovery")
	}
	if detail == "" {
		detail = "auto"
	}
	if detail != "low" && detail != "high" && detail != "auto" && detail != "original" {
		return 0, fmt.Errorf("unsupported image detail %q for local recovery", detail)
	}
	side, patches, multiplier := 2048, 2500, 120
	base, tile := 0, 0
	switch model {
	case "gpt-6-sol", "gpt-6.1-sol":
		// shortcut: empirical Sol profile; revisit when official image rules are published.
		limit := int64(30000)
		if detail == "high" {
			limit = 2500
		} else if detail == "low" {
			if longest := int64(max(width, height)); longest > 512 {
				// Round up so a fractional short edge cannot lose a patch row.
				width = int((int64(width)*512 + longest - 1) / longest)
				height = int((int64(height)*512 + longest - 1) / longest)
			}
		}
		n := min(((int64(width)+31)/32)*((int64(height)+31)/32), limit)
		// Single-image boundary responses exceed ceil(patches*1.2) by two.
		return int((n*120+99)/100) + 2, nil
	case "gpt-6-astra", "gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna":
		if detail == "low" {
			side, patches = 512, 0
		} else if detail == "auto" || detail == "original" {
			side, patches = 65535, 0
		} else if model == "gpt-6-astra" {
			side = 65535
		}
	case "gpt-5.5":
		if detail == "low" {
			side, patches = 512, 0
		} else if detail == "auto" || detail == "original" {
			side, patches = 6000, 10000
		}
	case "gpt-5.4", "gpt-5.4-mini", "gpt-5.4-nano":
		if detail == "low" {
			patches = 6144
		} else if detail == "original" {
			side, patches = 6000, 10000
		}
	case "gpt-5.2", "gpt-4.1-mini":
		if detail == "original" {
			return 0, fmt.Errorf("model %q does not support original image detail", model)
		}
		patches = 6144
		if model == "gpt-4.1-mini" {
			multiplier = 162
		}
	case "gpt-5.1":
		base, tile = 70, 140
	case "gpt-4.1", "gpt-4o":
		base, tile = 85, 170
	case "gpt-4o-mini":
		base, tile = 2833, 5667
	default:
		// shortcut: unlisted variants stay closed until their image rules are verified.
		return 0, fmt.Errorf("model %q has no verified image token budget for local recovery", model)
	}
	if tile > 0 {
		if detail == "original" {
			return 0, fmt.Errorf("model %q does not support original image detail", model)
		}
		if detail == "low" {
			return base, nil
		}
	}
	if longest := max(width, height); longest > side {
		width = max(1, int((int64(width)*int64(side)+int64(longest)/2)/int64(longest)))
		height = max(1, int((int64(height)*int64(side)+int64(longest)/2)/int64(longest)))
	}
	if tile > 0 {
		if shortest := min(width, height); shortest > 768 {
			width = int(int64(width) * 768 / int64(shortest))
			height = int(int64(height) * 768 / int64(shortest))
		}
		return base + ((width+511)/512)*((height+511)/512)*tile + 1, nil
	}
	n := ((width + 31) / 32) * ((height + 31) / 32)
	if patches > 0 {
		n = min(n, patches) // The API's proportional resize can only use fewer patches.
	}
	if n > 30000 {
		return 0, fmt.Errorf("image exceeds model %q's 30000-patch limit", model)
	}
	// Integer ceiling plus the documented one-token floating-point rounding margin.
	return (n*multiplier+99)/100 + 1, nil
}
