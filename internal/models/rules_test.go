package models

import "testing"

// The recette of 0.10.0 gave Paper's SERVER_JARFILE "foo bar; echo pwned" and
// its MINECRAFT_VERSION "not a version at all !!": both were accepted, and the
// server only failed at its next start. Pterodactyl checks a value against the
// egg's rules when it is given.
func TestVariableRules(t *testing.T) {
	cases := []struct {
		rules, value string
		ok           bool
	}{
		// Paper's.
		{`required|regex:/^([\w\d._-]+)(\.jar)$/`, "server.jar", true},
		{`required|regex:/^([\w\d._-]+)(\.jar)$/`, "foo bar; echo pwned", false},
		{`nullable|string|max:20`, "1.21.4", true},
		{`nullable|string|max:20`, "not a version at all !!", false},
		{`nullable|string|max:20`, "", true},
		{`required|string|max:20`, "", false},
		// Numbers.
		{`required|integer|between:1,100`, "50", true},
		{`required|integer|between:1,100`, "150", false},
		{`required|integer|between:1,100`, "five", false},
		{`required|numeric|min:0.5`, "0.25", false},
		{`required|digits_between:1,5`, "25565", true},
		{`required|digits_between:1,5`, "255650", false},
		{`required|digits:4`, "12a4", false},
		// Choices and the like.
		{`required|boolean`, "1", true},
		{`required|boolean`, "yes", false},
		{`required|in:survival,creative`, "creative", true},
		{`required|in:survival,creative`, "hardcore", false},
		{`required|alpha_dash`, "my-world_2", true},
		{`required|alpha_dash`, "my world", false},
		{`nullable|url`, "https://example.com/pack.zip", true},
		{`nullable|url`, "example.com", false},
		// A pipe inside a pattern, as Pelican's list form allows.
		{`required|regex:/^(latest|snapshot|\d+\.\d+(\.\d+)?)$/`, "snapshot", true},
		{`required|regex:/^(latest|snapshot|\d+\.\d+(\.\d+)?)$/`, "nightly", false},
		{`required|regex:/^paper$/i`, "PAPER", true},
		// What RE2 cannot read is let through rather than refused.
		{`required|regex:/^(?!admin).+$/`, "admin", true},
		{`required|sometimes|present`, "x", true},
	}
	for _, c := range cases {
		v := TemplateVariable{EnvVariable: "X", Rules: c.rules}
		if err := v.Validate(c.value); (err == nil) != c.ok {
			t.Errorf("rules %q, value %q: err = %v, want ok=%v", c.rules, c.value, err, c.ok)
		}
	}
}
