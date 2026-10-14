// Package launcherpredownload 实现 `--pretask LauncherPreDownload`：在任务开始前
// 查询鹰角启动器是否有**预下载**可用、或游戏**需要更新**，并把结果写进日志。
//
// 定位：这是一个 ProjectInterface V2 的 pretask，由 Client 在创建/连接 Controller
// **之前**作为独立子进程执行，不连接 MaaFramework Agent Socket，因此拿不到
// ctx / tasker / controller，也就不能截图或跑 Pipeline；反馈通道只有退出码与日志。
//
// 它**永远返回 true**：本功能是锦上添花的前置检查，配置缺失、本地文件读不出、
// 接口不可达、乃至真的检测到更新，任何一种情况都不应该让用户的日常任务跑不成。
package launcherpredownload

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/rs/zerolog/log"
)

const componentName = "launcherpredownload"

// optionValue 对应 Client 追加在 args 末尾的 option JSON。
// switch / select 序列化为 case.name 字符串；input 序列化为输入字段名到字符串值的对象。
type optionValue struct {
	Enable     string    `json:"LauncherPreDownloadEnable"`
	GamePath   inputPath `json:"LauncherPreDownloadGamePath"`
	AutoAction string    `json:"LauncherPreDownloadAutoAction"`
}

type inputPath struct {
	Path string `json:"Path"`
}

// LauncherPreDownloadAutoAction 的取值。默认只记日志，绝不擅自替用户触发下载。
const (
	autoActionLogOnly     = "LogOnly"
	autoActionPreDownload = "PreDownload"
)

// waitFor 轮询直到条件成立或超时。
//
// 用它替代固定 sleep：条件一旦满足立刻继续，慢机器上也只是多轮几次；
// 而固定延迟在慢机器上照样失败，还会把"其实没生效"伪装成"已经生效"。
func waitFor(timeout, interval time.Duration, condition func() bool) bool {
	deadline := time.Now().Add(timeout)

	for {
		if condition() {
			return true
		}

		if !time.Now().Before(deadline) {
			return false
		}

		time.Sleep(interval)
	}
}

// verdict 是检测结论。
type verdict int

const (
	verdictUpToDate verdict = iota
	verdictPreDownload
	verdictUpdateRequired
	verdictUnknown
)

// errNoPreDownloadButton 表示启动器界面上没有可点的预下载圆钮。
//
// 这属于"无事可做"而不是故障：用户本来就没开预下载、或者启动器根本没起，
// 都会走到这里。调用方据此把"跳过"与"失败"分开记日志。
var errNoPreDownloadButton = errors.New("launcherpredownload: the launcher shows no pre-download button")

// errNoUpdateButton 表示右下角主按钮不在预期状态。更新态下它显示「更新游戏」，
// 平时是「开始游戏」——认不出就跳过，绝不盲点。
var errNoUpdateButton = errors.New("launcherpredownload: the launcher shows no update button")

// isEnabled 判断「启用更新检测」开关是否打开；缺失或非法一律按关闭处理。
func isEnabled(value string) bool {
	return strings.EqualFold(strings.TrimSpace(value), "Yes")
}

// Run 是 pretask 入口，签名需匹配 pretask.Handler。
//
// 永远返回 true：本功能是锦上添花的前置检查，配置缺失、本地文件读不出、
// 接口不可达、乃至真的检测到更新，任何一种情况都不该让用户的日常任务跑不成。
func Run(args []string) bool {
	opts := parseOptions(args)

	// 联网查询与窗口操作是两件独立的事：关掉联网只是不做接口查询，如果用户选了
	// 自动操作，仍然会去识别启动器画面并触发更新——只是少了接口这层门禁，
	// 改由"认不认得出圆钮"来把关。
	var (
		cfg    *gameConfig
		latest *latestGame
	)

	if isEnabled(opts.Enable) {
		cfg, latest = checkOnline(strings.TrimSpace(opts.GamePath.Path))
	} else {
		log.Info().
			Str("component", componentName).
			Msg("online update check is disabled, skipping the API query")
	}

	if err := runAutoAction(opts.AutoAction, cfg, latest); err != nil {
		log.Warn().
			Err(err).
			Str("component", componentName).
			Str("auto_action", opts.AutoAction).
			Msg("automatic launcher action failed, the launcher can still be used manually")
	}

	return true
}

