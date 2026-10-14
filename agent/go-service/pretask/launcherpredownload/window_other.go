//go:build !windows

package launcherpredownload

import (
	"errors"
	"image"
)

// errUnsupportedPlatform 表示启动器窗口自动化只在 Windows 上实现。
var errUnsupportedPlatform = errors.New("launcherpredownload: launcher automation is only available on Windows")

type launcherWindow struct{}

func enableDPIAwareness() {}

func findLauncherWindow() (*launcherWindow, error) {
	return nil, errUnsupportedPlatform
}

func (w *launcherWindow) bringToFront() {}

func (w *launcherWindow) capture() (*image.RGBA, error) {
	return nil, errUnsupportedPlatform
}

func (w *launcherWindow) moveCursor(_, _ int) error {
	return errUnsupportedPlatform
}

func (w *launcherWindow) click(_, _ int) error {
	return errUnsupportedPlatform
}

func (w *launcherWindow) screenPoint(_, _ int) (int, int) {
	return 0, 0
}

func foregroundWindow() uintptr {
	return 0
}

func restoreForegroundWindow(_ uintptr) {}
