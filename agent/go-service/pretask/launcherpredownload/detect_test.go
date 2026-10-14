package launcherpredownload

import (
	"image"
	"image/color"
	"math"
	"testing"
)

var (
	darkBackground = color.RGBA{R: 20, G: 22, B: 28, A: 255}
	buttonYellow   = color.RGBA{R: 246, G: 246, B: 33, A: 255}
	dialogWhite    = color.RGBA{R: 235, G: 235, B: 235, A: 255}
)

func newFrame(width, height int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, width, height))

	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			img.Set(x, y, darkBackground)
		}
	}

	return img
}

func paintCircle(img *image.RGBA, c circle, fill color.RGBA) {
	for y := int(math.Floor(c.Center.Y - c.Radius)); y <= int(math.Ceil(c.Center.Y+c.Radius)); y++ {
		for x := int(math.Floor(c.Center.X - c.Radius)); x <= int(math.Ceil(c.Center.X+c.Radius)); x++ {
			dx := float64(x) - c.Center.X
			dy := float64(y) - c.Center.Y
			if dx*dx+dy*dy <= c.Radius*c.Radius {
				img.Set(x, y, fill)
			}
		}
	}
}

func paintBox(img *image.RGBA, b box, fill color.RGBA) {
	for y := int(b.Min.Y); y < int(b.Max.Y); y++ {
		for x := int(b.Min.X); x < int(b.Max.X); x++ {
			img.Set(x, y, fill)
		}
	}
}

func TestLayoutConstantsAtBaseSize(t *testing.T) {
	l := newLayout(1280, 720, 1)

	disc := l.disc()
	if disc.Center.X != 946 || disc.Center.Y != 637 {
		t.Errorf("disc center = (%.1f, %.1f), want (946, 637)", disc.Center.X, disc.Center.Y)
	}

	if disc.Radius != 30 {
		t.Errorf("disc radius = %.1f, want 30", disc.Radius)
	}

	confirm := l.confirmButton()
	if confirm.Width() != 202 || confirm.Height() != 40 {
		t.Errorf("confirm size = %.1fx%.1f, want 202x40", confirm.Width(), confirm.Height())
	}

	if confirm.Center().X != 749.5 || confirm.Center().Y != 447.5 {
		t.Errorf("confirm center = (%.1f, %.1f), want (749.5, 447.5)", confirm.Center().X, confirm.Center().Y)
	}
}

func TestLayoutCentresContentInLargerWindow(t *testing.T) {
	// 实机数据：客户区 1968x1131、DPI 缩放 1.5 → 内容区 1312x754 CSS。
	// 窗口比内容区大，内容居中，圆心实测 (1442.5, 979.5)、半径 45。
	l := newLayout(1968, 1131, 1.5)

	disc := l.disc()
	if math.Abs(disc.Center.X-1443) > 1 || math.Abs(disc.Center.Y-981) > 1 {
		t.Errorf("disc center = (%.1f, %.1f), want ~(1443, 981)", disc.Center.X, disc.Center.Y)
	}

	if math.Abs(disc.Radius-45) > 0.1 {
		t.Errorf("disc radius = %.1f, want 45", disc.Radius)
	}
}

func TestLayoutScalesWithDpi(t *testing.T) {
	// 200% 缩放下客户区是 2560x1440，几何应等比放大一倍。
	l := newLayout(2560, 1440, 2)

	disc := l.disc()
	if disc.Center.X != 1892 || disc.Center.Y != 1274 {
		t.Errorf("disc center = (%.1f, %.1f), want (1892, 1274)", disc.Center.X, disc.Center.Y)
	}

	if disc.Radius != 60 {
		t.Errorf("disc radius = %.1f, want 60", disc.Radius)
	}

	if got := l.confirmButton().Width(); got != 404 {
		t.Errorf("confirm width = %.1f, want 404", got)
	}
}

