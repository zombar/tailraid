package webui_test

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/zombar/tailraid/internal/testharness"
	"github.com/zombar/tailraid/internal/webui"
)

// thumbAsync asks for a thumbnail on its own goroutine and hands back the status and the
// Retry-After header (status -1: the request failed).
func (u *ui) thumbAsync(query string) <-chan [2]string {
	out := make(chan [2]string, 1)
	go func() {
		resp, err := u.client.Get(u.hs.URL + "/api/thumb?" + query) //nolint:noctx // a test client
		if err != nil {
			out <- [2]string{"-1", err.Error()}
			return
		}
		_ = resp.Body.Close()
		out <- [2]string{fmt.Sprint(resp.StatusCode), resp.Header.Get("Retry-After")}
	}()
	return out
}

// await advances the fake clock by ThumbWait until the request answers.
func (u *ui) await(t *testing.T, got <-chan [2]string) [2]string {
	t.Helper()
	var r [2]string
	testharness.Eventually(t, func() bool {
		select {
		case r = <-got:
			return true
		default:
			u.f.Clk.Advance(webui.ThumbWait)
			return false
		}
	}, "the thumbnail request never answered")
	return r
}

// drain waits for every background thumbnail job to finish.
func (u *ui) drain(t *testing.T) {
	t.Helper()
	testharness.Eventually(t, func() bool { return u.srv.ThumbJobsRunning() == 0 }, "a thumbnail job never finished")
}

// A thumbnail that takes longer than ThumbWait to make answers 202 with Retry-After while
// it is made in the background, and the next request after that answers the picture
// (2026-10-16: a few covers being made held every tile behind them).
// Invariant: I-374
func TestWebUI_Thumb_LaterWhileMade(t *testing.T) {
	u := newUI(t)
	u.write("/pic.png", 0, pngBytes(640, 360, red))
	u.flush()
	release := u.srv.HoldDecoders()
	r := u.await(t, u.thumbAsync("path=/pic.png"))
	if r != [2]string{"202", "2"} {
		t.Fatalf("a thumbnail still being made: %v, want 202 with Retry-After 2", r)
	}
	if n := u.srv.ThumbJobsRunning(); n != 1 {
		t.Fatalf("%d jobs running, want the one making it", n)
	}
	release()
	u.drain(t)
	code, hdr, body := u.thumb("path=/pic.png")
	if code != 200 || hdr.Get("Content-Type") != "image/jpeg" || hue(decodeJPEG(t, body)) != "red" {
		t.Fatalf("after the job: %d %s", code, hdr.Get("Content-Type"))
	}
}

// Two tiles asking for one thumbnail while it is made start one job, and both answer 202.
// Invariant: I-374
func TestWebUI_Thumb_OneJobPerKey(t *testing.T) {
	u := newUI(t)
	u.write("/pic.png", 0, pngBytes(640, 360, red))
	u.flush()
	release := u.srv.HoldDecoders()
	a, b := u.thumbAsync("path=/pic.png"), u.thumbAsync("path=/pic.png")
	for _, r := range [][2]string{u.await(t, a), u.await(t, b)} {
		if r[0] != "202" {
			t.Fatalf("a request while the thumbnail is made: %v", r)
		}
	}
	if n := u.srv.ThumbJobsRunning(); n != 1 {
		t.Fatalf("%d jobs for one thumbnail", n)
	}
	release()
	u.drain(t)
	if code, _, _ := u.thumb("path=/pic.png"); code != 200 {
		t.Fatalf("after the job: %d", code)
	}
}

// With ThumbJobs jobs running a request starts nothing and answers 202; once they are
// done the same request makes its thumbnail.
// Invariant: I-374
func TestWebUI_Thumb_FullJobsStartNothing(t *testing.T) {
	u := newUI(t)
	for i := range webui.ThumbJobs + 1 {
		u.write(fmt.Sprintf("/p%02d.jpg", i), 0, jpegBytes(64+i, 32, blue))
	}
	u.flush()
	release := u.srv.HoldDecoders()
	for i := range webui.ThumbJobs {
		if r := u.await(t, u.thumbAsync(fmt.Sprintf("path=/p%02d.jpg", i))); r[0] != "202" {
			t.Fatalf("p%02d: %v", i, r)
		}
	}
	last := fmt.Sprintf("path=/p%02d.jpg", webui.ThumbJobs)
	if r := u.await(t, u.thumbAsync(last)); r != [2]string{"202", "2"} {
		t.Fatalf("a request with every job place taken: %v", r)
	}
	if n := u.srv.ThumbJobsRunning(); n != webui.ThumbJobs {
		t.Fatalf("%d jobs running, want %d: a full table started one more", n, webui.ThumbJobs)
	}
	release()
	u.drain(t)
	if code, _, _ := u.thumb(last); code != 200 {
		t.Fatalf("the request again once the jobs were done: %d", code)
	}
}

// A folder's tile whose cover is still being made answers 202 the same way, then the
// cover.
// Invariant: I-374
func TestWebUI_FolderCover_LaterWhileMade(t *testing.T) {
	u := newUI(t)
	must(t, u.fs.MkdirAll("/alb", 0o755))
	u.write("/alb/cover.jpg", 0, jpegBytes(400, 400, green))
	u.write("/alb/01.flac", 0, []byte("not really audio"))
	u.flush()
	release := u.srv.HoldDecoders()
	if r := u.await(t, u.thumbAsync("path=/alb")); r[0] != "202" {
		t.Fatalf("a folder whose cover is being made: %v", r)
	}
	release()
	u.drain(t)
	code, _, body := u.thumb("path=/alb")
	if code != http.StatusOK || hue(decodeJPEG(t, body)) != "green" {
		t.Fatalf("the folder's cover after the job: %d", code)
	}
}
