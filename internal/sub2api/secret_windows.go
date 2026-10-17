package sub2api

import (
	"runtime"
	"unsafe"

	"golang.org/x/sys/windows"
)

func protectSecret(plain []byte) ([]byte, error) {
	var in, out windows.DataBlob
	if len(plain) > 0 {
		in.Size, in.Data = uint32(len(plain)), &plain[0]
	}
	if err := windows.CryptProtectData(&in, nil, nil, 0, nil, windows.CRYPTPROTECT_UI_FORBIDDEN, &out); err != nil {
		return nil, err
	}
	runtime.KeepAlive(plain)
	defer windows.LocalFree(windows.Handle(unsafe.Pointer(out.Data)))
	return append([]byte(nil), unsafe.Slice(out.Data, out.Size)...), nil
}

func unprotectSecret(cipher []byte) ([]byte, error) {
	var in, out windows.DataBlob
	if len(cipher) > 0 {
		in.Size, in.Data = uint32(len(cipher)), &cipher[0]
	}
	if err := windows.CryptUnprotectData(&in, nil, nil, 0, nil, windows.CRYPTPROTECT_UI_FORBIDDEN, &out); err != nil {
		return nil, err
	}
	runtime.KeepAlive(cipher)
	defer windows.LocalFree(windows.Handle(unsafe.Pointer(out.Data)))
	return append([]byte(nil), unsafe.Slice(out.Data, out.Size)...), nil
}