func TestFindDiscOnSyntheticFrames(t *testing.T) {
	tests := []struct {
		name   string
		width  int
		height int
		scale  float64
	}{
		{"100% 1080p", 1280, 720, 1},
		{"150%", 1920, 1080, 1.5},
		{"200% 4K", 2560, 1440, 2},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			l := newLayout(tt.width, tt.height, tt.scale)
			want := l.disc()

			img := newFrame(tt.width, tt.height)
			paintCircle(img, want, buttonYellow)

			got, ok := findDisc(img, want, 10*tt.scale)
			if !ok {
				t.Fatalf("findDisc() did not find the painted disc")
			}

			if math.Abs(got.Center.X-want.Center.X) > 2 || math.Abs(got.Center.Y-want.Center.Y) > 2 {
				t.Errorf("findDisc() center = (%.1f, %.1f), want (%.1f, %.1f)",
					got.Center.X, got.Center.Y, want.Center.X, want.Center.Y)
			}

			if math.Abs(got.Radius-want.Radius) > 2 {
				t.Errorf("findDisc() radius = %.1f, want %.1f", got.Radius, want.Radius)
			}
		})
	}
}

func TestFindDiscRejectsNonDiscShapes(t *testing.T) {
	l := newLayout(1280, 720, 1)
	want := l.disc()

	tests := []struct {
		name  string
		paint func(*image.RGBA)
	}{
		{
			name:  "empty frame",
			paint: func(*image.RGBA) {},
		},
		{
			name: "square instead of circle",
			paint: func(img *image.RGBA) {
				paintBox(img, box{
					Min: point{X: want.Center.X - want.Radius, Y: want.Center.Y - want.Radius},
					Max: point{X: want.Center.X + want.Radius, Y: want.Center.Y + want.Radius},
				}, buttonYellow)
			},
		},
		{
			name: "ring instead of solid disc",
			paint: func(img *image.RGBA) {
				paintCircle(img, circle{Center: want.Center, Radius: want.Radius}, buttonYellow)
				paintCircle(img, circle{Center: want.Center, Radius: want.Radius * 0.7}, darkBackground)
			},
		},
		{
			name: "disc dimmed by a modal scrim",
			paint: func(img *image.RGBA) {
				paintCircle(img, want, color.RGBA{R: 98, G: 98, B: 13, A: 255})
			},
		},
		{
			name: "disc drawn far from the expected anchor",
			paint: func(img *image.RGBA) {
				paintCircle(img, circle{Center: point{X: 200, Y: 200}, Radius: want.Radius}, buttonYellow)
			},
		},
		{
			name: "yellow noise everywhere",
			paint: func(img *image.RGBA) {
				paintBox(img, box{Min: point{X: 0, Y: 0}, Max: point{X: 1280, Y: 720}}, buttonYellow)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			img := newFrame(1280, 720)
			tt.paint(img)

			if _, ok := findDisc(img, want, 10); ok {
				t.Errorf("findDisc() accepted a frame that should be rejected")
			}
		})
	}
}

func TestDiscGlyphDetectsMotion(t *testing.T) {
	// 下载中的圆钮会给箭头挂循环动画，"可点击开始/继续"是静止的。
	// 用两次抓帧的指纹差异来区分这两者，这里钉住这个判据的基本行为。
	l := newLayout(1280, 720, 1)
	disc := l.disc()

	paint := func(offsetX, offsetY float64) *image.RGBA {
		img := newFrame(1280, 720)
		paintCircle(img, disc, buttonYellow)
		paintBox(img, box{
			Min: point{X: disc.Center.X + offsetX - 8, Y: disc.Center.Y + offsetY - 8},
			Max: point{X: disc.Center.X + offsetX + 8, Y: disc.Center.Y + offsetY + 8},
		}, darkBackground)

		return img
	}

	still := paint(0, 0)

	if changed := glyphDifference(discGlyph(still, disc), discGlyph(still, disc)); changed != 0 {
		t.Errorf("identical frames differ by %d cells, want 0", changed)
	}

	moved := paint(4, 12)
	if changed := glyphDifference(discGlyph(still, disc), discGlyph(moved, disc)); changed < glyphChangeThreshold {
		t.Errorf("moved glyph differs by %d cells, want >= %d", changed, glyphChangeThreshold)
	}
}

