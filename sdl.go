// Package sdl provides a Driver for making native graphical apps.
package sdl

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"log"
	"time"
	"unicode/utf8"

	"golang.org/x/image/bmp"

	"github.com/anaseto/gruid"
	"github.com/veandco/go-sdl2/sdl"
)

// TileManager manages tiles fetching.
type TileManager interface {
	// GetImage returns the image to be used for a given cell style.
	GetImage(gruid.Cell) image.Image

	// TileSize returns the (width, height) in pixels of the tiles. Both
	// should be positive and non-zero.
	TileSize() gruid.Point
}

// Driver implements gruid.Driver using the go-sdl2 bindings for the SDL
// library. When using an gruid.App, Start has to be used on the main routine,
// as the video functions of SDL are not thread safe.
type Driver struct {
	tm         TileManager
	width      int32
	height     int32
	fullscreen bool
	tw         int32
	th         int32

	window      *sdl.Window
	renderer    *sdl.Renderer
	textures    map[gruid.Cell]*sdl.Texture
	mousepos    gruid.Point
	mousedrag   gruid.MouseAction
	init        bool
	reqredraw   chan bool // request redraw
	noQuit      bool      // do not quit on close
	actions     chan func()
	accelerated bool
	scaleX      float32
	scaleY      float32
	title       string
	icon        image.Image
}

// Config contains configurations options for the driver.
type Config struct {
	TileManager TileManager // for retrieving tiles (required)
	Width       int32       // initial screen width in cells (default: 80)
	Height      int32       // initial screen height in cells (default: 24)
	Fullscreen  bool        // use “real” fullscreen with a videomode change
	Accelerated bool        // use accelerated renderer (rarely necessary)
	WindowTitle string      // window title (default: gruid go-sdl2)
	WindowIcon  image.Image // window icon (optional)
}

// NewDriver returns a new driver with given configuration options.
func NewDriver(cfg Config) *Driver {
	dr := &Driver{}
	dr.width = cfg.Width
	if dr.width <= 0 {
		dr.width = 80
	}
	dr.height = cfg.Height
	if dr.height <= 0 {
		dr.height = 24
	}
	dr.title = cfg.WindowTitle
	if dr.title == "" {
		dr.title = "gruid go-sdl2"
	}
	dr.fullscreen = cfg.Fullscreen
	dr.SetTileManager(cfg.TileManager)
	dr.accelerated = cfg.Accelerated
	dr.icon = cfg.WindowIcon
	return dr
}

// SetTileManager allows to change the used tile manager. If the driver is
// already running, change will take effect with next Flush so that the
// function is thread safe.
func (dr *Driver) SetTileManager(tm TileManager) {
	fn := func() {
		dr.tm = tm
		p := tm.TileSize()
		dr.tw, dr.th = int32(p.X), int32(p.Y)
		if dr.tw <= 0 {
			dr.tw = 1
		}
		if dr.th <= 0 {
			dr.th = 1
		}
		if dr.init {
			dr.ClearCache()
			scale := false
			if dr.scaleX > 0.1 && dr.scaleY > 0.1 {
				scale = dr.setScale(dr.scaleX, dr.scaleY)
			}
			if !scale {
				dr.scaleX = 0
				dr.scaleY = 0
				dr.resizeWindow()
			}
			select {
			case dr.reqredraw <- true:
			default:
			}
		}
	}
	if dr.init {
		select {
		case dr.actions <- fn:
		default:
		}
	} else {
		fn()
	}
}

func (dr *Driver) setScale(scaleX, scaleY float32) bool {
	err := dr.renderer.SetScale(scaleX, scaleY)
	if err != nil {
		log.Printf("SetScale: %v", err)
		return false
	}
	dr.scaleX = scaleX
	dr.scaleY = scaleY
	dr.resizeWindow()
	return true
}

func (dr *Driver) resizeWindow() {
	if dr.scaleX > 0.1 && dr.scaleY > 0.1 {
		dr.window.SetSize(int32(float32(dr.width*dr.tw)*dr.scaleX), int32(float32(dr.height*dr.th)*dr.scaleY))
	} else {
		dr.window.SetSize(dr.width*dr.tw, dr.height*dr.th)
	}
}

// SetScale modifies the rendering scale for rendering, and updates the window
// size accordingly. Integer values give more accurate results.
func (dr *Driver) SetScale(scaleX, scaleY float32) {
	fn := func() {
		dr.setScale(scaleX, scaleY)
	}
	dr.scaleX = scaleX
	dr.scaleY = scaleY
	if dr.init {
		select {
		case dr.actions <- fn:
		default:
		}
	}
}

