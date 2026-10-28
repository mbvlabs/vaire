package cli

import (
	"sync"

	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/engine"
)

// jobEvidenceWarnings carries an evidence failure into the node command's final
// result as well as its event stream. Human nodeReporter output otherwise only
// shows log/progress events, and structured consumers may only read the result.
type jobEvidenceWarnings struct {
	mu   sync.Mutex
	lost bool
}

func (w *jobEvidenceWarnings) reporter(next engine.Reporter) engine.Reporter {
	return func(ev output.Event) {
		if ev.Type == "warning" && ev.Code == "activity_not_recorded" {
			w.mu.Lock()
			w.lost = true
			w.mu.Unlock()
		}
		if next != nil {
			next(ev)
		}
	}
}

func (w *jobEvidenceWarnings) warnings() []output.Warning {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.lost {
		return nil
	}
	// Never promote free-form event text into the diagnostic metadata/result.
	return []output.Warning{{Code: "activity_not_recorded", Message: "some local activity evidence could not be recorded; the job was not retried automatically"}}
}