func TestDiscGlyphIgnoresBackgroundOutsideTheDisc(t *testing.T) {
	// 采样方格的四个角会伸到圆钮之外（半边长 r*0.8，对角长 r*1.13）。若把背景
	// 算进指纹，启动器底图的任何变化都会被误判成"图标在动"——实机踩过：底图
	// 由蓝转暗，凭空多出 6 格差异，于是"预下载已暂停"被当成"正在下载"，永远不点。
	disc := newLayout(1280, 720, 1).disc()

	build := func(background color.RGBA) string {
		img := newFrame(1280, 720)
		paintBox(img, box{Min: point{X: 0, Y: 0}, Max: point{X: 1280, Y: 720}}, background)
		paintCircle(img, disc, buttonYellow)
		paintBox(img, box{
			Min: point{X: disc.Center.X - 8, Y: disc.Center.Y - 8},
			Max: point{X: disc.Center.X + 8, Y: disc.Center.Y + 8},
		}, darkBackground)

		return discGlyph(img, disc)
	}

	bluish := build(color.RGBA{R: 130, G: 152, B: 200, A: 255})
	dark := build(color.RGBA{R: 64, G: 59, B: 64, A: 255})

	if changed := glyphDifference(bluish, dark); changed != 0 {
		t.Errorf("background leaked into the glyph: %d cells differ, want 0", changed)
	}
}

