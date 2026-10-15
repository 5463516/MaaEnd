package launcherpredownload

import (
	"image"
	"math"
)

// 启动器 UI 的**内容区**在 CSS 像素下恒为 1280x720，并在窗口里**居中**：
// 按钮条锚在内容区的右下角，弹窗在内容区居中。
//
// 因此几何换算分两步：先把客户区尺寸换成内容区坐标，再按内容区锚点定位。
// 物理像素 = CSS x DPI 缩放，与桌面分辨率无关。
//
// 这组常量来自五组真机数据：
//   - 100%@1080p、100%@2K、200%@4K：窗口正好等于内容区，圆心距右 333.5~337、距下 82.5~83.7
//   - 150% 实机（客户区 1968x1131 = 内容区 1312x754 CSS）：圆心距右 350.3、距下 101.0
//     差额恰好是窗口超出内容区的一半，即 (1312-1280)/2 = 16、(754-720)/2 = 17 —— 内容居中
//   - 圆钮 60x60、主按钮 220x60 在五组数据里恒定
const (
	baseContentWidth  = 1280.0
	baseContentHeight = 720.0

	discDiameterCSS         = 60.0
	discCenterFromRightCSS  = 334.0
	discCenterFromBottomCSS = 83.0

	// 右下角主按钮（「开始游戏」/「更新游戏」/「校验中」/「安装中」…）。
	// 实测 1968x1131 @1.5 下 bbox=(1506,935)-(1835,1024)，即 330x90 物理像素。
	mainButtonWidthCSS     = 220.0
	mainButtonHeightCSS    = 60.0
	mainButtonFromRightCSS = 184.0

	confirmWidthCSS             = 202.0
	confirmHeightCSS            = 40.0
	confirmOffsetXFromCenterCSS = 109.5
	confirmOffsetYFromCenterCSS = 87.5

	dialogCardWidthCSS  = 460.0
	dialogCardHeightCSS = 254.0
)

// 颜色判据刻意放宽：截图里的黄底实测是 (246,246,33)，而启动器 web bundle 中写的
// 是 (253,252,0)，说明已经存在色偏；HDR / 色彩管理还会再叠一层偏移。
const (
	yellowMinRG   = 185
	yellowMaxB    = 140
	yellowMinRGap = 70

	whiteMinChannel = 180
)

// 形状判据：圆钮的黄色填充率应接近 pi/4，明显低于 1 才排除方块类图形。
const (
	discFillMin         = 0.55
	discFillMax         = 0.92
	discRadiusTolerance = 0.18 // 实测半径允许偏离期望值的比例
	discAspectTolerance = 0.30 // |宽-高| / 高

	// 搜索域相对圆钮半径允许外扩的比例；见 findDisc 的说明。
	discSearchToleranceRatio = 0.20

	confirmAspectMin   = 4.2
	confirmAspectMax   = 6.0
	confirmFillMin     = 0.90
	confirmLengthScale = 0.20 // 长度允许偏离期望值的比例

	// 主按钮里还有图标与文字，黄色填充率远低于实心圆钮（实测约 0.70）。
	mainButtonAspectMin   = 2.8
	mainButtonAspectMax   = 5.0
	mainButtonFillMin     = 0.45
	mainButtonFillMax     = 0.92
	mainButtonLengthScale = 0.20

	// 只靠"有没有白色药丸"会在角色立绘等亮色背景上误判，
	// 因此再叠一层"卡片顶部是不是一片均匀暗色"的校验。
	cardDarkMaxChannel  = 110
	cardSampleTolerance = 40

	// 弹窗卡片的高度可能随文案行数变化（例如"已预下载"与否），使「确认」偏离
	// 期望位置；找不到时用这个更宽的位置搜索域再试一次。
	confirmFallbackToleranceCSS = 40.0
)

type point struct {
	X float64
	Y float64
}

type circle struct {
	Center point
	Radius float64
}

type box struct {
	Min point
	Max point
}

// Width 返回宽度。
func (b box) Width() float64 { return b.Max.X - b.Min.X }

// Height 返回高度。
func (b box) Height() float64 { return b.Max.Y - b.Min.Y }

// Center 返回中心点。
func (b box) Center() point {
	return point{X: (b.Min.X + b.Max.X) / 2, Y: (b.Min.Y + b.Max.Y) / 2}
}

