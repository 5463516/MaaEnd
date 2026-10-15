package launcherpredownload

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/hex"
	"testing"
)

// TestRunExitCode 验证退出码的三态：无事可做返回 true，"真的失败了"返回 false。
//
// 只挑**不会触达窗口**的输入——否则测试会把启动器真的拉起来。可用的两类是：
// 开关关着（不做任何操作），以及未知的动作名（在触达窗口之前就报错）。
func TestRunExitCode(t *testing.T) {
	// 无事可做：开关关着，也没有选自动操作。
	if !Run([]string{`{"LauncherPreDownloadEnable":"No"}`}) {
		t.Errorf("Run() = false，无事可做时应返回 true")
	}

	// 真实失败：未知的动作名在触达窗口之前就会返回错误。
	if Run([]string{`{"LauncherPreDownloadAutoAction":"Nonsense"}`}) {
		t.Errorf("Run() = true，真实失败应返回 false")
	}
}

// TestIsEnabled 验证开关取值的解析（含大小写与空值）。
func TestIsEnabled(t *testing.T) {
	tests := []struct {
		value string
		want  bool
	}{
		{"Yes", true},
		{"yes", true},
		{" Yes ", true},
		{"No", false},
		{"", false},
		{"true", false},
	}

	for _, tt := range tests {
		if got := isEnabled(tt.value); got != tt.want {
			t.Errorf("isEnabled(%q) = %v, want %v", tt.value, got, tt.want)
		}
	}
}

// TestPickAutoAction 验证两条互斥路径的路由：接口说有预下载走圆钮、说需要更新走主按钮，离线只走圆钮。
func TestPickAutoAction(t *testing.T) {
	cfg := &gameConfig{Version: "1.5.3"}

	tests := []struct {
		name   string
		mode   string
		cfg    *gameConfig
		latest *latestGame
		want   autoAction
	}{
		{
			name:   "log only never touches the window",
			mode:   autoActionLogOnly,
			cfg:    cfg,
			latest: &latestGame{PrePatch: &prePatch{Version: "1.6.3"}},
			want:   autoActionNone,
		},
		{
			name:   "no mode at all",
			mode:   "",
			cfg:    cfg,
			latest: &latestGame{Action: actionUpdateRequired},
			want:   autoActionNone,
		},
		{
			name:   "pre-download available routes to the round button",
			mode:   autoActionPreDownload,
			cfg:    cfg,
			latest: &latestGame{PrePatch: &prePatch{Version: "1.6.3"}},
			want:   autoActionPreDownloadButton,
		},
		{
			name:   "update required routes to the main button",
			mode:   autoActionPreDownload,
			cfg:    cfg,
			latest: &latestGame{Action: actionUpdateRequired, Version: "1.6.3"},
			want:   autoActionUpdateButton,
		},
		{
			name:   "up to date does nothing",
			mode:   autoActionPreDownload,
			cfg:    cfg,
			latest: &latestGame{Action: actionUpToDate, Version: "1.5.3"},
			want:   autoActionNone,
		},
		{
			name:   "offline keeps only the round-button path",
			mode:   autoActionPreDownload,
			cfg:    cfg,
			latest: nil,
			want:   autoActionPreDownloadButton,
		},
		{
			name:   "offline without local config still falls back to the round button",
			mode:   autoActionPreDownload,
			cfg:    nil,
			latest: nil,
			want:   autoActionPreDownloadButton,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := pickAutoAction(tt.mode, tt.cfg, tt.latest)
			if err != nil {
				t.Fatalf("pickAutoAction() error = %v", err)
			}

			if got != tt.want {
				t.Errorf("pickAutoAction() = %v, want %v", got, tt.want)
			}
		})
	}

	if _, err := pickAutoAction("Nonsense", cfg, nil); err == nil {
		t.Errorf("unknown mode should be an error")
	}
}

