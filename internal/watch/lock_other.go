//go:build !unix

package watch

import "errors"

// Lock is not implemented outside Unix: rikugan serve runs in a
// container, as a systemd unit, or under Docker Compose.
func Lock(string) (func() error, error) {
	return nil, errors.New("rikugan serve needs a Unix system")
}
