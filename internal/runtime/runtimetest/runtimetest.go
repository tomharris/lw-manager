// Package runtimetest provides synthetic screens, a matching graph, and
// fakes for testing the task runtime with no device attached.
//
// The synthetic frame is a grid of cells, one per (screen, anchor) pair, all
// carrying the same 8x8 pattern. Recognition is therefore exact and
// deterministic without needing patterns that are provably NCC-distinct from
// each other: an anchor matches when — and only when — its own cell was
// drawn into.
package runtimetest

import (
	"context"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"math/rand"
	"sort"
	"sync"
	"time"

	"github.com/tomharris/lw-manager/internal/runtime"
	"github.com/tomharris/lw-manager/internal/transport"
	"github.com/tomharris/lw-manager/internal/vision"
)

// The grid geometry. One cell per (screen, anchor) pair means a frame can
// render some of a screen's anchors and omit others, which is what "the
// button is not on screen" means — and every task's nothing-to-do path
// branches on it.
//
// Previously all of a screen's anchors shared one region, which made anchor
// presence inseparable from screen presence. The only way to simulate a
// missing button was to move its search region onto flat pixels, which tests
// the matcher rather than the task.
const (
	frameSize   = 128 // synthetic frames are frameSize x frameSize
	cellSize    = 16  // one anchor's slot; regions are disjoint by construction
	patternSize = 8   // the pattern, centred in its cell
	cellsPerRow = frameSize / cellSize

	// idAnchor is the identifying anchor every synthetic screen carries. It
	// is the only anchor with IdentifiesScreen set, so omitting an action
	// anchor never costs the screen its identity — which is precisely the
	// real behaviour a celebration frame or a greyed-out button has.
	idAnchor = "id"

	// flatGray is the background every frame is painted with. Any cell not
	// drawn into stays flat, and NCC against a flat patch scores 0.
	flatGray = 100
)

// layout names every screen's anchors in a fixed order. Cell assignment is
// derived from this, so it is deterministic across runs without any cell
// numbers being written down.
//
// It must cover every screen and anchor runtime.DefaultGraph() names, since
// Graph below returns that graph and Ctx validates it against Registry.
var layout = map[string][]string{
	vision.ScreenBase:               {idAnchor, "help_all_button", "collect_bubble", "alliance_button", "mail_button", "radar_button", "world_map_button", "vs_button"},
	vision.ScreenStaminaPrompt:      {idAnchor},
	vision.ScreenWorldMap:           {idAnchor, "base_button"},
	vision.ScreenAlliance:           {idAnchor, "tech_button", "members_button"},
	vision.ScreenAllianceTech:       {idAnchor, "tech_recommended_badge", "tab_recommended_badge"},
	vision.ScreenAllianceTechDonate: {idAnchor, "donate_button"},
	vision.ScreenAllianceMembers:    {idAnchor},
	vision.ScreenAllianceDuel:       {idAnchor, "ranking_button"},
	vision.ScreenVSRanking:          {idAnchor, "weekly_tab"},
	vision.ScreenVSRankingWeekly:    {idAnchor},
	vision.ScreenMail:               {idAnchor, "mail_alliance_button", "mail_event_button", "mail_system_button"},
	vision.ScreenMailAlliance:       {idAnchor, "claim_all_button", "rewards_banner"},
	vision.ScreenMailEvent:          {idAnchor, "claim_all_button", "rewards_banner"},
	vision.ScreenMailSystem:         {idAnchor, "claim_all_button", "rewards_banner"},
	vision.ScreenRadar:              {idAnchor, "quick_execute_button", "claim_all_button", "rewards_banner"},
}

