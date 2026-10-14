package engine

import (
	"context"
	"sync"

	"github.com/mbvlabs/eru/internal/config"
)

// Numbering numbers the steps of several Apply runs in one request as one
// sequence, so a command that runs the poolers, then a cluster's steps,
// then the backup steps prints [1/6] to [6/6] instead of starting over at
// [1/1], [1/3] and [1/2]. The caller counts the runs up front with
// NewNumbering and puts it on the context with WithNumbering; each Apply
// on that context takes the next numbers. A run that selects more steps
// than counted grows the total.
type Numbering struct {
	mu    sync.Mutex
	done  int
	total int
}

// NewNumbering counts the steps each of runs selects from catalog for cfg.
// A run that wouldn't prepare counts as none.
func NewNumbering(catalog []Step, cfg config.Cluster, runs ...Options) *Numbering {
	n := &Numbering{}
	for _, opts := range runs {
		if _, selected, err := prepare(catalog, cfg, opts); err == nil {
			n.total += len(selected)
		}
	}
	return n
}

// Total is the number of steps counted so far.
func (n *Numbering) Total() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.total
}

// span takes the next count numbers: the offset of the run's first step and
// the total to print.
func (n *Numbering) span(count int) (offset, total int) {
	n.mu.Lock()
	defer n.mu.Unlock()
	offset = n.done
	n.done += count
	if n.done > n.total {
		n.total = n.done
	}
	return offset, n.total
}

type numberingKey struct{}

// WithNumbering returns ctx carrying n for every Apply run on it.
func WithNumbering(ctx context.Context, n *Numbering) context.Context {
	return context.WithValue(ctx, numberingKey{}, n)
}

// numberingFrom is ctx's Numbering, or nil.
func numberingFrom(ctx context.Context) *Numbering {
	n, _ := ctx.Value(numberingKey{}).(*Numbering)
	return n
}
