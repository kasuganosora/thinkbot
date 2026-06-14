package errs

import "github.com/pkg/errors"

func New(message string) error {
	return errors.New(message)
}

func Wrap(err error, message string) error {
	return errors.Wrap(err, message)
}

func Cause(err error) error {
	return errors.Cause(err)
}
