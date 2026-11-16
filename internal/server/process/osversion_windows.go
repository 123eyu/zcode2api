//go:build windows

package process

import (
	"fmt"
	"golang.org/x/sys/windows"
)

func OSVersion() string {
	v := windows.RtlGetVersion()
	return fmt.Sprintf("%d.%d.%d", v.MajorVersion, v.MinorVersion, v.BuildNumber)
}
