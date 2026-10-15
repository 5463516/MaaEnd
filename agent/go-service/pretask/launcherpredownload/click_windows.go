//go:build windows

package launcherpredownload

import (
	"errors"
	"fmt"
	"image"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/MaaXYZ/MaaEnd/agent/go-service/taskersink/hdrcheck"
	"github.com/rs/zerolog/log"
	"golang.org/x/sys/windows/registry"
)

const (
	// 各阶段超时：都只用于"等异步界面出现"，满足条件立刻继续。
	launcherStartTimeout = 30 * time.Second
	// locateTimeout 要留足冷启动余量：刚拉起的启动器窗口会先出现、网页后渲染，
	// 实测有过窗口出来 11 秒还画不出圆钮的情况（10 秒会误判成"没有预下载"）。
	locateTimeout     = 25 * time.Second
	dialogWaitTimeout = 15 * time.Second
	dialogGoneTimeout = 15 * time.Second
	pollInterval      = 400 * time.Millisecond

	// 更新可能要下十几 GiB，因此**不能**按固定时长赌一把，而是按实际进度等：
	// 只要还在推进就继续等，长时间没有任何变化才判定卡住。
	updatePollInterval     = 3 * time.Second
	updateProgressInterval = time.Minute
	updateStallTimeout     = 20 * time.Minute
	updateHardTimeout      = 6 * time.Hour

	// 启动器把下载与解压的暂存文件都放在游戏目录下的这个子目录里。
	stagingDirName = "launcher_tmp"

	// 允许的定位偏差（CSS px）：模型实测误差不到 2px，留 12px 足够，
	// 又远小于圆钮到主按钮的 12px 间隙……所以 findDisc 内部还会再收窄一次。
	positionToleranceCSS = 12.0

	launcherRegistryRoot = `Software\Hypergryph\Launcher`
	launcherInstallValue = "install_path"
	launcherExecutable   = "Launcher.exe"
)

