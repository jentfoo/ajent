package command

import "context"

// Command is one registered slash command.
type Command struct {
	Name        string
	Description string
	Args        string // usage hint, e.g. "<optional-instructions>"
	// Complete returns argument candidates for prefix, or nil when none.
	Complete func(prefix string) []string
	// Handler runs the command, with long work on a goroutine.
	Handler func(ctx context.Context, args string, c Console) error
}

// Registry is the table commands, extensions and shell mode dispatch through.
type Registry struct {
	byName map[string]Command
	order  []string // registration order, for /help and completion
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry { return &Registry{byName: make(map[string]Command)} }

// Register adds or replaces a command by name, last write winning to let
// callers widen a built-in while keeping registration order for /help.
func (r *Registry) Register(cmd Command) {
	if _, exists := r.byName[cmd.Name]; !exists {
		r.order = append(r.order, cmd.Name)
	}
	r.byName[cmd.Name] = cmd
}

// Get returns the command named name (case sensitive, matching the lowercased
// name ParseLine produces).
func (r *Registry) Get(name string) (Command, bool) {
	c, ok := r.byName[name]
	return c, ok
}

// List returns every registered command in registration order, for /help.
func (r *Registry) List() []Command {
	out := make([]Command, 0, len(r.order))
	for _, n := range r.order {
		out = append(out, r.byName[n])
	}
	return out
}
