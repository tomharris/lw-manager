package transport_test

import (
	"testing"

	"github.com/tomharris/lw-manager/internal/transport"
)

func TestParseVersionName(t *testing.T) {
	const dump = `
Packages:
  Package [com.fun.lastwar.gp] (a1b2c3):
    userId=10234
    versionCode=3210 minSdk=24 targetSdk=34
    versionName=3.2.1
    splits=[base]
`
	if got := transport.ParseVersionName(dump); got != "3.2.1" {
		t.Fatalf("ParseVersionName = %q, want 3.2.1", got)
	}
}

func TestParseVersionNameTakesTheFirstOfSeveral(t *testing.T) {
	const dump = "    versionName=3.2.1\n    versionName=0.0.0\n"
	if got := transport.ParseVersionName(dump); got != "3.2.1" {
		t.Fatalf("ParseVersionName = %q, want 3.2.1", got)
	}
}

// An unknown version must not become a misleading empty-but-plausible value.
func TestParseVersionNameOnGarbageIsEmpty(t *testing.T) {
	if got := transport.ParseVersionName("no version here"); got != "" {
		t.Fatalf("ParseVersionName = %q, want empty", got)
	}
}
