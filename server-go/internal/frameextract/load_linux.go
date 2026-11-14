package frameextract

import (
	"fmt"
	"github.com/ebitengine/purego"
)

func openLibrary(path string) (uintptr, error) {
	h, err := purego.Dlopen(path, purego.RTLD_NOW|purego.RTLD_LOCAL)
	if err != nil {
		return 0, fmt.Errorf("load thumbnail library: %w", err)
	}
	return h, nil
}
func lookupSymbol(handle uintptr, name string) (uintptr, error) {
	p, err := purego.Dlsym(handle, name)
	if err != nil {
		return 0, fmt.Errorf("resolve thumbnail library function %s: %w", name, err)
	}
	return p, nil
}
