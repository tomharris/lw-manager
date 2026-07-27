package transport

import (
	"bytes"
	"context"
	"fmt"
	"image"
	_ "image/png" // registers the PNG decoder for screencap output
	"log/slog"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// DefaultPackage is the Last War package name.
const DefaultPackage = "com.fun.lastwar.gp"

// ADBTransport drives an Android device over adb. Every operation is
// per-device (`adb -s <serial>`), headless, and parallel-safe, so N devices
// run N accounts with no contention — the property that made adb the right
// choice over driving the host's own cursor.
type ADBTransport struct {
	adbPath string
	serial  string
	pkg     string
	size    image.Point
	log     *slog.Logger
}

// ADBOptions configures an ADBTransport.
type ADBOptions struct {
	ADBPath string // defaults to "adb"
	Serial  string // required
	Package string // defaults to DefaultPackage
}

// NewADBTransport connects to a device and probes its resolution once.
func NewADBTransport(ctx context.Context, opts ADBOptions) (*ADBTransport, error) {
	if opts.Serial == "" {
		return nil, fmt.Errorf("transport: adb serial is required")
	}
	t := &ADBTransport{
		adbPath: orDefault(opts.ADBPath, "adb"),
		serial:  opts.Serial,
		pkg:     orDefault(opts.Package, DefaultPackage),
		log:     slog.Default().With("transport", "adb", "serial", opts.Serial),
	}

	size, err := t.probeResolution(ctx)
	if err != nil {
		return nil, err
	}
	t.size = size
	t.log.Info("device connected", "width", size.X, "height", size.Y)
	return t, nil
}

var physicalSizeRe = regexp.MustCompile(`(\d+)x(\d+)`)

// probeResolution reads the screen size from `wm size`. Output looks like
// "Physical size: 1080x2400", sometimes followed by an "Override size:" line
// when the device is running at a non-native resolution — the override wins,
// so the last match is the one that matters.
func (t *ADBTransport) probeResolution(ctx context.Context) (image.Point, error) {
	out, err := t.run(ctx, "shell", "wm", "size")
	if err != nil {
		return image.Point{}, fmt.Errorf("transport: probing resolution: %w", err)
	}

	matches := physicalSizeRe.FindAllStringSubmatch(string(out), -1)
	if len(matches) == 0 {
		return image.Point{}, fmt.Errorf("transport: could not parse resolution from %q", out)
	}
	last := matches[len(matches)-1]

	w, err := strconv.Atoi(last[1])
	if err != nil {
		return image.Point{}, fmt.Errorf("transport: parsing width %q: %w", last[1], err)
	}
	h, err := strconv.Atoi(last[2])
	if err != nil {
		return image.Point{}, fmt.Errorf("transport: parsing height %q: %w", last[2], err)
	}
	return image.Point{X: w, Y: h}, nil
}

func (t *ADBTransport) Screenshot(ctx context.Context) (image.Image, error) {
	// exec-out, not shell: `adb shell` applies CRLF translation that corrupts
	// binary PNG output on many devices. This is the single most common cause
	// of "image: unknown format" from screencap.
	out, err := t.run(ctx, "exec-out", "screencap", "-p")
	if err != nil {
		return nil, fmt.Errorf("transport: capturing screen: %w", err)
	}

	img, _, err := image.Decode(bytes.NewReader(out))
	if err != nil {
		return nil, fmt.Errorf("transport: decoding screencap output (%d bytes): %w", len(out), err)
	}
	return img, nil
}

func (t *ADBTransport) Tap(ctx context.Context, p Norm) error {
	if err := checkPoints(p); err != nil {
		return err
	}
	x, y := p.Pixels(t.size)
	if _, err := t.run(ctx, "shell", "input", "tap", strconv.Itoa(x), strconv.Itoa(y)); err != nil {
		return fmt.Errorf("transport: tapping (%d,%d): %w", x, y, err)
	}
	return nil
}

func (t *ADBTransport) Swipe(ctx context.Context, from, to Norm, d time.Duration) error {
	if err := checkPoints(from, to); err != nil {
		return err
	}
	x1, y1 := from.Pixels(t.size)
	x2, y2 := to.Pixels(t.size)
	ms := int(d.Milliseconds())
	if ms <= 0 {
		ms = 300
	}
	_, err := t.run(ctx, "shell", "input", "swipe",
		strconv.Itoa(x1), strconv.Itoa(y1), strconv.Itoa(x2), strconv.Itoa(y2), strconv.Itoa(ms))
	if err != nil {
		return fmt.Errorf("transport: swiping (%d,%d)->(%d,%d): %w", x1, y1, x2, y2, err)
	}
	return nil
}

func (t *ADBTransport) TypeText(ctx context.Context, s string) error {
	if _, err := t.run(ctx, "shell", "input", "text", escapeInputText(s)); err != nil {
		return fmt.Errorf("transport: typing text: %w", err)
	}
	return nil
}

func (t *ADBTransport) AppRestart(ctx context.Context) error {
	if _, err := t.run(ctx, "shell", "am", "force-stop", t.pkg); err != nil {
		return fmt.Errorf("transport: force-stopping %s: %w", t.pkg, err)
	}
	if _, err := t.run(ctx, "shell", "monkey", "-p", t.pkg, "-c", "android.intent.category.LAUNCHER", "1"); err != nil {
		return fmt.Errorf("transport: launching %s: %w", t.pkg, err)
	}
	t.log.Warn("app restarted", "package", t.pkg)
	return nil
}

func (t *ADBTransport) Resolution() image.Point { return t.size }

func (t *ADBTransport) Close() error { return nil }

// Back presses the hardware back button. The panic route's first move.
func (t *ADBTransport) Back(ctx context.Context) error {
	_, err := t.run(ctx, "shell", "input", "keyevent", "KEYCODE_BACK")
	if err != nil {
		return fmt.Errorf("transport: pressing back: %w", err)
	}
	return nil
}

// run executes an adb subcommand against this device and returns stdout.
func (t *ADBTransport) run(ctx context.Context, args ...string) ([]byte, error) {
	full := append([]string{"-s", t.serial}, args...)
	cmd := exec.CommandContext(ctx, t.adbPath, full...)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("adb %s: %w (stderr: %s)",
			strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

// escapeInputText makes a string safe for `adb shell input text`, which runs
// through a shell and treats spaces as argument separators.
func escapeInputText(s string) string {
	r := strings.NewReplacer(
		`\`, `\\`,
		` `, `%s`,
		`'`, `\'`,
		`"`, `\"`,
		"`", "\\`",
		`$`, `\$`,
		`&`, `\&`,
		`;`, `\;`,
		`|`, `\|`,
		`<`, `\<`,
		`>`, `\>`,
		`(`, `\(`,
		`)`, `\)`,
		`*`, `\*`,
		`~`, `\~`,
	)
	return r.Replace(s)
}

// ListDevices returns the serials of all attached devices in the "device"
// state, skipping ones that are unauthorized or still booting.
func ListDevices(ctx context.Context, adbPath string) ([]string, error) {
	cmd := exec.CommandContext(ctx, orDefault(adbPath, "adb"), "devices")
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("transport: listing devices: %w", err)
	}

	var serials []string
	for _, line := range strings.Split(string(out), "\n")[1:] {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[1] == "device" {
			serials = append(serials, fields[0])
		}
	}
	return serials, nil
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// ParseVersionName pulls versionName out of `dumpsys package` output. It
// returns the first occurrence: a package with several split APKs repeats the
// field, and the first is the base.
func ParseVersionName(dumpsys string) string {
	const key = "versionName="
	for _, line := range strings.Split(dumpsys, "\n") {
		i := strings.Index(line, key)
		if i < 0 {
			continue
		}
		return strings.TrimSpace(line[i+len(key):])
	}
	return ""
}

// DeviceProps reads the device model and the installed game's version.
//
// This is a package function rather than a Transport method on purpose. The
// interface is deliberately narrow — pixels in, touches out — and adding a
// general shell escape hatch to it would hand every caller a way around
// invariant #1.
//
// A failed model read is a hard error: getprop failing on a device that is
// already attached and probed is genuinely anomalous, not a condition worth
// papering over. The game version is different — it is not essential to
// capturing, and the game may not be installed on a device used only for
// capture — so a failed dumpsys yields an empty gameVersion rather than an
// error: unknown is worth recording as unknown, and is not a reason to
// abandon a capture session.
func DeviceProps(ctx context.Context, adbPath, serial, pkg string) (model, gameVersion string, err error) {
	args := func(rest ...string) []string {
		return append([]string{"-s", serial}, rest...)
	}

	out, err := exec.CommandContext(ctx, adbPath, args("shell", "getprop", "ro.product.model")...).Output()
	if err != nil {
		return "", "", fmt.Errorf("transport: reading model from %s: %w", serial, err)
	}
	model = strings.TrimSpace(string(out))

	dump, err := exec.CommandContext(ctx, adbPath, args("shell", "dumpsys", "package", pkg)...).Output()
	if err != nil {
		// The game may not be installed on a device used only for capture.
		return model, "", nil
	}
	return model, ParseVersionName(string(dump)), nil
}
