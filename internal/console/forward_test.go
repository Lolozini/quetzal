package console

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"testing/iotest"
	"unicode/utf8"
)

// The recette of 0.10.0 sent a line of 3,000 "é" through a game: the pod's log
// had it whole, the panel's console twice "�" in its place, where a 4 KiB
// block ended in the middle of a character (R-23). Blocks are cut between
// characters now, however the reads fall.
func TestConsoleTextIsCutBetweenCharacters(t *testing.T) {
	line := strings.Repeat("é", 3000) + " Привет 世界 🎮\n"
	for name, r := range map[string]io.Reader{
		"blocks":      strings.NewReader(line),
		"byte a time": iotest.OneByteReader(strings.NewReader(line)),
		"half reads":  iotest.HalfReader(strings.NewReader(line)),
	} {
		var got strings.Builder
		err := forwardText(r, func(s string) {
			if !utf8.ValidString(s) {
				t.Errorf("%s: a block cut a character: %q", name, s[len(s)-min(len(s), 8):])
			}
			// What the WebSocket carries: JSON, which turns invalid bytes into U+FFFD.
			b, _ := json.Marshal(Message{Type: "stdout", Data: s})
			var m Message
			_ = json.Unmarshal(b, &m)
			got.WriteString(m.Data)
		})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got.String() != line {
			t.Errorf("%s: %d bytes came out for %d, %d replacement characters", name, got.Len(), len(line), strings.Count(got.String(), "�"))
		}
	}
}

// Output that is not UTF-8 at all is passed on, not held back.
func TestConsoleTextKeepsBytesThatAreNotUTF8(t *testing.T) {
	raw := []byte{'a', 0xff, 0xfe, 'b', 0xc3}
	var got bytes.Buffer
	if err := forwardText(bytes.NewReader(raw), func(s string) { got.WriteString(s) }); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got.Bytes(), raw) {
		t.Errorf("got %q, want %q", got.Bytes(), raw)
	}
}
