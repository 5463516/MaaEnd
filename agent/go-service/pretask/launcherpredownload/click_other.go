//go:build !windows

package launcherpredownload

// clickPreDownload 只在 Windows 上实现。
func clickPreDownload() error {
	return errUnsupportedPlatform
}

// clickUpdateButton 只在 Windows 上实现。
func clickUpdateButton(_ *gameConfig, _ string) error {
	return errUnsupportedPlatform
}
