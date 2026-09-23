package reconciler

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// The startup command and the STARTUP variable used to understand only plain
// {{NAME}}. Wings' dotted forms were passed through untouched, so the game got
// the literal text "{{server.build.env.X}}" as an argument. None of the eggs
// seeded today uses one in its startup -- they are a config.files idiom, and
// config.files already translated them -- but the two paths had no reason to
// differ, and now share one table.
func TestStartupTranslatesWingsPlaceholders(t *testing.T) {
	s, tmpl := testServerAndTemplate()
	tmpl.Startup = "java -Xmx{{server.build.memory}}M -jar {{server.build.env.SERVER_JARFILE}} " +
		"--port {{server.build.default.port}} --host {{server.build.default.ip}} " +
		"--motd {{env.MOTD}} --name {{ SERVER_NAME }} --keep {{not.a.known.thing}} {{bad name}}"

	want := "java -Xmx${SERVER_MEMORY}M -jar ${SERVER_JARFILE} " +
		"--port ${SERVER_PORT} --host 0.0.0.0 " +
		"--motd ${MOTD} --name ${SERVER_NAME} --keep {{not.a.known.thing}} {{bad name}}"

	cmd := startupCommand(tmpl)
	if len(cmd) == 0 || cmd[len(cmd)-1] != want {
		t.Errorf("startup command = %q\nwant              %q", cmd, want)
	}
	var startup string
	for _, e := range wingsEnv(s, tmpl) {
		if e.Name == "STARTUP" {
			startup = e.Value
		}
	}
	if startup != want {
		t.Errorf("STARTUP = %q\nwant      %q", startup, want)
	}
}

// What the translation produces has to run: the shell expands it against the
// environment the container actually gets.
func TestTranslatedStartupExpandsInAShell(t *testing.T) {
	_, tmpl := testServerAndTemplate()
	tmpl.Startup = `printf '%s|%s|%s' {{server.build.env.SERVER_JARFILE}} {{server.build.default.port}} {{server.build.memory}}`
	cmd := startupCommand(tmpl)
	sh := exec.Command(cmd[0], cmd[1:]...)
	sh.Env = append(os.Environ(), "SERVER_JARFILE=server.jar", "SERVER_PORT=25565", "SERVER_MEMORY=1024")
	out, err := sh.CombinedOutput()
	if err != nil {
		t.Fatalf("startup did not run: %v\n%s", err, out)
	}
	if got := strings.TrimSpace(string(out)); got != "server.jar|25565|1024" {
		t.Errorf("expanded to %q", got)
	}
}

// config.files keeps its literal port (it is rendered into a file, where no
// shell will expand it later) and gains server.build.memory.
func TestConfigFilesPlaceholders(t *testing.T) {
	cases := map[string]string{
		"{{server.build.default.port}}":   "25565",
		"{{server.build.env.DIFFICULTY}}": "${DIFFICULTY}",
		"{{server.build.memory}}":         "${SERVER_MEMORY}",
		"{{config.docker.interface}}":     "0.0.0.0",
		// Pelican eggs write the path Wings actually resolves.
		"{{server.allocations.default.port}}": "25565",
		"{{server.allocations.default.ip}}":   "0.0.0.0",
		"{{server.build.memory_limit}}":       "${SERVER_MEMORY}",
		"{{server.build.env.bad name}}":       "{{server.build.env.bad name}}",
	}
	for in, want := range cases {
		if got := toShellTemplate(in, 25565); got != want {
			t.Errorf("%s -> %q, want %q", in, got, want)
		}
	}
}
