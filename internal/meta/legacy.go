package meta

// LegacyDeviceTargets inspects a restored state for the shape clusters had when the
// device row was a placement target (docs/14 §9, I-119): device rows that still carry a
// budget, and chunk slots that still name a device index. each walks the chunk table as
// FSM.ForEachChunk does. A clean state returns nothing and 0.
func LegacyDeviceTargets(nodes []Node, each func(func(Chunk) bool)) (devices []Node, slots int) {
	device := map[NodeIdx]bool{}
	for i := range nodes {
		n := &nodes[i]
		if n.Kind != KindDevice || n.Idx == 0 {
			continue
		}
		device[n.Idx] = true
		if n.Capacity > 0 && n.State != Removed {
			devices = append(devices, *n)
		}
	}
	if each != nil {
		each(func(c Chunk) bool {
			for _, s := range c.Placement {
				if s != 0 && device[s] {
					slots++
				}
			}
			return true
		})
	}
	return devices, slots
}

// HasTargets reports whether the node table holds anything a chunk can be placed on: an
// Active disk of an Active device, or an Active S3 node (docs/14 §9, I-120). Without one
// every flush fails with ErrFull.
func HasTargets(nodes []Node) bool {
	active := map[NodeIdx]bool{}
	for i := range nodes {
		if n := &nodes[i]; n.Kind == KindDevice && n.State == Active && n.Idx != 0 {
			active[n.Idx] = true
		}
	}
	for i := range nodes {
		n := &nodes[i]
		if n.State != Active || n.Idx == 0 {
			continue
		}
		switch n.Kind {
		case KindS3:
			return true
		case KindDisk:
			if active[n.Host] {
				return true
			}
		case KindDevice, KindMirror: // a device stores nothing; a mirror holds snapshots, not slots (docs/16)
		}
	}
	return false
}
