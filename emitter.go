package portal

import "sync"

// Unsubscribe removes a previously-registered listener. Safe to call more than once.
type Unsubscribe func()

// listeners is a tiny concurrent listener registry. Callbacks are invoked
// outside any SDK lock, in registration order.
type listeners[T any] struct {
	mu   sync.Mutex
	next int
	fns  map[int]T
	ids  []int
}

func (l *listeners[T]) add(fn T) Unsubscribe {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.fns == nil {
		l.fns = map[int]T{}
	}
	id := l.next
	l.next++
	l.fns[id] = fn
	l.ids = append(l.ids, id)
	return func() {
		l.mu.Lock()
		defer l.mu.Unlock()
		if _, ok := l.fns[id]; !ok {
			return
		}
		delete(l.fns, id)
		for i, held := range l.ids {
			if held == id {
				l.ids = append(l.ids[:i], l.ids[i+1:]...)
				break
			}
		}
	}
}

func (l *listeners[T]) snapshot() []T {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]T, 0, len(l.ids))
	for _, id := range l.ids {
		out = append(out, l.fns[id])
	}
	return out
}
