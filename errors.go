package split

import "errors"

// Errors returned by the split package.
var (
	ErrConfigInvalid  = errors.New("split: invalid config")
	ErrExperimentGone = errors.New("split: experiment not found")
	ErrGroupInvalid   = errors.New("split: invalid mutex group")
	ErrVersionMissing = errors.New("split: config version not found")
	ErrPercentInvalid = errors.New("split: rollout percent out of range")
	ErrRecorderClosed = errors.New("split: event recorder closed")
)