// layout 描述一次捕获的几何换算：客户区物理尺寸 + DPI 缩放比。
//
// 缩放比取自 GetDpiForWindow/96，而不是"客户区宽 / 1280"——因为窗口可能比内容区
// 大（用户拉大过窗口），那样算出来的比例会偏大，圆心会整体偏出去。
type layout struct {
	width  int
	height int
	scale  float64
}

// newLayout 由客户区物理尺寸与 DPI 缩放比构造换算关系。
func newLayout(width, height int, scale float64) layout {
	if scale <= 0 {
		scale = 1
	}

	return layout{width: width, height: height, scale: scale}
}

// contentOrigin 返回内容区左上角在客户区里的位置（物理像素）。
// 窗口大于内容区时内容居中，两侧各留一半。
func (l layout) contentOrigin() point {
	offsetX := (float64(l.width)/l.scale - baseContentWidth) / 2
	offsetY := (float64(l.height)/l.scale - baseContentHeight) / 2

	return point{X: offsetX * l.scale, Y: offsetY * l.scale}
}

// disc 返回预下载圆钮在客户区里的期望位置与半径（物理像素）。
func (l layout) disc() circle {
	origin := l.contentOrigin()

	return circle{
		Center: point{
			X: origin.X + (baseContentWidth-discCenterFromRightCSS)*l.scale,
			Y: origin.Y + (baseContentHeight-discCenterFromBottomCSS)*l.scale,
		},
		Radius: discDiameterCSS * l.scale / 2,
	}
}

// confirmButton 返回弹窗里「确认」按钮的期望外接矩形（物理像素）。
func (l layout) confirmButton() box {
	origin := l.contentOrigin()
	center := point{
		X: origin.X + (baseContentWidth/2+confirmOffsetXFromCenterCSS)*l.scale,
		Y: origin.Y + (baseContentHeight/2+confirmOffsetYFromCenterCSS)*l.scale,
	}
	halfWidth := confirmWidthCSS * l.scale / 2
	halfHeight := confirmHeightCSS * l.scale / 2

	return box{
		Min: point{X: center.X - halfWidth, Y: center.Y - halfHeight},
		Max: point{X: center.X + halfWidth, Y: center.Y + halfHeight},
	}
}

// mainButton 返回右下角主按钮（「开始游戏」/「更新游戏」/「校验中」/「安装中」…）
// 在客户区里的期望外接矩形（物理像素）。
//
// 主按钮与预下载圆钮同高（垂直中心都在内容区下边往上 83 CSS），宽 220、高 60、
// 中心距内容区右边 184 CSS。
func (l layout) mainButton() box {
	origin := l.contentOrigin()
	center := point{
		X: origin.X + (baseContentWidth-mainButtonFromRightCSS)*l.scale,
		Y: origin.Y + (baseContentHeight-discCenterFromBottomCSS)*l.scale,
	}
	halfWidth := mainButtonWidthCSS * l.scale / 2
	halfHeight := mainButtonHeightCSS * l.scale / 2

	return box{
		Min: point{X: center.X - halfWidth, Y: center.Y - halfHeight},
		Max: point{X: center.X + halfWidth, Y: center.Y + halfHeight},
	}
}

// dialogCard 返回弹窗卡片在客户区里的期望外接矩形（物理像素）。
func (l layout) dialogCard() box {
	origin := l.contentOrigin()
	width := dialogCardWidthCSS * l.scale
	height := dialogCardHeightCSS * l.scale
	centerX := origin.X + baseContentWidth/2*l.scale
	centerY := origin.Y + baseContentHeight/2*l.scale

	return box{
		Min: point{X: centerX - width/2, Y: centerY - height/2},
		Max: point{X: centerX + width/2, Y: centerY + height/2},
	}
}

