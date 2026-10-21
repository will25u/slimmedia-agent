//go:build !windows

package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "the SlimMedia tray app is a Windows-only executable")
	os.Exit(1)
}