// clickPreDownload 拉起（或复用）鹰角启动器，点掉预下载圆钮，再点掉确认弹窗。
//
// 全程按"识别 -> 操作 -> 再识别"推进：每一步动手前先确认目标在预期位置，
// 动完手再轮询确认界面真的变了，任何一环不成立就放弃，绝不盲点。
func clickPreDownload() error {
	// 规范提示：HDR / "自动管理应用的颜色" 会影响取色准确性，模板容易与用户实际
	// 显示不符。这里**只提示、不跳过**：颜色判据本身很宽松（饱和黄底，另叠形状、
	// 尺寸、位置、填充率多重校验），实测在 HDR 开启的机器上依然稳定命中；而万一
	// 真的选不中，结果也只是"跳过"，不会误点——与直接跳过同样安全，却不会无谓地
	// 废掉可用场景。
	if hdr, err := hdrcheck.IsHDREnabled(); err != nil {
		log.Debug().
			Err(err).
			Str("component", componentName).
			Msg("failed to query the display HDR status, continuing")
	} else if hdr {
		log.Warn().
			Str("component", componentName).
			Msg("display HDR is enabled, colour-based button detection may be less reliable")
	}

	// 必须最早声明 DPI 感知：否则拿到的客户区尺寸与屏幕坐标都是被系统虚拟化过的
	// （实测 150% 缩放下会报 1312x754 而不是 1968x1131），抓帧与点击会整体错位。
	// 本进程不创建窗口，因此在这里调用仍然有效。
	enableDPIAwareness()

	previous := foregroundWindow()
	defer restoreForegroundWindow(previous)

	win, err := ensureLauncherWindow()
	if err != nil {
		return err
	}

	l := newLayout(win.width, win.height, win.scale)
	tolerance := positionToleranceCSS * win.scale

	// 抬窗是否成功不做假设，靠"能不能认出圆钮"来验证：抓到的帧里没有圆钮
	// 就再抬一次重试（窗口可能还没重绘，或者被别的窗口压着）。
	var (
		disc       circle
		glyphDelta = -1
	)

	// Windows 会拒绝低完整性级别进程对提权窗口做 z 序调整——实测 SetWindowPos
	// 与 BringWindowToTop 都返回 ACCESS_DENIED，也就是说 raiseToFront 未必能把
	// 压在别的窗口后面的启动器翻上来。实测有效的一招是**再跑一次 Launcher.exe**：
	// 单实例机制会把已有窗口带到前台。只在第一次没认出来时试一次，避免反复拉起。
	poked := false

	located := waitFor(locateTimeout, pollInterval, func() bool {
		win.raiseToFront()

		found, delta, ok := locateDisc(win, l, tolerance)
		glyphDelta = delta

		if !ok {
			if !poked {
				poked = true

				if err := startLauncher(); err != nil {
					log.Warn().
						Err(err).
						Str("component", componentName).
						Msg("failed to nudge the launcher window to the front")
				}
			}

			return false
		}

		disc = found

		return true
	})
	if !located {
		// 认不出圆钮、或图标不是"可点击预下载"，都属于"无事可做"：用户没开
		// 预下载、或启动器界面并非预期状态。返回哨兵错误让上层按"跳过"记日志，
		// 而不是报成故障。
		if glyphDelta >= 0 {
			return fmt.Errorf(
				"%w (the button shows a different state, glyph differs by %d cells)",
				errNoPreDownloadButton, glyphDelta,
			)
		}

		return errNoPreDownloadButton
	}

	// 下载正在进行时不能点：那会把用户的下载暂停。启动器给下载中的箭头挂了
	// 循环动画，比较两帧就能识别，不必认识具体图标。
	if win.discAnimating(disc) {
		log.Info().
			Str("component", componentName).
			Msg("a pre-download is already running, leaving it alone")

		return nil
	}

	// 动画采样跨了 2.1 秒（要覆盖一整个动画周期），这期间窗口可能被挪动、界面也
	// 可能变化。按"识别 -> 操作 -> 再识别"，点击前必须**重新识别一次**，而不是
	// 沿用两秒前的坐标——顺带也把"这两秒里预下载恰好完成"这种情况挡掉。
	fresh, freshDelta, ok := locateDisc(win, l, tolerance)
	if !ok {
		if freshDelta >= 0 {
			return fmt.Errorf(
				"%w (the button changed state while the animation was sampled, glyph differs by %d cells)",
				errNoPreDownloadButton, freshDelta,
			)
		}

		return errNoPreDownloadButton
	}

	disc = fresh

	log.Info().
		Str("component", componentName).
		Int("x", int(disc.Center.X)).
		Int("y", int(disc.Center.Y)).
		Msg("clicking the pre-download button")

	if err := win.click(int(disc.Center.X), int(disc.Center.Y)); err != nil {
		return err
	}

	var dialog *image.RGBA

	appeared := waitFor(dialogWaitTimeout, pollInterval, func() bool {
		shot, captureErr := win.capture()
		if captureErr != nil {
			return false
		}

		_, hasConfirm := findConfirmButton(shot, l.confirmButton(), tolerance)
		if hasConfirm && hasDialogBackdrop(shot, l.dialogCard()) {
			dialog = shot

			return true
		}

		return false
	})
	if !appeared {
		// 没有确认弹窗有两种可能：一是预下载本来就是"已暂停"，这一下点击只是
		// 继续下载（拖动条会立刻开始跑）；二是界面状态出乎预期。用"图标是否
		// 开始动"来区分，避免把前者误报成失败。
		if win.discAnimating(disc) {
			log.Info().
				Str("component", componentName).
				Msg("pre-download resumed without a confirmation dialog")

			return nil
		}

		return errors.New("launcherpredownload: the confirmation dialog never appeared after clicking")
	}

	if err := confirmDialog(win, dialog, l, tolerance); err != nil {
		return err
	}

	log.Info().
		Str("component", componentName).
		Msg("pre-download has been triggered")

	return nil
}

