package storage

import (
	"path/filepath"
	"sort"
)

// Keep the uncertainty of a discarded alias at its retained source and target.
// A folded target is represented by the first hidden path below a retained
// ancestor. Marking that ancestor itself could poison every Host directory
// when the ancestor is the scan root. Repeated aliases share one frontier entry.
func (s *Scanner) foldReference(source, target string) {
	for p := target; !s.retained[p]; p = filepath.Dir(p) {
		parent := filepath.Dir(p)
		if s.retained[parent] {
			target = p
			break
		}
		if p == parent {
			break
		}
	}
	if s.omittedTargets[source] == nil {
		s.omittedTargets[source] = map[string]bool{}
	}
	s.omittedTargets[source][target] = true
}

func (s *Scanner) foldedReferenceTargets(source string) []string {
	if len(s.omittedTargets[source]) == 0 {
		return nil
	}
	targets := make([]string, 0, len(s.omittedTargets[source]))
	for target := range s.omittedTargets[source] {
		targets = append(targets, target)
	}
	sort.Strings(targets)
	return targets
}
