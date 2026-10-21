//go:build windows
package main

import "testing"

func TestControlThemeSubAppForClass(t *testing.T) {
	tests := []struct {
		cls  string
		dark bool
		want string
	}{
		{"Edit", true, "DarkMode_CFD"},
		{"EDIT", true, "DarkMode_CFD"},
		{"edit", true, "DarkMode_CFD"},
		{"Edit", false, "CFD"},
		{"EDIT", false, "CFD"},
		{"ComboBox", true, "DarkMode_CFD"},
		{"COMBOBOX", true, "DarkMode_CFD"},
		{"ComboBox", false, "CFD"},
		{"Button", true, "DarkMode_Explorer"},
		{"Button", false, "Explorer"},
		{"msctls_progress32", true, "DarkMode_Explorer"},
		{"msctls_progress32", false, "Explorer"},
		{"\\o/ Walk_MainWindow_Class \\o/", true, "DarkMode_Explorer"},
	}

	for _, tt := range tests {
		got := controlThemeSubAppForClass(tt.cls, tt.dark)
		if got != tt.want {
			t.Errorf("controlThemeSubAppForClass(%q, %v) = %q, want %q", tt.cls, tt.dark, got, tt.want)
		}
	}
}
