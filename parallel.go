package frame

import (
	"context"
	"fmt"
	"strings"
	"sync"
)

// multiError holds one or more errors and formats them as a single message.
// It supports errors.Is/As unwrapping over the full list.
type multiError []error

func (e multiError) Error() string {
	if len(e) == 1 {
		return e[0].Error()
	}
	var b strings.Builder
	b.WriteString(fmt.Sprintf("%d errors occurred:", len(e)))
	for _, err := range e {
		b.WriteString("\n  - ")
		b.WriteString(err.Error())
	}
	return b.String()
}

func (e multiError) Unwrap() []error { return []error(e) }

// parallelRun launches a set of functions concurrently and collects all errors.
// An optional cancel function is called on the first error so that sibling
// goroutines can observe context cancellation and exit early.
type parallelRun struct {
	mu     sync.Mutex
	wg     sync.WaitGroup
	errs   multiError
	cancel context.CancelFunc // may be nil
	once   sync.Once
}

// do starts f(ctx) in a new goroutine tagged with ident for error messages.
// Panics inside f are recovered and converted to errors.
func (p *parallelRun) do(ctx context.Context, ident string, f func(context.Context) error) {
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		defer func() {
			if r := recover(); r != nil {
				var perr error
				if re, ok := r.(error); ok {
					perr = fmt.Errorf("panic in %q: %w", ident, re)
				} else {
					perr = fmt.Errorf("panic in %q: %v", ident, r)
				}
				p.record(perr)
			}
		}()
		if err := f(ctx); err != nil {
			p.record(fmt.Errorf("%s: %w", ident, err))
		}
	}()
}

func (p *parallelRun) record(err error) {
	p.mu.Lock()
	p.errs = append(p.errs, err)
	p.mu.Unlock()
	p.once.Do(func() {
		if p.cancel != nil {
			p.cancel()
		}
	})
}

// wait blocks until all goroutines have returned and then returns any
// accumulated errors as a single multiError, or nil if there were none.
func (p *parallelRun) wait() error {
	p.wg.Wait()
	if len(p.errs) > 0 {
		return p.errs
	}
	return nil
}
