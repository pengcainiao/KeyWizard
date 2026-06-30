package main

import (
	"bytes"
	"embed"
	"encoding/json"
	"image"
	"image/color"
	_ "image/png"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"sync"
	"time"

	"github.com/hajimehoshi/ebiten/v2"
	"github.com/hajimehoshi/ebiten/v2/inpututil"
	"github.com/hajimehoshi/ebiten/v2/text"
	"github.com/hajimehoshi/ebiten/v2/vector"
	hook "github.com/robotn/gohook"
	"golang.org/x/image/font/basicfont"
)

//go:embed assets/sprites/idle.png assets/sprites/cry.png assets/sprites/tap_left.png assets/sprites/tap_right.png
var assetsFS embed.FS

const (
	screenW = 360
	screenH = 340

	closeX = screenW - 22 // 关闭按钮圆心
	closeY = 22
	closeR = 11
	gearX  = closeX - 28 // 设置(齿轮)按钮圆心
	gearY  = closeY
	gearR  = closeR

	idleAfter   = 650 * time.Millisecond // 停手多久回到待机
	tapHold     = 110 * time.Millisecond // 单次按键拳头/高亮保持时长
	cryWindow   = 900 * time.Millisecond // 统计打字速度的时间窗
	cryKeyCount = 6                      // 窗口内按键数 >= 此值 → 大哭

	// 键盘变换默认值
	defOffX   = 0.0
	defOffY   = 272.0
	defSkew   = 0.34
	defSquash = 0.60
	kbNarrow  = 0.92 // 横向收窄(固定)

	// 设置面板布局
	panelX = 12
	panelY = 44
	panelW = 200
	btnX   = panelX + 8
	btnW   = panelW - 16
	btnH   = 26
	btnGap = 8
	btn0Y  = panelY + 10
)

// 键盘布局（每行的键帽字符）
var kbRows = []string{"QWERTYUIOP", "ASDFGHJKL", "ZXCVBNM"}

// 素材文件名
var spriteNames = []string{"idle.png", "cry.png", "tap_left.png", "tap_right.png"}

type sprites struct {
	idle, cry, tapL, tapR *ebiten.Image
}

// Config 持久化到 config.json
type Config struct {
	KbOffX   float64 `json:"kb_off_x"`
	KbOffY   float64 `json:"kb_off_y"`
	KbSkew   float64 `json:"kb_skew"`
	KbSquash float64 `json:"kb_squash"`
}

type rectI struct{ x, y, w, h int }

func (r rectI) hit(px, py int) bool {
	return px >= r.x && px < r.x+r.w && py >= r.y && py < r.y+r.h
}

func inCircle(px, py, cx, cy, r int) bool {
	dx, dy := px-cx, py-cy
	return dx*dx+dy*dy <= r*r
}

