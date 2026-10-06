//go:build !linux

package pdftext

import "errors"

func applyLimits(cpuSec, asBytes uint64) error {
	return errors.New("resource limits are only implemented on linux")
}
