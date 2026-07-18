package main

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// TestThemeSwapIsRespectedAtStartup proves the core claim of the theming
// system: pointing MAIL_THEME_CSS/MAIL_PRODUCT_* at different values
// changes a rendered email's branding with zero Go code changes. It runs
// in a subprocess because hermesEngine() is a process-lifetime singleton
// (env is read once at startup, matching how the real server runs) — env
// vars set after another test in this binary already built the singleton
// would have no effect, so a fresh process is required to observe startup
// behavior.
func TestThemeSwapIsRespectedAtStartup(t *testing.T) {
	if os.Getenv("MAILTHEME_TEST_CHILD") == "1" {
		got := buildClientEmail(sampleFormData())
		if !strings.Contains(got, "#ff1493") {
			t.Fatalf("rendered email missing the swapped theme's accent color:\n%s", got)
		}
		if !strings.Contains(got, "Acme Co") {
			t.Fatalf("rendered email missing the swapped product name:\n%s", got)
		}
		if strings.Contains(got, "artisanhosting.net") {
			t.Fatalf("rendered email still links to the default product, swap had no effect:\n%s", got)
		}
		return
	}

	cmd := exec.Command(os.Args[0], "-test.run=TestThemeSwapIsRespectedAtStartup", "-test.v")
	cmd.Env = append(os.Environ(),
		"MAILTHEME_TEST_CHILD=1",
		"MAIL_THEME_CSS=../../theme/testdata/alt_theme.css",
		"MAIL_PRODUCT_NAME=Acme Co",
		"MAIL_PRODUCT_LINK=https://acme.example.com",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("child process failed: %v\n%s", err, out)
	}
}