// hasDialogBackdrop 判断弹窗卡片是否真的存在：卡片顶部一条横带应是一片均匀暗色。
//
// 这一步是为了压掉"亮色背景被当成白色按钮"的误判——常态下窗口中央是角色立绘，
// 亮色像素很容易凑出一个宽高比接近的团；而弹窗卡片本身是均匀深灰，两者可以区分。
//
// 采样点只取**顶部 15% 高的一条横带**：卡片中下部依次是标题、进度条、说明条和
// 底部两个按钮，从四角按比例内缩会正好落到「确认」那颗白药丸上（实测踩过这个坑）。
func hasDialogBackdrop(img image.Image, card box) bool {
	limit := boundsOf(img)
	origin := img.Bounds().Min

	sampleY := int(card.Min.Y + card.Height()*0.15)
	if sampleY < limit.Min.Y || sampleY > limit.Max.Y {
		return false
	}

	lowest, highest := 0, 0

	for index, ratio := range []float64{0.10, 0.35, 0.65, 0.90} {
		x := int(card.Min.X + card.Width()*ratio)
		if x < limit.Min.X || x > limit.Max.X {
			return false
		}

		r, g, b, _ := img.At(origin.X+x, origin.Y+sampleY).RGBA()
		value := brightestChannel(uint8(r>>8), uint8(g>>8), uint8(b>>8))
		if value > cardDarkMaxChannel {
			return false
		}

		if index == 0 {
			lowest, highest = value, value

			continue
		}

		if value < lowest {
			lowest = value
		}

		if value > highest {
			highest = value
		}
	}

	return highest-lowest <= cardSampleTolerance
}

// brightestChannel 返回三个通道里最大的那个，用来衡量一个像素有多亮。
func brightestChannel(r, g, b uint8) int {
	value := int(r)
	if int(g) > value {
		value = int(g)
	}

	if int(b) > value {
		value = int(b)
	}

	return value
}

// 圆钮内部图标的采样网格与判据。
//
// 启动器的小圆钮恒为 60x60、`bg-primary rounded-full`，所有状态下**底色都不变**，
// 只有内部的 SVG 图标换（源码 res/web/index.*.js 里的 pre-download-btn）。
// 其中"可点击开始/继续"与"下载中"用的是同一个云朵图标，只差一个循环动画
// （arrowLoopAnimating）；因此比较两次抓帧里图标的形状就能判断下载是否正在进行，
// 不需要认识具体图形，也就不必为每种状态准备样本。
const (
	glyphGridSize        = 16
	glyphDarkMaxChannel  = 128
	glyphChangeThreshold = 4
)

// discGlyph 把圆钮内部图标编码成"暗像素位图"字符串，供两次抓帧比较。
func discGlyph(img image.Image, disc circle) string {
	limit := boundsOf(img)
	origin := img.Bounds().Min

	radius := disc.Radius * 0.8
	step := 2 * radius / glyphGridSize
	startX := disc.Center.X - radius
	startY := disc.Center.Y - radius

	glyph := make([]byte, 0, glyphGridSize*glyphGridSize)

	for row := 0; row < glyphGridSize; row++ {
		for column := 0; column < glyphGridSize; column++ {
			x := int(startX + (float64(column)+0.5)*step)
			y := int(startY + (float64(row)+0.5)*step)

			if x < limit.Min.X || x >= limit.Max.X || y < limit.Min.Y || y >= limit.Max.Y {
				glyph = append(glyph, '.')

				continue
			}

			// 只采圆内的格子。采样方格的四个角会伸到圆钮之外（半边长 r*0.8 的
			// 方格，对角长 r*1.13），把启动器底图算进来——底图一变就会被误判成
			// "图标在动"（实机踩过：底图由蓝转暗，凭空多出 6 格差异）。
			dx := float64(x) + 0.5 - disc.Center.X
			dy := float64(y) + 0.5 - disc.Center.Y
			if dx*dx+dy*dy > radius*radius {
				glyph = append(glyph, '.')

				continue
			}

			r, g, b, _ := img.At(origin.X+x, origin.Y+y).RGBA()
			if brightestChannel(uint8(r>>8), uint8(g>>8), uint8(b>>8)) < glyphDarkMaxChannel {
				glyph = append(glyph, '#')

				continue
			}

			glyph = append(glyph, '.')
		}
	}

	return string(glyph)
}

// glyphDifference 返回两个指纹里不同的格子数。
func glyphDifference(left, right string) int {
	if len(left) != len(right) {
		return len(left) + len(right)
	}

	changed := 0

	for index := 0; index < len(left); index++ {
		if left[index] != right[index] {
			changed++
		}
	}

	return changed
}

// downloadGlyphTolerance 是"当前图标是否就是可点击的预下载图标"允许的差异格数。
//
// 同一状态在不同 DPI 下实测差 0 格，而"已完成"状态差 15 格，取 6 既留出了
// 渲染抖动余量，又远小于跨状态的差异。
const downloadGlyphTolerance = 6