// SetWindowTitle sets the window title.
func (dr *Driver) SetWindowTitle(title string) {
	fn := func() {
		dr.window.SetTitle(title)
	}
	dr.title = title
	if dr.init {
		select {
		case dr.actions <- fn:
		default:
		}
	}
}

// PreventQuit will make next call to Close keep sdl and the main window
// running. It can be used to chain two applications with the same sdl session
// and window. It is then your reponsibility to either run another application
// or call Close manually to properly quit.
func (dr *Driver) PreventQuit() {
	dr.noQuit = true
}

// Init implements gruid.Driver.Init. It initializes structures and calls
// sdl.Init().
func (dr *Driver) Init() error {
	dr.reqredraw = make(chan bool, 1)
	dr.actions = make(chan func(), 4)
	if dr.tm == nil {
		return errors.New("no tile manager provided")
	}
	var err error
	if dr.init {
		dr.resizeWindow()
	} else {
		if err = sdl.Init(sdl.INIT_VIDEO); err != nil {
			return err
		}
		dr.window, err = sdl.CreateWindow(dr.title, sdl.WINDOWPOS_UNDEFINED, sdl.WINDOWPOS_UNDEFINED,
			dr.width*dr.tw, dr.height*dr.th, sdl.WINDOW_SHOWN)
		if err != nil {
			return fmt.Errorf("failed to create sdl window: %v", err)
		}
		if dr.accelerated {
			dr.renderer, err = sdl.CreateRenderer(dr.window, -1, sdl.RENDERER_ACCELERATED)
		} else {
			dr.renderer, err = sdl.CreateRenderer(dr.window, -1, sdl.RENDERER_SOFTWARE)
		}
		if err != nil {
			return fmt.Errorf("failed to create sdl renderer: %v", err)
		}
		dr.window.SetResizable(false)
		dr.setIcon()
		if dr.fullscreen {
			err := dr.window.SetFullscreen(sdl.WINDOW_FULLSCREEN)
			if err != nil {
				log.Printf("set fullscreen: %v", err)
			}
		}
		if dr.scaleX > 0.1 || dr.scaleY > 0.1 {
			dr.setScale(dr.scaleX, dr.scaleY)
		}
		err := dr.renderer.Clear()
		if err != nil {
			log.Printf("renderer clear: %v", err)
		}
		sdl.StartTextInput()
		rect := sdl.Rect{X: 0, Y: 0, W: 100, H: 100}
		sdl.SetTextInputRect(&rect)
	}
	dr.textures = make(map[gruid.Cell]*sdl.Texture)
	dr.mousedrag = -1
	dr.init = true
	return nil
}

func (dr *Driver) setIcon() {
	if dr.icon == nil {
		return
	}
	sf, err := imageToSurface(dr.icon)
	if err != nil {
		log.Printf("bad icon image: %v", err)
		return
	}
	dr.window.SetIcon(sf)
}

func (dr *Driver) coords(x, y int32) gruid.Point {
	if dr.scaleX > 0.1 && dr.scaleY > 0.1 {
		x = int32(float32(x) / dr.scaleX)
		y = int32(float32(y) / dr.scaleY)
	}
	return gruid.Point{X: int((x - 1) / dr.tw), Y: int((y - 1) / dr.th)}
}

// PollMsg makes Driver implement gruid.DriverPollMsg. It returns return an
// input message, if any, in a non-blocking way.
func (dr *Driver) PollMsg() (gruid.Msg, error) {
	for {
		select {
		case <-dr.reqredraw:
			w, h := dr.window.GetSize()
			return gruid.MsgScreen{Width: int(w / dr.tw), Height: int(h / dr.th), Time: time.Now()}, nil
		default:
		}
		event := sdl.PollEvent()
		if event == nil {
			return nil, nil
		}
		var msg gruid.Msg
		switch ev := event.(type) {
		case *sdl.QuitEvent:
			msg = gruid.MsgQuit(time.Now())
		case *sdl.TextInputEvent:
			msg = dr.pollTextInputEvent(ev)
		//case *sdl.TextEditingEvent:
		// TODO: Handling this would allow to use an input
		// method for making compositions and chosing text.
		// I'm not sure what the API for this should be in
		// gruid or the driver.
		case *sdl.KeyboardEvent:
			msg = dr.pollKeyboardEvent(ev)
		case *sdl.MouseButtonEvent:
			msg = dr.pollMouseButtonEvent(ev)
		case *sdl.MouseMotionEvent:
			msg = dr.pollMouseMotionEvent(ev)
		case *sdl.MouseWheelEvent:
			msg = dr.pollMouseWheelEvent(ev)
		case *sdl.WindowEvent:
			msg = dr.pollWindowEvent(ev)
		}
		if msg == nil {
			continue
		}
		return msg, nil
	}
}