// checkOnline 读本地配置、查询官方接口并记日志，返回接口结论供自动操作做门禁。
//
// 任何一步失败都只记日志并返回 nil——没有结论时自动操作改由窗口识别把关。
func checkOnline(gameDir string) (*gameConfig, *latestGame) {
	gameDir = strings.TrimSpace(gameDir)
	if gameDir == "" {
		log.Warn().
			Str("component", componentName).
			Msg("game install directory is not set, skipping the online check; fill it in the option")

		return nil, nil
	}

	cfg, err := readGameConfig(gameDir)
	if err != nil {
		log.Warn().
			Err(err).
			Str("component", componentName).
			Str("game_dir", gameDir).
			Msg("failed to read the local game config, skipping the online check")

		return nil, nil
	}

	log.Info().
		Str("component", componentName).
		Str("installed_version", cfg.Version).
		Str("region", cfg.Region).
		Str("channel", cfg.Channel).
		Str("sub_channel", cfg.SubChannel).
		Msg("local game config loaded")

	latest, err := fetchLatest(cfg)
	if err != nil {
		log.Warn().
			Err(err).
			Str("component", componentName).
			Str("endpoint", cfg.LauncherURL).
			Msg("failed to query the launcher API, skipping")

		return nil, nil
	}

	report(cfg, latest)

	return cfg, latest
}

// updateStableSamples 是判定"更新已完成"所需的连续命中次数。
const updateStableSamples = 2

// notFoundGlyphDelta 表示连按钮本身都没找到（区别于"找到了但图标不符"）。
const notFoundGlyphDelta = -1

// progressWriteThreshold 是判定"确实在推进"所需的写入增量。
//
// 卡住的进程偶尔也会写几行日志，那不该算推进；而下载 / 校验 / 解压 / 安装
// 任何一个阶段都会产生远大于 1 MiB 的写入。
const progressWriteThreshold = 1 << 20

// updateProgressSample 是"更新是否还在推进"的一次采样。
type updateProgressSample struct {
	stagingBytes uint64 // 游戏目录下启动器暂存区的字节数
	writtenBytes uint64 // 启动器进程的累计写入字节数
}

// progressed 判断相对上一次采样是否确实在推进。
func (s updateProgressSample) progressed(from updateProgressSample) bool {
	if s.stagingBytes != from.stagingBytes {
		return true
	}

	return s.writtenBytes > from.writtenBytes+progressWriteThreshold
}

// updateWatcher 跟踪轮询到的本地版本，用来判断更新是否真的结束了。
//
// 单独抽出来是为了可测：更新过程中要反复读本地配置，而写盘瞬间可能读到中间态，
// 所以要求连续若干次都读到目标版本才算完成。
type updateWatcher struct {
	target string
	stable int
}

// observe 喂入一次读到的本地版本，返回是否已判定更新完成。
func (w *updateWatcher) observe(version string) bool {
	if strings.TrimSpace(version) != w.target {
		w.stable = 0

		return false
	}

	w.stable++

	return w.stable >= updateStableSamples
}

// autoAction 是自动操作要走的路径。
type autoAction int

const (
	autoActionNone autoAction = iota
	autoActionPreDownloadButton
	autoActionUpdateButton
)

// pickAutoAction 决定这次该走哪条路。
//
// 两条路是**互斥**的，由启动器的界面状态决定（真机验证：预下载态有圆钮、没有
// 「更新游戏」；更新态有主按钮、没有圆钮）：
//   - 圆钮：预下载可用 / 断点续传 / 正在下载
//   - 主按钮：更新游戏 → 校验中 → 解压中 → 安装中
//
// 有接口结论时由结论说了算；没联网时（latest == nil）只保留圆钮那条路——盲点
// 主按钮在「开始游戏」状态下会把游戏拉起来，那不是这个 pretask 该做的事。
func pickAutoAction(mode string, cfg *gameConfig, latest *latestGame) (autoAction, error) {
	if mode == "" || mode == autoActionLogOnly {
		return autoActionNone, nil
	}

	if mode != autoActionPreDownload {
		return autoActionNone, fmt.Errorf("launcherpredownload: unknown auto action %q", mode)
	}

	if cfg == nil || latest == nil {
		return autoActionPreDownloadButton, nil
	}

	switch decide(cfg.Version, latest) {
	case verdictPreDownload:
		return autoActionPreDownloadButton, nil
	case verdictUpdateRequired:
		return autoActionUpdateButton, nil
	case verdictUpToDate, verdictUnknown:
		return autoActionNone, nil
	}

	return autoActionNone, nil
}

