package frameextract

import (
	"fmt"
	"path/filepath"
	"syscall"
	"unsafe"
)

// ライブラリは専用ワーカーにだけロードする。この検索先は他のサーバープロセスへ波及しない。
func openLibrary(path string) (uintptr, error) {
	directory, err := syscall.UTF16PtrFromString(filepath.Dir(path))
	if err != nil {
		return 0, err
	}
	result, _, callErr := syscall.NewLazyDLL("kernel32.dll").NewProc("SetDllDirectoryW").Call(uintptr(unsafe.Pointer(directory)))
	if result == 0 {
		return 0, fmt.Errorf("set thumbnail library directory: %w", callErr)
	}
	dll, err := syscall.LoadDLL(path)
	if err != nil {
		return 0, fmt.Errorf("load thumbnail library: %w", err)
	}
	return uintptr(dll.Handle), nil
}
func lookupSymbol(handle uintptr, name string) (uintptr, error) {
	p, err := syscall.GetProcAddress(syscall.Handle(handle), name)
	if err != nil {
		return 0, fmt.Errorf("resolve thumbnail library function %s: %w", name, err)
	}
	return p, nil
}