// PollMsgs implements gruid.Driver.PollMsgs.
func (dr *Driver) PollMsgs(ctx context.Context, msgs chan<- gruid.Msg) error {
	var t *time.Timer
	for {
		msg, err := dr.PollMsg()
		select {
		case <-ctx.Done():
			return nil
		default:
			if err != nil {
				return err
			}
			if msg == nil {
				if t == nil {
					t = time.NewTimer(2 * time.Millisecond)
				} else {
					t.Reset(2 * time.Millisecond)
				}
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-t.C:
					continue
				}
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case msgs <- msg:
		}
	}
}

func (dr *Driver) pollTextInputEvent(ev *sdl.TextInputEvent) gruid.Msg {
	s := ev.GetText()
	if utf8.RuneCountInString(s) != 1 {
		// TODO: handle the case where an input
		// event would produce several
		// characters? We would have to keep
		// track of those characters, and send
		// several messages in a row.
		return nil
	}
	msg := gruid.MsgKeyDown{}
	msg.Key = gruid.Key(s)
	msg.Time = time.Now()
	return msg
}

func (dr *Driver) pollKeyboardEvent(ev *sdl.KeyboardEvent) gruid.Msg {
	c := ev.Keysym.Sym
	if ev.Type == sdl.KEYUP {
		return nil
	}
	msg := gruid.MsgKeyDown{}
	if sdl.KMOD_LALT&ev.Keysym.Mod != 0 {
		msg.Mod |= gruid.ModAlt
	}
	if sdl.KMOD_LSHIFT&ev.Keysym.Mod != 0 || sdl.KMOD_RSHIFT&ev.Keysym.Mod != 0 {
		msg.Mod |= gruid.ModShift
	}
	if sdl.KMOD_LCTRL&ev.Keysym.Mod != 0 || sdl.KMOD_RCTRL&ev.Keysym.Mod != 0 {
		msg.Mod |= gruid.ModCtrl
	}
	if sdl.KMOD_RGUI&ev.Keysym.Mod != 0 {
		msg.Mod |= gruid.ModMeta
	}
	switch c {
	case sdl.K_DOWN:
		msg.Key = gruid.KeyArrowDown
	case sdl.K_LEFT:
		msg.Key = gruid.KeyArrowLeft
	case sdl.K_RIGHT:
		msg.Key = gruid.KeyArrowRight
	case sdl.K_UP:
		msg.Key = gruid.KeyArrowUp
	case sdl.K_BACKSPACE:
		msg.Key = gruid.KeyBackspace
	case sdl.K_DELETE:
		msg.Key = gruid.KeyDelete
	case sdl.K_END:
		msg.Key = gruid.KeyEnd
	case sdl.K_ESCAPE:
		msg.Key = gruid.KeyEscape
	case sdl.K_RETURN:
		msg.Key = gruid.KeyEnter
	case sdl.K_HOME:
		msg.Key = gruid.KeyHome
	case sdl.K_INSERT:
		msg.Key = gruid.KeyInsert
	case sdl.K_PAGEUP:
		msg.Key = gruid.KeyPageUp
	case sdl.K_PAGEDOWN:
		msg.Key = gruid.KeyPageDown
	case sdl.K_TAB:
		msg.Key = gruid.KeyTab
	}
	if ev.Keysym.Mod&sdl.KMOD_NUM == 0 {
		switch c {
		case sdl.K_KP_2:
			msg.Key = gruid.KeyArrowDown
		case sdl.K_KP_4:
			msg.Key = gruid.KeyArrowLeft
		case sdl.K_KP_6:
			msg.Key = gruid.KeyArrowRight
		case sdl.K_KP_8:
			msg.Key = gruid.KeyArrowUp
		case sdl.K_KP_BACKSPACE:
			msg.Key = gruid.KeyBackspace
		case sdl.K_KP_PERIOD:
			msg.Key = gruid.KeyDelete
		case sdl.K_KP_1:
			msg.Key = gruid.KeyEnd
		case sdl.K_KP_5, sdl.K_KP_ENTER:
			msg.Key = gruid.KeyEnter
		case sdl.K_KP_7:
			msg.Key = gruid.KeyHome
		case sdl.K_KP_0:
			msg.Key = gruid.KeyInsert
		case sdl.K_KP_9:
			msg.Key = gruid.KeyPageUp
		case sdl.K_KP_3:
			msg.Key = gruid.KeyPageDown
		}
	}
	if msg.Key == "" {
		return nil
	}
	msg.Time = time.Now()
	return msg
}

