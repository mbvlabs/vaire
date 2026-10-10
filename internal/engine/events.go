package engine

import "github.com/mbvlabs/eru/internal/cli/output"

// Reporter receives the events a run emits: step, log and warning. The node
// dispatcher redacts them, writes them to stdout and to the run log.
type Reporter func(output.Event)

func (r Reporter) emit(ev output.Event) {
	if r != nil {
		r(ev)
	}
}

func intPtr(v int) *int { return &v }
