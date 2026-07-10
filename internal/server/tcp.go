package server

import (
	"fmt"
	"sync"
)

// PortAllocator hands out public TCP ports from a fixed range for raw TCP
// tunnels. Ports are returned to the pool when a tunnel closes.
type PortAllocator struct {
	mu    sync.Mutex
	start uint16
	end   uint16
	used  map[uint16]bool
	next  uint16
}

// NewPortAllocator builds an allocator over [start, end] inclusive.
func NewPortAllocator(start, end uint16) *PortAllocator {
	return &PortAllocator{
		start: start,
		end:   end,
		used:  make(map[uint16]bool),
		next:  start,
	}
}

// Allocate reserves a port. preferred != 0 requests a specific port (which
// must be in range and free); preferred == 0 picks the next free one.
func (a *PortAllocator) Allocate(preferred uint16) (uint16, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if preferred != 0 {
		if preferred < a.start || preferred > a.end {
			return 0, fmt.Errorf("port %d outside allowed range %d-%d", preferred, a.start, a.end)
		}
		if a.used[preferred] {
			return 0, fmt.Errorf("port %d already in use", preferred)
		}
		a.used[preferred] = true
		return preferred, nil
	}

	// Linear scan from next, wrapping once through the whole range.
	total := int(a.end) - int(a.start) + 1
	for i := 0; i < total; i++ {
		p := a.next
		if a.next == a.end {
			a.next = a.start
		} else {
			a.next++
		}
		if !a.used[p] {
			a.used[p] = true
			return p, nil
		}
	}
	return 0, fmt.Errorf("no free ports in range %d-%d", a.start, a.end)
}

// Release returns a port to the pool.
func (a *PortAllocator) Release(port uint16) {
	a.mu.Lock()
	delete(a.used, port)
	a.mu.Unlock()
}
