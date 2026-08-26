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

func TestLoadDotEnvMultiLineQuotedValue(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	// STATUS_SERVICES is a list, and is only readable written one entry per
	// line -- which means the loader has to carry a value across lines.
	content := `ARTISAN_TEST_BEFORE=head
ARTISAN_TEST_LIST="
https://one.example.com/|One
https://two.example.com|Two
"
ARTISAN_TEST_AFTER=tail
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"ARTISAN_TEST_BEFORE", "ARTISAN_TEST_LIST", "ARTISAN_TEST_AFTER"} {
		_ = os.Unsetenv(key)
		t.Cleanup(func() { _ = os.Unsetenv(key) })
	}

	loadDotEnv(path)

	want := "\nhttps://one.example.com/|One\nhttps://two.example.com|Two\n"
	if got := os.Getenv("ARTISAN_TEST_LIST"); got != want {
		t.Errorf("list = %q, want %q", got, want)
	}
	// Parsing must resume after the closing quote, not treat the rest of the
	// file as part of the value.
	if got := os.Getenv("ARTISAN_TEST_AFTER"); got != "tail" {
		t.Errorf("value after the list = %q, want %q", got, "tail")
	}
	if got := os.Getenv("ARTISAN_TEST_BEFORE"); got != "head" {
		t.Errorf("value before the list = %q, want %q", got, "head")
	}
}

func TestLoadDotEnvUnterminatedQuoteReadsToEndOfFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	if err := os.WriteFile(path, []byte("ARTISAN_TEST_OPEN=\"one\ntwo\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_ = os.Unsetenv("ARTISAN_TEST_OPEN")
	t.Cleanup(func() { _ = os.Unsetenv("ARTISAN_TEST_OPEN") })

	loadDotEnv(path)

	if got := os.Getenv("ARTISAN_TEST_OPEN"); got != "one\ntwo\n" {
		t.Errorf("unterminated value = %q", got)
	}
}