func (dr *Driver) pollMouseButtonEvent(ev *sdl.MouseButtonEvent) gruid.Msg {
	var action gruid.MouseAction
	switch ev.Button {
	case sdl.BUTTON_LEFT:
		action = gruid.MouseMain
	case sdl.BUTTON_MIDDLE:
		action = gruid.MouseAuxiliary
	case sdl.BUTTON_RIGHT:
		action = gruid.MouseSecondary
	default:
		return nil
	}
	msg := gruid.MsgMouse{}
	msg.P = dr.coords(ev.X, ev.Y)
	switch ev.Type {
	case sdl.MOUSEBUTTONDOWN:
		if dr.mousedrag != -1 {
			return nil
		}
		if msg.P.X < 0 || msg.P.X >= int(dr.width) ||
			msg.P.Y < 0 || msg.P.Y >= int(dr.height) {
			return nil
		}
		msg.Time = time.Now()
		msg.Action = action
		dr.mousedrag = action
	case sdl.MOUSEBUTTONUP:
		if dr.mousedrag != action {
			return nil
		}
		if msg.P.X < 0 || msg.P.X >= int(dr.width) ||
			msg.P.Y < 0 || msg.P.Y >= int(dr.height) {
			msg.P = gruid.Point{}
		}
		msg.Time = time.Now()
		msg.Action = gruid.MouseRelease
		dr.mousedrag = -1
	}
	mod := sdl.GetModState()
	if sdl.KMOD_LALT&mod != 0 {
		msg.Mod |= gruid.ModAlt
	}
	if sdl.KMOD_LSHIFT&mod != 0 || sdl.KMOD_RSHIFT&mod != 0 {
		msg.Mod |= gruid.ModShift
	}
	if sdl.KMOD_LCTRL&mod != 0 || sdl.KMOD_RCTRL&mod != 0 {
		msg.Mod |= gruid.ModCtrl
	}
	if sdl.KMOD_RGUI&mod != 0 {
		msg.Mod |= gruid.ModMeta
	}
	dr.mousepos = msg.P
	return msg
}

func (dr *Driver) pollMouseMotionEvent(ev *sdl.MouseMotionEvent) gruid.Msg {
	msg := gruid.MsgMouse{}
	msg.P = dr.coords(ev.X, ev.Y)
	if msg.P == dr.mousepos {
		return nil
	}
	if msg.P.X < 0 || msg.P.X >= int(dr.width) ||
		msg.P.Y < 0 || msg.P.Y >= int(dr.height) {
		return nil
	}
	msg.Time = time.Now()
	msg.Action = gruid.MouseMove
	dr.mousepos = msg.P
	mod := sdl.GetModState()
	if sdl.KMOD_LALT&mod != 0 {
		msg.Mod |= gruid.ModAlt
	}
	if sdl.KMOD_LSHIFT&mod != 0 || sdl.KMOD_RSHIFT&mod != 0 {
		msg.Mod |= gruid.ModShift
	}
	if sdl.KMOD_LCTRL&mod != 0 || sdl.KMOD_RCTRL&mod != 0 {
		msg.Mod |= gruid.ModCtrl
	}
	if sdl.KMOD_RGUI&mod != 0 {
		msg.Mod |= gruid.ModMeta
	}
	return msg
}

func (dr *Driver) pollMouseWheelEvent(ev *sdl.MouseWheelEvent) gruid.Msg {
	msg := gruid.MsgMouse{}
	if ev.Y > 0 {
		msg.Action = gruid.MouseWheelUp
	} else if ev.Y < 0 {
		msg.Action = gruid.MouseWheelDown
	} else {
		return nil
	}
	msg.P = dr.mousepos
	msg.Time = time.Now()
	return msg
}

