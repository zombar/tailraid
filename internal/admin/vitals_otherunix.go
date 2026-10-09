//go:build unix && !linux && !darwin

package admin

// platformVitals on the other unixes: the process fields only.
func platformVitals(m *MachineReport) { rusageVitals(m) }