// TestUpdateWatcher 验证"连续两次读到目标版本"才判定更新完成，且中途回退要重新累计。
func TestUpdateWatcher(t *testing.T) {
	watcher := &updateWatcher{target: "1.6.3"}

	observations := []struct {
		version string
		done    bool
	}{
		{"1.5.3", false}, // 还没更新完
		{"", false},      // 读到写盘中途的中间态
		{"1.6.3", false}, // 第一次读到目标版本，还不敢确认
		{"1.6.3", true},  // 连续两次 → 完成
		{"1.6.3", true},  // 之后一直是完成
	}

	for index, observation := range observations {
		if got := watcher.observe(observation.version); got != observation.done {
			t.Errorf("第 %d 次 observe(%q) = %v，期望 %v",
				index+1, observation.version, got, observation.done)
		}
	}

	// 中途回退（例如又读到了旧版本）必须重新累计。
	reverted := &updateWatcher{target: "1.6.3"}
	reverted.observe("1.6.3")

	if reverted.observe("1.5.3") {
		t.Errorf("版本回退后不应判定完成")
	}

	if reverted.observe("1.6.3") {
		t.Errorf("回退后需要重新累计，这一次不应判定完成")
	}
}

// TestUpdateProgressSample 验证推进判据：暂存区变化算推进，日志级别的少量写入不算。
func TestUpdateProgressSample(t *testing.T) {
	base := updateProgressSample{stagingBytes: 1000, writtenBytes: 1000}

	tests := []struct {
		name string
		from updateProgressSample
		to   updateProgressSample
		want bool
	}{
		{
			name: "nothing changed means stalled",
			from: base,
			to:   base,
			want: false,
		},
		{
			name: "staging grew means the download is running",
			from: base,
			to:   updateProgressSample{stagingBytes: 2000, writtenBytes: 1000},
			want: true,
		},
		{
			name: "staging shrank means files moved on to the next phase",
			from: base,
			to:   updateProgressSample{stagingBytes: 500, writtenBytes: 1000},
			want: true,
		},
		{
			name: "a trickle of log writes does not count as progress",
			from: base,
			to:   updateProgressSample{stagingBytes: 1000, writtenBytes: 1000 + 4096},
			want: false,
		},
		{
			name: "real writes count as progress",
			from: base,
			to:   updateProgressSample{stagingBytes: 1000, writtenBytes: 1000 + progressWriteThreshold + 1},
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.to.progressed(tt.from); got != tt.want {
				t.Errorf("progressed() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestDecide 验证接口响应折算成四种结论的判定。
func TestDecide(t *testing.T) {
	tests := []struct {
		name      string
		installed string
		latest    *latestGame
		want      verdict
	}{
		{
			name:      "nil response is unknown",
			installed: "1.5.3",
			latest:    nil,
			want:      verdictUnknown,
		},
		{
			name:      "up to date",
			installed: "1.5.3",
			latest:    &latestGame{Action: 0, Version: "1.5.3"},
			want:      verdictUpToDate,
		},
		{
			name:      "pre-download available",
			installed: "1.5.3",
			latest: &latestGame{
				Action:  0,
				Version: "1.5.3",
				PrePatch: &prePatch{
					Version:     "1.6.3",
					PackageSize: "9364056947",
				},
			},
			want: verdictPreDownload,
		},
		{
			name:      "pre-download version equals installed",
			installed: "1.6.3",
			latest: &latestGame{
				Action:   0,
				Version:  "1.6.3",
				PrePatch: &prePatch{Version: "1.6.3"},
			},
			want: verdictUpToDate,
		},
		{
			name:      "update required",
			installed: "1.5.0",
			latest:    &latestGame{Action: 1, Version: "1.5.3"},
			want:      verdictUpdateRequired,
		},
		{
			name:      "update wins over pre-download",
			installed: "1.5.0",
			latest: &latestGame{
				Action:   1,
				Version:  "1.5.3",
				PrePatch: &prePatch{Version: "1.6.3"},
			},
			want: verdictUpdateRequired,
		},
		{
			name:      "unrecognised action",
			installed: "1.5.3",
			latest:    &latestGame{Action: 7, Version: "1.5.3"},
			want:      verdictUnknown,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := decide(tt.installed, tt.latest); got != tt.want {
				t.Errorf("decide(%q, %+v) = %d, want %d", tt.installed, tt.latest, got, tt.want)
			}
		})
	}
}

// TestParseINI 验证 config.ini 的键值解析。
func TestParseINI(t *testing.T) {
	text := "[Game]\r\n" +
		"version=1.5.3\r\n" +
		"appcode=6LL0KJuqHBVz33WK\r\n" +
		"region=cn\r\n" +
		"channel=1\r\n" +
		"sub_channel=1\r\n" +
		"\r\n" +
		"; comment\r\n" +
		"# another comment\r\n" +
		"no_equals_sign\r\n" +
		"\r\n" +
		"[Other]\r\n" +
		"key = spaced value \r\n"

	got := parseINI(text)

	want := map[string]string{
		"Game/version":     "1.5.3",
		"Game/appcode":     "6LL0KJuqHBVz33WK",
		"Game/region":      "cn",
		"Game/channel":     "1",
		"Game/sub_channel": "1",
		"Other/key":        "spaced value",
	}

	for key, value := range want {
		if got[key] != value {
			t.Errorf("parseINI()[%q] = %q, want %q", key, got[key], value)
		}
	}

	if _, ok := got["Game/no_equals_sign"]; ok {
		t.Errorf("parseINI() kept a line without '='")
	}

	if len(got) != len(want) {
		t.Errorf("parseINI() returned %d entries, want %d: %v", len(got), len(want), got)
	}
}

// TestParseOptions 验证客户端传来的 option JSON 解析。
func TestParseOptions(t *testing.T) {
	payload := `{"LauncherPreDownloadEnable":"Yes",` +
		`"LauncherPreDownloadGamePath":{"Path":"D:\\Hypergryph\\Arknights Endfield"}}`

	opts := parseOptions([]string{"LauncherPreDownload", payload})

	if opts.Enable != "Yes" {
		t.Errorf("Enable = %q, want %q", opts.Enable, "Yes")
	}

	if want := `D:\Hypergryph\Arknights Endfield`; opts.GamePath.Path != want {
		t.Errorf("GamePath.Path = %q, want %q", opts.GamePath.Path, want)
	}
}

// TestParseOptionsWithoutPayload 验证没有 payload 时退化为零值而不是报错。
func TestParseOptionsWithoutPayload(t *testing.T) {
	for _, args := range [][]string{nil, {}, {"LauncherPreDownload"}, {"--pretask"}} {
		if opts := parseOptions(args); opts != (optionValue{}) {
			t.Errorf("parseOptions(%v) = %+v, want zero value", args, opts)
		}
	}
}

// TestDecryptRoundTrip 验证 AES-256-CBC 解密能还原明文。
func TestDecryptRoundTrip(t *testing.T) {
	plain := []byte("[Game]\nversion=1.5.3\nappcode=6LL0KJuqHBVz33WK\n")

	sealed, err := encryptForTest(plain)
	if err != nil {
		t.Fatalf("encryptForTest() error = %v", err)
	}

	got, err := decrypt(sealed)
	if err != nil {
		t.Fatalf("decrypt() error = %v", err)
	}

	if string(got) != string(plain) {
		t.Errorf("decrypt() = %q, want %q", got, plain)
	}
}

// TestDecryptRejectsMisalignedCiphertext 验证密文长度不是块大小整数倍时被拒绝，而不是崩溃。
func TestDecryptRejectsMisalignedCiphertext(t *testing.T) {
	if _, err := decrypt([]byte("too short")); err == nil {
		t.Errorf("decrypt() accepted a ciphertext that is not a multiple of the block size")
	}

	if _, err := decrypt(nil); err == nil {
		t.Errorf("decrypt() accepted an empty ciphertext")
	}
}

// TestHumanGiB 验证字节数格式化成 GiB，以及非法输入返回空串。
func TestHumanGiB(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"9364056947", "8.72 GiB"},
		{"20399935566", "19.00 GiB"},
		{"0", ""},
		{"", ""},
		{"not a number", ""},
	}

	for _, tt := range tests {
		if got := humanGiB(tt.in); got != tt.want {
			t.Errorf("humanGiB(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// encryptForTest 用与被测代码相同的 key/IV 做一次 PKCS#7 + CBC 加密，
// 仅用于验证 decrypt 的往返一致性；线上没有任何加密写入路径。
func encryptForTest(plain []byte) ([]byte, error) {
	key, err := hex.DecodeString(gameConfigKeyHex)
	if err != nil {
		return nil, err
	}

	iv, err := hex.DecodeString(gameConfigIVHex)
	if err != nil {
		return nil, err
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}

	padding := aes.BlockSize - len(plain)%aes.BlockSize
	padded := make([]byte, 0, len(plain)+padding)
	padded = append(padded, plain...)

	for i := 0; i < padding; i++ {
		padded = append(padded, byte(padding))
	}

	sealed := make([]byte, len(padded))
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(sealed, padded)

	return sealed, nil
}
