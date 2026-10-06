package models

import (
	"fmt"
	"path"
	"strings"
)

// MinecraftJavaKeep is what a clean reinstall keeps on a Minecraft Java server
// unless its template says otherwise: the worlds (world, plus world_nether and
// world_the_end on Bukkit-family servers), the server's settings and player
// lists, and its plugins, which an administrator adds and no pack ships.
// What a modpack ships -- mods, config, defaultconfigs, kubejs, libraries --
// is left out, so its next version goes in clean.
var MinecraftJavaKeep = []string{
	"world*",
	"plugins",
	"server.properties",
	"eula.txt",
	"ops.json",
	"whitelist.json",
	"banned-players.json",
	"banned-ips.json",
	"usercache.json",
	"usernamecache.json",
	"server-icon.png",
}

// Bounds on a list of paths to keep. They travel to the install step in the
// pod spec, one per line.
const (
	MaxKeepPaths   = 64
	MaxKeepPathLen = 255
)

// CleanKeepPaths checks and tidies a list of paths a clean reinstall keeps.
// Each is relative to the server's files and may use shell patterns (*, ?,
// [...]). Blank entries and repeats are dropped, and a leading "./" or a
// trailing "/" trimmed. A path that would leave the server's files, names them
// all, or spans lines -- the install step reads the list one path per line --
// is refused. A list with nothing left in it comes back nil.
func CleanKeepPaths(in []string) ([]string, error) {
	var out []string
	seen := map[string]bool{}
	for _, raw := range in {
		p := strings.TrimSpace(raw)
		if p == "" {
			continue
		}
		switch {
		case strings.ContainsAny(p, "\n\r\x00"):
			return nil, fmt.Errorf("%q: a path to keep goes on one line", raw)
		case len(p) > MaxKeepPathLen:
			return nil, fmt.Errorf("%q: a path to keep is at most %d characters", p, MaxKeepPathLen)
		case strings.HasPrefix(p, "/"):
			return nil, fmt.Errorf("%q: give the path from the server's files, without a leading /", p)
		}
		for _, seg := range strings.Split(p, "/") {
			if seg == ".." {
				return nil, fmt.Errorf("%q: a path to keep cannot leave the server's files", p)
			}
		}
		p = path.Clean(p)
		if p == "." {
			return nil, fmt.Errorf("%q names all the server's files; a reinstall that does not delete them keeps them all", raw)
		}
		if _, err := path.Match(p, ""); err != nil {
			return nil, fmt.Errorf("%q: malformed pattern", p)
		}
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	if len(out) > MaxKeepPaths {
		return nil, fmt.Errorf("at most %d paths to keep", MaxKeepPaths)
	}
	return out, nil
}