// locateDisc 做一轮"取帧 -> 定位圆钮 -> 校验图标确实是可点击的预下载"。
//
// 抽出来是为了满足「识别 -> 操作 -> 再识别」：点击必须基于**刚刚**识别出来的
// 坐标，所以定位循环与点击之前各调一次。
//
// 返回的 delta 是图标指纹差异：notFoundGlyphDelta 表示圆钮压根没找到，
// 其余值表示找到了但图标与"可点击预下载"的基准不符（例如预下载已完成，
// 图标换成了云朵加叉）。
func locateDisc(win *launcherWindow, l layout, tolerance float64) (circle, int, bool) {
	shot, err := win.capture()
	if err != nil {
		return circle{}, notFoundGlyphDelta, false
	}

	found, ok := findDisc(shot, l.disc(), tolerance)
	if !ok {
		return circle{}, notFoundGlyphDelta, false
	}

	delta := glyphDifference(discGlyph(shot, found), downloadGlyph)
	if delta > downloadGlyphTolerance {
		return circle{}, delta, false
	}

	return found, delta, true
}

// clickUpdateButton 拉起（或复用）鹰角启动器，点掉右下角的主按钮（此时显示
// 「更新游戏」），再点掉确认弹窗。
//
// 与预下载流程同构，区别只在目标是**主按钮**而不是圆钮：真机六张截图与前端
// 状态机都证实，更新态下启动器根本不渲染那颗圆钮。
func clickUpdateButton(cfg *gameConfig, targetVersion string) error {
	// 必须最早声明 DPI 感知，且**不能**依赖 clickPreDownload —— pickAutoAction 在
	// 接口说"需要更新"时会直接进这里，压根不经过预下载那条路。少了这一句，
	// Windows 会把客户区尺寸与鼠标坐标都虚拟化（150% 缩放下报 1312x754 而不是
	// 1968x1131），每一次点击都会偏。
	enableDPIAwareness()

	previous := foregroundWindow()
	defer restoreForegroundWindow(previous)

	win, err := ensureLauncherWindow()
	if err != nil {
		return err
	}

	l := newLayout(win.width, win.height, win.scale)
	tolerance := positionToleranceCSS * win.scale

	var button box

	poked := false

	located := waitFor(locateTimeout, pollInterval, func() bool {
		win.raiseToFront()

		shot, captureErr := win.capture()
		if captureErr != nil {
			return false
		}

		found, ok := findMainButton(shot, l.mainButton(), tolerance)
		if !ok {
			if !poked {
				poked = true

				if nudgeErr := startLauncher(); nudgeErr != nil {
					log.Warn().
						Err(nudgeErr).
						Str("component", componentName).
						Msg("failed to nudge the launcher window to the front")
				}
			}

			return false
		}

		button = found

		return true
	})
	if !located {
		return errNoUpdateButton
	}

	log.Info().
		Str("component", componentName).
		Int("x", int(button.Center().X)).
		Int("y", int(button.Center().Y)).
		Msg("clicking the main button to start the update")

	if err := win.click(int(button.Center().X), int(button.Center().Y)); err != nil {
		return err
	}

	var dialog *image.RGBA

	appeared := waitFor(dialogWaitTimeout, pollInterval, func() bool {
		shot, captureErr := win.capture()
		if captureErr != nil {
			return false
		}

		_, hasConfirm := findConfirmButton(shot, l.confirmButton(), tolerance)
		if hasConfirm && hasDialogBackdrop(shot, l.dialogCard()) {
			dialog = shot

			return true
		}

		return false
	})
	if !appeared {
		return errors.New("launcherpredownload: the update confirmation dialog never appeared after clicking")
	}

	if err := confirmDialog(win, dialog, l, tolerance); err != nil {
		return err
	}

	log.Info().
		Str("component", componentName).
		Str("target_version", targetVersion).
		Msg("game update has been started, waiting for it to finish")

	if !awaitUpdateComplete(win, cfg, targetVersion) {
		// 没等到结束就别动启动器——更新很可能还在下载或安装，杀掉它等于打断
		// 用户的更新。留在原地，让它在后台继续跑完。
		log.Warn().
			Str("component", componentName).
			Msg("leaving the launcher running because the update has not finished")

		return nil
	}

	// 更新完成后主按钮会变回「开始游戏」，启动器窗口已经没用了；留着它只会挡在
	// 游戏窗口前面，所以顺手关掉。
	if err := win.terminate(); err != nil {
		log.Warn().
			Err(err).
			Str("component", componentName).
			Msg("failed to close the launcher, continuing anyway")
	} else {
		log.Info().
			Str("component", componentName).
			Msg("launcher closed")
	}

	return nil
}

