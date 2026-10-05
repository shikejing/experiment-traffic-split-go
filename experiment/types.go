// Package experiment provides reusable experiment targeting, stable traffic
// splitting, mutually exclusive experiment layers, hot-reloadable and
// versioned configuration with gradual rollout, and asynchronous
// assignment-event recording.
package experiment

// User is the immutable per-request context used for assignment.
type User struct {
	ID         string           // stable user identifier (the bucketing key)
	Attributes map[string]Value // targeting attributes; may be nil
}

// Value is a raw targeting attribute value.
type Value = any

// Assignment is the result of evaluating one experiment for a user.
type Assignment struct {
	Experiment   string // experiment key
	Variant      string // variant key; empty when the user is not in experiment
	InExperiment bool
	Bucket       uint32 // bucket value in [0, Scale)
	Reason       string // machine-readable explanation
	ConfigFrom   string // version whose variant definition was used
}

// Decision is the full evaluation result across a configuration snapshot.
type Decision struct {
	Assignments map[string]*Assignment // keyed by experiment key
	ConfigFrom  string                 // effective config version
}

// Reason codes returned in Assignment.Reason.
const (
	ReasonAssigned          = "assigned"
	ReasonNotInExperiment   = "not_in_experiment"
	ReasonTargetingMismatch = "targeting_mismatch"
	ReasonExperimentOff     = "experiment_off"
	ReasonLayerExcluded     = "layer_excluded"
	ReasonNoVariants        = "no_variants"
	ReasonInvalidWeight     = "invalid_weight"
)
