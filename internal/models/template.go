package models

import (
	"encoding/json"
	"strconv"
	"strings"
	"time"
)

// ConsoleType describes how the control plane sends commands to a server.
type ConsoleType string

const (
	// ConsoleAttach uses the Kubernetes attach subresource (stdin of the main
	// container). This is the default, generic, sidecar-free mechanism.
	ConsoleAttach ConsoleType = "attach"
	// ConsoleRCON uses a game RCON protocol (opt-in, per template).
	ConsoleRCON ConsoleType = "rcon"
)

// Template is the Quetzal equivalent of a Pterodactyl "egg": a declarative
// description of how to run a given game/application. It is game-agnostic.
type Template struct {
	ID          uint   `gorm:"primaryKey" json:"id"`
	Slug        string `gorm:"uniqueIndex;size:190" json:"slug"`
	Name        string `json:"name"`
	Author      string `json:"author,omitempty"`
	Description string `json:"description,omitempty"`
	Category    string `json:"category,omitempty"`
	// Version is bumped on every change so servers can pin a template version.
	Version   int       `gorm:"default:1" json:"version"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`

	// Images are the selectable container images (egg "docker_images").
	Images []TemplateImage `gorm:"serializer:json" json:"images"`
	// Variables are the configurable inputs surfaced to the user.
	Variables []TemplateVariable `gorm:"serializer:json" json:"variables"`

	// Startup is the command run inside the container, with {{VARS}} substitution.
	Startup string `json:"startup"`
	// Done lists the console lines that mean the server has finished starting
	// (egg config.startup.done). As in Wings, each is text to find in an output
	// line, or a regular expression when prefixed "regex:". The server reads as
	// Starting until one appears; with none, it is Running once its container is.
	Done []string `gorm:"column:done_lines;serializer:json" json:"done,omitempty"`
	// WakeProtocol is how the wake-on-connect activator tells a player from a
	// port scanner: WakeMinecraft wakes only on a Minecraft Java login and
	// answers server-list pings itself, WakeAnyConnection wakes on any
	// connection. Empty picks WakeMinecraft for templates with the "eula" egg
	// feature (Minecraft Java servers and proxies), WakeAnyConnection otherwise.
	WakeProtocol string `json:"wakeProtocol,omitempty"`
	// DoneRegex is the single-line field Done replaced, still read from older
	// templates and exports. Despite its name, it was always matched as text.
	DoneRegex string `json:"doneRegex,omitempty"`
	// StopCommand is sent to stdin for a graceful stop (egg config.stop),
	// e.g. "stop" or "^C". Empty means SIGTERM.
	StopCommand string `json:"stopCommand,omitempty"`
	// StopGraceSeconds is the pod termination grace period (time the game has to
	// shut down after the stop command / SIGTERM before SIGKILL). 0 => default.
	StopGraceSeconds int `json:"stopGraceSeconds,omitempty"`

	// ConfigFiles are rendered/patched at startup by the entrypoint shim
	// (egg config.files).
	ConfigFiles []ConfigFile `gorm:"serializer:json" json:"configFiles,omitempty"`

	// Install is the optional install step (egg scripts.installation), run as an
	// initContainer/Job that populates the data volume.
	Install *InstallScript `gorm:"serializer:json" json:"install,omitempty"`

	// Features are panel-understood feature flags (egg "features"),
	// e.g. "eula", "java_version", "pid_limit".
	Features []string `gorm:"serializer:json" json:"features,omitempty"`
	// FileDenylist lists files the user may not view/edit (egg file_denylist).
	FileDenylist []string `gorm:"serializer:json" json:"fileDenylist,omitempty"`
	// ReinstallKeep lists what a clean reinstall keeps on this template's
	// servers -- the world, the player lists -- as paths relative to the data
	// volume, shell patterns allowed. A clean reinstall deletes everything
	// else, then runs the install: what updating a modpack takes, since its
	// install unpacks the new version over the old one and leaves behind the
	// mods and scripts the new one dropped. Empty takes the game's default,
	// see EffectiveReinstallKeep.
	ReinstallKeep []string `gorm:"serializer:json" json:"reinstallKeep,omitempty"`
	// EffectiveKeep is EffectiveReinstallKeep, computed for the panel (not
	// persisted), which offers it for a server's first clean reinstall.
	EffectiveKeep []string `gorm:"-" json:"effectiveReinstallKeep,omitempty"`

	// Console selects how commands are delivered to the server.
	Console ConsoleConfig `gorm:"serializer:json" json:"console"`

	// DataPath is where the persistent volume is mounted in the container.
	DataPath string `gorm:"default:/data" json:"dataPath"`
	// Ports declared by this game (game + query + voice, etc.).
	Ports []PortSpec `gorm:"serializer:json" json:"ports,omitempty"`
	// SuggestedPorts is a computed (not persisted) hint for the create form: ports
	// inferred from the template's port-like variables when the template declares
	// none (imported eggs allocate ports per server). See DetectPorts.
	SuggestedPorts []PortSpec `gorm:"-" json:"suggestedPorts,omitempty"`
	// AllocatedPort, computed like SuggestedPorts, says the game listens on the
	// port its server is given (UsesAllocation): the create form asks for it.
	AllocatedPort bool `gorm:"-" json:"allocatedPort,omitempty"`

	// SecurityContext defaults for the workload (overridable per server).
	SecurityContext SecurityContext `gorm:"serializer:json" json:"securityContext"`
}

