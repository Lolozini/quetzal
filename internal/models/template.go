package models

import "time"

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
	// DoneRegex marks the line indicating the server finished starting
	// (egg config.startup.done).
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

	// Console selects how commands are delivered to the server.
	Console ConsoleConfig `gorm:"serializer:json" json:"console"`

	// DataPath is where the persistent volume is mounted in the container.
	DataPath string `gorm:"default:/data" json:"dataPath"`
	// Ports declared by this game (game + query + voice, etc.).
	Ports []PortSpec `gorm:"serializer:json" json:"ports,omitempty"`

	// SecurityContext defaults for the workload (overridable per server).
	SecurityContext SecurityContext `gorm:"serializer:json" json:"securityContext"`
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
}

// SecurityContext holds the subset of pod/container security settings Quetzal manages.
type SecurityContext struct {
	RunAsUser    *int64 `json:"runAsUser,omitempty"`
	RunAsGroup   *int64 `json:"runAsGroup,omitempty"`
	FSGroup      *int64 `json:"fsGroup,omitempty"`
	RunAsNonRoot *bool  `json:"runAsNonRoot,omitempty"`
}
