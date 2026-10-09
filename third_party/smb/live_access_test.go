// SPDX-License-Identifier: BSD-3-Clause

package smb

import "testing"

// Access decides at every connection, so a change made while the server runs
// reaches the next one -- which a list given at Share cannot.
func TestAccessDecidesAtEachConnection(t *testing.T) {
	srv := New()
	srv.AddUser("alice", "hunter2")
	srv.AddUser("bob", "swordfish")
	// who may do what, changed below while the server exists.
	rules := map[string][2]bool{"alice": {true, true}}
	decide := func(user string) (bool, bool) { r := rules[user]; return r[0], r[1] }
	if err := srv.Share("photos", nothingFS{}, Access(decide), AllowUsers("bob")); err != nil {
		t.Fatal(err)
	}
	if err := srv.Share("museum", nothingFS{}, Access(decide), ReadOnly()); err != nil {
		t.Fatal(err)
	}

	check := func(user, share string, wantStatus, wantAccess uint32, why string) {
		t.Helper()
		st, access := connectAs(t, newConn(srv, nil), user, share)
		if st != wantStatus || access != wantAccess {
			t.Errorf("%s on %s: status %#x access %#08x, want %#x %#08x (%s)", user, share, st, access, wantStatus, wantAccess, why)
		}
	}
	check("alice", "photos", statusSuccess, accessAll, "the callback lets her write")
	check("bob", "photos", statusAccessDenied, 0, "the callback, not AllowUsers, decides")
	check("alice", "museum", statusSuccess, accessRead, "ReadOnly still beats the callback")

	// bob joins, read-only; alice loses write.
	rules["bob"] = [2]bool{true, false}
	rules["alice"] = [2]bool{true, false}
	check("bob", "photos", statusSuccess, accessRead, "a change reaches the next connection")
	check("alice", "photos", statusSuccess, accessRead, "and takes write away")
	delete(rules, "alice")
	check("alice", "photos", statusAccessDenied, 0, "and access")

	// The share listing asks the same question.
	var names []string
	for _, e := range srv.sharesFor("alice") {
		names = append(names, e.name)
	}
	for _, n := range names {
		if n == "PHOTOS" || n == "photos" {
			t.Errorf("alice, refused photos, is still offered it: %v", names)
		}
	}
}

// A removed user cannot authenticate; one that was never there is no error.
func TestRemoveUser(t *testing.T) {
	srv := New()
	if err := srv.AddUserHash("bob", md4sum(utf16le("swordfish"))); err != nil {
		t.Fatal(err)
	}
	if _, ok := srv.credentialFor("bob"); !ok {
		t.Fatal("bob was not added")
	}
	srv.RemoveUser("bob")
	if _, ok := srv.credentialFor("bob"); ok {
		t.Error("a removed user still has a credential")
	}
	srv.RemoveUser("nobody")
}
