//go:build !linux

package sandbox

import (
	"context"
	"fmt"
	"runtime"
)

func DefaultHost() Host { return unavailableHost{} }

type unavailableHost struct{}

func (unavailableHost) NewSession(context.Context) (Session, error) {
	return nil, fmt.Errorf("go-sandbox execution requires linux, not %s/%s", runtime.GOOS, runtime.GOARCH)
}

var _ Host = unavailableHost{}
