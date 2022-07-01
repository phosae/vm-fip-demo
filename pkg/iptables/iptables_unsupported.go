//go:build !linux
// +build !linux

package iptables

import (
	"fmt"
	"os"
)

func grabIptablesLocks(lock14filePath, lock16filePath string) (iptablesLocker, error) {
	return nil, fmt.Errorf("iptables unsupported on this platform")
}

func grabIptablesFileLock(f *os.File) error {
	return fmt.Errorf("iptables unsupported on this platform")
}
