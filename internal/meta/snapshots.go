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

// snapshotHeal lowers a finished row's missing count to what its manifest still lists,
// complete at zero; the count never rises (docs/16 §5, I-391).
func (a *applier) snapshotHeal(c *SnapshotHeal) error {
	if c == nil {
		return ErrBadCommand
	}
	sn, ok := a.st.Snaps[c.ID]
	if !ok || (sn.State != SnapComplete && sn.State != SnapIncomplete) {
		return ErrNotExist
	}
	if c.Missing > sn.Missing {
		return ErrBadCommand
	}
	a.clock(c.Now)
	sn.Missing = c.Missing
	if sn.Missing == 0 {
		sn.State = SnapComplete
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

// snapshotImport records a row for a manifest its mirror's host found with no row
// (docs/16 §5): complete or incomplete at once, with the manifest's own time, and an ID
// above every manifest on that disk so no later sync can name one of them.
func (a *applier) snapshotImport(c *SnapshotImport) (Result, error) {
	if c == nil || !validSnapName(c.Name) || c.After == ^SnapID(0) {
		return Result{}, ErrBadCommand
	}
	st := a.st
	m, ok := st.Nodes[c.Mirror]
	if !ok || m.Kind != KindMirror || m.State != Active {
		return Result{}, ErrNoSuchNode
	}
	for _, sn := range st.Snaps {
		if sn.Mirror == c.Mirror && sn.Name == c.Name {
			return Result{}, ErrExist
		}
	}
	a.clock(c.Now)
	id := max(st.NextSnap, c.After+1, 1)
	st.NextSnap = id + 1
	state := SnapComplete
	if c.Missing > 0 {
		state = SnapIncomplete
	}
	st.Snaps[id] = &Snapshot{
		ID: id, Mirror: c.Mirror, Name: c.Name, Index: a.index, At: c.At, Auto: c.Auto, State: state,
		Root: c.Root, Files: c.Files, Bytes: c.Bytes, NewBytes: c.NewBytes, Missing: c.Missing,
	}
	return Result{SnapID: id}, nil
}
