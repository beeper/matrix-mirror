package service

import (
	"strings"
	"testing"
)

func TestRenderUnit(t *testing.T) {
	unit := renderUnit(testOptions)
	wantExec := `ExecStart="/Users/me/go/bin/matrix-mirror" "sync" "--home" "/Users/me/odd & <dir> $$HOME 50%%" "--backfill-days" "90"`
	for _, want := range []string{wantExec, "Restart=always", "WantedBy=default.target"} {
		if !strings.Contains(unit, want+"\n") {
			t.Errorf("unit missing %q:\n%s", want, unit)
		}
	}
	if got := systemdQuote(`a "quoted" \path`); got != `"a \"quoted\" \\path"` {
		t.Errorf("quote: %s", got)
	}
}
