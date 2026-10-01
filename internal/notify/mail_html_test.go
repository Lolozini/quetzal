package notify

import (
	"bytes"
	"encoding/base64"
	"io"
	"mime"
	"mime/multipart"
	"net/mail"
	"strings"
	"testing"
)

// A mail with HTML reaches the reader as the text, then the HTML with its
// images: each part where a client looks for it, and each decoding back to
// what was sent.
func TestBuildMailWithHTML(t *testing.T) {
	long := strings.Repeat("A line longer than SMTP's 998 characters, which quoted-printable must fold. ", 20)
	in := Mail{
		Subject: "Invitation to Café",
		Text:    "Hello\nsee https://panel.example/#invite=tok\n" + long,
		HTML:    `<p>Hello</p><img src="cid:logo@q"><p>` + long + `</p>`,
		Inline:  []Inline{{ID: "logo@q", Name: "q.png", ContentType: "image/png", Data: bytes.Repeat([]byte{0x89, 'P', 'N', 'G'}, 100)}},
	}
	raw, err := buildMail("a@x.test", []string{"b@y.test"}, in)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(raw), "\r\n") {
		if len(line) > 998 {
			t.Fatalf("a line of %d characters, more than SMTP allows", len(line))
		}
	}
	m, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("message does not parse: %v", err)
	}
	if subj, _ := new(mime.WordDecoder).DecodeHeader(m.Header.Get("Subject")); subj != in.Subject {
		t.Errorf("subject = %q", subj)
	}
	mt, params, _ := mime.ParseMediaType(m.Header.Get("Content-Type"))
	if mt != "multipart/alternative" {
		t.Fatalf("content type = %q", mt)
	}
	alt := multipart.NewReader(m.Body, params["boundary"])

	text, err := alt.NextPart() // multipart decodes quoted-printable on its own
	if err != nil {
		t.Fatal(err)
	}
	if ct := text.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("first part = %q, want the text: clients show the last part they can", ct)
	}
	if got, _ := io.ReadAll(text); strings.ReplaceAll(string(got), "\r\n", "\n") != in.Text {
		t.Errorf("text = %q", got)
	}

	rel, err := alt.NextPart()
	if err != nil {
		t.Fatal(err)
	}
	mt, params, _ = mime.ParseMediaType(rel.Header.Get("Content-Type"))
	if mt != "multipart/related" || params["type"] != "text/html" {
		t.Fatalf("second part = %q %v", mt, params)
	}
	parts := multipart.NewReader(rel, params["boundary"])
	html, err := parts.NextPart()
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := io.ReadAll(html); string(got) != in.HTML {
		t.Errorf("html = %q", got)
	}
	img, err := parts.NextPart()
	if err != nil {
		t.Fatal(err)
	}
	if img.Header.Get("Content-ID") != "<logo@q>" || img.Header.Get("Content-Type") != "image/png" {
		t.Errorf("image headers = %v", img.Header)
	}
	if got, _ := io.ReadAll(base64.NewDecoder(base64.StdEncoding, img)); !bytes.Equal(got, in.Inline[0].Data) {
		t.Errorf("the image does not decode back to what was sent")
	}
	if _, err := parts.NextPart(); err != io.EOF {
		t.Errorf("more parts than the HTML and its image: %v", err)
	}
}