// screenOrder is layout's keys, sorted, so cell indices never depend on map
// iteration order.
func screenOrder() []string {
	names := make([]string, 0, len(layout))
	for n := range layout {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// cellIndex returns the grid cell owned by one (screen, anchor) pair.
func cellIndex(screen, anchorID string) int {
	i := 0
	for _, s := range screenOrder() {
		for _, a := range layout[s] {
			if s == screen && a == anchorID {
				return i
			}
			i++
		}
	}
	panic(fmt.Sprintf("runtimetest: no cell for %s/%s", screen, anchorID))
}

// cellOrigin is a cell's top-left pixel.
func cellOrigin(i int) (int, int) {
	if i >= cellsPerRow*cellsPerRow {
		panic(fmt.Sprintf("runtimetest: cell %d does not fit a %dx%d grid; raise frameSize", i, cellsPerRow, cellsPerRow))
	}
	return (i % cellsPerRow) * cellSize, (i / cellsPerRow) * cellSize
}

// cellRect is a cell's normalized search region. Cells never overlap, so no
// anchor can match another's pattern.
func cellRect(i int) transport.Rect {
	x, y := cellOrigin(i)
	return transport.Rect{
		X1: float64(x) / frameSize,
		Y1: float64(y) / frameSize,
		X2: float64(x+cellSize) / frameSize,
		Y2: float64(y+cellSize) / frameSize,
	}
}

func pattern() *image.Gray {
	g := image.NewGray(image.Rect(0, 0, patternSize, patternSize))
	for y := 0; y < patternSize; y++ {
		for x := 0; x < patternSize; x++ {
			g.SetGray(x, y, color.Gray{Y: uint8((x*37 + y*53) % 256)})
		}
	}
	return g
}

// Registry builds the synthetic registry at ReferenceHeight frameSize. Each
// screen carries an identifying anchor plus every action anchor its task
// needs, each in its own cell.
func Registry() *vision.Registry {
	p := pattern()
	reg := &vision.Registry{ReferenceHeight: frameSize}
	for _, name := range screenOrder() {
		var anchors []vision.Anchor
		for _, id := range layout[name] {
			anchors = append(anchors, vision.Anchor{
				ID:               id,
				Template:         p,
				Region:           cellRect(cellIndex(name, id)),
				Threshold:        0.9,
				IdentifiesScreen: id == idAnchor,
			})
		}
		reg.Screens = append(reg.Screens, vision.Screen{Name: name, Anchors: anchors})
	}
	return reg
}

// Graph is the production topology. It is returned rather than re-declared
// because layout now provides every screen and anchor DefaultGraph names —
// and a second copy of the edge list is a second thing to forget to update,
// which is the drift screens.go was written to prevent, one layer down.
func Graph() *runtime.Graph { return runtime.DefaultGraph() }

// Frame renders every anchor of one screen.
func Frame(screen string) image.Image { return FrameWithout(screen) }

// FrameWithout renders a screen with the named anchors absent. A button that
// is not rendered is a button the matcher cannot find, which is what every
// "nothing to do" branch means.
func FrameWithout(screen string, omit ...string) image.Image {
	skip := make(map[string]bool, len(omit))
	for _, o := range omit {
		skip[o] = true
	}
	img := Blank()
	drawScreen(img, screen, skip)
	return img
}

// FrameCelebrating renders a screen with its rewards_banner present, which is
// what a real celebration frame looks like: the origin screen, fully
// recognizable, with an animation drawn on it. There is no second screen
// involved — the celebration is not a screen.
func FrameCelebrating(screen string) image.Image { return Frame(screen) }

func drawScreen(img *image.Gray, screen string, skip map[string]bool) {
	anchors, ok := layout[screen]
	if !ok {
		panic(fmt.Sprintf("runtimetest: unknown screen %q", screen))
	}
	p := pattern()
	for _, id := range anchors {
		if skip[id] {
			continue
		}
		x, y := cellOrigin(cellIndex(screen, id))
		off := (cellSize - patternSize) / 2
		draw.Draw(img,
			image.Rect(x+off, y+off, x+off+patternSize, y+off+patternSize),
			p, p.Bounds().Min, draw.Src)
	}
}

// Blank is a frame no screen recognizes. It returns the concrete type so the
// frame constructors can draw into it without a type assertion.
func Blank() *image.Gray {
	out := image.NewGray(image.Rect(0, 0, frameSize, frameSize))
	for y := 0; y < frameSize; y++ {
		for x := 0; x < frameSize; x++ {
			out.SetGray(x, y, color.Gray{Y: flatGray})
		}
	}
	return out
}

// Frames is a convenience for staging a replay script by screen name;
// "" stages a Blank frame.
func Frames(names ...string) []image.Image {
	out := make([]image.Image, len(names))
	for i, n := range names {
		if n == "" {
			out[i] = Blank()
		} else {
			out[i] = Frame(n)
		}
	}
	return out
}

// AnchorRegion is the search region one anchor owns, so a test can assert a
// tap landed on the anchor it named without hard-coding grid arithmetic that
// shifts whenever layout gains an entry.
func AnchorRegion(screen, anchorID string) transport.Rect {
	return cellRect(cellIndex(screen, anchorID))
}

// FakeKill is a settable kill switch for tests.
type FakeKill struct {
	mu  sync.Mutex
	err error
}

// Set makes every subsequent Check return err (nil re-enables).
func (k *FakeKill) Set(err error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.err = err
}

func (k *FakeKill) Check(ctx context.Context) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.err
}

// Options returns runtime.Options wired for fast deterministic tests:
// synthetic registry and graph, seeded rand, millisecond timing.
func Options(tr transport.Transport, ks runtime.KillSwitch) runtime.Options {
	return runtime.Options{
		Transport:      tr,
		Registry:       Registry(),
		Graph:          Graph(),
		Kill:           ks,
		AccountID:      1,
		Rand:           rand.New(rand.NewSource(42)),
		PollInterval:   time.Millisecond,
		WaitTimeout:    50 * time.Millisecond,
		RestartTimeout: 50 * time.Millisecond,
	}
}
