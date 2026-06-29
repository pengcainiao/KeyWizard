package main

import (
	"bytes"
	"embed"
	"image"
	"image/color"
	_ "image/png"
	"log"
	"os"
	"strconv"
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

	idleAfter   = 650 * time.Millisecond // 停手多久回到待机
	tapHold     = 110 * time.Millisecond // 单次按键拳头/高亮保持时长
	cryWindow   = 900 * time.Millisecond // 统计打字速度的时间窗
	cryKeyCount = 6                      // 窗口内按键数 >= 此值 → 大哭
)

// 键盘布局（每行的键帽字符）
var kbRows = []string{"QWERTYUIOP", "ASDFGHJKL", "ZXCVBNM"}

type sprites struct {
	idle, cry, tapL, tapR *ebiten.Image
}

func loadSprite(name string) *ebiten.Image {
	data, err := assetsFS.ReadFile("assets/sprites/" + name)
	if err != nil {
		log.Fatalf("读取素材失败 %s: %v", name, err)
	}
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		log.Fatalf("解码素材失败 %s: %v", name, err)
	}
	return ebiten.NewImageFromImage(img)
}

type Game struct {
	sp     sprites
	events chan hook.Event

	lastKey  time.Time
	tapSide  bool        // false=左, true=右，每次按键翻转
	recent  []time.Time         // 最近按键时间，用于判断打字速度
	pressed map[uint16]time.Time // 当前高亮的键(gohook Keycode) -> 过期时间
	capCode map[rune]uint16     // 键帽字符 -> gohook Keycode

	dragging bool
	grabX    int
	grabY    int
	opaque   bool
	kbImg    *ebiten.Image // 键盘离屏图（用于倾斜贴回）
}

// envFloat 读取浮点环境变量，便于实时调参；无则用默认值。
func envFloat(key string, def float64) float64 {
	if s := os.Getenv(key); s != "" {
		if v, err := strconv.ParseFloat(s, 64); err == nil {
			return v
		}
	}
	return def
}

func (g *Game) Update() error {
	now := time.Now()

	// 1) 抽干全局键盘事件
	for {
		select {
		case ev := <-g.events:
			if ev.Kind == hook.KeyDown {
				g.onKeyDown(ev, now)
			}
		default:
			goto drained
		}
	}
drained:

	// 2) 清理过期高亮
	for code, exp := range g.pressed {
		if now.After(exp) {
			delete(g.pressed, code)
		}
	}

	// 3) 拖动窗口（左键）
	if inpututil.IsMouseButtonJustPressed(ebiten.MouseButtonLeft) {
		cx, cy := ebiten.CursorPosition()
		g.dragging = true
		g.grabX, g.grabY = cx, cy
	}
	if inpututil.IsMouseButtonJustReleased(ebiten.MouseButtonLeft) {
		g.dragging = false
	}
	if g.dragging {
		wx, wy := ebiten.WindowPosition()
		cx, cy := ebiten.CursorPosition()
		ebiten.SetWindowPosition(wx+cx-g.grabX, wy+cy-g.grabY)
	}

	// 4) 右键退出
	if inpututil.IsMouseButtonJustPressed(ebiten.MouseButtonRight) {
		return ebiten.Termination
	}

	return nil
}