// runAutoAction 按用户选择执行自动操作。返回值只用于记日志——上层永远继续跑任务。
func runAutoAction(mode string, cfg *gameConfig, latest *latestGame) error {
	action, err := pickAutoAction(mode, cfg, latest)
	if err != nil {
		return err
	}

	switch action {
	case autoActionPreDownloadButton:
		return skipIfNoButton(clickPreDownload(), "the launcher shows no pre-download button, nothing to do")

	case autoActionUpdateButton:
		return skipIfNoButton(
			clickUpdateButton(cfg, latest.Version),
			"the launcher shows no update button, nothing to do",
		)

	case autoActionNone:
		if mode != "" && mode != autoActionLogOnly {
			log.Info().
				Str("component", componentName).
				Msg("the API reports nothing to do, skipping the automatic action")
		}

		return nil
	}

	return nil
}

// skipIfNoButton 把"界面上没有可点的按钮"这一哨兵错误降级为一条 info 日志：
// 那属于"无事可做"，不是故障。
func skipIfNoButton(err error, message string) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, errNoPreDownloadButton), errors.Is(err, errNoUpdateButton):
		log.Info().
			Str("component", componentName).
			Msg(message)

		return nil
	default:
		return err
	}
}

// parseOptions 解析 Client 追加的 option JSON；缺失或非法时返回零值，
// 由调用方按"未启用"处理。
func parseOptions(args []string) optionValue {
	if len(args) == 0 {
		return optionValue{}
	}

	raw := strings.TrimSpace(args[len(args)-1])
	if !strings.HasPrefix(raw, "{") {
		return optionValue{}
	}

	var opts optionValue
	if err := json.Unmarshal([]byte(raw), &opts); err != nil {
		log.Warn().
			Err(err).
			Str("component", componentName).
			Msg("failed to parse pretask options, treating as disabled")

		return optionValue{}
	}

	return opts
}

// decide 把接口响应与本地已安装版本折算成一个结论。
//
// 判定依据来自实测：version 传空或旧版本时 action=1 且 pre_patch 为 null；
// 只有传**当前正式版本**时 action=0 且 pre_patch 才会带上预下载数据。
func decide(installed string, latest *latestGame) verdict {
	if latest == nil {
		return verdictUnknown
	}

	switch latest.Action {
	case actionUpdateRequired:
		return verdictUpdateRequired
	case actionUpToDate:
	default:
		return verdictUnknown
	}

	future := latest.PreDownloadVersion()
	if future != "" && future != strings.TrimSpace(installed) {
		return verdictPreDownload
	}

	return verdictUpToDate
}

// report 只把结论写进日志——本 pretask 按设计不做任何自动操作。
func report(cfg *gameConfig, latest *latestGame) {
	switch decide(cfg.Version, latest) {
	case verdictPreDownload:
		log.Warn().
			Str("component", componentName).
			Str("installed_version", cfg.Version).
			Str("pre_download_version", latest.PreDownloadVersion()).
			Str("pre_download_size", humanGiB(latest.PreDownloadPackageSize())).
			Msg("pre-download is available")

	case verdictUpdateRequired:
		log.Warn().
			Str("component", componentName).
			Str("installed_version", cfg.Version).
			Str("latest_version", latest.Version).
			Msg("a game update is required, open the launcher to update")

	case verdictUpToDate:
		log.Info().
			Str("component", componentName).
			Str("installed_version", cfg.Version).
			Str("latest_version", latest.Version).
			Msg("game is up to date")

	default:
		log.Warn().
			Int("action", latest.Action).
			Str("installed_version", cfg.Version).
			Str("latest_version", latest.Version).
			Msg("unrecognised response from the launcher API")
	}
}

// humanGiB 把接口返回的字节数字符串格式化为 GiB；无法解析时返回空串。
func humanGiB(size string) string {
	n, err := strconv.ParseInt(strings.TrimSpace(size), 10, 64)
	if err != nil || n <= 0 {
		return ""
	}

	return fmt.Sprintf("%.2f GiB", float64(n)/(1<<30))
}