func clamp(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

type Game struct {
	sp     sprites
	events chan hook.Event

	mu      sync.Mutex // 保护下面被监听 goroutine 与渲染共享的状态
	lastKey time.Time
	tapSide bool                 // false=左, true=右，每次按键翻转
	recent  []time.Time          // 最近按键时间，用于判断打字速度
	pressed map[uint16]time.Time // 当前高亮的键(gohook Keycode) -> 过期时间
	capCode map[rune]uint16      // 键帽字符 -> gohook Keycode

	// 键盘变换参数（可拖动调整、持久化）
	offX, offY, skew, squash float64

	// 交互状态
	dragging                       bool // 拖窗口
	kbDragging                     bool // 拖键盘位置
	tiltDragging                   bool // 拖键盘倾斜
	grabX, grabY                   int
	kbStartX, kbStartY             int
	kbStartOffX, kbStartOffY       float64
	tiltStartX, tiltStartY         int
	tiltStartSkew, tiltStartSquash float64
	showSettings                   bool

	opaque   bool
	kbImg    *ebiten.Image // 键盘离屏图（用于倾斜贴回）
	babyOp   *ebiten.DrawImageOptions
	kbOp     *ebiten.DrawImageOptions
	downSnap map[uint16]bool // Draw 用的按下键快照（复用，避免每帧分配）

	appDir    string
	spriteDir string
}

// ---------- 路径 / 配置 ----------

func appDir() string {
	if exe, err := os.Executable(); err == nil {
		return filepath.Dir(exe)
	}
	return "."
}

func (g *Game) configPath() string { return filepath.Join(g.appDir, "config.json") }

func (g *Game) loadConfig() {
	g.offX, g.offY, g.skew, g.squash = defOffX, defOffY, defSkew, defSquash
	data, err := os.ReadFile(g.configPath())
	if err != nil {
		return
	}
	var c Config
	if json.Unmarshal(data, &c) != nil {
		return
	}
	g.offX, g.offY, g.skew, g.squash = c.KbOffX, c.KbOffY, c.KbSkew, c.KbSquash
	if g.squash == 0 { // 兼容空配置
		g.squash = defSquash
	}
}

func (g *Game) save() {
	c := Config{KbOffX: g.offX, KbOffY: g.offY, KbSkew: g.skew, KbSquash: g.squash}
	if data, err := json.MarshalIndent(c, "", "  "); err == nil {
		_ = os.WriteFile(g.configPath(), data, 0644)
	}
}

// ---------- 素材 ----------

func decodeImg(data []byte) (*ebiten.Image, error) {
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	return ebiten.NewImageFromImage(img), nil
}

// loadSprites 优先从外部 sprites/ 读，缺失则用内置默认。
func (g *Game) loadSprites() {
	load := func(name string) *ebiten.Image {
		if g.spriteDir != "" {
			if data, err := os.ReadFile(filepath.Join(g.spriteDir, name)); err == nil {
				if img, err := decodeImg(data); err == nil {
					return img
				}
			}
		}
		data, err := assetsFS.ReadFile("assets/sprites/" + name)
		if err != nil {
			log.Fatalf("内置素材缺失 %s: %v", name, err)
		}
		img, err := decodeImg(data)
		if err != nil {
			log.Fatalf("解码内置素材失败 %s: %v", name, err)
		}
		return img
	}
	// 先全部加载好新图，再释放旧纹理，避免重载时累积 GPU 资源。
	ns := sprites{
		idle: load("idle.png"),
		cry:  load("cry.png"),
		tapL: load("tap_left.png"),
		tapR: load("tap_right.png"),
	}
	for _, im := range []*ebiten.Image{g.sp.idle, g.sp.cry, g.sp.tapL, g.sp.tapR} {
		if im != nil {
			im.Deallocate()
		}
	}
	g.sp = ns
}

// ensureSpriteDir 建好外部文件夹，并把内置默认图导出（缺哪张补哪张）。
func (g *Game) ensureSpriteDir() {
	_ = os.MkdirAll(g.spriteDir, 0755)
	for _, name := range spriteNames {
		p := filepath.Join(g.spriteDir, name)
		if _, err := os.Stat(p); err != nil {
			if data, err := assetsFS.ReadFile("assets/sprites/" + name); err == nil {
				_ = os.WriteFile(p, data, 0644)
			}
		}
	}
}

func openFolder(path string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", path)
	case "windows":
		cmd = exec.Command("explorer", path)
	default:
		cmd = exec.Command("xdg-open", path)
	}
	_ = cmd.Start()
}

// ---------- 更新 ----------

