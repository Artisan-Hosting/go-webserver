package main

import (
	"net/http"
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	if os.Getenv("MAIL_THEME_CSS") == "" {
		_ = os.Setenv("MAIL_THEME_CSS", "../../theme/default.css")
	}
	os.Exit(m.Run())
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
