package taskgroup

import "sync"

type Result[T any] struct {
	Value  T
	Err    error
	Shared bool
}

type Group[T any] struct {
	mu      sync.Mutex
	running map[string]*call[T]
}

type call[T any] struct {
	done   chan struct{}
	value  T
	err    error
	waited bool
}

func (g *Group[T]) Do(key string, fn func() (T, error)) Result[T] {
	g.mu.Lock()
	if g.running == nil {
		g.running = make(map[string]*call[T])
	}
	if c := g.running[key]; c != nil {
		c.waited = true
		g.mu.Unlock()
		<-c.done
		return Result[T]{Value: c.value, Err: c.err, Shared: true}
	}

	c := &call[T]{done: make(chan struct{})}
	g.running[key] = c
	g.mu.Unlock()

	c.value, c.err = fn()
	close(c.done)

	g.mu.Lock()
	delete(g.running, key)
	shared := c.waited
	g.mu.Unlock()

	return Result[T]{Value: c.value, Err: c.err, Shared: shared}
}