// awaitUpdateComplete 等到更新真正结束，返回是否等到。
//
// 完成判据用**本地版本号**而不是按钮上的文字：更新过程中主按钮会依次显示
// 下载中 / 校验中 / 解压中 / 安装中，那是文案，既依赖语言又得上 OCR；而
// config.ini 里的版本号是确定性的、与语言无关，而且本包本来就会解密它。
// 连续两次读到目标版本才算完成，避免读到写入中途的中间态。
//
// 等待时长**按实际进度自适应**，不按网速拍一个固定值：只要暂存区字节数或
// 启动器进程的写入量还在变，就认为更新仍在推进、继续等下去；连续
// updateStallTimeout 没有任何推进才判定卡住。这样百兆宽带和十几兆宽带都能
// 等到真正结束，而真正卡死（例如网络断了）时又不会永远挂着。
func awaitUpdateComplete(win *launcherWindow, cfg *gameConfig, targetVersion string) bool {
	targetVersion = strings.TrimSpace(targetVersion)

	if cfg == nil || cfg.Dir == "" || targetVersion == "" {
		log.Warn().
			Str("component", componentName).
			Msg("no local game directory or target version, cannot wait for the update")

		return false
	}

	watcher := &updateWatcher{target: targetVersion}
	previous := probeUpdateProgress(win, cfg.Dir)

	var (
		deadline     = time.Now().Add(updateHardTimeout)
		lastProgress = time.Now()
		announced    = time.Now()
	)

	for {
		// 轮询间隔：更新由启动器这个**外部进程**推进，没有任何事件可以订阅，
		// 只能定期去看一眼。这不是"用延迟掩盖问题"——完成与否由版本号独立判定。
		time.Sleep(updatePollInterval)

		current, readErr := readGameConfig(cfg.Dir)
		if readErr == nil && watcher.observe(current.Version) {
			log.Info().
				Str("component", componentName).
				Str("installed_version", targetVersion).
				Msg("game update finished")

			return true
		}

		sample := probeUpdateProgress(win, cfg.Dir)
		if sample.progressed(previous) {
			previous = sample
			lastProgress = time.Now()
		} else if time.Since(lastProgress) >= updateStallTimeout {
			log.Warn().
				Str("component", componentName).
				Dur("stalled_for", updateStallTimeout).
				Msg("the update made no progress for a while, giving up")

			return false
		}

		if time.Since(deadline) > 0 {
			log.Warn().
				Str("component", componentName).
				Dur("waited", updateHardTimeout).
				Msg("gave up waiting for the update after the hard timeout")

			return false
		}

		if time.Since(announced) >= updateProgressInterval {
			announced = time.Now()

			installed := ""

			if readErr == nil {
				installed = current.Version
			}

			log.Info().
				Str("component", componentName).
				Str("installed_version", installed).
				Str("target_version", targetVersion).
				Msg("still updating")
		}
	}
}

// probeUpdateProgress 采一次"更新是否还在推进"。
//
// 两个信号互补：暂存区大小覆盖下载与解压；启动器进程的写入量覆盖校验与安装
// （那两个阶段主要是在游戏目录里读写，暂存区未必变化）。
func probeUpdateProgress(win *launcherWindow, gameDir string) updateProgressSample {
	return updateProgressSample{
		stagingBytes: stagingBytes(gameDir),
		writtenBytes: win.writtenBytes(),
	}
}

// stagingBytes 返回游戏目录下启动器暂存区的总字节数。
func stagingBytes(gameDir string) uint64 {
	var total uint64

	_ = filepath.WalkDir(filepath.Join(gameDir, stagingDirName), func(_ string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return nil
		}

		if info, infoErr := entry.Info(); infoErr == nil {
			total += uint64(info.Size())
		}

		return nil
	})

	return total
}

