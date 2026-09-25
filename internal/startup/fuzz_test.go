package startup

import "testing"

// Done lines come from eggs; output lines from the game. Neither may break the
// watcher.
func FuzzMatch(f *testing.F) {
	f.Add(")! For help", "[12:04:21 INFO]: Done (9.412s)! For help, type \"help\"")
	f.Add("regex:^Done \\([0-9.]+s\\)", "\x1b[32mDone (1.0s)\x1b[0m\r\n")
	f.Add("regex:(", "anything")
	f.Fuzz(func(t *testing.T, done, line string) {
		ms, _ := Compile([]string{done})
		_ = Match(ms, []byte(line))
	})
}
