// Package provider reads provider files. A provider file describes one host
// as data: how its agent's shell is detected, where its session id lives, the
// doors that wake its sessions and how its hook events are spelled. No Go
// code names a host; every host fact is in its file.
package provider

import (
	"regexp"
	"time"
)

// Transport is how a door delivers a nudge.
type Transport string

// The four transports. An http door runs in the server; the others run in the
// client's watcher.
const (
	TransportHTTP   Transport = "http"
	TransportExec   Transport = "exec"
	TransportStdout Transport = "stdout"
	TransportExit   Transport = "exit"
)

// Evidence says where a file's golden payloads come from.
type Evidence string

// The evidence levels.
const (
	EvidenceMeasured   Evidence = "measured"
	EvidenceDocumented Evidence = "documented"
	EvidenceUnverified Evidence = "unverified"
)

// Stream is the standard stream a hook output is written to.
type Stream string

// The output streams.
const (
	StreamStdout Stream = "stdout"
	StreamStderr Stream = "stderr"
)

// Provider is one decoded provider file. Load fills the defaults the schema
// names, so a zero field means the file left it out and the key has none.
type Provider struct {
	Hooks    Hooks `yaml:"hooks"`
	set      map[string]bool
	Name     string   `yaml:"name"`
	Extends  string   `yaml:"extends"`
	Evidence Evidence `yaml:"evidence"`
	Notes    string   `yaml:"notes"`
	Session  Session  `yaml:"session"`
	Detect   Detect   `yaml:"detect"`
	Doors    []Door   `yaml:"doors"`
}

// Detect identifies the host from the agent's shell environment.
type Detect struct {
	Env []string `yaml:"env"`
}

// Session says where the host keeps its session id.
type Session struct {
	Record        *Record  `yaml:"record"`
	Env           string   `yaml:"env"`
	Stdin         string   `yaml:"stdin"`
	Process       []string `yaml:"process"`
	ProcessShared []string `yaml:"process_shared"`
}

// Record locates a session's own record in the host's state directory, read
// when a hook's stdin session differs from its environment's.
type Record struct {
	tmpl          *compiled
	DirEnv        string   `yaml:"dir_env"`
	DirDefault    string   `yaml:"dir_default"`
	Glob          string   `yaml:"glob"`
	Parent        string   `yaml:"parent"`
	Reason        string   `yaml:"reason"`
	SameAgentNote string   `yaml:"same_agent_note"`
	SameAgent     []string `yaml:"same_agent"`
}

// Door is one layer 1 or layer 2 wake. Which keys beyond the common ones a
// door may carry depends on its transport (doorKeys).
type Door struct {
	tmpl         *doorTemplates
	Headers      map[string]string `yaml:"headers"`
	Match        Match             `yaml:"match"`
	Name         string            `yaml:"name"`
	Transport    Transport         `yaml:"transport"`
	Start        string            `yaml:"start"`
	Asset        string            `yaml:"asset"`
	Notes        string            `yaml:"notes"`
	URL          string            `yaml:"url"`
	Body         string            `yaml:"body"`
	Line         string            `yaml:"line"`
	Idle         string            `yaml:"idle"`
	End          string            `yaml:"end"`
	Gone         []int             `yaml:"gone"`
	Argv         []string          `yaml:"argv"`
	Layer        int               `yaml:"layer"`
	DedupeWindow time.Duration     `yaml:"dedupe_window"`
	Hold         time.Duration     `yaml:"hold"`
	EndExit      int               `yaml:"end_exit"`
}

// Match picks the identities an http door serves: those of Provider (the
// file's own name when left out) and, when Host is set, that host label.
type Match struct {
	Provider string `yaml:"provider"`
	Host     string `yaml:"host"`
}

// Hooks holds the hook roles a host offers. A role the file leaves out is nil.
type Hooks struct {
	SessionStart *SessionStartRole `yaml:"session_start"`
	Prompt       *PromptRole       `yaml:"prompt"`
	Stop         *StopRole         `yaml:"stop"`
	TurnEnd      *LifecycleRole    `yaml:"turn_end"`
	SessionEnd   *LifecycleRole    `yaml:"session_end"`
	SubagentStop *LifecycleRole    `yaml:"subagent_stop"`
}

// Role holds the keys every hook role takes.
type Role struct {
	// When lets the role act only when each named stdin field holds one of
	// its listed values.
	When  map[string][]string `yaml:"when"`
	Event string              `yaml:"event"`
	// Budget is the request budget, set only when under the 3 s default.
	Budget time.Duration `yaml:"budget"`
	// Once suppresses the role's output after its first print for a session.
	Once bool `yaml:"once"`
}

// Stdin names the hook stdin paths grelot reads at every role.
type Stdin struct {
	AgentID string `yaml:"agent_id"`
}

// PromptStdin adds the prompt text's path, read only at the prompt role.
type PromptStdin struct {
	AgentID string `yaml:"agent_id"`
	Prompt  string `yaml:"prompt"`
}

// Output is how the host takes one output kind.
type Output struct {
	tmpl   *compiled
	Text   string `yaml:"text"`
	Stream Stream `yaml:"stream"`
	Exit   int    `yaml:"exit"`
}

// SessionStartRole is the session_start role.
type SessionStartRole struct {
	Source  *Source      `yaml:"source"`
	Stdin   Stdin        `yaml:"stdin"`
	Outputs StartOutputs `yaml:"outputs"`
	Role    `yaml:",inline"`
}

// Source names the stdin path of the start source and which of its values
// mean a take-over or a fork.
type Source struct {
	Path     string   `yaml:"path"`
	TakeOver []string `yaml:"take_over"`
	Fork     []string `yaml:"fork"`
}

// StartOutputs are the output kinds session_start can print.
type StartOutputs struct {
	Context *Output `yaml:"context"`
}

// PromptRole is the prompt role, at turn start.
type PromptRole struct {
	signal  *regexp.Regexp
	Outputs PromptOutputs `yaml:"outputs"`
	Stdin   PromptStdin   `yaml:"stdin"`
	// Signal is an RE2 pattern over the whole prompt whose group "line"
	// captures a signal line delivered inside a host's own wrapper.
	Signal string `yaml:"signal"`
	Role   `yaml:",inline"`
}

// SignalPattern is Signal compiled at load, or nil when the file sets none.
func (r *PromptRole) SignalPattern() *regexp.Regexp { return r.signal }

// PromptOutputs are the output kinds the prompt role can print.
type PromptOutputs struct {
	Context *Output `yaml:"context"`
	Block   *Output `yaml:"block"`
}

// StopRole is the stop role, at turn end.
type StopRole struct {
	Outputs StopOutputs `yaml:"outputs"`
	Stdin   Stdin       `yaml:"stdin"`
	Role    `yaml:",inline"`
	// OncePerTurn says the host runs its stop hooks at most once per turn, so
	// the hold closes the turn whether or not it blocks.
	OncePerTurn bool `yaml:"once_per_turn"`
}

// StopOutputs are the output kinds the stop role can print.
type StopOutputs struct {
	Block  *Output `yaml:"block"`
	Notice *Output `yaml:"notice"`
}

// LifecycleRole is turn_end, session_end or subagent_stop: each reports to
// the server and prints nothing.
type LifecycleRole struct {
	Stdin Stdin `yaml:"stdin"`
	Role  `yaml:",inline"`
}

// Has reports whether the file set the top-level key itself, which extends
// needs to tell an inherited key from one the file replaces.
func (p *Provider) Has(key string) bool { return p.set[key] }
