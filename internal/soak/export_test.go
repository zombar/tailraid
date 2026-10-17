package soak

// Check records a check from a phase a test defines (a fault set up between standard
// phases is a phase too, and every phase must assert something).
func (r *Run) Check(cites, name string, ok bool, text string) { r.check(cites, name, ok, text) }
