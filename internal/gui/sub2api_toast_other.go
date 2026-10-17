//go:build !windows && !nogui

package gui

import "fmt"

func prepareSub2APIToast(_ *notifier, _, _ string) (func(func() error), func() error, error) {
	return nil, nil, fmt.Errorf("Sub2API notifications require the Windows desktop app")
}
