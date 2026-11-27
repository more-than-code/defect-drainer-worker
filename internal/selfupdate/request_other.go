//go:build !linux

package selfupdate

import "errors"

var errLinuxOnly = errors.New("self-update runs on Linux hosts only")

func takeRequest(string) ([]byte, error) { return nil, errLinuxOnly }

func holdLock(string) (func(), error) { return nil, errLinuxOnly }
