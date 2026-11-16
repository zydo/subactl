package api

// Termination codes. Every lease ends with exactly one.
const (
	CodeCompleted       = 200
	CodeReleased        = 204
	CodeWallExceeded    = 408
	CodeIdleReaped      = 410
	CodeOOMLeaseLimit   = 413
	CodeOutputExceeded  = 414
	CodeEscapeDetected  = 451
	CodeKilled          = 499
	CodeSubactlInternal = 500
	CodeParentDied      = 502
	CodeSafetyNet       = 503
	CodeReapIncomplete  = 504
	CodeExternalAbort   = 520
)

var codeNames = map[int]string{
	CodeCompleted:       "COMPLETED",
	CodeReleased:        "RELEASED",
	CodeWallExceeded:    "WALL_EXCEEDED",
	CodeIdleReaped:      "IDLE_REAPED",
	CodeOOMLeaseLimit:   "OOM_LEASE_LIMIT",
	CodeOutputExceeded:  "OUTPUT_EXCEEDED",
	CodeEscapeDetected:  "ESCAPE_DETECTED",
	CodeKilled:          "KILLED",
	CodeSubactlInternal: "SUBACTL_INTERNAL",
	CodeParentDied:      "PARENT_DIED",
	CodeSafetyNet:       "SAFETY_NET",
	CodeReapIncomplete:  "REAP_INCOMPLETE",
	CodeExternalAbort:   "EXTERNAL_ABORT",
}

// CodeName returns the name of a termination code, or "" if it is unknown.
func CodeName(code int) string { return codeNames[code] }

// Codes returns every defined termination code.
func Codes() []int {
	out := make([]int, 0, len(codeNames))
	for c := range codeNames {
		out = append(out, c)
	}
	return out
}
