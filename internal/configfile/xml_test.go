package configfile

import (
	"strings"
	"testing"

	"github.com/beevik/etree"
)

func xmlDoc(t *testing.T, s string) *etree.Document {
	t.Helper()
	d := etree.NewDocument()
	if err := d.ReadFromString(strings.TrimPrefix(s, string(utf8BOM))); err != nil {
		t.Fatalf("output is not XML: %v\n%s", err, s)
	}
	return d
}

func textAt(d *etree.Document, path string) string {
	if e := d.FindElement(path); e != nil {
		return e.Text()
	}
	return "<missing>"
}

// The Trackmania 2020 egg's dedicated_cfg.xml: nested keys, an existing value
// replaced, a missing one created, comments and other settings kept.
func TestXMLPatchesNestedElements(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "dedicated_cfg.xml", `<?xml version="1.0" encoding="utf-8" ?>
<dedicated>
	<!-- server identity -->
	<server_options>
		<name>Old name</name>
		<max_players>32</max_players>
		<keep>untouched</keep>
	</server_options>
	<system_config>
		<server_port>2350</server_port>
	</system_config>
</dedicated>
`)
	err := Render(dir, []Spec{{Path: "dedicated_cfg.xml", Parser: "xml", Find: map[string]string{
		"dedicated.server_options.name":        "${SERVER_NAME}",
		"dedicated.server_options.password":    "s3cr&t<",
		"dedicated.system_config.server_port":  "27015",
		"dedicated.system_config.xmlrpc_port":  "5000",
		"dedicated.masterserver_account.login": "me",
	}}}, env(map[string]string{"SERVER_NAME": "My <Server>"}))
	if err != nil {
		t.Fatal(err)
	}
	out := read(t, dir, "dedicated_cfg.xml")
	d := xmlDoc(t, out)
	for path, want := range map[string]string{
		"./dedicated/server_options/name":        "My <Server>",
		"./dedicated/server_options/password":    "s3cr&t<",
		"./dedicated/server_options/max_players": "32",
		"./dedicated/server_options/keep":        "untouched",
		"./dedicated/system_config/server_port":  "27015",
		"./dedicated/system_config/xmlrpc_port":  "5000",
		"./dedicated/masterserver_account/login": "me",
	} {
		if got := textAt(d, path); got != want {
			t.Errorf("%s = %q, want %q", path, got, want)
		}
	}
	if !strings.Contains(out, "<!-- server identity -->") || !strings.HasPrefix(out, `<?xml version="1.0" encoding="utf-8" ?>`) {
		t.Errorf("comment or declaration lost:\n%s", out)
	}
	// Special characters are escaped, not injected as markup.
	if !strings.Contains(out, "My &lt;Server&gt;") {
		t.Errorf("value not escaped:\n%s", out)
	}
}

// A file that does not exist yet is created from the keys.
func TestXMLCreatesTheFile(t *testing.T) {
	dir := t.TempDir()
	err := Render(dir, []Spec{{Path: "Settings.xml", Parser: "xml", Find: map[string]string{"Settings.Port": "4499"}}}, env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if got := textAt(xmlDoc(t, read(t, dir, "Settings.xml")), "./Settings/Port"); got != "4499" {
		t.Errorf("Port = %q", got)
	}
}

// Space Engineers configs: a namespaced root, a UTF-8 BOM, an attribute set
// with Wings' [name='value'] form, and a wildcard that sets every match.
func TestXMLAttributesWildcardsAndBOM(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "SpaceEngineers-Dedicated.cfg", string(utf8BOM)+`<?xml version="1.0"?>
<MyConfigDedicated xmlns:xsd="http://www.w3.org/2001/XMLSchema" xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance">
  <SessionSettings>
    <GameMode>Creative</GameMode>
    <MaxPlayers>4</MaxPlayers>
  </SessionSettings>
  <Mods><Mod id="1">a</Mod><Mod id="2">b</Mod></Mods>
  <ServerPort>27016</ServerPort>
</MyConfigDedicated>`)
	err := Render(dir, []Spec{{Path: "SpaceEngineers-Dedicated.cfg", Parser: "xml", Find: map[string]string{
		"MyConfigDedicated.SessionSettings.GameMode": "Survival",
		"MyConfigDedicated.ServerPort":               "27020",
		"MyConfigDedicated.Mods.Mod":                 "[enabled='true']",
		"MyConfigDedicated.*.MaxPlayers":             "16",
	}}}, env(nil))
	if err != nil {
		t.Fatal(err)
	}
	out := read(t, dir, "SpaceEngineers-Dedicated.cfg")
	if !strings.HasPrefix(out, string(utf8BOM)) {
		t.Error("the byte order mark was dropped")
	}
	d := xmlDoc(t, out)
	if got := textAt(d, "./MyConfigDedicated/SessionSettings/GameMode"); got != "Survival" {
		t.Errorf("GameMode = %q", got)
	}
	if got := textAt(d, "./MyConfigDedicated/ServerPort"); got != "27020" {
		t.Errorf("ServerPort = %q", got)
	}
	if got := textAt(d, "./MyConfigDedicated/SessionSettings/MaxPlayers"); got != "16" {
		t.Errorf("wildcard MaxPlayers = %q", got)
	}
	mods := d.FindElements("./MyConfigDedicated/Mods/Mod")
	if len(mods) != 2 || mods[0].SelectAttrValue("enabled", "") != "true" || mods[0].Text() != "a" {
		t.Errorf("attribute not set on the matches: %s", out)
	}
	if d.Root().SelectAttrValue("xmlns:xsd", "") == "" {
		t.Errorf("namespace declaration lost:\n%s", out)
	}
}

// A key naming another root (an egg bug seen in the wild) changes nothing,
// rather than growing empty elements under the real root.
func TestXMLWrongRootChangesNothing(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "Sandbox.sbc", `<MyObjectBuilder_Checkpoint><Settings><GameMode>Creative</GameMode></Settings></MyObjectBuilder_Checkpoint>`)
	err := Render(dir, []Spec{{Path: "Sandbox.sbc", Parser: "xml", Find: map[string]string{
		"MyConfigDedicated.SessionSettings.GameMode": "Survival",
	}}}, env(nil))
	if err != nil {
		t.Fatal(err)
	}
	out := read(t, dir, "Sandbox.sbc")
	if strings.Contains(out, "SessionSettings") || textAt(xmlDoc(t, out), "./MyObjectBuilder_Checkpoint/Settings/GameMode") != "Creative" {
		t.Errorf("file changed:\n%s", out)
	}
}

// A file that is not XML is reported and left as it was.
func TestXMLInvalidFileIsLeftAlone(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "broken.xml", "<a><b></a>")
	err := Render(dir, []Spec{{Path: "broken.xml", Parser: "xml", Find: map[string]string{"a.b": "1"}}}, env(nil))
	if err == nil || !strings.Contains(err.Error(), "not valid XML") {
		t.Errorf("err = %v", err)
	}
	if got := read(t, dir, "broken.xml"); got != "<a><b></a>" {
		t.Errorf("file rewritten: %q", got)
	}
}
