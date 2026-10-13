// Package refusal is an action refused for a reason worth showing to the user, as opposed to a
// failure of the program.
package refusal

import (
	"errors"
	"fmt"
)

// Error is a refusal; its message is written for the user.
type Error struct{ Msg string }

func (r *Error) Error() string { return r.Msg }

// New refuses, for the reason given.
func New(format string, a ...any) error { return &Error{Msg: fmt.Sprintf(format, a...)} }

// As returns the refusal inside err, if there is one.
func As(err error) (*Error, bool) {
	var r *Error
	ok := errors.As(err, &r)
	return r, ok
}
