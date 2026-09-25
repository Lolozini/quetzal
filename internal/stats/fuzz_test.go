package stats

import "testing"

// These parse what a command run inside the game container printed, so the
// tenant controls the bytes. Byte counts can't be negative whatever it prints.
func FuzzParseNetDev(f *testing.F) {
	f.Add([]byte("Inter-|   Receive                                                |  Transmit\n face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed\n    lo:  123 1 0 0 0 0 0 0 123 1 0 0 0 0 0 0\n  eth0: 5000 40 0 0 0 0 0 0 7000 50 0 0 0 0 0 0\n"))
	f.Add([]byte("eth0: -5 0 0 0 0 0 0 0 -7 0 0 0 0 0 0 0\n"))
	f.Fuzz(func(t *testing.T, raw []byte) {
		rx, tx := ParseNetDev(raw)
		if rx < 0 || tx < 0 {
			t.Fatalf("ParseNetDev = %d, %d: negative", rx, tx)
		}
	})
}

func FuzzParseDuUsed(f *testing.F) {
	f.Add([]byte("1048576\t/data\n"))
	f.Add([]byte("-3\t/data\n"))
	f.Add([]byte("9223372036854775807\t/data\n"))
	f.Fuzz(func(t *testing.T, raw []byte) {
		if got := ParseDuUsed(raw); got < -1 {
			t.Fatalf("ParseDuUsed(%q) = %d", raw, got)
		}
	})
}
