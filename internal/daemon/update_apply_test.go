package daemon

import (
	"reflect"
	"testing"
)

func TestCleanRelaunchArgs(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want []string
	}{
		{
			name: "empty args falls back to run",
			args: nil,
			want: []string{"run"},
		},
		{
			name: "single binary arg falls back to run",
			args: []string{"slimmedia-agent"},
			want: []string{"run"},
		},
		{
			name: "non-run command falls back to run",
			args: []string{"slimmedia-agent", "audit", "/data"},
			want: []string{"run"},
		},
		{
			name: "run without pair preserves flags",
			args: []string{"slimmedia-agent", "run", "--server", "https://slimmedia.app", "--media-dir", "/media"},
			want: []string{"run", "--server", "https://slimmedia.app", "--media-dir", "/media"},
		},
		{
			name: "start command supported",
			args: []string{"slimmedia-agent", "start", "--server", "https://slimmedia.app"},
			want: []string{"start", "--server", "https://slimmedia.app"},
		},
		{
			name: "strips double-dash pair flag and value",
			args: []string{"slimmedia-agent", "run", "--pair", "PAIR123", "--media-dir", "/media"},
			want: []string{"run", "--media-dir", "/media"},
		},
		{
			name: "strips single-dash pair flag and value",
			args: []string{"slimmedia-agent", "run", "-pair", "PAIR123", "--media-dir", "/media"},
			want: []string{"run", "--media-dir", "/media"},
		},
		{
			name: "strips double-dash pair with equals syntax",
			args: []string{"slimmedia-agent", "run", "--pair=PAIR123", "--media-dir", "/media"},
			want: []string{"run", "--media-dir", "/media"},
		},
		{
			name: "strips single-dash pair with equals syntax",
			args: []string{"slimmedia-agent", "run", "-pair=PAIR123", "--media-dir", "/media"},
			want: []string{"run", "--media-dir", "/media"},
		},
		{
			name: "strips multiple pair flags if repeated",
			args: []string{"slimmedia-agent", "run", "--pair", "CODE1", "-pair=CODE2", "--name", "TestAgent"},
			want: []string{"run", "--name", "TestAgent"},
		},
		{
			name: "strips trailing pair flag with no value",
			args: []string{"slimmedia-agent", "run", "--name", "TestAgent", "--pair"},
			want: []string{"run", "--name", "TestAgent"},
		},
		{
			name: "strips pair flag followed immediately by another flag",
			args: []string{"slimmedia-agent", "run", "--pair", "--media-dir", "/media"},
			want: []string{"run", "--media-dir", "/media"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := cleanRelaunchArgs(tc.args)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("cleanRelaunchArgs(%v) = %v, want %v", tc.args, got, tc.want)
			}
		})
	}
}
