package service

import (
	"errors"
	"io"
	"strings"

	"github.com/shovan04/storvia-project/internal/apperr"
)

// readerFromString adapts a string to a reader for the content-source stub.
func readerFromString(value string) io.Reader {
	return strings.NewReader(value)
}

// kindOfErr returns the apperr kind of an error for assertions.
func kindOfErr(err error) apperr.Kind { return apperr.KindOf(err) }

// errorf builds a plain error, standing in for an unnormalized provider failure.
func errorf(format string) error {
	return errors.New(format)
}

// containsAll reports whether text contains every fragment.
func containsAll(text string, fragments ...string) bool {
	for _, fragment := range fragments {
		if !strings.Contains(text, fragment) {
			return false
		}
	}
	return true
}