func (g *Game) Update() error {
	now := time.Now()

	// 1) 清理过期高亮，并判断是否仍在动画中（共享状态加锁）
	g.mu.Lock()
	for code, exp := range g.pressed {
		if now.After(exp) {
			delete(g.pressed, code)
		}
	}
	animating := len(g.pressed) > 0 || now.Sub(g.lastKey) < idleAfter
	g.mu.Unlock()

	// 3) 左键按下：按优先级分派
	if inpututil.IsMouseButtonJustPressed(ebiten.MouseButtonLeft) {
		cx, cy := ebiten.CursorPosition()
		switch {
		case inCircle(cx, cy, closeX, closeY, closeR):
			g.save()
			return ebiten.Termination
		case inCircle(cx, cy, gearX, gearY, gearR):
			g.showSettings = !g.showSettings
		case g.showSettings && g.handleSettingsClick(cx, cy):
			// 已被设置面板处理
		case g.showSettings && g.tiltHandleHit(cx, cy):
			g.tiltDragging = true
			g.tiltStartX, g.tiltStartY = cx, cy
			g.tiltStartSkew, g.tiltStartSquash = g.skew, g.squash
		case g.kbHit(cx, cy):
			g.kbDragging = true
			g.kbStartX, g.kbStartY = cx, cy
			g.kbStartOffX, g.kbStartOffY = g.offX, g.offY
		default:
			g.dragging = true
			g.grabX, g.grabY = cx, cy
		}
	}
	if inpututil.IsMouseButtonJustReleased(ebiten.MouseButtonLeft) {
		if g.kbDragging || g.tiltDragging {
			g.save()
		}
		g.dragging = false
		g.kbDragging = false
		g.tiltDragging = false
	}
	if g.dragging {
		wx, wy := ebiten.WindowPosition()
		cx, cy := ebiten.CursorPosition()
		ebiten.SetWindowPosition(wx+cx-g.grabX, wy+cy-g.grabY)
	}
	if g.kbDragging {
		cx, cy := ebiten.CursorPosition()
		g.offX = g.kbStartOffX + float64(cx-g.kbStartX)
		g.offY = g.kbStartOffY + float64(cy-g.kbStartY)
	}
	if g.tiltDragging {
		cx, cy := ebiten.CursorPosition()
		g.skew = clamp(g.tiltStartSkew+float64(cx-g.tiltStartX)*0.004, -0.9, 0.9)
		g.squash = clamp(g.tiltStartSquash+float64(g.tiltStartY-cy)*0.004, 0.30, 1.0)
	}

	// 4) 右键退出
	if inpututil.IsMouseButtonJustPressed(ebiten.MouseButtonRight) {
		g.save()
		return ebiten.Termination
	}

	// 5) 仅在有动画/交互时请求下一帧；空闲时不出帧 → 渲染层零增长
	if animating || g.dragging || g.kbDragging || g.tiltDragging || g.showSettings {
		ebiten.ScheduleFrame()
	}
	return nil
}

const tiltHandleR = 10

// 倾斜调节手柄位置（键盘上方偏右）
func (g *Game) tiltHandlePos() (int, int) {
	return int(float64(screenW)/2 + g.offX + 120), int(g.offY - 30)
}

func (g *Game) tiltHandleHit(px, py int) bool {
	hx, hy := g.tiltHandlePos()
	return inCircle(px, py, hx, hy, tiltHandleR)
}

// 键盘点击命中区域（近似的轴对齐框）
func (g *Game) kbHit(px, py int) bool {
	cx := float64(screenW)/2 + g.offX
	return float64(px) >= cx-150 && float64(px) <= cx+150 &&
		float64(py) >= g.offY-40 && float64(py) <= g.offY+40
}

func settingsButtons() []rectI {
	rs := make([]rectI, 3)
	for i := 0; i < 3; i++ {
		rs[i] = rectI{btnX, btn0Y + i*(btnH+btnGap), btnW, btnH}
	}
	return rs
}

// handleSettingsClick 处理面板点击，返回是否消费了本次点击。
func (g *Game) handleSettingsClick(px, py int) bool {
	for i, r := range settingsButtons() {
		if r.hit(px, py) {
			switch i {
			case 0:
				g.ensureSpriteDir()
				openFolder(g.spriteDir)
			case 1:
				g.loadSprites()
			case 2:
				g.offX, g.offY = defOffX, defOffY
				g.save()
			}
			return true
		}
	}
	// 点在面板内但不是按钮：也消费，避免误触发拖拽
	return rectI{panelX, panelY, panelW, panelHeight()}.hit(px, py)
}

func panelHeight() int { return 10 + 3*(btnH+btnGap) + 58 }

