//go:build windows

package main

import (
	"testing"
	"unsafe"
)

func TestIShellLinkVtblOffsets(t *testing.T) {
	var vtbl iShellLinkVtbl
	if offset := unsafe.Offsetof(vtbl.setPath); offset != 20*unsafe.Sizeof(vtbl.queryInterface) {
		t.Fatalf("setPath offset = %d, want %d", offset, 20*unsafe.Sizeof(vtbl.queryInterface))
	}
	if offset := unsafe.Offsetof(vtbl.getPath); offset != 3*unsafe.Sizeof(vtbl.queryInterface) {
		t.Fatalf("getPath offset = %d, want %d", offset, 3*unsafe.Sizeof(vtbl.queryInterface))
	}
}
