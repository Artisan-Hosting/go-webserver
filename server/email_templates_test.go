package main

import (
	"strings"
	"testing"
)

func sampleFormData() FormData {
	data := FormData{
		Name:          "Jane Doe",
		Email:         "jane@example.com",
		Business:      "Doe Design Co",
		SiteURL:       "https://doedesign.example.com",
		ClientMessage: "We need a new marketing site with a blog.",
	}
	data.OwnerMessage = buildOwnerMessage(data)
	return data
}

func TestBuildOwnerEmailContainsSubmittedData(t *testing.T) {
	data := sampleFormData()
	got := buildOwnerEmail(data)

	for _, want := range []string{data.Name, data.Email, data.Business, data.SiteURL, data.ClientMessage} {
		if !strings.Contains(got, want) {
			t.Errorf("owner email missing expected content %q", want)
		}
	}
}

func TestBuildClientEmailGreetsSubmitter(t *testing.T) {
	data := sampleFormData()
	got := buildClientEmail(data)

	if !strings.Contains(got, "Jane Doe") {
		t.Errorf("client email does not greet the submitter by name")
	}

	data.Name = "   "
	fallback := buildClientEmail(data)
	if !strings.Contains(fallback, "Thanks, there.") {
		t.Errorf("client email should fall back to a generic greeting when name is blank, got: %s", fallback)
	}
}

func TestEmailsEscapeUserSuppliedHTML(t *testing.T) {
	data := sampleFormData()
	data.Name = `Jane <script>alert(1)</script> Doe`
	data.ClientMessage = `<img src=x onerror=alert(1)>`
	data.OwnerMessage = buildOwnerMessage(data)

	owner := buildOwnerEmail(data)
	client := buildClientEmail(data)

	for name, rendered := range map[string]string{"owner": owner, "client": client} {
		if strings.Contains(rendered, "<script>") || strings.Contains(rendered, "<img src=x onerror") {
			t.Errorf("%s email did not escape user-supplied HTML (found a live tag, not an escaped one)", name)
		}
		if !strings.Contains(rendered, "&lt;script&gt;") && strings.Contains(rendered, "Jane") {
			t.Errorf("%s email: expected the script payload to appear HTML-escaped", name)
		}
	}
}
