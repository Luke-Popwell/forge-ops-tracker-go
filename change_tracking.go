package forgeops

import (
	"os"
	"regexp"
	"runtime"
	"runtime/debug"
	"sort"
	"strings"
	"sync"
	"time"
)

// The kinds of change ForgeOps accepts. RecordChange sends any other kind as ChangeKindOther, since
// the server rejects a kind it doesn't know.
const (
	ChangeKindFeatureFlag    = "feature_flag"
	ChangeKindConfig         = "config"
	ChangeKindMigration      = "migration"
	ChangeKindDependency     = "dependency"
	ChangeKindInfrastructure = "infrastructure"
	ChangeKindOther          = "other"
)

const (
	maxChangeTitleLength       = 200
	maxSnapshotEntries         = 3000
	maxDependencyNameLength    = 200
	maxDependencyVersionLength = 100
)

var changeKinds = map[string]bool{
	ChangeKindFeatureFlag:    true,
	ChangeKindConfig:         true,
	ChangeKindMigration:      true,
	ChangeKindDependency:     true,
	ChangeKindInfrastructure: true,
	ChangeKindOther:          true,
}

// ChangeOptions holds RecordChange's optional fields; any left at its zero value is left out.
type ChangeOptions struct {
	// Details is a small JSON object of anything else worth knowing (the flag's new value, the
	// migration's name).
	Details map[string]any
	// Environment defaults to Configuration.Environment.
	Environment string
	// Service names the service that changed. This client has no service setting of its own, so
	// there is no default.
	Service string
	// Actor is who made the change: a person, a CI job, a script.
	Actor string
	// URL links to more (a pull request, a deploy log); http or https.
	URL string
	// ID is an idempotency key: recording the same ID twice records one change.
	ID string
	// OccurredAt defaults to now.
	OccurredAt time.Time
}

// RecordChange records one change you made that isn't a deploy (a feature flag flipped, a config
// value edited, a migration run) so it shows up next to your errors and performance data:
//
//	forgeops.RecordChange(forgeops.ChangeKindFeatureFlag, "Enabled new checkout", &forgeops.ChangeOptions{
//		Details: map[string]any{"flag": "new_checkout", "enabled": true},
//		Actor:   "luke",
//	})
//
// Delivered on the same background goroutine as errors, so it never blocks the caller, and never
// panics or returns an error. A kind other than the ChangeKind* constants is sent as "other", a
// title longer than 200 characters is shortened, and an empty title is dropped. A no-op when the
// client isn't enabled, exactly like CaptureError; a plan without change tracking rejects it
// server-side and it's dropped like any other failed delivery.
func RecordChange(kind, title string, options *ChangeOptions) {
	config, r := state()
	defer func() {
		if p := recover(); p != nil {
			config.Logger.Debugf("record change failed: %v", p)
		}
	}()

	if !config.IsEnabled() {
		return
	}
	payload := buildChangePayload(config, kind, title, options)
	if payload == nil {
		config.Logger.Debugf("dropped a change with an empty title")
		return
	}
	r.deliveryQueue.pushTo(r.deliveryQueue.client.DeliverChange, payload)
}

func buildChangePayload(config *Configuration, kind, title string, options *ChangeOptions) map[string]any {
	title = strings.TrimSpace(title)
	if title == "" {
		return nil
	}
	if !changeKinds[kind] {
		kind = ChangeKindOther
	}
	if options == nil {
		options = &ChangeOptions{}
	}
	occurredAt := options.OccurredAt
	if occurredAt.IsZero() {
		occurredAt = time.Now()
	}
	environment := options.Environment
	if environment == "" {
		environment = config.Environment
	}

	payload := map[string]any{
		"kind":        kind,
		"title":       truncateRunes(title, maxChangeTitleLength),
		"occurred_at": occurredAt.UTC().Format(time.RFC3339),
	}
	optional := map[string]string{
		"environment": environment,
		"service":     options.Service,
		"actor":       options.Actor,
		"url":         options.URL,
		"id":          options.ID,
	}
	for key, value := range optional {
		if value != "" {
			payload[key] = value
		}
	}
	if len(options.Details) > 0 {
		payload["details"] = options.Details
	}
	return payload
}

var (
	changeSnapshotMu   sync.Mutex
	changeSnapshotSent bool
)