func TestGlyphSequenceAnimating(t *testing.T) {
	// arrowLoopAnimating 的周期是 2 秒，其中约 40% 的时间箭头**完全透明**，
	// 另有 10% 静止。这里用"静止帧"代表那些看起来没变化的相位。
	disc := newLayout(1280, 720, 1).disc()

	paint := func(offsetX, offsetY float64) string {
		img := newFrame(1280, 720)
		paintCircle(img, disc, buttonYellow)
		paintBox(img, box{
			Min: point{X: disc.Center.X + offsetX - 8, Y: disc.Center.Y + offsetY - 8},
			Max: point{X: disc.Center.X + offsetX + 8, Y: disc.Center.Y + offsetY + 8},
		}, darkBackground)

		return discGlyph(img, disc)
	}

	still := paint(0, 0)
	moving := paint(4, 12)

	tests := []struct {
		name   string
		glyphs []string
		want   bool
	}{
		{
			name:   "a single sample can never prove motion",
			glyphs: []string{still},
			want:   false,
		},
		{
			name:   "all samples identical means static",
			glyphs: []string{still, still, still, still},
			want:   false,
		},
		{
			name:   "two samples landing inside the invisible window miss it",
			glyphs: []string{still, still},
			want:   false,
		},
		{
			name:   "covering the whole cycle catches it",
			glyphs: []string{still, still, still, moving, still, still, still, still},
			want:   true,
		},
		{
			name:   "a change in the very last sample still counts",
			glyphs: []string{still, still, still, moving},
			want:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := glyphSequenceAnimating(tt.glyphs); got != tt.want {
				t.Errorf("glyphSequenceAnimating() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestGlyphMatchesDownloadButton(t *testing.T) {
	if len(downloadGlyph) != glyphGridSize*glyphGridSize {
		t.Fatalf("基准指纹长度 = %d，应为 %d", len(downloadGlyph), glyphGridSize*glyphGridSize)
	}

	if !glyphMatchesDownloadButton(downloadGlyph) {
		t.Errorf("基准自身应当匹配")
	}

	// 差 1 格仍应匹配：真机渲染会有轻微抖动。
	near := []byte(downloadGlyph)
	if near[0] == '.' {
		near[0] = '#'
	} else {
		near[0] = '.'
	}

	if !glyphMatchesDownloadButton(string(near)) {
		t.Errorf("与基准差 1 格应当仍然匹配")
	}

	// 差 15 格必须不匹配——这正是实测中"预下载已完成"（云朵加叉）与基准的差距。
	far := []byte(downloadGlyph)
	flipped := 0

	for index := range far {
		if far[index] == '.' {
			far[index] = '#'
		} else {
			far[index] = '.'
		}

		flipped++
		if flipped == 15 {
			break
		}
	}

	if glyphMatchesDownloadButton(string(far)) {
		t.Errorf("与基准差 15 格（已完成状态的实测差距）不应当匹配")
	}
}

func TestFindConfirmButton(t *testing.T) {
	l := newLayout(1280, 720, 1)
	want := l.confirmButton()

	img := newFrame(1280, 720)
	paintBox(img, want, dialogWhite)

	got, ok := findConfirmButton(img, want, 10)
	if !ok {
		t.Fatalf("findConfirmButton() did not find the painted pill")
	}

	if math.Abs(got.Width()-202) > 3 || math.Abs(got.Height()-40) > 3 {
		t.Errorf("findConfirmButton() size = %.0fx%.0f, want ~202x40", got.Width(), got.Height())
	}
}

func TestFindConfirmButtonRejectsWrongShapes(t *testing.T) {
	l := newLayout(1280, 720, 1)
	want := l.confirmButton()

	tests := []struct {
		name  string
		paint func(*image.RGBA)
	}{
		{
			name:  "empty frame",
			paint: func(*image.RGBA) {},
		},
		{
			name: "white square instead of a wide pill",
			paint: func(img *image.RGBA) {
				paintBox(img, box{
					Min: point{X: want.Center().X - 20, Y: want.Center().Y - 20},
					Max: point{X: want.Center().X + 20, Y: want.Center().Y + 20},
				}, dialogWhite)
			},
		},
		{
			name: "yellow main button, not the dialog pill",
			paint: func(img *image.RGBA) {
				paintBox(img, want, buttonYellow)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			img := newFrame(1280, 720)
			tt.paint(img)

			if _, ok := findConfirmButton(img, want, 10); ok {
				t.Errorf("findConfirmButton() accepted a frame that should be rejected")
			}
		})
	}
}

func TestFindDiscIgnoresTheNeighbouringPill(t *testing.T) {
	// 圆钮右手边约 12 CSS px 就是主按钮。搜索域一旦够到它，两者会被算成一团，
	// 圆心右偏、半径虚大——实机两帧都复现过，所以这里钉一条回归。
	l := newLayout(1280, 720, 1)
	want := l.disc()

	img := newFrame(1280, 720)
	paintCircle(img, want, buttonYellow)

	pillLeft := want.Center.X + want.Radius + 12
	paintBox(img, box{
		Min: point{X: pillLeft, Y: want.Center.Y - 30},
		Max: point{X: pillLeft + 220, Y: want.Center.Y + 30},
	}, buttonYellow)

	got, ok := findDisc(img, want, 12)
	if !ok {
		t.Fatalf("findDisc() did not find the disc sitting next to the pill")
	}

	if math.Abs(got.Center.X-want.Center.X) > 2 {
		t.Errorf("centre X = %.1f, want %.1f: the pill was merged into the disc",
			got.Center.X, want.Center.X)
	}

	if math.Abs(got.Radius-want.Radius) > 2 {
		t.Errorf("radius = %.1f, want %.1f: the pill was merged into the disc",
			got.Radius, want.Radius)
	}
}

func TestHasDialogBackdrop(t *testing.T) {
	l := newLayout(1280, 720, 1)
	card := l.dialogCard()
	brightArt := color.RGBA{R: 230, G: 232, B: 238, A: 255}
	cardFill := color.RGBA{R: 41, G: 40, B: 43, A: 255}
	full := box{Min: point{X: 0, Y: 0}, Max: point{X: 1280, Y: 720}}

	tests := []struct {
		name  string
		paint func(*image.RGBA)
		want  bool
	}{
		{
			name: "uniform dark card over a bright background",
			paint: func(img *image.RGBA) {
				paintBox(img, full, brightArt)
				paintBox(img, card, cardFill)
			},
			want: true,
		},
		{
			name: "bright artwork without any card",
			paint: func(img *image.RGBA) {
				paintBox(img, full, brightArt)
			},
			want: false,
		},
		{
			name: "card present but a bright highlight lands on the sample band",
			paint: func(img *image.RGBA) {
				paintBox(img, full, brightArt)
				paintBox(img, card, cardFill)

				y := card.Min.Y + card.Height()*0.15
				x := card.Min.X + card.Width()*0.35
				paintBox(img, box{
					Min: point{X: x - 6, Y: y - 4},
					Max: point{X: x + 6, Y: y + 4},
				}, dialogWhite)
			},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			img := newFrame(1280, 720)
			tt.paint(img)

			if got := hasDialogBackdrop(img, card); got != tt.want {
				t.Errorf("hasDialogBackdrop() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestIsButtonYellowTolerance(t *testing.T) {
	tests := []struct {
		name    string
		r, g, b uint8
		want    bool
	}{
		{"measured screenshot yellow", 246, 246, 33, true},
		{"hover yellow from the web bundle", 254, 253, 77, true},
		{"css declared yellow", 253, 252, 0, true},
		{"dimmed by modal scrim", 98, 98, 13, false},
		{"white", 245, 245, 245, false},
		{"dark background", 20, 22, 28, false},
		{"yellowish grey", 200, 200, 170, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isButtonYellow(tt.r, tt.g, tt.b); got != tt.want {
				t.Errorf("isButtonYellow(%d, %d, %d) = %v, want %v", tt.r, tt.g, tt.b, got, tt.want)
			}
		})
	}
}
