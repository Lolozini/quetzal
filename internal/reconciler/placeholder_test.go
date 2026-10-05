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
		"--motd {{env.MOTD}} --name {{ SERVER_NAME }} --level {{server.environment.LEVEL}} " +
		"--keep {{not.a.known.thing}} {{bad name}}"

	want := "java -Xmx${SERVER_MEMORY}M -jar ${SERVER_JARFILE} " +
		"--port ${SERVER_PORT} --host 0.0.0.0 " +
		"--motd ${MOTD} --name ${SERVER_NAME} --level ${LEVEL} " +
		"--keep {{not.a.known.thing}} {{bad name}}"

	cmd := startupCommand(s, tmpl)
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
	s, tmpl := testServerAndTemplate()
	tmpl.Startup = `printf '%s|%s|%s' {{server.build.env.SERVER_JARFILE}} {{server.build.default.port}} {{server.build.memory}}`
	cmd := startupCommand(s, tmpl)
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
		// ... and reach their variables under environment.
		"{{server.environment.MAX_SLOTS}}":   "${MAX_SLOTS}",
		"{{ server.environment.LEVEL }}":     "${LEVEL}",
		"{{server.environment.bad name}}":    "{{server.environment.bad name}}",
		"{{server.environment.}}":            "{{server.environment.}}",
		"slots={{server.environment.SLOTS}}": "slots=${SLOTS}",
	}
	for in, want := range cases {
		if got := toShellTemplate(in, 25565); got != want {
			t.Errorf("%s -> %q, want %q", in, got, want)
		}
	}
}

// Every placeholder form the published egg repositories use in config.files,
// with how many times each appeared across pelican-eggs in September 2026.
// {{server.environment.X}} was missing: 327 uses in 51 eggs (Rust, Factorio,
// ARK, DayZ, Satisfactory...) reached the game as literal text, and Rust
// failed to load "{{server.environment.LEVEL}}" while Factorio's
// server-settings.json no longer parsed.
func TestEveryPublishedPlaceholderFormIsTranslated(t *testing.T) {
	forms := []string{
		"{{server.build.env.MAX_PLAYERS}}",    // 955
		"{{server.environment.MAX_PLAYERS}}",  // 327
		"{{server.build.default.port}}",       // 248
		"{{env.MAX_PLAYERS}}",                 // 196
		"{{server.allocations.default.port}}", // 74
		"{{config.docker.interface}}",         // 10
		"{{server.build.default.ip}}",         // 4
		"{{server.allocations.default.ip}}",   // 2
	}
	for _, f := range forms {
		if got := toShellTemplate(f, 25565); strings.Contains(got, "{{") {
			t.Errorf("%s is left as %q", f, got)
		}
	}
}

// A server given a startup command of its own runs it, in the container's
// command and in STARTUP alike, with its placeholders filled in as the
// template's are. One without runs its template's. TeamSpeak on MariaDB needs
// four arguments its egg has no variable for, and the template had to be
// copied for that one server.
func TestAServerRunsItsOwnStartup(t *testing.T) {
	s, tmpl := testServerAndTemplate()
	startup := func() (cmd, env string) {
		c := startupCommand(s, tmpl)
		if len(c) > 0 {
			cmd = c[len(c)-1]
		}
		for _, e := range wingsEnv(s, tmpl) {
			if e.Name == "STARTUP" {
				env = e.Value
			}
		}
		return cmd, env
	}
	if cmd, env := startup(); cmd != "echo ${MSG}; sleep 1" || env != cmd {
		t.Errorf("without a startup of its own: command %q, STARTUP %q, want the template's", cmd, env)
	}

	s.Startup = "./ts3server default_voice_port={{SERVER_PORT}} dbplugin=ts3db_mariadb"
	if cmd, env := startup(); cmd != "./ts3server default_voice_port=${SERVER_PORT} dbplugin=ts3db_mariadb" || env != cmd {
		t.Errorf("with one: command %q, STARTUP %q, want the server's", cmd, env)
	}
	if got := BuildDeployment(s, tmpl, "", nil).Spec.Template.Spec.Containers[0].Command; !strings.Contains(strings.Join(got, " "), "dbplugin=ts3db_mariadb") {
		t.Errorf("the game container runs %q, not the server's startup", got)
	}

	// A template that leaves the start to its image's entrypoint still runs a
	// command the server was given.
	tmpl.Startup = ""
	if cmd, _ := startup(); !strings.Contains(cmd, "dbplugin") {
		t.Errorf("on an entrypoint-driven template, the server's startup was dropped: %q", cmd)
	}
	s.Startup = "  "
	if c := startupCommand(s, tmpl); c != nil {
		t.Errorf("a blank startup of its own ran %q instead of the image's entrypoint", c)
	}
}
