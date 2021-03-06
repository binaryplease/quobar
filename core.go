package quobar

import (
	"errors"
	"fmt"
	"image"
	"image/draw"
	"os"
	"sort"
	"time"

	"github.com/BurntSushi/xgb"
	"github.com/BurntSushi/xgb/randr"
	"github.com/BurntSushi/xgb/xproto"
	"github.com/BurntSushi/xgbutil"
	"github.com/BurntSushi/xgbutil/ewmh"
	"github.com/BurntSushi/xgbutil/xevent"
	"github.com/BurntSushi/xgbutil/xgraphics"
	"github.com/BurntSushi/xgbutil/xwindow"
)

type Image interface {
	draw.Image
	// SubImage provides a sub image of Image without copying image
	// data. The underlying type returned is expected to implement
	// draw.Image.
	SubImage(r image.Rectangle) image.Image
}

func drawAll(img Image, drawers []Drawer) error {
	offset := image.Pt(img.Bounds().Max.X, 0).Div(len(drawers))
	shape := image.Rect(0, 0, offset.X, img.Bounds().Max.Y)
	for idx, drawer := range drawers {
		sub := img.SubImage(shape.Add(offset.Mul(idx)))
		if sub == nil {
			return fmt.Errorf("buggy shape math: shape=%v offset=%v idx=%v", shape, offset, idx)
		}
		dr, ok := sub.(draw.Image)
		if !ok {
			return fmt.Errorf("drawer subimage is not drawable: %v", drawer)
		}
		if err := drawer.Draw(dr); err != nil {
			return fmt.Errorf("drawer failed: %v", err)
		}
	}
	return nil
}

func stopMainloop(xu *xgbutil.XUtil, event interface{}) bool {
	xevent.Quit(xu)
	return true
}

type outputInfo struct {
	screenHeightInPixels      uint16
	screenWidthInPixels       uint16
	screenHeightInMillimeters uint32
}

var errUnplugged = errors.New("display is not connected")

func getOutput(X *xgb.Conn, configTimestamp xproto.Timestamp, output randr.Output) (*outputInfo, error) {
	randrOutput, err := randr.GetOutputInfo(X, output, configTimestamp).Reply()
	if err != nil {
		return nil, fmt.Errorf("cannot get RandR screen resources: %v", err)
	}

	// is there a more direct way to detect unplugged monitors?
	if randrOutput.MmHeight == 0 {
		return nil, errUnplugged
	}

	randrCrtcInfo, err := randr.GetCrtcInfo(X, randrOutput.Crtc, configTimestamp).Reply()
	if err != nil {
		return nil, fmt.Errorf("cannot get RandR monitor info: %v", err)
	}

	info := &outputInfo{
		screenHeightInPixels:      randrCrtcInfo.Height,
		screenWidthInPixels:       randrCrtcInfo.Width,
		screenHeightInMillimeters: randrOutput.MmHeight,
	}
	return info, nil
}

func findOutput(X *xgb.Conn, screen *xproto.ScreenInfo) (*outputInfo, error) {
	randrScreenResources, err := randr.GetScreenResourcesCurrent(X, screen.Root).Reply()
	if err != nil {
		return nil, fmt.Errorf("cannot get RandR screen resources: %v", err)
	}

	outputs := make([]randr.Output, 0, 1+len(randrScreenResources.Outputs))

	randrPrimary, err := randr.GetOutputPrimary(X, screen.Root).Reply()
	if err != nil {
		return nil, fmt.Errorf("cannot get RandR primary: %v", err)
	}
	if randrPrimary.Output != 0 {
		outputs = append(outputs, randrPrimary.Output)
	}

	outputs = append(outputs, randrScreenResources.Outputs...)

	for _, output := range outputs {
		info, err := getOutput(X, randrScreenResources.ConfigTimestamp, output)
		if err == errUnplugged {
			continue
		}
		if err != nil {
			return nil, err
		}
		return info, nil
	}
	return nil, errors.New("cannot find any plugged-in output")
}

