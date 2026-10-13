package httpapi

import "log"

// Apply the original console level selection; request records remain complete.
func (s *Server) runtimeLog(level, format string, args ...any) {
	levels := []string{"info", "warning", "error"}
	if s.store != nil {
		if cfg, err := s.store.Config(); err == nil {
			if configured := stringSliceValue(cfg["log_levels"]); len(configured) > 0 {
				levels = configured
			}
		}
	}
	for _, selected := range levels {
		if selected == level {
			log.Printf("["+level+"] "+format, args...)
			return
		}
	}
}
