//go:build windows

package launcherpredownload

import (
	"errors"
	"fmt"
	"image"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// 采集图标动画相位时的采样参数。
//
// 启动器给"下载中"的箭头挂的是 `arrowLoopAnimating 2s ease-in-out 1s infinite`，
// 而这条关键帧里有大约 40% 的时间箭头**完全透明**（opacity: 0），另有 10% 静止。
// 只采两帧的话很可能双双落在透明窗口里，把"正在下载"误判成"静止"——那样就会去
// 点一下，把用户正在跑的下载暂停（实机连续复现过）。
// 因此采样必须**覆盖一整个 2 秒周期**再比较。
const (
	animationSampleInterval = 300 * time.Millisecond
	animationSampleCount    = 8 // 7 个间隔 x 300ms = 2.1s > 2s 周期
)

var (
	user32   = windows.NewLazySystemDLL("user32.dll")
	gdi32    = windows.NewLazySystemDLL("gdi32.dll")
	kernel32 = windows.NewLazySystemDLL("kernel32.dll")

	procSetProcessDpiAwarenessContext = user32.NewProc("SetProcessDpiAwarenessContext")
	procSetProcessDPIAware            = user32.NewProc("SetProcessDPIAware")
	procEnumWindows                   = user32.NewProc("EnumWindows")
	procIsWindowVisible               = user32.NewProc("IsWindowVisible")
	procGetWindowThreadProcessId      = user32.NewProc("GetWindowThreadProcessId")
	procGetClassNameW                 = user32.NewProc("GetClassNameW")
	procGetClientRect                 = user32.NewProc("GetClientRect")
	procClientToScreen                = user32.NewProc("ClientToScreen")
	procGetDC                         = user32.NewProc("GetDC")
	procReleaseDC                     = user32.NewProc("ReleaseDC")
	procGetForegroundWindow           = user32.NewProc("GetForegroundWindow")
	procSetForegroundWindow           = user32.NewProc("SetForegroundWindow")
	procSetWindowPos                  = user32.NewProc("SetWindowPos")
	procIsIconic                      = user32.NewProc("IsIconic")
	procShowWindow                    = user32.NewProc("ShowWindow")
	procSetCursorPos                  = user32.NewProc("SetCursorPos")
	procMouseEvent                    = user32.NewProc("mouse_event")
	procGetDpiForWindow               = user32.NewProc("GetDpiForWindow")

	procBitBlt                 = gdi32.NewProc("BitBlt")
	procCreateCompatibleDC     = gdi32.NewProc("CreateCompatibleDC")
	procCreateCompatibleBitmap = gdi32.NewProc("CreateCompatibleBitmap")
	procSelectObject           = gdi32.NewProc("SelectObject")
	procDeleteObject           = gdi32.NewProc("DeleteObject")
	procDeleteDC               = gdi32.NewProc("DeleteDC")
	procGetDIBits              = gdi32.NewProc("GetDIBits")

	procOpenProcess                = kernel32.NewProc("OpenProcess")
	procQueryFullProcessImageNameW = kernel32.NewProc("QueryFullProcessImageNameW")
	procCloseHandle                = kernel32.NewProc("CloseHandle")
	procTerminateProcess           = kernel32.NewProc("TerminateProcess")
	procGetProcessIoCounters       = kernel32.NewProc("GetProcessIoCounters")
)

const (
	// (DPI_AWARENESS_CONTEXT)-4 = PER_MONITOR_AWARE_V2。不声明 DPI 感知的话，
	// Windows 会把 200% 缩放下的窗口报成一半尺寸，点击落点会整体偏掉。
	dpiAwarenessContextPerMonitorAwareV2 = ^uintptr(3)

	swRestore                      = 9
	srcCopy                        = 0x00CC0020
	biRGB                          = 0
	dibRGBColors                   = 0
	processQueryLimitedInformation = 0x1000
	processTerminate               = 0x0001
	windowClassBuffer              = 256
	processPathBuffer              = 260

	// SetWindowPos：抬到 Z 序最前，且不改位置/尺寸、不抢激活。
	hwndTop       = 0
	swpNoSize     = 0x0001
	swpNoMove     = 0x0002
	swpNoActivate = 0x0010
	swpShowWindow = 0x0040

	mouseEventLeftDown = 0x0002
	mouseEventLeftUp   = 0x0004

	// 经实测：承载启动器 UI 的进程是 Games.exe，窗口类为 Qt5158QWindowIcon。
	launcherProcessName = "Games.exe"
	launcherWindowClass = "Qt5158QWindowIcon"

	// 真实窗口的客户区至少是内容区（1280x720 CSS）那么大；小于此值的都是辅助窗口。
	minWindowWidth  = 800
	minWindowHeight = 600
)

type win32Rect struct {
	Left   int32
	Top    int32
	Right  int32
	Bottom int32
}

type win32Point struct {
	X int32
	Y int32
}

type bitmapInfoHeader struct {
	Size          uint32
	Width         int32
	Height        int32
	Planes        uint16
	BitCount      uint16
	Compression   uint32
	SizeImage     uint32
	XPelsPerMeter int32
	YPelsPerMeter int32
	ClrUsed       uint32
	ClrImportant  uint32
}

type bitmapInfo struct {
	Header bitmapInfoHeader
	Colors [1]uint32
}

// launcherWindow 是一个已定位到的启动器窗口。
type launcherWindow struct {
	handle uintptr
	width  int
	height int
	scale  float64 // DPI 缩放比，96 DPI = 1.0
	origin point   // 客户区左上角在屏幕坐标系里的位置
}

// enableDPIAwareness 把当前进程声明为 per-monitor DPI 感知。
//
// 必须尽早调用：未声明时 Windows 会对窗口尺寸与鼠标坐标做虚拟化，
// 同一窗口在 150% 缩放下会被报成 1312x754 而不是 1968x1131。
func enableDPIAwareness() {
	if err := procSetProcessDpiAwarenessContext.Find(); err == nil {
		if result, _, _ := procSetProcessDpiAwarenessContext.Call(dpiAwarenessContextPerMonitorAwareV2); result != 0 {
			return
		}
	}

	// 回退到 Win8.1 之前的 API。
	if err := procSetProcessDPIAware.Find(); err == nil {
		_, _, _ = procSetProcessDPIAware.Call()
	}
}

// findLauncherWindow 在所有可见的顶层窗口里找鹰角启动器。
//
// Games.exe 同时持有多个 Qt5158QWindowIcon 顶层窗口，其中一些是 0x0 的辅助窗口；
// 因此这里**取客户区最大的那个**，而不是第一个命中的（实测踩过：第一个是 0x0）。
func findLauncherWindow() (*launcherWindow, error) {
	var (
		best     *launcherWindow
		bestArea int
		lastErr  error
	)

	callback := syscall.NewCallback(func(hwnd uintptr, _ uintptr) uintptr {
		if visible, _, _ := procIsWindowVisible.Call(hwnd); visible == 0 {
			return 1
		}

		if !strings.EqualFold(windowClass(hwnd), launcherWindowClass) {
			return 1
		}

		if !isLauncherProcess(hwnd) {
			return 1
		}

		candidate, err := describeWindow(hwnd)
		if err != nil {
			// 最小化时客户区会报 0x0，先还原再量一次。
			if iconic, _, _ := procIsIconic.Call(hwnd); iconic != 0 {
				_, _, _ = procShowWindow.Call(hwnd, swRestore)

				candidate, err = describeWindow(hwnd)
			}

			if err != nil {
				if lastErr == nil {
					lastErr = err
				}

				return 1
			}
		}

		if candidate.width < minWindowWidth || candidate.height < minWindowHeight {
			return 1
		}

		if area := candidate.width * candidate.height; area > bestArea {
			best = candidate
			bestArea = area
		}

		return 1
	})

	_, _, _ = procEnumWindows.Call(callback, 0)

	if best != nil {
		return best, nil
	}

	if lastErr != nil {
		return nil, lastErr
	}

	return nil, errors.New(
		"launcherpredownload: launcher window not found, make sure the Hypergryph Launcher is running and not minimised",
	)
}

// raiseToFront 还原（若最小化）并把窗口抬到 Z 序最前。
//
// 光靠 SetForegroundWindow 不够：Windows 只允许"当前前台进程"抢前台，而本进程
// 是被客户端拉起来的子进程，通常没这个资格，调用会**静默失败**，屏幕 DC 拍到
// 的就是压在上面的窗口（实机踩过：抓到的帧里没有圆钮）。
// SetWindowPos(HWND_TOP) 不需要前台权限，能把窗口抬到其他普通窗口之上。
//
// 是否真的拍到目标，不靠这里的返回值判断——调用方用"识别圆钮"来验证。
func (w *launcherWindow) raiseToFront() {
	if iconic, _, _ := procIsIconic.Call(w.handle); iconic != 0 {
		_, _, _ = procShowWindow.Call(w.handle, swRestore)
	}

	_, _, _ = procSetWindowPos.Call(
		w.handle, hwndTop, 0, 0, 0, 0,
		swpNoMove|swpNoSize|swpNoActivate|swpShowWindow,
	)

	_, _, _ = procSetForegroundWindow.Call(w.handle)
}

// capture 抓取客户区像素。
//
// 只能用屏幕 DC 做 BitBlt：对这个 Qt WebEngine 窗口，PrintWindow 在普通权限与
// 提权下都返回全黑（前次实测 5 帧，无一例外）。
func (w *launcherWindow) capture() (*image.RGBA, error) {
	screenDC, _, _ := procGetDC.Call(0)
	if screenDC == 0 {
		return nil, errors.New("launcherpredownload: GetDC failed")
	}
	defer func() {
		_, _, _ = procReleaseDC.Call(0, screenDC)
	}()

	memoryDC, _, _ := procCreateCompatibleDC.Call(screenDC)
	if memoryDC == 0 {
		return nil, errors.New("launcherpredownload: CreateCompatibleDC failed")
	}
	defer func() {
		_, _, _ = procDeleteDC.Call(memoryDC)
	}()

	bitmap, _, _ := procCreateCompatibleBitmap.Call(screenDC, uintptr(w.width), uintptr(w.height))
	if bitmap == 0 {
		return nil, errors.New("launcherpredownload: CreateCompatibleBitmap failed")
	}
	defer func() {
		_, _, _ = procDeleteObject.Call(bitmap)
	}()

	previous, _, _ := procSelectObject.Call(memoryDC, bitmap)
	defer func() {
		_, _, _ = procSelectObject.Call(memoryDC, previous)
	}()

	ok, _, _ := procBitBlt.Call(
		memoryDC, 0, 0, uintptr(w.width), uintptr(w.height),
		screenDC, uintptr(int(w.origin.X)), uintptr(int(w.origin.Y)), srcCopy,
	)
	if ok == 0 {
		return nil, errors.New("launcherpredownload: BitBlt failed")
	}

	info := bitmapInfo{}
	info.Header.Size = uint32(unsafe.Sizeof(bitmapInfoHeader{}))
	info.Header.Width = int32(w.width)
	info.Header.Height = -int32(w.height) // 负高度表示自上而下
	info.Header.Planes = 1
	info.Header.BitCount = 32
	info.Header.Compression = biRGB

	pixels := make([]byte, w.width*w.height*4)

	lines, _, _ := procGetDIBits.Call(
		memoryDC, bitmap, 0, uintptr(w.height),
		uintptr(unsafe.Pointer(&pixels[0])),
		uintptr(unsafe.Pointer(&info)),
		dibRGBColors,
	)
	if lines == 0 {
		return nil, errors.New("launcherpredownload: GetDIBits failed")
	}

	img := image.NewRGBA(image.Rect(0, 0, w.width, w.height))
	for i := 0; i+3 < len(pixels); i += 4 {
		img.Pix[i] = pixels[i+2] // BGRA -> RGBA
		img.Pix[i+1] = pixels[i+1]
		img.Pix[i+2] = pixels[i]
		img.Pix[i+3] = 0xFF
	}

	return img, nil
}

// discAnimating 判断圆钮里的图标是否正在动。
//
// 动 = 启动器正在预下载（源码给它挂了循环动画）。此时**不能点**：那会把用户
// 正在跑的下载暂停。反过来，"可点击开始/继续"的图标是静止的。
//
// 采样覆盖整个动画周期再交给 glyphSequenceAnimating 判断。
func (w *launcherWindow) discAnimating(disc circle) bool {
	glyphs := make([]string, 0, animationSampleCount)

	for index := 0; index < animationSampleCount; index++ {
		if index > 0 {
			// 采样一个已知周期的循环动画，必须隔开一段时间才能采到不同相位。
			time.Sleep(animationSampleInterval)
		}

		frame, err := w.capture()
		if err != nil {
			return false
		}

		glyphs = append(glyphs, discGlyph(frame, disc))
	}

	return glyphSequenceAnimating(glyphs)
}

// processIOCounters 对应 Win32 的 IO_COUNTERS。
type processIOCounters struct {
	ReadOperationCount  uint64
	WriteOperationCount uint64
	ReadTransferCount   uint64
	WriteTransferCount  uint64
	OtherOperationCount uint64
	OtherTransferCount  uint64
}

// writtenBytes 返回该窗口所属进程的累计写入字节数；取不到时返回 0。
//
// 用来判断更新是否还在推进：下载 / 校验 / 解压 / 安装都会持续写入，
// 而真正卡住的进程几乎不写。
func (w *launcherWindow) writtenBytes() uint64 {
	var pid uint32

	_, _, _ = procGetWindowThreadProcessId.Call(w.handle, uintptr(unsafe.Pointer(&pid)))
	if pid == 0 {
		return 0
	}

	handle, _, _ := procOpenProcess.Call(processQueryLimitedInformation, 0, uintptr(pid))
	if handle == 0 {
		return 0
	}
	defer func() {
		_, _, _ = procCloseHandle.Call(handle)
	}()

	var counters processIOCounters

	if ok, _, _ := procGetProcessIoCounters.Call(handle, uintptr(unsafe.Pointer(&counters))); ok == 0 {
		return 0
	}

	return counters.WriteTransferCount
}

// terminate 结束承载该窗口的进程。
//
// 更新完成后启动器就没用了，留着它只会挡在游戏窗口前面。需要与启动器同为
// 管理员权限：否则 OpenProcess(PROCESS_TERMINATE) 会被拒绝——那种情况下由
// 调用方按"只记日志"处理，不影响后续任务。
func (w *launcherWindow) terminate() error {
	var pid uint32

	_, _, _ = procGetWindowThreadProcessId.Call(w.handle, uintptr(unsafe.Pointer(&pid)))
	if pid == 0 {
		return errors.New("launcherpredownload: cannot resolve the launcher process id")
	}

	handle, _, _ := procOpenProcess.Call(processTerminate, 0, uintptr(pid))
	if handle == 0 {
		return errors.New(
			"launcherpredownload: cannot open the launcher process for termination, " +
				"run MaaEnd as administrator to have it closed automatically",
		)
	}
	defer func() {
		_, _, _ = procCloseHandle.Call(handle)
	}()

	if ok, _, _ := procTerminateProcess.Call(handle, 0); ok == 0 {
		return errors.New("launcherpredownload: TerminateProcess failed")
	}

	return nil
}

// moveCursor 把光标移到客户区坐标 (x, y)，**不点击**。
//
// 失败几乎只有一个原因：鹰角启动器以管理员身份运行，而本进程没有。Windows 的
// UIPI 禁止低完整性级别进程向高完整性级别窗口注入输入，此时 SetCursorPos 会
// 直接返回 0。PostMessage / UI Automation 同样会被拦，没有绕过办法。
func (w *launcherWindow) moveCursor(x, y int) error {
	screenX, screenY := w.screenPoint(x, y)

	if ok, _, _ := procSetCursorPos.Call(uintptr(screenX), uintptr(screenY)); ok == 0 {
		return fmt.Errorf(
			"launcherpredownload: cannot move the cursor to (%d, %d): "+
				"the launcher is running as administrator, so MaaEnd must run as administrator too "+
				"to click it (detection itself works without elevation)",
			screenX, screenY,
		)
	}

	return nil
}

// click 把光标移到客户区坐标 (x, y) 并左键单击一次。
func (w *launcherWindow) click(x, y int) error {
	if err := w.moveCursor(x, y); err != nil {
		return err
	}

	_, _, _ = procMouseEvent.Call(mouseEventLeftDown, 0, 0, 0, 0)
	_, _, _ = procMouseEvent.Call(mouseEventLeftUp, 0, 0, 0, 0)

	return nil
}

// screenPoint 把客户区坐标换算成屏幕坐标。
func (w *launcherWindow) screenPoint(x, y int) (int, int) {
	return int(w.origin.X) + x, int(w.origin.Y) + y
}

// foregroundWindow 返回当前前台窗口句柄，供操作完成后恢复。
func foregroundWindow() uintptr {
	handle, _, _ := procGetForegroundWindow.Call()

	return handle
}

// restoreForegroundWindow 还原之前保存的前台窗口。
func restoreForegroundWindow(handle uintptr) {
	if handle == 0 {
		return
	}

	_, _, _ = procSetForegroundWindow.Call(handle)
}

// describeWindow 把窗口句柄折算成 launcherWindow：客户区尺寸、左上角屏幕坐标与 DPI 缩放。
func describeWindow(hwnd uintptr) (*launcherWindow, error) {
	var rect win32Rect
	if ok, _, _ := procGetClientRect.Call(hwnd, uintptr(unsafe.Pointer(&rect))); ok == 0 {
		return nil, errors.New("launcherpredownload: GetClientRect failed")
	}

	origin := win32Point{}
	if ok, _, _ := procClientToScreen.Call(hwnd, uintptr(unsafe.Pointer(&origin))); ok == 0 {
		return nil, errors.New("launcherpredownload: ClientToScreen failed")
	}

	width := int(rect.Right - rect.Left)
	height := int(rect.Bottom - rect.Top)
	if width <= 0 || height <= 0 {
		return nil, fmt.Errorf(
			"launcherpredownload: launcher client area is empty (%dx%d)", width, height,
		)
	}

	return &launcherWindow{
		handle: hwnd,
		width:  width,
		height: height,
		scale:  windowScale(hwnd),
		origin: point{X: float64(origin.X), Y: float64(origin.Y)},
	}, nil
}

// windowScale 返回该窗口的 DPI 缩放比（96 DPI = 1.0）。
//
// 必须用 GetDpiForWindow 而不是"客户区宽 / 内容区宽"：用户把窗口拉大之后，
// 后者会算出一个偏大的比例，圆心会整体偏出去（实机踩过）。
func windowScale(hwnd uintptr) float64 {
	if err := procGetDpiForWindow.Find(); err == nil {
		if dpi, _, _ := procGetDpiForWindow.Call(hwnd); dpi != 0 {
			return float64(dpi) / 96.0
		}
	}

	return 1.0
}

// windowClass 取窗口类名，用来识别 Qt 窗口。
func windowClass(hwnd uintptr) string {
	buffer := make([]uint16, windowClassBuffer)

	length, _, _ := procGetClassNameW.Call(
		hwnd, uintptr(unsafe.Pointer(&buffer[0])), uintptr(len(buffer)),
	)
	if length == 0 {
		return ""
	}

	return windows.UTF16ToString(buffer[:length])
}

// isLauncherProcess 判断窗口是否属于鹰角启动器的进程（Games.exe）。
func isLauncherProcess(hwnd uintptr) bool {
	var pid uint32

	_, _, _ = procGetWindowThreadProcessId.Call(hwnd, uintptr(unsafe.Pointer(&pid)))
	if pid == 0 {
		return false
	}

	handle, _, _ := procOpenProcess.Call(processQueryLimitedInformation, 0, uintptr(pid))
	if handle == 0 {
		return false
	}
	defer func() {
		_, _, _ = procCloseHandle.Call(handle)
	}()

	buffer := make([]uint16, processPathBuffer)
	size := uint32(len(buffer))

	ok, _, _ := procQueryFullProcessImageNameW.Call(
		handle, 0, uintptr(unsafe.Pointer(&buffer[0])), uintptr(unsafe.Pointer(&size)),
	)
	if ok == 0 {
		return false
	}

	return strings.EqualFold(filepath.Base(windows.UTF16ToString(buffer[:size])), launcherProcessName)
}
