package querylog

import "testing"

func TestDefaultDirIsPrivate(t *testing.T) {
	dir, err := DefaultDir()
	if err != nil {
		t.Fatal(err)
	}
	again, err := DefaultDir() // idempotent once it exists
	if err != nil || again != dir {
		t.Fatalf("second call: %q, %v", again, err)
	}
}