// MarshalJSON writes a template's images and variables as lists, empty ones
// included. A template without variables -- a simple egg, `variables: []` --
// went out as "variables": null, and the panel's create form, which reads it
// as a list, crashed into a blank page for every account the moment that
// template came first.
func (t Template) MarshalJSON() ([]byte, error) {
	type plain Template // no methods: marshals without coming back here
	p := plain(t)
	if p.Images == nil {
		p.Images = []TemplateImage{}
	}
	if p.Variables == nil {
		p.Variables = []TemplateVariable{}
	}
	return json.Marshal(p)
}

// HasFeature reports whether the template declares the given egg feature flag
// (e.g. "eula").
func (t *Template) HasFeature(name string) bool {
	for _, f := range t.Features {
		if f == name {
			return true
		}
	}
	return false
}

// EffectiveReinstallKeep resolves what a clean reinstall keeps by default:
// the template's own list, else MinecraftJavaKeep for templates with the
// "eula" feature, which Minecraft Java eggs carry and others do not, else
// nothing.
func (t *Template) EffectiveReinstallKeep() []string {
	switch {
	case len(t.ReinstallKeep) > 0:
		return append([]string(nil), t.ReinstallKeep...)
	case t.HasFeature("eula"):
		return append([]string(nil), MinecraftJavaKeep...)
	default:
		return nil
	}
}

// Wake protocols (see Template.WakeProtocol).
const (
	WakeAnyConnection = "any"
	WakeMinecraft     = "minecraft"
)

// ValidWakeProtocol reports whether p is a wake protocol a template may name
// (empty meaning the default).
func ValidWakeProtocol(p string) bool {
	return p == "" || p == WakeAnyConnection || p == WakeMinecraft
}

// EffectiveWakeProtocol resolves the template's wake protocol, defaulting on
// the "eula" feature, which Minecraft Java eggs carry and others do not.
func (t *Template) EffectiveWakeProtocol() string {
	switch {
	case t.WakeProtocol != "":
		return t.WakeProtocol
	case t.HasFeature("eula"):
		return WakeMinecraft
	default:
		return WakeAnyConnection
	}
}

// DoneLines returns the lines that mean the server has finished starting,
// folding in the single-line field older templates carry.
func (t *Template) DoneLines() []string {
	var out []string
	for _, l := range t.Done {
		if l != "" {
			out = append(out, l)
		}
	}
	if len(out) == 0 && t.DoneRegex != "" {
		out = append(out, t.DoneRegex)
	}
	return out
}

// DetectPorts suggests the ports of a template that declares none: an imported
// egg, whose ports Pterodactyl allocates per server. They are its port-like
// variables (QUERY_PORT, RCON_PORT, TV_PORT…): a variable whose name ends in
// PORT and whose default is a valid port number. A blank/0/non-numeric default
// means the port is unset or disabled, and is skipped.
//
// Each is suggested on TCP and on UDP, as Wings exposes an allocation: an egg
// does not say which one a port speaks, and a game's port on TCP only is a game
// nobody can join.
//
// None is primary when the egg hands the game its allocation (UsesAllocation):
// the game's port is that one, which no variable holds. Taking a variable for
// it instead put Counter-Strike 2 on its SourceTV port, 27020, next to SourceTV
// itself, and on TCP only. An egg that does not use its allocation has its
// first port variable as primary.
// Returns nil when nothing matches.
func DetectPorts(t *Template) []PortSpec {
	primary := !t.UsesAllocation()
	var out []PortSpec
	seen := map[int32]bool{}
	for _, v := range t.Variables {
		name := strings.ToUpper(v.EnvVariable)
		// Wings globals Quetzal sets itself are not user-allocatable ports.
		if name == "SERVER_PORT" || name == "SERVER_IP" || name == "SERVER_MEMORY" {
			continue
		}
		if name != "PORT" && !strings.HasSuffix(name, "PORT") {
			continue
		}
		n, err := strconv.Atoi(strings.TrimSpace(v.Default))
		if err != nil || n < 1 || n > 65535 {
			continue
		}
		p := int32(n)
		if seen[p] {
			continue
		}
		seen[p] = true
		out = append(out,
			PortSpec{Name: portVarName(v.EnvVariable), Port: p, Protocol: "TCP", Primary: primary && len(out) == 0},
			PortSpec{Name: portVarName(v.EnvVariable), Port: p, Protocol: "UDP"},
		)
	}
	return out
}

