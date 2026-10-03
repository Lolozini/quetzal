package mailtmpl

import (
	"strings"
	"testing"
	"time"
)

func TestInvitation(t *testing.T) {
	link := "https://panel.example/#invite=tok123"
	m, err := Invitation("alice", `Bob's <b>"world"</b>`, []string{"view", "files"}, link,
		time.Date(2026, 10, 9, 1, 2, 0, 0, time.UTC), true, "https://panel.example")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{link, "alice invited you", "View: its page", "Files: its files", "9 October 2026, 01:02 UTC", "create one", "panel.example"} {
		if !strings.Contains(m.Text, want) {
			t.Errorf("text lacks %q:\n%s", want, m.Text)
		}
	}
	// What the owner typed is text in the HTML, never markup.
	if strings.Contains(m.HTML, "<b>") || !strings.Contains(m.HTML, "&lt;b&gt;") {
		t.Error("the server name is not escaped in the HTML")
	}
	if !strings.Contains(m.HTML, `href="`+link+`"`) {
		t.Error("the button does not open the link")
	}
	// Both logos are referenced and sent.
	if len(m.Inline) != 2 {
		t.Fatalf("%d inline images, want the light and the dark logo", len(m.Inline))
	}
	for _, f := range m.Inline {
		if !strings.Contains(m.HTML, "cid:"+f.ID) || len(f.Data) == 0 {
			t.Errorf("image %s is not shown, or empty", f.ID)
		}
	}

	m, _ = Invitation("alice", "x", []string{"view"}, link, time.Now(), false, "")
	if strings.Contains(m.Text, "create one") {
		t.Error("offers an account the panel will not create")
	}
}

func TestPasswordReset(t *testing.T) {
	link := "https://panel.example/#reset=tok"
	m, err := PasswordReset("carol", link, time.Hour, "https://panel.example/")
	if err != nil {
		t.Fatal(err)
	}
	if m.Subject != "Reset your Quetzal password" {
		t.Errorf("subject = %q", m.Subject)
	}
	for _, want := range []string{"Hi carol", link, "within the next hour", "panel.example"} {
		if !strings.Contains(m.Text, want) {
			t.Errorf("text lacks %q:\n%s", want, m.Text)
		}
	}
	if !strings.Contains(m.HTML, `href="`+link+`"`) {
		t.Error("the button does not open the link")
	}
}

func TestWithin(t *testing.T) {
	for d, want := range map[time.Duration]string{
		time.Hour: "within the next hour", 3 * time.Hour: "within 3 hours", 7 * 24 * time.Hour: "within 7 days",
		24 * time.Hour: "within a day",
	} {
		if got := within(d); got != want {
			t.Errorf("within(%v) = %q, want %q", d, got, want)
		}
	}
}
