package tools

import (
	"fmt"
	"sync"
)

// Registry manages all available tools
type Registry struct {
	tools map[string]*ToolInfo
	mu    sync.RWMutex
}

// NewRegistry creates a new tool registry
func NewRegistry() *Registry {
	return &Registry{
		tools: make(map[string]*ToolInfo),
	}
}

// Register adds a tool to the registry
func (r *Registry) Register(info *ToolInfo) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	name := info.Tool.Name()
	if name == "" {
		return fmt.Errorf("tool name cannot be empty")
	}

	if _, exists := r.tools[name]; exists {
		return fmt.Errorf("tool already registered: %s", name)
	}

	r.tools[name] = info
	return nil
}

// Unregister removes a tool from the registry
func (r *Registry) Unregister(name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if _, exists := r.tools[name]; !exists {
		return fmt.Errorf("tool not found: %s", name)
	}

	delete(r.tools, name)
	return nil
}

// Get retrieves a tool by name
func (r *Registry) Get(name string) (Tool, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	info, exists := r.tools[name]
	if !exists {
		return nil, fmt.Errorf("tool not found: %s", name)
	}

	return info.Tool, nil
}

// GetInfo retrieves tool info by name
func (r *Registry) GetInfo(name string) (*ToolInfo, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	info, exists := r.tools[name]
	if !exists {
		return nil, fmt.Errorf("tool not found: %s", name)
	}

	return info, nil
}

// GetAll returns all registered tools
func (r *Registry) GetAll() []Tool {
	r.mu.RLock()
	defer r.mu.RUnlock()

	tools := make([]Tool, 0, len(r.tools))
	for _, info := range r.tools {
		tools = append(tools, info.Tool)
	}
	return tools
}

// GetAllInfo returns all registered tool info
func (r *Registry) GetAllInfo() []*ToolInfo {
	r.mu.RLock()
	defer r.mu.RUnlock()

	infos := make([]*ToolInfo, 0, len(r.tools))
	for _, info := range r.tools {
		infos = append(infos, info)
	}
	return infos
}

// GetForMode returns tools allowed in the given mode
func (r *Registry) GetForMode(mode string) []Tool {
	r.mu.RLock()
	defer r.mu.RUnlock()

	tools := make([]Tool, 0)
	for _, info := range r.tools {
		if info.IsAllowedInMode(mode) {
			tools = append(tools, info.Tool)
		}
	}
	return tools
}

// IsAllowed checks if a tool is allowed in the given mode
func (r *Registry) IsAllowed(mode string, toolName string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()

	info, exists := r.tools[toolName]
	if !exists {
		return false
	}

	return info.IsAllowedInMode(mode)
}

// GetByCategory returns tools in a specific category
func (r *Registry) GetByCategory(category ToolCategory) []Tool {
	r.mu.RLock()
	defer r.mu.RUnlock()

	tools := make([]Tool, 0)
	for _, info := range r.tools {
		if info.Category == category {
			tools = append(tools, info.Tool)
		}
	}
	return tools
}

// Count returns the number of registered tools
func (r *Registry) Count() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.tools)
}

// DefaultRegistry is the global default registry
var DefaultRegistry = NewRegistry()

// RegisterDefault registers a tool in the default registry
func RegisterDefault(info *ToolInfo) error {
	return DefaultRegistry.Register(info)
}

// GetDefault retrieves a tool from the default registry
func GetDefault(name string) (Tool, error) {
	return DefaultRegistry.Get(name)
}

// CreateDefaultRegistry creates a registry with all built-in tools
func CreateDefaultRegistry() *Registry {
	registry := NewRegistry()

	// Register all built-in tools
	// Note: These will be implemented in separate files

	// File operations - allowed in both modes (read-only in plan)
	// registry.Register(&ToolInfo{...})

	// Search operations - allowed in both modes
	// registry.Register(&ToolInfo{...})

	// Command execution - act mode only
	// registry.Register(&ToolInfo{...})

	// User interaction - both modes
	// registry.Register(&ToolInfo{...})

	// Completion - both modes
	// registry.Register(&ToolInfo{...})

	return registry
}
