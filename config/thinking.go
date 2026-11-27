package config

import "slices"

var ThinkingLevels = []string{"off", "minimal", "low", "medium", "high", "xhigh"}

// Levels lists the thinking levels the model supports.
func (m ModelInfo) Levels() []string {
	switch {
	case m.Reasoning != nil && !*m.Reasoning:
		return []string{"off"}
	case len(m.Thinking) > 0:
		return m.Thinking
	default:
		return ThinkingLevels
	}
}

// Level picks the nearest supported level to requested, preferring a higher
// one, as Pi does. A name outside ThinkingLevels is kept when every level is
// supported, so that milestone 1's pass-through of unknown names stays.
func (m ModelInfo) Level(requested string) string {
	supported := m.Levels()
	if requested == "" || slices.Contains(supported, requested) {
		return requested
	}
	i := slices.Index(ThinkingLevels, requested)
	if i < 0 {
		if len(m.Thinking) == 0 && (m.Reasoning == nil || *m.Reasoning) {
			return requested
		}
		return supported[0]
	}
	for j := i + 1; j < len(ThinkingLevels); j++ {
		if slices.Contains(supported, ThinkingLevels[j]) {
			return ThinkingLevels[j]
		}
	}
	for j := i - 1; j >= 0; j-- {
		if slices.Contains(supported, ThinkingLevels[j]) {
			return ThinkingLevels[j]
		}
	}
	return supported[0]
}

// WireEffort returns the reasoning value to send for requested, and whether
// to send any reasoning field at all.
func (m ModelInfo) WireEffort(requested string) (string, bool) {
	if m.Reasoning != nil && !*m.Reasoning {
		return "", false
	}
	level := m.Level(requested)
	if v, ok := m.Effort[level]; ok {
		return v, v != ""
	}
	if level == "" || level == "off" {
		return "", false
	}
	return level, true
}

// override returns m with every field that o sets replaced; the id is kept.
func (m ModelInfo) override(o ModelInfo) ModelInfo {
	if o.ContextWindow != 0 {
		m.ContextWindow = o.ContextWindow
	}
	if o.MaxOutput != 0 {
		m.MaxOutput = o.MaxOutput
	}
	if o.Thinking != nil {
		m.Thinking = o.Thinking
	}
	if o.Reasoning != nil {
		m.Reasoning = o.Reasoning
	}
	if o.Effort != nil {
		m.Effort = o.Effort
	}
	if o.ThinkingFormat != "" {
		m.ThinkingFormat = o.ThinkingFormat
	}
	if o.Input != nil {
		m.Input = o.Input
	}
	if o.InputPrice != 0 {
		m.InputPrice = o.InputPrice
	}
	if o.OutputPrice != 0 {
		m.OutputPrice = o.OutputPrice
	}
	if o.CachedPrice != 0 {
		m.CachedPrice = o.CachedPrice
	}
	return m
}
