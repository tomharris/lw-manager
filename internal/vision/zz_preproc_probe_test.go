//go:build scrolldiag

package vision

import (
	"flag"
	"image/png"
	"os"
	"testing"

	"github.com/tomharris/lw-manager/internal/transport"
)

var (
	ppIn  = flag.String("ppin", "", "frame to crop and preprocess")
	ppOut = flag.String("ppout", "", "where to write the preprocessed crop")
	ppY1  = flag.Float64("ppy1", 0.404, "")
	ppY2  = flag.Float64("ppy2", 0.438, "")
	ppX1  = flag.Float64("ppx1", 0.03, "")
	ppX2  = flag.Float64("ppx2", 0.97, "")
)

// TestPreprocProbe writes what the OCR engine is actually handed, so the
// preprocessing chain can be inspected rather than inferred from its output.
func TestPreprocProbe(t *testing.T) {
	if *ppIn == "" || *ppOut == "" {
		t.Skip("need -ppin and -ppout")
	}
	f, err := os.Open(*ppIn)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	img, err := png.Decode(f)
	if err != nil {
		t.Fatal(err)
	}
	out := Preprocess(img, Options{
		Region: transport.Rect{X1: *ppX1, Y1: *ppY1, X2: *ppX2, Y2: *ppY2},
	})
	w, err := os.Create(*ppOut)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if err := png.Encode(w, out); err != nil {
		t.Fatal(err)
	}
	t.Logf("wrote %s bounds=%v", *ppOut, out.Bounds())
}
