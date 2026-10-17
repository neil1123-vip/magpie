//go:build !windows || nogui || !sub2api_webview2

package sub2apibrowser

import (
	"context"
	"os"

	"github.com/yetone/magpie/internal/sub2api"
)

func Available() (bool, string) { return false, unavailable }

func Login(context.Context, sub2api.BrowserRequest) (sub2api.BrowserCandidate, error) {
	return sub2api.BrowserCandidate{}, &sub2api.Error{Kind: sub2api.KindInvalid, Message: unavailable}
}

func MaybeRun() bool {
	return len(os.Args) == 2 && os.Args[1] == hostArg
}