// listen 在独立 goroutine 里消费全局键盘事件，并唤醒渲染。
// 因为采用按需出帧(FPSModeVsyncOffMinimum)，空闲时 Update 不再运行，
// 必须由这里 ScheduleFrame 把桌宠"叫醒"来播放反应动画。
func (g *Game) listen() {
	for ev := range g.events {
		if ev.Kind != hook.KeyDown {
			continue
		}
		g.mu.Lock()
		g.applyKeyDown(ev, time.Now())
		g.mu.Unlock()
		ebiten.ScheduleFrame()
	}
}

// applyKeyDown 必须在持有 g.mu 时调用。
func (g *Game) applyKeyDown(ev hook.Event, now time.Time) {
	if os.Getenv("KW_DEBUG") != "" {
		log.Printf("KeyDown Keycode=%d", ev.Keycode)
	}
	g.lastKey = now
	g.tapSide = !g.tapSide

	g.recent = append(g.recent, now)
	cut := now.Add(-cryWindow)
	i := 0
	for ; i < len(g.recent); i++ {
		if g.recent[i].After(cut) {
			break
		}
	}
	g.recent = g.recent[i:]

	// 高亮被敲的键。KeyDown 时 Keychar 为空，用 gohook 的跨平台 Keycode 字段。
	g.pressed[ev.Keycode] = now.Add(tapHold)
}

// currentSpriteLocked 必须在持有 g.mu 时调用。
func (g *Game) currentSpriteLocked(now time.Time) *ebiten.Image {
	if now.Sub(g.lastKey) > idleAfter {
		return g.sp.idle
	}
	if len(g.recent) >= cryKeyCount {
		return g.sp.cry
	}
	if g.tapSide {
		return g.sp.tapR
	}
	return g.sp.tapL
}

// ---------- 绘制 ----------

func (g *Game) Draw(screen *ebiten.Image) {
	now := time.Now()

	if g.opaque {
		screen.Fill(color.NRGBA{0xf2, 0xf2, 0xf4, 0xff})
	}

	// 取共享状态快照（加锁），避免与监听 goroutine 竞争
	if g.downSnap == nil {
		g.downSnap = map[uint16]bool{}
	}
	for k := range g.downSnap {
		delete(g.downSnap, k)
	}
	g.mu.Lock()
	spr := g.currentSpriteLocked(now)
	for code := range g.pressed {
		g.downSnap[code] = true
	}
	g.mu.Unlock()

	// 娃
	const babyW = 286.0
	scale := babyW / 512.0
	op := g.babyOp
	*op = ebiten.DrawImageOptions{}
	op.GeoM.Scale(scale, scale)
	op.GeoM.Translate((screenW-babyW)/2, -10)
	screen.DrawImage(spr, op)

	// 键盘：画到离屏图再斜着贴回
	const kbW, kbH = screenW, 110
	if g.kbImg == nil {
		g.kbImg = ebiten.NewImage(kbW, kbH)
	}
	g.kbImg.Clear()
	g.drawKeyboard(g.kbImg, g.downSnap)

	const kbCX, kbCY = kbW / 2, 50.0
	kop := g.kbOp
	*kop = ebiten.DrawImageOptions{}
	kop.GeoM.Translate(-kbCX, -kbCY)
	kop.GeoM.Skew(g.skew, 0)
	kop.GeoM.Scale(kbNarrow, g.squash)
	kop.GeoM.Translate(screenW/2+g.offX, g.offY)
	kop.Filter = ebiten.FilterLinear
	screen.DrawImage(g.kbImg, kop)

	// 设置面板（在按钮之前画，按钮始终在最上层）
	if g.showSettings {
		g.drawSettings(screen)
	}

	// 齿轮按钮（汉堡三横线）
	white := color.NRGBA{0xff, 0xff, 0xff, 255}
	vector.DrawFilledCircle(screen, gearX, gearY, gearR, color.NRGBA{0x55, 0x5b, 0x68, 235}, true)
	for i := -1; i <= 1; i++ {
		yy := float32(gearY + i*4)
		vector.StrokeLine(screen, gearX-5, yy, gearX+5, yy, 2, white, true)
	}

	// 关闭按钮（红圆 + ×）
	vector.DrawFilledCircle(screen, closeX, closeY, closeR, color.NRGBA{0xe0, 0x5b, 0x5b, 235}, true)
	vector.StrokeLine(screen, closeX-4, closeY-4, closeX+4, closeY+4, 2, white, true)
	vector.StrokeLine(screen, closeX-4, closeY+4, closeX+4, closeY-4, 2, white, true)
}

