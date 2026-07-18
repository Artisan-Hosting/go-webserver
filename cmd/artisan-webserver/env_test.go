package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadDotEnv(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	content := "# comment\nARTISAN_TEST_ALPHA=one\nexport ARTISAN_TEST_BETA='two words'\nARTISAN_TEST_EXISTING=new\nINVALID\n=empty\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"ARTISAN_TEST_ALPHA", "ARTISAN_TEST_BETA"} {
		_ = os.Unsetenv(key)
		t.Cleanup(func() { _ = os.Unsetenv(key) })
	}
	t.Setenv("ARTISAN_TEST_EXISTING", "old")
	loadDotEnv(path)
	if os.Getenv("ARTISAN_TEST_ALPHA") != "one" || os.Getenv("ARTISAN_TEST_BETA") != "two words" {
		t.Fatalf("values=%q %q", os.Getenv("ARTISAN_TEST_ALPHA"), os.Getenv("ARTISAN_TEST_BETA"))
	}
	if os.Getenv("ARTISAN_TEST_EXISTING") != "old" {
		t.Fatal("existing environment value was overwritten")
	}
	loadDotEnv(filepath.Join(dir, "missing"))
}