func (g *Game) onKeyDown(ev hook.Event, now time.Time) {
	if os.Getenv("KW_DEBUG") != "" {
		log.Printf("KeyDown Rawcode=%d Keycode=%d Keychar=%d(%q)",
			ev.Rawcode, ev.Keycode, ev.Keychar, string(ev.Keychar))
	}
	g.lastKey = now
	g.tapSide = !g.tapSide

	// 记录速度
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

func (g *Game) currentSprite(now time.Time) *ebiten.Image {
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

func (g *Game) Draw(screen *ebiten.Image) {
	now := time.Now()

	if g.opaque {
		screen.Fill(color.NRGBA{0xf2, 0xf2, 0xf4, 0xff})
	}

	// 娃
	spr := g.currentSprite(now)
	const babyW = 286.0
	scale := babyW / 512.0
	op := &ebiten.DrawImageOptions{}
	op.GeoM.Scale(scale, scale)
	op.GeoM.Translate((screenW-babyW)/2, -10)
	screen.DrawImage(spr, op)

	// 键盘：先画到离屏图，再斜着贴回，产生"斜看"的立体感
	const kbW, kbH = screenW, 110
	if g.kbImg == nil {
		g.kbImg = ebiten.NewImage(kbW, kbH)
	}
	g.kbImg.Clear()
	g.drawKeyboard(g.kbImg)

	skew := envFloat("KW_SKEW", 0.34)     // 倾斜角(弧度)
	squash := envFloat("KW_SQUASH", 0.60) // 垂直压扁比
	narrow := envFloat("KW_NARROW", 0.92) // 横向收窄
	offX := envFloat("KW_KBX", 0)         // 相对窗口中心的水平微调
	offY := envFloat("KW_KBY", 272)       // 键盘中心的屏幕 y

	// 绕键盘自身中心做倾斜/缩放，保证始终水平居中（与娃对齐）
	const kbCX, kbCY = kbW / 2, 50.0
	kop := &ebiten.DrawImageOptions{}
	kop.GeoM.Translate(-kbCX, -kbCY)
	kop.GeoM.Skew(skew, 0)
	kop.GeoM.Scale(narrow, squash)
	kop.GeoM.Translate(screenW/2+offX, offY)
	kop.Filter = ebiten.FilterLinear
	screen.DrawImage(g.kbImg, kop)
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

func (g *Game) drawKeyboard(screen *ebiten.Image) {
	const (
		keyW   = 24.0
		keyH   = 16.0
		gap    = 3.0
		rowGap = 4.0
		top    = 8.0 // 离屏图内的局部顶边
		lip    = 3.5 // 键帽高度（立体感）
	)
	baseFill := color.NRGBA{0x3a, 0x3f, 0x4b, 235}
	capTop := color.NRGBA{0xf4, 0xf6, 0xfa, 255}   // 顶面
	capSide := color.NRGBA{0xb2, 0xb9, 0xc6, 255}  // 侧壁/底座(深)
	capGloss := color.NRGBA{0xff, 0xff, 0xff, 200} // 顶部高光
	downTop := color.NRGBA{0x9a, 0xc0, 0xf7, 255}  // 按下顶面
	downSide := color.NRGBA{0x5f, 0x86, 0xc4, 255} // 按下侧壁
	txtCol := color.NRGBA{0x2b, 0x2f, 0x38, 255}

	// 键盘底座(圆角)
	fillRoundRect(screen, 10, top-7, screenW-20, 4*(keyH+rowGap)+lip+10, 8, baseFill)

	var y float32 = top
	drawCap := func(x float32, w float32, label string, code uint16) {
		const r = 4.5
		_, down := g.pressed[code]
		topFill, sideFill := capTop, capSide
		if down {
			topFill, sideFill = downTop, downSide
		}
		// 侧壁/底座：始终在下方，露出 lip 形成高度
		fillRoundRect(screen, x, y+lip, w, keyH, r, sideFill)
		// 顶面：未按下时浮在上方；按下时下移与底座齐平
		ty := y
		if down {
			ty = y + lip
		}
		fillRoundRect(screen, x, ty, w, keyH, r, topFill)
		fillRoundRect(screen, x+3, ty+1.5, w-6, 2, 1, capGloss) // 顶部高光
		if label != "" {
			tx := int(x + (w-7)/2)
			tyl := int(ty + 12)
			text.Draw(screen, label, basicfont.Face7x13, tx, tyl, txtCol)
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

	// 空格键
	spaceW := float32(150)
	sx := float32(screenW)/2 - spaceW/2
	drawCap(sx, spaceW, "", g.capCode[' '])
}

func (g *Game) Layout(outsideWidth, outsideHeight int) (int, int) {
	return screenW, screenH
}

func main() {
	g := &Game{
		sp: sprites{
			idle: loadSprite("idle.png"),
			cry:  loadSprite("cry.png"),
			tapL: loadSprite("tap_left.png"),
			tapR: loadSprite("tap_right.png"),
		},
		pressed: map[uint16]time.Time{},
		capCode: map[rune]uint16{},
		lastKey: time.Now().Add(-time.Hour),
	}

	// 键帽字符 -> gohook Keycode（用库自带名称表，避免硬编码）
	for _, row := range kbRows {
		for _, ch := range row {
			name := string(ch + 32) // 大写转小写
			if code, ok := hook.Keycode[name]; ok {
				g.capCode[ch] = code
			}
		}
	}
	g.capCode[' '] = hook.Keycode["space"]

	// 启动全局键盘监听（需要 macOS「输入监控」权限）
	// KW_NOHOOK=1 跳过监听，用于排查渲染冲突
	if os.Getenv("KW_NOHOOK") == "" {
		g.events = hook.Start()
		defer hook.End()
	} else {
		g.events = make(chan hook.Event)
	}

	ebiten.SetWindowSize(screenW, screenH)
	ebiten.SetWindowTitle("哭娃打字桌宠")
	ebiten.SetWindowFloating(true)
	ebiten.SetWindowResizingMode(ebiten.WindowResizingModeDisabled)

	// KW_OPAQUE=1 关闭透明，用于排查 Metal 画布分配失败
	transparent := os.Getenv("KW_OPAQUE") == ""
	g.opaque = !transparent
	if transparent {
		ebiten.SetWindowDecorated(false)
	}

	op := &ebiten.RunGameOptions{ScreenTransparent: transparent}
	if err := ebiten.RunGameWithOptions(g, op); err != nil && err != ebiten.Termination {
		log.Fatal(err)
	}
}