func (g *Game) drawSettings(screen *ebiten.Image) {
	white := color.NRGBA{0xff, 0xff, 0xff, 255}
	gray := color.NRGBA{0xc2, 0xc7, 0xd0, 255}
	// 面板背景
	fillRoundRect(screen, panelX, panelY, panelW, float32(panelHeight()), 10, color.NRGBA{0x23, 0x27, 0x30, 240})
	labels := []string{"Open image folder", "Reload images", "Reset keyboard"}
	for i, r := range settingsButtons() {
		fillRoundRect(screen, float32(r.x), float32(r.y), float32(r.w), float32(r.h), 6, color.NRGBA{0x46, 0x4d, 0x5d, 255})
		text.Draw(screen, labels[i], basicfont.Face7x13, r.x+10, r.y+17, white)
	}
	// 提示
	hy := btn0Y + 3*(btnH+btnGap) + 4
	text.Draw(screen, "Drag keyboard = move it", basicfont.Face7x13, btnX, hy+10, gray)
	text.Draw(screen, "Drag baby = move window", basicfont.Face7x13, btnX, hy+24, gray)
	text.Draw(screen, "Drag dot = tilt / flatten", basicfont.Face7x13, btnX, hy+38, gray)

	// 倾斜调节手柄（青色圆 + 对角线）
	hx, hyy := g.tiltHandlePos()
	vector.DrawFilledCircle(screen, float32(hx), float32(hyy), tiltHandleR, color.NRGBA{0x2f, 0xb6, 0xa8, 245}, true)
	vector.StrokeLine(screen, float32(hx-5), float32(hyy+5), float32(hx+5), float32(hyy-5), 2, white, true)
}

// fillRoundRect 用两个交叉矩形 + 四角圆画圆角矩形（不透明填充）。
func fillRoundRect(dst *ebiten.Image, x, y, w, h, r float32, clr color.Color) {
	if r > w/2 {
		r = w / 2
	}
	if r > h/2 {
		r = h / 2
	}
	vector.DrawFilledRect(dst, x+r, y, w-2*r, h, clr, true)
	vector.DrawFilledRect(dst, x, y+r, w, h-2*r, clr, true)
	vector.DrawFilledCircle(dst, x+r, y+r, r, clr, true)
	vector.DrawFilledCircle(dst, x+w-r, y+r, r, clr, true)
	vector.DrawFilledCircle(dst, x+r, y+h-r, r, clr, true)
	vector.DrawFilledCircle(dst, x+w-r, y+h-r, r, clr, true)
}

