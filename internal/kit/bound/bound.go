// Package bound keeps maps keyed by values from the network bounded.
package bound

// MakeRoom makes room for key in m, dropping an arbitrary entry once m holds
// max. Keys come from the network, so these maps must be bounded. The caller
// holds the lock that guards m.
func MakeRoom[K comparable, V any](m map[K]V, key K, max int) {
	if _, ok := m[key]; ok || len(m) < max {
		return
	}
	for k := range m {
		delete(m, k)
		return
	}
}
