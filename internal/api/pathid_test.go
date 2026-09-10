package api

import (
	"math"
	"math/big"
	"net/http"
	"strconv"
	"testing"
)

// An id parsed as 64 bits and then converted to uint does not fail on a 32-bit
// build -- it silently becomes a different number. "4294967296" arrives as 0,
// and the handler loads and acts on that row instead of rejecting the request.
//
// So the property is not "rejects big numbers", it is "never accepts an id and
// changes it". That holds on every platform, and on the one where uint is 32
// bits it is the difference between a 400 and touching the wrong row.
func TestPathIDNeverAcceptsAnIDItHasChanged(t *testing.T) {
	// One past whatever this platform's uint can hold: 2^64 on a 64-bit build,
	// 2^32 on a 32-bit one. Always the first value that cannot round-trip.
	overflow := new(big.Int).Add(new(big.Int).SetUint64(uint64(math.MaxUint)), big.NewInt(1)).String()

	for _, in := range []string{
		"0", "1", "42",
		strconv.FormatUint(uint64(math.MaxUint), 10), // the largest id that fits
		overflow,
		overflow + "0", // and well past it
	} {
		r := &http.Request{}
		r.SetPathValue("id", in)
		got, ok := pathID(r, "id")
		if !ok {
			continue // refusing is always a correct answer
		}
		if back := strconv.FormatUint(uint64(got), 10); back != in {
			t.Errorf("pathID(%q) accepted and returned %s: the request would act on a different row", in, back)
		}
	}
}

func TestPathIDRejectsWhatIsNotAnID(t *testing.T) {
	for _, in := range []string{"", "  ", "-1", "1.0", "0x2a", "1e3", "abc", "١٢"} {
		r := &http.Request{}
		r.SetPathValue("id", in)
		if _, ok := pathID(r, "id"); ok {
			t.Errorf("pathID(%q) was accepted", in)
		}
	}
}
