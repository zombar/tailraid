package smb

import (
	"encoding/binary"
	"testing"
)

// A CANCEL of a WRITE answered STATUS_PENDING does not cancel it: the driver's write
// cannot be taken back once it started, so the server answers the CANCEL nothing and
// the WRITE its real reply, success with the bytes written (MS-SMB2 3.3.5.16: a request
// not successfully cancelled continues and the CANCEL gets no response). Before, the
// CANCEL was answered STATUS_CANCELLED while the bytes landed anyway, and the WRITE's
// own reply was dropped: the client believed a range unwritten that was written.
// Invariant: I-247
func TestCancel_AsyncWrite_CompletesNotCancelled(t *testing.T) {
	c, of, gf, client := asyncConn(t)
	interimCh := make(chan []byte, 1)
	go func() { interimCh <- readReply(t, client) }()
	c.alone = true
	if out, err := c.dispatch(writeRequest(of, 7, 4, []byte("data"))); err != nil || out != nil {
		t.Fatalf("the WRITE: %v, reply %v", err, out)
	}
	c.alone = false
	<-gf.entered
	interim := <-interimCh
	asyncID := binary.LittleEndian.Uint64(interim[offAsyncID:])

	cancel := request(cmdCancel, 0, []byte{4, 0, 0, 0})
	binary.LittleEndian.PutUint32(cancel[offFlags:], flagAsyncCommand)
	binary.LittleEndian.PutUint64(cancel[offAsyncID:], asyncID)
	binary.LittleEndian.PutUint64(cancel[offMessageID:], 7)
	nextCh := make(chan []byte, 1)
	go func() { nextCh <- readReply(t, client) }()
	if out, err := c.dispatch(cancel); err != nil || out != nil {
		t.Fatalf("the CANCEL: %v, reply %v", err, out)
	}
	close(gf.gate)
	next := <-nextCh
	h, err := parseHeader(next)
	if err != nil {
		t.Fatal(err)
	}
	if h.status != statusSuccess || h.messageID != 7 || binary.LittleEndian.Uint64(next[offAsyncID:]) != asyncID {
		t.Fatalf("after the CANCEL the client got status %#x for message %d, want the WRITE's success", h.status, h.messageID)
	}
	if got := binary.LittleEndian.Uint32(next[headerLen+4:]); got != 4 {
		t.Errorf("the WRITE counts %d bytes written, want 4", got)
	}
	if string(gf.data[4:]) != "data" {
		t.Errorf("the driver holds %q", gf.data)
	}
}

// A related request after one that answered a warning (severity 2, such as
// STATUS_BUFFER_OVERFLOW for a QUERY_INFO whose buffer was too small) is carried out: a
// warning is a success with partial output, and only an error (severity 3) stops the
// chain (MS-SMB2 3.3.5.2.7.2). Before, the CLOSE of a CREATE + QUERY_INFO + CLOSE chain
// was skipped after an overflow and the handle stayed open.
// Invariant: I-247
func TestChain_GoesOnAfterAWarning(t *testing.T) {
	fs := &tinyFS{body: []byte("hello")}
	c, of := opened(t, fs, "/file.txt", false)
	live := of.id

	query := make([]byte, 40)
	query[2], query[3] = infoTypeFile, fileAllInformation
	// room for the fixed part, not the name: BUFFER_OVERFLOW
	binary.LittleEndian.PutUint32(query[4:], uint32(fixedPart(infoTypeFile, fileAllInformation, 1<<10)))
	copy(query[24:], of.id[:])
	first := request(cmdQueryInfo, 1, query)
	for len(first)%8 != 0 {
		first = append(first, 0)
	}
	binary.LittleEndian.PutUint32(first[offNextCommand:], uint32(len(first)))
	closing := make([]byte, 24)
	copy(closing[8:], allOnesFileID[:])
	second := request(cmdClose, 0, closing)
	binary.LittleEndian.PutUint32(second[offFlags:], flagRelatedOps)

	out, err := c.dispatchChain(append(first, second...))
	if err != nil {
		t.Fatal(err)
	}
	h, err := parseHeader(out)
	if err != nil {
		t.Fatal(err)
	}
	if h.status != statusBufferOverflow || h.nextCommand == 0 {
		t.Fatalf("the QUERY_INFO answered %#x (next %d), want BUFFER_OVERFLOW in a chain", h.status, h.nextCommand)
	}
	if st := statusOf(t, out[h.nextCommand:]); st != statusSuccess {
		t.Errorf("the related CLOSE answered %#x, want success", st)
	}
	if c.files[live] != nil {
		t.Error("the related CLOSE after a warning did not close the handle")
	}
}
