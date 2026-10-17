//go:build !linux && !darwin

package localapi

import (
	"errors"
	"os"
)

func prepareSocket(string) (string, *os.File, error) {
	return "", nil, errors.New("local API requires macOS or Linux")
}
func checkPrivateFile(string) error { return errors.New("local API requires macOS or Linux") }