// UsesAllocation reports whether an egg hands the game its Pterodactyl
// allocation: SERVER_PORT in its startup, or server.build.default.port in its
// config files. The game then listens on whatever port its server is given.
func (t *Template) UsesAllocation() bool {
	if strings.Contains(t.Startup, "SERVER_PORT") {
		return true
	}
	for _, f := range t.ConfigFiles {
		for _, v := range f.Find {
			if strings.Contains(v, "SERVER_PORT") || strings.Contains(v, "server.build.default.port") {
				return true
			}
		}
	}
	return false
}

// portVarName derives a short, DNS-friendly Service port name from a port
// variable's env name (QUERY_PORT -> "query", STEAM_PORT -> "steam").
func portVarName(env string) string {
	n := strings.ToLower(env)
	n = strings.TrimSuffix(n, "_port")
	n = strings.TrimSuffix(n, "port")
	n = strings.Trim(n, "_-")
	n = strings.ReplaceAll(n, "_", "-")
	if n == "" {
		n = "game"
	}
	if len(n) > 15 {
		n = n[:15]
	}
	return n
}

// TemplateImage is one selectable container image for a template.
type TemplateImage struct {
	DisplayName string `json:"displayName"`
	Ref         string `json:"ref"`
	Default     bool   `json:"default,omitempty"`
}

// VariableType constrains how a variable is validated/rendered.
type VariableType string

const (
	VarString VariableType = "string"
	VarInt    VariableType = "int"
	VarBool   VariableType = "bool"
	VarEnum   VariableType = "enum"
)

// TemplateVariable maps to an egg variable.
type TemplateVariable struct {
	Name        string       `json:"name"`
	Description string       `json:"description,omitempty"`
	EnvVariable string       `json:"envVariable"`
	Type        VariableType `json:"type"`
	Default     string       `json:"default,omitempty"`
	// Rules is the raw validation expression (Pterodactyl/Laravel style),
	// kept for fidelity; Quetzal interprets the common subset.
	Rules    string   `json:"rules,omitempty"`
	Required bool     `json:"required,omitempty"`
	Options  []string `json:"options,omitempty"` // for enum
	Viewable bool     `json:"viewable"`
	Editable bool     `json:"editable"`
	// Secret marks the value as sensitive: it is stored encrypted, materialized
	// into a Kubernetes Secret, and never returned by the API.
	Secret bool `json:"secret,omitempty"`
}

// ConfigFileParser enumerates the supported file parsers (egg config.files).
type ConfigFileParser string

const (
	ParserFile       ConfigFileParser = "file"
	ParserYAML       ConfigFileParser = "yaml"
	ParserProperties ConfigFileParser = "properties"
	ParserINI        ConfigFileParser = "ini"
	ParserJSON       ConfigFileParser = "json"
	ParserXML        ConfigFileParser = "xml"
)

// ConfigFile describes a config file to render/patch at startup.
type ConfigFile struct {
	Path   string            `json:"path"`
	Parser ConfigFileParser  `json:"parser"`
	Find   map[string]string `json:"find"` // key -> value (with {{env.VAR}} support)
}

// InstallScript describes the one-shot install step.
type InstallScript struct {
	Image      string `json:"image"`
	Entrypoint string `json:"entrypoint,omitempty"`
	Script     string `json:"script"`
}

// ConsoleConfig selects the console mechanism for a template.
type ConsoleConfig struct {
	Type ConsoleType `json:"type"`
	// RCON settings (only when Type == rcon).
	RCONPortEnv     string `json:"rconPortEnv,omitempty"`
	RCONPasswordEnv string `json:"rconPasswordEnv,omitempty"`
}

// PortSpec is a network port a server exposes.
type PortSpec struct {
	Name     string `json:"name"`
	Port     int32  `json:"port"`
	Protocol string `json:"protocol"` // TCP | UDP
	// Primary marks the main game port (the one players connect to).
	Primary bool `json:"primary,omitempty"`
	// NodePort is the node port allocated from the control-plane pool when the
	// server is exposed via NodePort (0 = none / not applicable). It is assigned
	// per server, so it lives on the server's port copy, not the template's.
	NodePort int32 `json:"nodePort,omitempty"`
}

// SecurityContext holds the subset of pod/container security settings Quetzal manages.
type SecurityContext struct {
	RunAsUser    *int64 `json:"runAsUser,omitempty"`
	RunAsGroup   *int64 `json:"runAsGroup,omitempty"`
	FSGroup      *int64 `json:"fsGroup,omitempty"`
	RunAsNonRoot *bool  `json:"runAsNonRoot,omitempty"`
}

// TemplateRevision is an earlier version of a template, kept while a server
// still runs it. A running server keeps the template it started with: an
// edited or re-imported template used to restart every running server that
// used it at once, players and all, where Pterodactyl's panel applies an egg's
// changes at a server's next start. Data is the template as it was, in JSON.
type TemplateRevision struct {
	ID         uint   `gorm:"primaryKey"`
	TemplateID uint   `gorm:"uniqueIndex:idx_template_revision"`
	Version    int    `gorm:"uniqueIndex:idx_template_revision"`
	Data       string `gorm:"type:text"`
}
