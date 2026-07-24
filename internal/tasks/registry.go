// Package tasks holds the executable task catalogue. Each task file
// self-registers in init, the database/sql driver pattern, so importing the
// package is enough to populate the registry.
//
// The Tier 1 tasks are skeletons: written against the screen names and
// anchors the real corpus is expected to produce. Graph validation refuses
// to run them until those templates exist, which is the designed behavior.
package tasks

import (
	"fmt"
	"sort"
	"sync"

	"github.com/tomharris/lw-manager/internal/runtime"
)

var (
	mu       sync.RWMutex
	registry = map[string]runtime.TaskFunc{}
)

// Register adds a task. Duplicate or empty names are init-time bugs and
// panic loudly.
func Register(name string, fn runtime.TaskFunc) {
	mu.Lock()
	defer mu.Unlock()
	if name == "" || fn == nil {
		panic("tasks: Register called with empty name or nil func")
	}
	if _, dup := registry[name]; dup {
		panic(fmt.Sprintf("tasks: Register called twice for %q", name))
	}
	registry[name] = fn
}

// Get looks a task up by name.
func Get(name string) (runtime.TaskFunc, bool) {
	mu.RLock()
	defer mu.RUnlock()
	fn, ok := registry[name]
	return fn, ok
}

// Names lists registered tasks, sorted.
func Names() []string {
	mu.RLock()
	defer mu.RUnlock()
	out := make([]string, 0, len(registry))
	for n := range registry {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}