// glyphMatchesDownloadButton 判断圆钮图标是否就是「可点击开始/继续预下载」那一个。
//
// 启动器在预下载已完成、出错、排队等状态下会换成别的图标，此时不该再点它。
func glyphMatchesDownloadButton(glyph string) bool {
	return glyphDifference(glyph, downloadGlyph) <= downloadGlyphTolerance
}

// glyphSequenceAnimating 判断一串按固定间隔采样的图标指纹里是否存在"在动"。
//
// 与**首帧**比较而不是相邻比较：循环动画的周期里有一段箭头完全透明的时间，
// 相邻两帧很可能双双落在其中、看起来一模一样。只要有一帧与首帧不同，就说明
// 动画确实在跑。调用方负责让采样覆盖一整个动画周期。
func glyphSequenceAnimating(glyphs []string) bool {
	if len(glyphs) < 2 {
		return false
	}

	for _, glyph := range glyphs[1:] {
		if glyphDifference(glyphs[0], glyph) >= glyphChangeThreshold {
			return true
		}
	}

	return false
}

// findDisc 在期望位置附近寻找实心圆钮，找到则返回圆心与实测半径。
//
// tolerance 是允许的位置偏差（物理像素），但会被**收窄到圆钮半径的 20%**：
// 圆钮右手边约 12 CSS px 就是主按钮，搜索域一旦够到它，主按钮的左端会被算进
// 同一团，圆心右偏、半径虚大（实机两帧都踩过这个坑）。收窄后仍留出约 9px
// 物理误差余量，而模型实测误差不到 2px。
//
// 一次命中要求同时满足颜色、尺寸、圆形填充率与宽高比四项，
// 任何一项不满足都返回 false——宁可漏检，不可误点。
func findDisc(img image.Image, want circle, tolerance float64) (circle, bool) {
	limit := boundsOf(img)

	if clearance := want.Radius * discSearchToleranceRatio; tolerance > clearance {
		tolerance = clearance
	}

	maxDistance := want.Radius + tolerance

	search := box{
		Min: point{X: want.Center.X - maxDistance, Y: want.Center.Y - maxDistance},
		Max: point{X: want.Center.X + maxDistance, Y: want.Center.Y + maxDistance},
	}

	var (
		count      int
		minX, minY = math.Inf(1), math.Inf(1)
		maxX, maxY = math.Inf(-1), math.Inf(-1)
	)

	forEachPixel(img, search, limit, func(x, y int, r, g, b uint8) {
		dx := float64(x) + 0.5 - want.Center.X
		dy := float64(y) + 0.5 - want.Center.Y
		if dx*dx+dy*dy > maxDistance*maxDistance {
			return
		}

		if !isButtonYellow(r, g, b) {
			return
		}

		count++

		minX = math.Min(minX, float64(x))
		maxX = math.Max(maxX, float64(x))
		minY = math.Min(minY, float64(y))
		maxY = math.Max(maxY, float64(y))
	})

	if count == 0 {
		return circle{}, false
	}

	width := maxX - minX + 1
	height := maxY - minY + 1
	if width <= 0 || height <= 0 {
		return circle{}, false
	}

	if math.Abs(width-height)/height > discAspectTolerance {
		return circle{}, false
	}

	radius := (width + height) / 4
	if math.Abs(radius-want.Radius) > want.Radius*discRadiusTolerance {
		return circle{}, false
	}

	fill := float64(count) / (width * height)
	if fill < discFillMin || fill > discFillMax {
		return circle{}, false
	}

	return circle{Center: point{X: (minX + maxX) / 2, Y: (minY + maxY) / 2}, Radius: radius}, true
}

// findMainButton 在期望位置附近确认主按钮确实在。
//
// 与圆钮不同，主按钮里还有图标与文字，因此黄色填充率低得多（实测约 0.70），
// 判据相应放宽；但尺寸、宽高比与位置仍然卡住，避免把别处的黄色认成它。
func findMainButton(img image.Image, want box, tolerance float64) (box, bool) {
	limit := boundsOf(img)
	search := box{
		Min: point{X: want.Min.X - tolerance, Y: want.Min.Y - tolerance},
		Max: point{X: want.Max.X + tolerance, Y: want.Max.Y + tolerance},
	}

	var (
		count      int
		minX, minY = math.Inf(1), math.Inf(1)
		maxX, maxY = math.Inf(-1), math.Inf(-1)
	)

	forEachPixel(img, search, limit, func(x, y int, r, g, b uint8) {
		if !isButtonYellow(r, g, b) {
			return
		}

		count++

		minX = math.Min(minX, float64(x))
		maxX = math.Max(maxX, float64(x))
		minY = math.Min(minY, float64(y))
		maxY = math.Max(maxY, float64(y))
	})

	if count == 0 {
		return box{}, false
	}

	found := box{Min: point{X: minX, Y: minY}, Max: point{X: maxX + 1, Y: maxY + 1}}
	if found.Height() <= 0 {
		return box{}, false
	}

	aspect := found.Width() / found.Height()
	if aspect < mainButtonAspectMin || aspect > mainButtonAspectMax {
		return box{}, false
	}

	if math.Abs(found.Width()-want.Width()) > want.Width()*mainButtonLengthScale {
		return box{}, false
	}

	fill := float64(count) / (found.Width() * found.Height())
	if fill < mainButtonFillMin || fill > mainButtonFillMax {
		return box{}, false
	}

	return found, true
}