func (g *Game) drawKeyboard(screen *ebiten.Image, down map[uint16]bool) {
	const (
		keyW   = 24.0
		keyH   = 16.0
		gap    = 3.0
		rowGap = 4.0
		top    = 8.0 // 离屏图内的局部顶边
		lip    = 3.5 // 键帽高度（立体感）
	)
	baseFill := color.NRGBA{0x3a, 0x3f, 0x4b, 235}
	capTop := color.NRGBA{0xf4, 0xf6, 0xfa, 255}
	capSide := color.NRGBA{0xb2, 0xb9, 0xc6, 255}
	capGloss := color.NRGBA{0xff, 0xff, 0xff, 200}
	downTop := color.NRGBA{0x9a, 0xc0, 0xf7, 255}
	downSide := color.NRGBA{0x5f, 0x86, 0xc4, 255}
	txtCol := color.NRGBA{0x2b, 0x2f, 0x38, 255}

	fillRoundRect(screen, 10, top-7, screenW-20, 4*(keyH+rowGap)+lip+10, 8, baseFill)

	var y float32 = top
	drawCap := func(x float32, w float32, label string, code uint16) {
		const r = 4.5
		isDown := down[code]
		topFill, sideFill := capTop, capSide
		if isDown {
			topFill, sideFill = downTop, downSide
		}
		fillRoundRect(screen, x, y+lip, w, keyH, r, sideFill)
		ty := y
		if isDown {
			ty = y + lip
		}
		fillRoundRect(screen, x, ty, w, keyH, r, topFill)
		fillRoundRect(screen, x+3, ty+1.5, w-6, 2, 1, capGloss)
		if label != "" {
			text.Draw(screen, label, basicfont.Face7x13, int(x+(w-7)/2), int(ty+12), txtCol)
		}
	}

	for _, row := range kbRows {
		n := float32(len(row))
		rowW := n*keyW + (n-1)*gap
		x := float32(screenW)/2 - rowW/2
		for _, ch := range row {
			drawCap(x, keyW, string(ch), g.capCode[ch])
			x += keyW + gap
		}
		y += keyH + rowGap
	}

	spaceW := float32(150)
	sx := float32(screenW)/2 - spaceW/2
	drawCap(sx, spaceW, "", g.capCode[' '])
}

func (g *Game) Layout(outsideWidth, outsideHeight int) (int, int) {
	return screenW, screenH
}

func main() {
	dir := appDir()
	g := &Game{
		pressed:   map[uint16]time.Time{},
		capCode:   map[rune]uint16{},
		lastKey:   time.Now().Add(-time.Hour),
		babyOp:    &ebiten.DrawImageOptions{},
		kbOp:      &ebiten.DrawImageOptions{},
		appDir:    dir,
		spriteDir: filepath.Join(dir, "sprites"),
	}
	g.loadConfig()
	g.loadSprites()

	// 键帽字符 -> gohook Keycode
	for _, row := range kbRows {
		for _, ch := range row {
			if code, ok := hook.Keycode[string(ch+32)]; ok {
				g.capCode[ch] = code
			}
		}
	}
	g.capCode[' '] = hook.Keycode["space"]

	// 兜底：定期把空闲内存还给系统，避免后台长时间运行 RSS 高水位
	go func() {
		for range time.Tick(30 * time.Second) {
			debug.FreeOSMemory()
		}
	}()

	// 诊断：KW_MEMLOG=1 定期打印内存/协程，帮助定位是否仍在增长
	if os.Getenv("KW_MEMLOG") != "" {
		go func() {
			var m runtime.MemStats
			for range time.Tick(10 * time.Second) {
				runtime.ReadMemStats(&m)
				log.Printf("MEM heapAlloc=%dKB heapSys=%dKB sys=%dKB goroutines=%d",
					m.HeapAlloc/1024, m.HeapSys/1024, m.Sys/1024, runtime.NumGoroutine())
			}
		}()
	}

	// 全局键盘监听（需 macOS「输入监控」权限）；KW_NOHOOK=1 可跳过
	if os.Getenv("KW_NOHOOK") == "" {
		g.events = hook.Start()
		defer hook.End()
	} else {
		g.events = make(chan hook.Event)
	}
	go g.listen() // 独立 goroutine 消费按键并唤醒渲染

	ebiten.SetWindowSize(screenW, screenH)
	ebiten.SetWindowTitle("哭娃打字桌宠")
	ebiten.SetWindowFloating(true)
	ebiten.SetWindowResizingMode(ebiten.WindowResizingModeDisabled)
	// 按需出帧：空闲时不渲染 → 渲染层零增长；靠 ScheduleFrame 唤醒
	ebiten.SetFPSMode(ebiten.FPSModeVsyncOffMinimum)

	transparent := os.Getenv("KW_OPAQUE") == ""
	g.opaque = !transparent
	if transparent {
		ebiten.SetWindowDecorated(false)
	}

	op := &ebiten.RunGameOptions{ScreenTransparent: transparent}
	if err := ebiten.RunGameWithOptions(g, op); err != nil && err != ebiten.Termination {
		log.Fatal(err)
	}
	g.save()
}
