package webui_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/zombar/tailraid/internal/webui"
)

// The index page's CSP admits every source the TypeScript gives an image: a thumbnail or
// a folder cover fetched and shown through an object URL needs blob:, an inline picture
// data:. v0.8.1 showed every thumbnail through an object URL under an img-src without
// blob:, so the browser drew none of them.
// Invariant: I-315
func TestStatic_CSPAdmitsEveryImageSource(t *testing.T) {
	ts := readAll(t, glob(t, filepath.Join(srcDir, "*.ts"))...)
	var imgSrc string
	for d := range strings.SplitSeq(webui.IndexCSP, ";") {
		if d = strings.TrimSpace(d); strings.HasPrefix(d, "img-src ") {
			imgSrc = d
		}
	}
	if imgSrc == "" {
		t.Fatalf("no img-src in %q", webui.IndexCSP)
	}
	for _, tc := range []struct{ uses, scheme string }{
		{"URL.createObjectURL(", "blob:"},
		{"data:image/", "data:"},
	} {
		if strings.Contains(ts, tc.uses) && !strings.Contains(imgSrc, " "+tc.scheme) {
			t.Errorf("the TypeScript shows images from %s but the CSP %q does not admit %s", tc.uses, imgSrc, tc.scheme)
		}
	}
}
