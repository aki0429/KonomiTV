//go:build !linux && !windows

package frameextract

import "fmt"

func openLibrary(string) (uintptr, error) {
	return 0, fmt.Errorf("thumbnail library loading is unsupported on this platform")
}
func lookupSymbol(uintptr, string) (uintptr, error) {
	return 0, fmt.Errorf("thumbnail library loading is unsupported on this platform")
}