// confirmDialog 点掉已经识别到的「系统提示」弹窗，并等它消失。
//
// 预下载与更新两条流程共用：两者的确认弹窗是同一套布局（同一张 460x254 卡片、
// 同一颗 202x40 的白色「确认」；实测更新弹窗的中心偏差不到 4px）。
func confirmDialog(win *launcherWindow, dialog *image.RGBA, l layout, tolerance float64) error {
	confirm, ok := findConfirmButton(dialog, l.confirmButton(), tolerance)
	if !ok {
		// 卡片高度可能随文案行数变化，导致「确认」偏离期望位置；放宽**位置**搜索域
		// 再试一次。白药丸的形状、尺寸与填充率判据不变，仍然只有它会被认出来。
		confirm, ok = findConfirmButton(dialog, l.confirmButton(), confirmFallbackToleranceCSS*l.scale)
	}

	if !ok {
		return errors.New("launcherpredownload: confirmation button vanished before clicking")
	}

	log.Info().
		Str("component", componentName).
		Int("x", int(confirm.Center().X)).
		Int("y", int(confirm.Center().Y)).
		Msg("clicking the confirm button of the dialog")

	if err := win.click(int(confirm.Center().X), int(confirm.Center().Y)); err != nil {
		return err
	}

	closed := waitFor(dialogGoneTimeout, pollInterval, func() bool {
		shot, captureErr := win.capture()
		if captureErr != nil {
			return false
		}

		_, hasConfirm := findConfirmButton(shot, l.confirmButton(), tolerance)

		return !hasConfirm || !hasDialogBackdrop(shot, l.dialogCard())
	})
	if !closed {
		return errors.New("launcherpredownload: the confirmation dialog did not close after confirming")
	}

	return nil
}

// ensureLauncherWindow 复用已有的启动器窗口；没有就拉起一个再等它出现。
//
// 必须自己拉：PI 协议规定预任务跑在 Client 的"前置程序"之前，此刻启动器通常还没起来。
func ensureLauncherWindow() (*launcherWindow, error) {
	if win, err := findLauncherWindow(); err == nil {
		return win, nil
	}

	if err := startLauncher(); err != nil {
		return nil, err
	}

	var win *launcherWindow

	started := waitFor(launcherStartTimeout, pollInterval, func() bool {
		found, err := findLauncherWindow()
		if err != nil {
			return false
		}

		win = found

		return true
	})
	if !started {
		return nil, errors.New(
			"launcherpredownload: launched the launcher but no usable window appeared, " +
				"if it is minimised, restore it manually",
		)
	}

	return win, nil
}

// startLauncher 从注册表读出启动器安装路径并把它拉起来。
func startLauncher() error {
	key, err := registry.OpenKey(registry.CURRENT_USER, launcherRegistryRoot, registry.ENUMERATE_SUB_KEYS)
	if err != nil {
		return fmt.Errorf("launcherpredownload: open %s: %w", launcherRegistryRoot, err)
	}
	defer key.Close()

	names, err := key.ReadSubKeyNames(-1)
	if err != nil {
		return fmt.Errorf("launcherpredownload: enumerate %s: %w", launcherRegistryRoot, err)
	}

	for _, name := range names {
		executable, ok := launcherExecutableUnder(launcherRegistryRoot + `\` + name)
		if !ok {
			continue
		}

		command := exec.Command(executable)
		command.Dir = filepath.Dir(executable)

		if err := command.Start(); err != nil {
			return fmt.Errorf("launcherpredownload: start %s: %w", executable, err)
		}

		log.Info().
			Str("component", componentName).
			Str("executable", executable).
			Msg("started the Hypergryph Launcher")

		return nil
	}

	return fmt.Errorf("launcherpredownload: no launcher install path found under %s", launcherRegistryRoot)
}

// launcherExecutableUnder 读取某个子键下的 install_path 并拼出启动器可执行文件路径。
func launcherExecutableUnder(path string) (string, bool) {
	key, err := registry.OpenKey(registry.CURRENT_USER, path, registry.QUERY_VALUE)
	if err != nil {
		return "", false
	}
	defer key.Close()

	installPath, _, err := key.GetStringValue(launcherInstallValue)
	if err != nil || installPath == "" {
		return "", false
	}

	executable := filepath.Join(installPath, launcherExecutable)
	if _, err := os.Stat(executable); err != nil {
		return "", false
	}

	return executable, true
}
