//go:build windows

package main

import "testing"

func TestServiceConfigPath(t *testing.T) {
	for _, test := range []struct {
		args []string
		want string
	}{
		{[]string{"-config", `C:\Users\a\rtelegram.conf`}, `C:\Users\a\rtelegram.conf`},
		{[]string{"-no-live", "--config", `C:\b.conf`}, `C:\b.conf`},
		{[]string{`-config=C:\c.conf`}, `C:\c.conf`},
		{[]string{"-config"}, ""},
		{nil, ""},
	} {
		if got := serviceConfigPath(test.args); got != test.want {
			t.Errorf("serviceConfigPath(%q) = %q, want %q", test.args, got, test.want)
		}
	}
}