// Main runs the main loop for quobar. It is available in library form
// to keep github.com/tv42/quobar/cmd/quobar short and easy to copy
// for editing.
func Main(defaultConfig Config) error {
	Xu, err := xgbutil.NewConn()
	if err != nil {
		return fmt.Errorf("cannot connect to X11: %v", err)
	}
	X := Xu.Conn()
	defer X.Close()

	setup := xproto.Setup(X)
	screen := setup.DefaultScreen(X)

	if err := randr.Init(X); err != nil {
		return fmt.Errorf("initializing RandR: %v", err)
	}

	info, err := findOutput(X, screen)
	if err != nil {
		return fmt.Errorf("choosing monitor to display on: %v", err)
	}

	state := &State{
		Resolution: NewResolution(info.screenHeightInPixels, info.screenHeightInMillimeters),
		Config:     defaultConfig,
	}
	// TODO load config

	// TODO get plugins from config
	//
	// as a placeholder, just include all of them, but make sure it's
	// the same order on every run
	pluginNames := make([]string, 0, len(plugins))
	for k := range plugins {
		pluginNames = append(pluginNames, k)
	}
	sort.Strings(pluginNames)

	// TODO feed config to each plugin
	drawers := make([]Drawer, 0, len(plugins))
	for _, name := range pluginNames {
		p := plugins[name]
		if !p.first {
			continue
		}
		d, err := p.New(state)
		if err != nil {
			return fmt.Errorf("plugin error: %v", err)
		}
		drawers = append(drawers, d)
	}

	// Height of the status bar, in pixels.
	height := state.Resolution.Pixels(state.Config.HeightMillimeters)

	win, err := xwindow.Generate(Xu)
	if err != nil {
		return fmt.Errorf("cannot create X11 window: %v", err)
	}
	win.Create(screen.Root,
		0, int(info.screenHeightInPixels)-height,
		int(info.screenWidthInPixels), height,
		xproto.CwBackPixel, 0xffffff)
	win.Stack(xproto.StackModeBelow)

	// http://standards.freedesktop.org/wm-spec/wm-spec-latest.html

	if err := ewmh.WmWindowTypeSet(Xu, win.Id, []string{"_NET_WM_WINDOW_TYPE_DOCK"}); err != nil {
		return fmt.Errorf("cannot set window to be a dock: %v", err)
	}

	if err := ewmh.WmPidSet(Xu, win.Id, uint(os.Getpid())); err != nil {
		return fmt.Errorf("cannot set pid: %v", err)
	}

	if err := ewmh.WmStateReq(Xu, win.Id, ewmh.StateAdd, "_NET_WM_STATE_BELOW"); err != nil {
		return fmt.Errorf("cannot lower window: %v", err)
	}

	if err := ewmh.WmNameSet(Xu, win.Id, "quobar"); err != nil {
		return fmt.Errorf("cannot set window title: %v", err)
	}
	win.Map()

	if err := ewmh.WmStrutSet(Xu, win.Id, &ewmh.WmStrut{
		Left:   0,
		Right:  0,
		Top:    0,
		Bottom: uint(height),
	}); err != nil {
		return fmt.Errorf("setting struts: %v", err)
	}

	errCh := make(chan error, 1)
	go func() {
		// xgbutil's quit mechanism is only meant to be used from the
		// same goroutine where xevent.Main is running (from the
		// callbacks). We'd really like to say `defer xevent.Quit(Xu)`
		// here, but have to do this weird thing (and wait for the
		// next event) to be goroutine safe.
		//
		// https://github.com/BurntSushi/xgbutil/issues/9
		defer xevent.HookFun(stopMainloop).Connect(Xu)
		defer close(errCh)
		ximg := xgraphics.New(Xu, image.Rect(0, 0, int(info.screenWidthInPixels), height))
		defer ximg.Destroy()
		for {
			draw.Draw(ximg, ximg.Bounds(), image.NewUniform(state.Config.Background), image.ZP, draw.Src)

			if err := drawAll(ximg, drawers); err != nil {
				errCh <- fmt.Errorf("draw error: %v", err)
				return
			}

			if err := ximg.XSurfaceSet(win.Id); err != nil {
				errCh <- fmt.Errorf("XSurfaceSet: %v", err)
				return
			}
			ximg.XDraw()
			ximg.XPaint(win.Id)
			time.Sleep(1 * time.Second)
		}
	}()
	go xevent.Main(Xu)
	return <-errCh
}
