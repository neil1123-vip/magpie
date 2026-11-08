package gateway

import (
	"fmt"
	"testing"
)

func TestNativeRecoveryImageSolBudgets(t *testing.T) {
	for _, model := range []string{"gpt-6-sol", "gpt-6.1-sol"} {
		for _, tt := range []struct {
			detail     string
			w, h, want int
		}{
			{"original", 1, 1, 4},
			{"original", 31, 31, 4},
			{"original", 32, 32, 4},
			{"original", 33, 32, 5},
			{"original", 33, 33, 7},
			{"original", 257, 129, 56},
			{"original", 400, 120, 65},
			{"original", 1024, 1024, 1231},
			{"low", 512, 512, 310},
			{"low", 513, 513, 310},
			{"low", 1025, 64, 22},
			{"low", 1025, 65, 41},
			{"low", 65, 1025, 41},
			{"high", 4096, 512, 2460},
			{"high", 1600, 1600, 3002},
			{"high", 1601, 1600, 3002},
			{"high", 3201, 800, 3002},
			{"high", 800, 3201, 3002},
			{"original", 6400, 4800, 36002},
			{"original", 6401, 4800, 36002},
			{"original", 8000, 6000, 36002},
			{"auto", 2048, 2048, 4918},
			{"", 2048, 2048, 4918},
			{"auto", 9601, 3200, 36002},
			{"", 9601, 3200, 36002},
			{"original", 65535, 64, 4918},
			{"original", 65537, 64, 4920},
			{"original", 1 << 30, 1 << 30, 36002},
		} {
			t.Run(fmt.Sprintf("%s/%s/%dx%d", model, tt.detail, tt.w, tt.h), func(t *testing.T) {
				got, err := recoveryImageTokens(model, tt.detail, tt.w, tt.h)
				if err != nil || got != tt.want {
					t.Fatalf("budget = %d, %v; want %d", got, err, tt.want)
				}
			})
		}
	}
}

func TestNativeRecoveryImageTokens(t *testing.T) {
	for _, tt := range []struct {
		model, detail string
		w, h, want    int
	}{
		{"gpt-6-astra", "high", 1024, 1024, 1230},
		{"gpt-6-astra", "high", 2048, 2048, 3001},
		{"gpt-6-astra", "high", 4096, 512, 2459},
		{"gpt-6-astra", "low", 2048, 2048, 309},
		{"gpt-6-astra", "", 2048, 2048, 4917},
		{"gpt-6-astra", "original", 2048, 2048, 4917},
		{"gpt-5.6-sol", "high", 4096, 512, 616},
		{"gpt-5.6-terra", "auto", 2048, 2048, 4917},
		{"gpt-5.6-luna", "low", 2048, 2048, 309},
		{"gpt-5.5", "high", 8192, 8192, 3001},
		{"gpt-5.5", "auto", 8192, 8192, 12001},
		{"gpt-5.5", "original", 8192, 8192, 12001},
		{"gpt-5.5", "low", 2048, 2048, 309},
		{"gpt-5.4", "low", 8192, 8192, 4917},
		{"gpt-5.4-mini", "auto", 8192, 8192, 3001},
		{"gpt-5.4-nano", "original", 8192, 8192, 12001},
		{"gpt-5.2", "low", 8192, 8192, 4917},
		{"gpt-5.2", "high", 8192, 8192, 4917},
		{"gpt-4.1-mini", "auto", 1024, 1024, 1660},
		{"gpt-5.1", "low", 2048, 2048, 70},
		{"gpt-5.1", "high", 1024, 1024, 631},
		{"gpt-4.1", "low", 2048, 2048, 85},
		{"gpt-4o", "high", 2048, 4096, 1106},
		{"gpt-4o-mini", "low", 2048, 2048, 2833},
		{"gpt-4o-mini", "auto", 512, 512, 8501},
		{"gpt-6-astra", "high", 1, 100000, 2459},
		{"gpt-5.4", "high", 1, 100000, 78},
		{"gpt-4o", "high", 1, 100000, 766},
		{"gpt-6-astra", "low", 1, 1, 3},
		{"gpt-4o", "auto", 1, 1, 256},
	} {
		t.Run(tt.model+"/"+tt.detail, func(t *testing.T) {
			got, err := recoveryImageTokens(tt.model, tt.detail, tt.w, tt.h)
			if err != nil || got != tt.want {
				t.Fatalf("image %dx%d: got %d, %v; want %d", tt.w, tt.h, got, err, tt.want)
			}
		})
	}
}

func TestNativeRecoveryImageTokensRefuseUnknown(t *testing.T) {
	for _, tt := range []struct {
		model, detail string
		w, h          int
	}{
		{"gpt-6.1-sol-2099-01-01", "high", 512, 512},
		{"gpt-6-sol-2099-01-01", "high", 512, 512},
		{"provider/gpt-6.1-sol", "high", 512, 512},
		{"gpt-6.1-sol", "future", 512, 512},
		{"gpt-5.5-codex", "high", 512, 512},
		{"gpt-5.5-2099-01-01", "high", 512, 512},
		{"provider/gpt-5.5", "high", 512, 512},
		{"gpt-4.1-nano", "high", 512, 512},
		{"", "high", 512, 512},
		{"gpt-5.5", "unrecognized", 512, 512},
		{"gpt-5.2", "original", 512, 512},
		{"gpt-4.1-mini", "original", 512, 512},
		{"gpt-4o", "original", 512, 512},
		{"gpt-6-astra", "original", 8192, 8192},
		{"gpt-5.6-sol", "auto", 8192, 8192},
		{"gpt-5.6-terra", "", 8192, 8192},
		{"gpt-5.5", "high", 0, 512},
		{"gpt-5.5", "high", 512, -1},
		{"gpt-5.5", "high", 1<<30 + 1, 512},
	} {
		if n, err := recoveryImageTokens(tt.model, tt.detail, tt.w, tt.h); err == nil {
			t.Errorf("accepted %s %s %dx%d: %d tokens", tt.model, tt.detail, tt.w, tt.h, n)
		}
	}
}

func TestNativeRecoveryImagePatchBoundary(t *testing.T) {
	for _, size := range []int{31, 32, 33} {
		want := 3
		if size == 33 {
			want = 6
		}
		got, err := recoveryImageTokens("gpt-6-astra", "original", size, size)
		if err != nil || got != want {
			t.Fatalf("%d square: got %d, %v; want %d", size, got, err, want)
		}
	}
	for _, w := range []int{30000 * 32, 30001 * 32} {
		// The dimension limit applies before the rejection limit, even for original.
		if got, err := recoveryImageTokens("gpt-6-astra", "original", w, 1); err != nil || got != 2459 {
			t.Fatalf("thin original: got %d, %v", got, err)
		}
	}
	if got, err := recoveryImageTokens("gpt-6-astra", "original", 160*32, 187*32); err != nil || got != 35905 {
		t.Fatalf("under 30000 patches: got %d, %v", got, err)
	}
	if got, err := recoveryImageTokens("gpt-6-astra", "original", 150*32, 200*32); err != nil || got != 36001 {
		t.Fatalf("at 30000 patches: got %d, %v", got, err)
	}
	if _, err := recoveryImageTokens("gpt-6-astra", "original", 160*32, 188*32); err == nil {
		t.Fatal("accepted more than 30000 original patches")
	}
}