func (dr *Driver) pollWindowEvent(ev *sdl.WindowEvent) gruid.Msg {
	switch ev.Event {
	case sdl.WINDOWEVENT_EXPOSED:
		w, h := dr.window.GetSize()
		return gruid.MsgScreen{Width: int(w / dr.tw), Height: int(h / dr.th), Time: time.Now()}
		//log.Print("exposed")
		//case sdl.WINDOWEVENT_SHOWN:
		//log.Print("shown")
		//case sdl.WINDOWEVENT_HIDDEN:
		//log.Print("hidden")
		//case sdl.WINDOWEVENT_MOVED:
		//log.Print("moved")
		//case sdl.WINDOWEVENT_RESIZED:
		//log.Print("resized")
		//case sdl.WINDOWEVENT_SIZE_CHANGED:
		//log.Print("size changed")
		//case sdl.WINDOWEVENT_MINIMIZED:
		//log.Print("minimized")
		//case sdl.WINDOWEVENT_MAXIMIZED:
		//log.Print("maximized")
		//case sdl.WINDOWEVENT_RESTORED:
		//log.Print("restored")
		//case sdl.WINDOWEVENT_ENTER:
		//log.Print("enter")
		//case sdl.WINDOWEVENT_LEAVE:
		//log.Print("leave")
		//case sdl.WINDOWEVENT_FOCUS_GAINED:
		//log.Print("focus gained")
		//case sdl.WINDOWEVENT_FOCUS_LOST:
		//log.Print("focus lost")
		//case sdl.WINDOWEVENT_CLOSE:
		//log.Print("close")
		//case sdl.WINDOWEVENT_TAKE_FOCUS:
		//log.Print("take focus")
		//case sdl.WINDOWEVENT_HIT_TEST:
		//log.Print("hit test")
		//case sdl.WINDOWEVENT_NONE:
		//log.Print("none")
	}
	return nil
}

// Flush implements gruid.Driver.Flush.
func (dr *Driver) Flush(frame gruid.Frame) {
actions:
	for {
		select {
		case fn := <-dr.actions:
			fn()
		default:
			break actions
		}
	}
	if frame.Width != int(dr.width) || frame.Height != int(dr.height) {
		dr.width = int32(frame.Width)
		dr.height = int32(frame.Height)
		dr.resizeWindow()
	}
	for _, fc := range frame.Cells {
		cs := fc.Cell
		x, y := fc.P.X, fc.P.Y
		dr.draw(cs, x, y)
	}
	dr.renderer.Present()
}

func imageToSurface(img image.Image) (*sdl.Surface, error) {
	buf := bytes.Buffer{}
	err := bmp.Encode(&buf, img)
	if err != nil {
		return nil, err
	}
	src, err := sdl.RWFromMem(buf.Bytes())
	if err != nil {
		return nil, err
	}
	sf, err := sdl.LoadBMPRW(src, true)
	if err != nil {
		return nil, err
	}
	return sf, nil
}

func (dr *Driver) draw(cell gruid.Cell, x, y int) {
	var tx *sdl.Texture
	if t, ok := dr.textures[cell]; ok {
		tx = t
	} else {
		img := dr.tm.GetImage(cell)
		if img == nil {
			log.Printf("no tile for %+v", cell)
			return
		}
		sf, err := imageToSurface(img)
		if err != nil {
			log.Println(err)
			return
		}
		tx, err = dr.renderer.CreateTextureFromSurface(sf)
		if err != nil {
			log.Println(err)
			return
		}
		sf.Free()
		dr.textures[cell] = tx
	}
	rect := sdl.Rect{X: int32(x) * dr.tw, Y: int32(y) * dr.th, W: dr.tw, H: dr.th}
	err := dr.renderer.Copy(tx, nil, &rect)
	if err != nil {
		log.Printf("draw: copy: %v", err)
	}
}

// Close implements gruid.Driver.Close. It releases some resources and calls sdl.Quit.
func (dr *Driver) Close() {
	if !dr.init {
		return
	}
	dr.ClearCache()
	dr.textures = nil
	if !dr.noQuit {
		sdl.StopTextInput()
		err := dr.renderer.Destroy()
		if err != nil {
			log.Printf("renderer destroy: %v", err)
		}
		err = dr.window.Destroy()
		if err != nil {
			log.Printf("window destroy: %v", err)
		}
		sdl.Quit()
		dr.init = false
	}
	dr.noQuit = false
}

// ClearCache clears the tile textures internal cache.
func (dr *Driver) ClearCache() {
	for i, s := range dr.textures {
		err := s.Destroy()
		if err != nil {
			log.Printf("surface destroy: %v", err)
		}
		delete(dr.textures, i)
	}
}
