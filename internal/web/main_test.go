package web

import (
	"io"
	"log"
	"os"
	"testing"
)

// Request logs would bury test failures; set GRUS_TEST_LOG=1 to see them.
func TestMain(m *testing.M) {
	if os.Getenv("GRUS_TEST_LOG") == "" {
		log.SetOutput(io.Discard)
	}
	os.Exit(m.Run())
}
