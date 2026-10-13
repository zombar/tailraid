// SPDX-License-Identifier: BSD-3-Clause

package smb

// Share access (MS-FSA 2.1.5.1.2.1, tailraid I-265): an open says what it will do to a
// file (DesiredAccess) and what it lets others do meanwhile (ShareAccess), and an open
// whose wishes conflict with an open already held is refused with
// STATUS_SHARING_VIOLATION before it changes anything. Only data access counts: an open
// for attributes alone never conflicts. The opens checked are this share's, of every
// connection and session; a file opened through another server over the same volume
// (another gateway, NFS) is not seen.

// The access bits that take part in sharing.
const (
	accReadData   uint32 = 0x00000001
	accWriteData  uint32 = 0x00000002
	accAppendData uint32 = 0x00000004
	accExecute    uint32 = 0x00000020
	accDelete     uint32 = 0x00010000
	accMaxAllowed uint32 = 0x02000000
	accGenericAll uint32 = 0x10000000
	accGenericEx  uint32 = 0x20000000
	accGenericW   uint32 = 0x40000000
	accGenericR   uint32 = 0x80000000

	shareRead   uint32 = 0x1
	shareWrite  uint32 = 0x2
	shareDelete uint32 = 0x4
)

// dataAccess maps a DesiredAccess to the data access it grants, generic rights spelled out
// (MAXIMUM_ALLOWED is granted everything).
func dataAccess(a uint32) uint32 {
	if a&(accGenericAll|accMaxAllowed) != 0 {
		return accReadData | accWriteData | accAppendData | accExecute | accDelete
	}
	if a&accGenericR != 0 {
		a |= accReadData
	}
	if a&accGenericW != 0 {
		a |= accWriteData | accAppendData
	}
	if a&accGenericEx != 0 {
		a |= accExecute
	}
	return a & (accReadData | accWriteData | accAppendData | accExecute | accDelete)
}

// conflicts reports whether an open of data access a sharing s meets one holding access
// ha sharing hs.
func conflicts(a, s, ha, hs uint32) bool {
	return (a&(accReadData|accExecute) != 0 && hs&shareRead == 0) ||
		(a&(accWriteData|accAppendData) != 0 && hs&shareWrite == 0) ||
		(a&accDelete != 0 && hs&shareDelete == 0) ||
		(ha&(accReadData|accExecute) != 0 && s&shareRead == 0) ||
		(ha&(accWriteData|accAppendData) != 0 && s&shareWrite == 0) ||
		(ha&accDelete != 0 && s&shareDelete == 0)
}

// sharingViolation reports whether an open of file ino with data access a sharing s
// conflicts with an open of it this share holds.
func (s *share) sharingViolation(ino uint64, a, sa uint32) bool {
	if a == 0 {
		return false // attributes only
	}
	s.hmu.Lock()
	defer s.hmu.Unlock()
	for of := range s.handles {
		if of.ino == ino && of.access != 0 && conflicts(a, sa, of.access, of.shareAccess) {
			return true
		}
	}
	return false
}