// sendChangeSnapshotOnce queues the startup change snapshot the first time Init leaves the client
// enabled with DetectChanges on: once per process, however many times Init is called. Called with
// the configuration Init just finished applying. Building it happens on its own goroutine, so a
// slow build info read can never delay Init, and a panic there is swallowed.
func sendChangeSnapshotOnce(config *Configuration, r *Reporter) {
	if !config.DetectChanges || !config.IsEnabled() {
		return
	}
	changeSnapshotMu.Lock()
	defer changeSnapshotMu.Unlock()
	if changeSnapshotSent {
		return
	}
	changeSnapshotSent = true

	go func() {
		defer func() {
			if p := recover(); p != nil {
				config.Logger.Debugf("change snapshot failed: %v", p)
			}
		}()
		r.deliveryQueue.pushTo(r.deliveryQueue.client.DeliverChangeSnapshot, buildChangeSnapshot(config))
	}()
}

// buildChangeSnapshot is what this process can reliably say about itself: the Go version it was
// built with, the module versions compiled into it (from the binary's own build info, so exactly
// what's running, never a guess from a go.mod on disk), and, only when TrackEnvVarNames is on, the
// names of its environment variables. A key this can't determine is left out, which the server
// reads as unknown rather than as everything having been removed. No schema version: Go has no
// standard migration library to ask.
func buildChangeSnapshot(config *Configuration) map[string]any {
	state := map[string]any{
		"runtime": "go " + strings.TrimPrefix(runtime.Version(), "go"),
	}
	if dependencies, ok := buildDependencies(); ok {
		state["dependencies"] = dependencies
	}
	if config.TrackEnvVarNames {
		state["env_var_names"] = trackedEnvVarNames(os.Environ())
	}
	return map[string]any{"environment": config.Environment, "state": state}
}

// buildDependencies reads every module compiled into this binary, following a replace directive to
// the version actually used. A module with no version at all (a replace pointing at a local
// directory) is left out rather than sent as a made-up one.
func buildDependencies() (map[string]string, bool) {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return nil, false
	}
	dependencies := map[string]string{}
	for _, module := range info.Deps {
		if len(dependencies) >= maxSnapshotEntries {
			break
		}
		if module == nil {
			continue
		}
		version := module.Version
		if module.Replace != nil {
			version = module.Replace.Version
		}
		if module.Path == "" || version == "" {
			continue
		}
		dependencies[truncateRunes(module.Path, maxDependencyNameLength)] = truncateRunes(version, maxDependencyVersionLength)
	}
	return dependencies, true
}

// trackedEnvVarNames turns os.Environ()-style "NAME=value" entries into a sorted, deduplicated
// list of names, dropping the value immediately and every name isNoisyEnvVarName rejects.
func trackedEnvVarNames(environ []string) []string {
	seen := map[string]bool{}
	names := []string{}
	for _, entry := range environ {
		name, _, _ := strings.Cut(entry, "=")
		if name == "" || seen[name] || isNoisyEnvVarName(name) {
			continue
		}
		seen[name] = true
		names = append(names, name)
	}
	sort.Strings(names)
	if len(names) > maxSnapshotEntries {
		names = names[:maxSnapshotEntries]
	}
	return names
}

var noisyEnvVarNames = map[string]bool{
	"HOSTNAME": true, "HOST": true, "HOME": true, "PATH": true, "PWD": true, "OLDPWD": true,
	"SHLVL": true, "_": true, "TERM": true, "USER": true, "LOGNAME": true, "SHELL": true,
	"LANG": true, "TMPDIR": true, "TZ": true, "PORT": true, "DYNO": true, "INVOCATION_ID": true,
	"JOURNAL_STREAM": true,
}

var noisyEnvVarPrefixes = []string{"LC_", "SYSTEMD_", "MEMORY_PRESSURE_", "KUBERNETES_", "FORGE_OPS_"}

// Kubernetes' per-service variables (REDIS_SERVICE_HOST, REDIS_SERVICE_PORT_HTTP,
// REDIS_PORT_6379_TCP_ADDR) differ from pod to pod as services come and go.
var noisyEnvVarPattern = regexp.MustCompile(`_SERVICE_HOST$|_SERVICE_PORT|_PORT_.*_TCP`)

// isNoisyEnvVarName reports whether name is host-specific noise (set by the shell, the init
// system, the platform or this client itself) that would make every host in a fleet look like it
// changed. Compared uppercased, since Windows treats names case-insensitively.
func isNoisyEnvVarName(name string) bool {
	upper := strings.ToUpper(name)
	if noisyEnvVarNames[upper] {
		return true
	}
	for _, prefix := range noisyEnvVarPrefixes {
		if strings.HasPrefix(upper, prefix) {
			return true
		}
	}
	return noisyEnvVarPattern.MatchString(upper)
}

func truncateRunes(s string, limit int) string {
	runes := []rune(s)
	if len(runes) <= limit {
		return s
	}
	return string(runes[:limit])
}
