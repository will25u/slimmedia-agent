package updater

import (
	"io"
	"log"
)

func newTestLogger(w io.Writer) *log.Logger {
	return log.New(w, "test-helper: ", 0)
}
