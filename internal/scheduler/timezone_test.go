package scheduler

import (
	"testing"
	"time"
)

// A cron was read in whatever zone the control plane ran in — UTC in a
// container — so "0 4 * * *" fired at 4am UTC, which is 6am in Paris in summer.
// Nothing in the panel said so, and the only lever was a panel-wide TZ variable.
func TestNextRunInHonoursTheZone(t *testing.T) {
	// Mid-July: Paris is UTC+2.
	after := time.Date(2026, 7, 15, 1, 0, 0, 0, time.UTC)

	paris, err := NextRunIn("0 4 * * *", after, "Europe/Paris")
	if err != nil {
		t.Fatal(err)
	}
	utc, err := NextRunIn("0 4 * * *", after, "UTC")
	if err != nil {
		t.Fatal(err)
	}
	if !utc.Equal(time.Date(2026, 7, 15, 4, 0, 0, 0, time.UTC)) {
		t.Errorf("UTC next run = %s, want 04:00Z", utc)
	}
	// 4am in Paris is 2am UTC: earlier in absolute terms than 4am UTC.
	if !paris.Equal(time.Date(2026, 7, 15, 2, 0, 0, 0, time.UTC)) {
		t.Errorf("Paris next run = %s, want 02:00Z (04:00 local)", paris.UTC())
	}
	if !paris.Before(utc) {
		t.Error("the two zones produced the same instant; the zone is being ignored")
	}
}

// The zone is part of what a user gets wrong, so it has to be refused at the
// door rather than silently falling back to UTC.
func TestUnknownZoneIsRefused(t *testing.T) {
	if _, err := LoadZone("Mars/Olympus_Mons"); err == nil {
		t.Error("an unknown zone should be an error")
	}
	if _, err := NextRunIn("0 4 * * *", time.Now(), "not a zone"); err == nil {
		t.Error("NextRunIn should refuse an unknown zone")
	}
	// Empty keeps the process zone, which is the pre-existing behaviour.
	loc, err := LoadZone("")
	if err != nil || loc != time.Local {
		t.Errorf("empty zone = %v (%v), want time.Local", loc, err)
	}
}

// The zone database has to be in the binary: the runtime image is distroless
// and carries no /usr/share/zoneinfo, so this is what would break in production
// while passing on any developer machine.
func TestZoneDatabaseIsAvailable(t *testing.T) {
	for _, tz := range []string{"Europe/Paris", "America/New_York", "Asia/Tokyo", "Australia/Sydney"} {
		if _, err := LoadZone(tz); err != nil {
			t.Errorf("%s: %v — is _ \"time/tzdata\" still imported by the commands?", tz, err)
		}
	}
}

// Daylight saving is where cron implementations go wrong, and a schedule that
// silently stops or fires twice is worse than one at the wrong hour. Paris
// springs forward on 2026-03-29, so 02:30 local does not exist that day.
func TestSpringForwardSkipsTheMissingHour(t *testing.T) {
	// 00:00Z on the 28th is 01:00 local, before that day's 02:30.
	start := time.Date(2026, 3, 28, 0, 0, 0, 0, time.UTC)
	first, err := NextRunIn("30 2 * * *", start, "Europe/Paris")
	if err != nil {
		t.Fatal(err)
	}
	// Still winter time (UTC+1): 02:30 local is 01:30Z.
	if !first.Equal(time.Date(2026, 3, 28, 1, 30, 0, 0, time.UTC)) {
		t.Fatalf("first run = %s, want 01:30Z (02:30 local)", first.UTC())
	}

	// The 29th has no 02:30 local, so the run skips to the 30th — by then summer
	// time (UTC+2), which puts 02:30 local at 00:30Z. What must not happen is a
	// run on the 29th or a run that goes backwards.
	second, err := NextRunIn("30 2 * * *", first, "Europe/Paris")
	if err != nil {
		t.Fatal(err)
	}
	if !second.Equal(time.Date(2026, 3, 30, 0, 30, 0, 0, time.UTC)) {
		t.Errorf("second run = %s, want 2026-03-30 00:30Z (02:30 local, past the skipped hour)", second.UTC())
	}
	if !second.After(first) {
		t.Errorf("next run %s is not after %s", second.UTC(), first.UTC())
	}
	// And it really is 02:30 in the reader's own terms, both times.
	paris, _ := LoadZone("Europe/Paris")
	for _, got := range []time.Time{first, second} {
		local := got.In(paris)
		if local.Hour() != 2 || local.Minute() != 30 {
			t.Errorf("%s is %02d:%02d local, want 02:30", got.UTC(), local.Hour(), local.Minute())
		}
	}
}
