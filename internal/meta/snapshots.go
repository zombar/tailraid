package meta

// The snapshot catalogue (docs/16 §3–§5, docs/02 §Apply rules). The snapshots themselves
// live on their mirror disks; the FSM keeps their rows and, while a row is syncing, holds
// GC back (gcDelete, I-181). Every applier validates fully before it mutates.

// snapshotBegin records a syncing row at this command's own index.
func (a *applier) snapshotBegin(c *SnapshotBegin) (Result, error) {
	if c == nil || !validSnapName(c.Name) {
		return Result{}, ErrBadCommand
	}
	st := a.st
	m, ok := st.Nodes[c.Mirror]
	if !ok || m.Kind != KindMirror || m.State != Active {
		return Result{}, ErrNoSuchNode
	}
	for _, sn := range st.Snaps {
		if sn.Mirror != c.Mirror {
			continue
		}
		if sn.Name == c.Name || sn.State == SnapSyncing {
			return Result{}, ErrExist // a taken name, or a sync already under way on this mirror
		}
	}
	now := a.clock(c.Now)
	id := st.NextSnap
	if id == 0 {
		id = 1 // a state decoded from before codec 7 starts its IDs at 1
	}
	st.NextSnap = id + 1
	st.Snaps[id] = &Snapshot{ID: id, Mirror: c.Mirror, Name: c.Name, Index: a.index, At: now, Auto: c.Auto, State: SnapSyncing}
	return Result{SnapID: id}, nil
}

// snapshotEnd completes a sync or removes its row; either way the hold ends.
func (a *applier) snapshotEnd(c *SnapshotEnd) error {
	if c == nil {
		return ErrBadCommand
	}
	sn, ok := a.st.Snaps[c.ID]
	if !ok || sn.State != SnapSyncing {
		return ErrNotExist // a late or repeated end is harmless
	}
	a.clock(c.Now)
	if !c.OK {
		delete(a.st.Snaps, c.ID)
		return nil
	}
	sn.Root, sn.Files, sn.Bytes, sn.NewBytes, sn.Missing = c.Root, c.Files, c.Bytes, c.NewBytes, c.Missing
	sn.State = SnapComplete
	if c.Missing > 0 {
		sn.State = SnapIncomplete
	}
	return nil
}

// snapshotDelete is the two-step deletion of docs/16 §5: the leader's thinning marks a
// row Deleting, and the mirror's host, once it has swept the snapshot off the disk,
// removes the row with Done. No user path proposes it (I-183).
func (a *applier) snapshotDelete(c *SnapshotDelete) error {
	if c == nil {
		return ErrBadCommand
	}
	sn, ok := a.st.Snaps[c.ID]
	if !ok {
		return ErrNotExist
	}
	switch {
	case c.Done && sn.State != SnapDeleting:
		return ErrExist
	case !c.Done && sn.State == SnapSyncing:
		return ErrExist // end the sync first
	}
	a.clock(c.Now)
	if c.Done {
		delete(a.st.Snaps, c.ID)
		return nil
	}
	sn.State = SnapDeleting // a second mark changes nothing
	return nil
}