// findConfirmButton 在期望位置附近寻找弹窗里的白色药丸按钮。
func findConfirmButton(img image.Image, want box, tolerance float64) (box, bool) {
	limit := boundsOf(img)
	search := box{
		Min: point{X: want.Min.X - tolerance, Y: want.Min.Y - tolerance},
		Max: point{X: want.Max.X + tolerance, Y: want.Max.Y + tolerance},
	}

	var (
		count      int
		minX, minY = math.Inf(1), math.Inf(1)
		maxX, maxY = math.Inf(-1), math.Inf(-1)
	)

	forEachPixel(img, search, limit, func(x, y int, r, g, b uint8) {
		if !isWhiteButton(r, g, b) {
			return
		}

		count++

		minX = math.Min(minX, float64(x))
		maxX = math.Max(maxX, float64(x))
		minY = math.Min(minY, float64(y))
		maxY = math.Max(maxY, float64(y))
	})

	if count == 0 {
		return box{}, false
	}

	found := box{Min: point{X: minX, Y: minY}, Max: point{X: maxX + 1, Y: maxY + 1}}
	if found.Height() <= 0 {
		return box{}, false
	}

	aspect := found.Width() / found.Height()
	if aspect < confirmAspectMin || aspect > confirmAspectMax {
		return box{}, false
	}

	if math.Abs(found.Width()-want.Width()) > want.Width()*confirmLengthScale {
		return box{}, false
	}

	if float64(count)/(found.Width()*found.Height()) < confirmFillMin {
		return box{}, false
	}

	return found, true
}

// isButtonYellow 判断是否为主按钮/圆钮的黄底。
func isButtonYellow(r, g, b uint8) bool {
	return r >= yellowMinRG && g >= yellowMinRG &&
		b <= yellowMaxB && int(r)-int(b) >= yellowMinRGap
}

// isWhiteButton 判断是否为弹窗里的白色按钮。
func isWhiteButton(r, g, b uint8) bool {
	return r >= whiteMinChannel && g >= whiteMinChannel && b >= whiteMinChannel
}

// boundsOf 把图像的包围盒归一到 (0,0) 起点，方便按客户区坐标遍历。
func boundsOf(img image.Image) image.Rectangle {
	bounds := img.Bounds()

	return image.Rect(0, 0, bounds.Dx(), bounds.Dy())
}

// forEachPixel 遍历 search 与图像边界的交集，回调收到的是相对图像左上角的坐标。
func forEachPixel(
	img image.Image,
	search box,
	limit image.Rectangle,
	fn func(x, y int, r, g, b uint8),
) {
	origin := img.Bounds().Min
	x0 := clampInt(int(math.Floor(search.Min.X)), limit.Min.X, limit.Max.X)
	x1 := clampInt(int(math.Ceil(search.Max.X)), limit.Min.X, limit.Max.X)
	y0 := clampInt(int(math.Floor(search.Min.Y)), limit.Min.Y, limit.Max.Y)
	y1 := clampInt(int(math.Ceil(search.Max.Y)), limit.Min.Y, limit.Max.Y)

	for y := y0; y <= y1; y++ {
		for x := x0; x <= x1; x++ {
			r, g, b, _ := img.At(origin.X+x, origin.Y+y).RGBA()
			fn(x, y, uint8(r>>8), uint8(g>>8), uint8(b>>8))
		}
	}
}

// clampInt 把 value 夹到 [low, high] 区间内。
func clampInt(value, low, high int) int {
	if value < low {
		return low
	}

	if value > high {
		return high
	}

	return value
}
