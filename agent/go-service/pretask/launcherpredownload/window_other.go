//go:build !windows

package launcherpredownload

import (
	"errors"
	"image"
)

// errUnsupportedPlatform 表示启动器窗口自动化只在 Windows 上实现。
var errUnsupportedPlatform = errors.New("launcherpredownload: launcher automation is only available on Windows")

type launcherWindow struct{}

// enableDPIAwareness 在非 Windows 平台上无事可做（Windows 实现见 window_windows.go）。
func enableDPIAwareness() {}

// findLauncherWindow 只在 Windows 上实现。
func findLauncherWindow() (*launcherWindow, error) {
	return nil, errUnsupportedPlatform
}

// bringToFront 只在 Windows 上实现。
func (w *launcherWindow) bringToFront() {}

// capture 只在 Windows 上实现。
func (w *launcherWindow) capture() (*image.RGBA, error) {
	return nil, errUnsupportedPlatform
}

// moveCursor 只在 Windows 上实现。
func (w *launcherWindow) moveCursor(_, _ int) error {
	return errUnsupportedPlatform
}

// click 只在 Windows 上实现。
func (w *launcherWindow) click(_, _ int) error {
	return errUnsupportedPlatform
}

// screenPoint 只在 Windows 上实现。
func (w *launcherWindow) screenPoint(_, _ int) (int, int) {
	return 0, 0
}

// foregroundWindow 只在 Windows 上实现。
func foregroundWindow() uintptr {
	return 0
}

// restoreForegroundWindow 只在 Windows 上实现。
func restoreForegroundWindow(_ uintptr) {}
