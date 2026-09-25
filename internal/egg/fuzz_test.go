package egg

import (
	"regexp"
	"testing"
)

// Eggs are pasted by admins or fetched from any URL. A malformed one must be
// refused with an error, never take the panel down.
func FuzzToTemplate(f *testing.F) {
	f.Add([]byte(`{"meta":{"version":"PTDL_v2"},"name":"Paper","docker_images":{"Java 21":"ghcr.io/pterodactyl/yolks:java_21"},"startup":"java -jar server.jar","config":{"files":"{}","startup":"{\"done\":\")! For help\"}","stop":"stop"},"variables":[{"name":"Version","env_variable":"MC_VERSION","default_value":"latest","user_editable":true,"rules":"required|string"}]}`))
	f.Add([]byte("meta:\n  version: PTDL_v2\nname: Valheim\nstartup: ./valheim_server.x86_64\n"))
	f.Add([]byte(`{}`))
	f.Add([]byte(`not an egg`))
	f.Fuzz(func(t *testing.T, data []byte) {
		_, _ = ToTemplate(data)
	})
}

func FuzzParse(f *testing.F) {
	f.Add([]byte(`{"slug":"minecraft-paper","name":"Minecraft (Paper)","images":[{"displayName":"Java 21","ref":"itzg/minecraft-server:java21","default":true}]}`))
	f.Add([]byte("slug: generic\nname: Generic\n"))
	f.Fuzz(func(t *testing.T, data []byte) {
		_, _ = Parse(data)
	})
}

var slugShape = regexp.MustCompile(`^([a-z0-9]+(-[a-z0-9]+)*)?$`)

// A slug ends up in Kubernetes names, which accept lowercase letters, digits
// and inner dashes only.
func FuzzSlugify(f *testing.F) {
	for _, s := range []string{"Minecraft (Paper)", "  Valheim — Vikings  ", "ÉTÉ", "---", ""} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		if got := Slugify(s); !slugShape.MatchString(got) {
			t.Fatalf("Slugify(%q) = %q", s, got)
		}
	})
}
