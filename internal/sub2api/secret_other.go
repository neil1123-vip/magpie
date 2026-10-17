//go:build !windows

package sub2api

import "errors"

func protectSecret([]byte) ([]byte, error) {
	return nil, errors.New("Sub2API session storage requires Windows")
}

func unprotectSecret([]byte) ([]byte, error) {
	return nil, errors.New("Sub2API session storage requires Windows")
}
