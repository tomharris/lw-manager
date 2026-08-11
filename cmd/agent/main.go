// Command agent drives devices: capture, and later, task execution.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"math/rand"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/tomharris/lw-manager/internal/blob"
	"github.com/tomharris/lw-manager/internal/capture"
	"github.com/tomharris/lw-manager/internal/config"
	"github.com/tomharris/lw-manager/internal/db"
	"github.com/tomharris/lw-manager/internal/logging"
	"github.com/tomharris/lw-manager/internal/runtime"
	"github.com/tomharris/lw-manager/internal/scheduler"
	"github.com/tomharris/lw-manager/internal/tasks"
	"github.com/tomharris/lw-manager/internal/transport"
	"github.com/tomharris/lw-manager/internal/vision"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "agent: %v\n", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `usage: agent <command> [flags]

commands:
  register  register a device and account, probing the device over adb
  capture   capture one screenshot for an account
  run-task  run one task for an account, on demand
  run       run the scheduler loop against attached devices
  record    burst-capture frames into the fixture corpus
  corpus    manage the fixture corpus: index, push, pull
  studio    serve the corpus labelling and cropping UI
  score     score the recognizer against the labeled corpus (the M1 gate)
  devices   list attached adb devices
  accounts  list registered accounts
`)
}

func run() error {
	if len(os.Args) < 2 {
		usage()
		return fmt.Errorf("a command is required")
	}

	// Ctrl-C must reach in-flight adb calls, which run under this context.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	logging.Setup(cfg.Log)

	switch os.Args[1] {
	case "register":
		return runRegister(ctx, cfg, os.Args[2:])
	case "capture":
		return runCapture(ctx, cfg, os.Args[2:])
	case "run-task":
		return runTask(ctx, cfg, os.Args[2:])
	case "run":
		return runScheduler(ctx, cfg, os.Args[2:])
	case "record":
		return runRecord(ctx, cfg, os.Args[2:])
	case "corpus":
		return runCorpus(ctx, cfg, os.Args[2:])
	case "studio":
		return runStudio(ctx, cfg, os.Args[2:])
	case "score":
		return runScore(ctx, cfg, os.Args[2:])
	case "devices":
		return runDevices(ctx, cfg)
	case "accounts":
		return runAccounts(ctx, cfg)
	case "-h", "--help", "help":
		usage()
		return nil
	default:
		usage()
		return fmt.Errorf("unknown command %q", os.Args[1])
	}
}

func runRegister(ctx context.Context, cfg config.Config, args []string) error {
	fs := flag.NewFlagSet("register", flag.ExitOnError)
	serial := fs.String("serial", "", "device serial; optional when exactly one device is attached")
	nickname := fs.String("nickname", "", "account nickname (required)")
	role := fs.String("role", "farm", "account role: "+strings.Join(db.ValidRoles, ", "))
	pkg := fs.String("package", transport.DefaultPackage, "game package name")
	clone := fs.Int("clone", 0, "clone slot on the device, for dual-app installs")
	gameUID := fs.String("game-uid", "", "in-game uid, if known")
	server := fs.Int("server", 0, "server number, if known")
	if err := fs.Parse(args); err != nil {
		return err
	}

	if *nickname == "" {
		fs.Usage()
		return fmt.Errorf("--nickname is required")
	}
	if !db.IsValidRole(*role) {
		return fmt.Errorf("invalid --role %q, want one of %s", *role, strings.Join(db.ValidRoles, ", "))
	}

	resolved, err := resolveSerial(ctx, cfg.ADBPath, *serial)
	if err != nil {
		return err
	}

	// Probe over adb rather than accepting a resolution flag. Registration is
	// the one moment we can cheaply prove the device is actually reachable and
	// the serial is right — a wrong serial discovered later shows up as a
	// mystifying capture failure.
	tr, err := transport.NewADBTransport(ctx, transport.ADBOptions{
		ADBPath: cfg.ADBPath,
		Serial:  resolved,
		Package: *pkg,
	})
	if err != nil {
		return fmt.Errorf("probing device %s: %w", resolved, err)
	}
	defer tr.Close()
	res := tr.Resolution()

	pool, err := db.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	dev, err := pool.UpsertDevice(ctx, resolved, "adb", res.X, res.Y)
	if err != nil {
		return err
	}
	instanceID, err := pool.EnsureAppInstance(ctx, dev.ID, *pkg, *clone)
	if err != nil {
		return err
	}
	acct, err := pool.UpsertAccount(ctx, instanceID, *nickname, *role, optString(*gameUID), optInt(*server))
	if err != nil {
		return err
	}

	fmt.Printf("account_id=%d device_id=%d serial=%s resolution=%dx%d package=%s clone=%d role=%s\n",
		acct.ID, dev.ID, dev.Serial, res.X, res.Y, *pkg, *clone, acct.Role)
	fmt.Fprintf(os.Stderr, "\nnext: ./bin/agent capture --account %d\n", acct.ID)
	return nil
}

// resolveSerial defaults to the only attached device when --serial is
// omitted, and refuses to guess when there is more than one.
func resolveSerial(ctx context.Context, adbPath, serial string) (string, error) {
	if serial != "" {
		return serial, nil
	}
	serials, err := transport.ListDevices(ctx, adbPath)
	if err != nil {
		return "", err
	}
	switch len(serials) {
	case 0:
		return "", fmt.Errorf("no devices attached; start an emulator or pass --serial")
	case 1:
		return serials[0], nil
	default:
		return "", fmt.Errorf("%d devices attached (%s); pass --serial to choose one",
			len(serials), strings.Join(serials, ", "))
	}
}

func runAccounts(ctx context.Context, cfg config.Config) error {
	pool, err := db.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	targets, err := pool.ListCaptureTargets(ctx)
	if err != nil {
		return err
	}
	if len(targets) == 0 {
		fmt.Println("no accounts registered; run: agent register --nickname <name>")
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "ACCOUNT\tNICKNAME\tROLE\tENABLED\tSERIAL\tRESOLUTION\tCLONE")
	for _, t := range targets {
		fmt.Fprintf(w, "%d\t%s\t%s\t%t\t%s\t%dx%d\t%d\n",
			t.AccountID, t.Nickname, t.Role, t.Enabled, t.Serial, t.ResolutionW, t.ResolutionH, t.CloneID)
	}
	return w.Flush()
}

func optString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func optInt(i int) *int {
	if i == 0 {
		return nil
	}
	return &i
}

func runCapture(ctx context.Context, cfg config.Config, args []string) error {
	fs := flag.NewFlagSet("capture", flag.ExitOnError)
	accountID := fs.Int64("account", 0, "account id to capture for (required)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *accountID == 0 {
		fs.Usage()
		return fmt.Errorf("--account is required")
	}

	pool, err := db.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	blobs, err := blob.New(ctx, cfg.Blob)
	if err != nil {
		return err
	}

	svc := capture.New(pool, blobs, capture.ADBFactory(cfg.ADBPath))
	res, err := svc.Capture(ctx, *accountID)
	if err != nil {
		return err
	}

	// stdout stays machine-readable; all logging goes to stderr.
	fmt.Printf("screenshot_id=%d key=%s sha256=%s bytes=%d resolution=%dx%d deduplicated=%t\n",
		res.ScreenshotID, res.ObjectKey, res.SHA256, res.Bytes,
		res.Resolution.X, res.Resolution.Y, res.Deduplicated)
	return nil
}

func runDevices(ctx context.Context, cfg config.Config) error {
	serials, err := transport.ListDevices(ctx, cfg.ADBPath)
	if err != nil {
		return err
	}
	if len(serials) == 0 {
		fmt.Println("no devices attached")
		return nil
	}
	for _, s := range serials {
		t, err := transport.NewADBTransport(ctx, transport.ADBOptions{ADBPath: cfg.ADBPath, Serial: s})
		if err != nil {
			fmt.Printf("%s\terror: %v\n", s, err)
			continue
		}
		res := t.Resolution()
		fmt.Printf("%s\t%dx%d\n", s, res.X, res.Y)
		t.Close()
	}
	return nil
}

func runTask(ctx context.Context, cfg config.Config, args []string) error {
	fs := flag.NewFlagSet("run-task", flag.ExitOnError)
	accountID := fs.Int64("account", 0, "account id to run for (required)")
	taskName := fs.String("task", "", "task to run; one of: "+strings.Join(tasks.Names(), ", "))
	manifest := fs.String("templates", "templates/manifest.yaml", "template manifest path")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *accountID == 0 || *taskName == "" {
		fs.Usage()
		return fmt.Errorf("--account and --task are required")
	}
	fn, ok := tasks.Get(*taskName)
	if !ok {
		return fmt.Errorf("unknown task %q, want one of: %s", *taskName, strings.Join(tasks.Names(), ", "))
	}

	// Registry load and graph validation happen before any connection is
	// opened: a manifest missing the graph's screens must fail here, loudly,
	// not as a mid-task mystery.
	reg, err := vision.LoadRegistry(*manifest)
	if err != nil {
		return err
	}
	graph := runtime.DefaultGraph()

	pool, err := db.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	blobs, err := blob.New(ctx, cfg.Blob)
	if err != nil {
		return err
	}

	target, err := pool.CaptureTargetByAccount(ctx, *accountID)
	if err != nil {
		return err
	}
	tr, err := transport.NewADBTransport(ctx, transport.ADBOptions{
		ADBPath: cfg.ADBPath,
		Serial:  target.Serial,
		Package: target.Package,
	})
	if err != nil {
		return fmt.Errorf("opening transport for device %s: %w", target.Serial, err)
	}
	defer tr.Close()

	rt, err := runtime.New(runtime.Options{
		Transport: tr,
		Registry:  reg,
		Graph:     graph,
		Kill:      runtime.NewDBKillSwitch(pool, *accountID),
		Capture:   capture.New(pool, blobs, nil),
		AccountID: *accountID,
		Rand:      rand.New(rand.NewSource(time.Now().UnixNano())),
	})
	if err != nil {
		return err
	}

	out, err := runtime.Run(ctx, pool, rt, *taskName, fn)
	if out.RunID != 0 {
		// stdout stays machine-readable; the error itself goes to stderr via
		// the main error path.
		fmt.Printf("run_id=%d task=%s account=%d status=%s\n", out.RunID, *taskName, *accountID, out.Status)
	}
	return err
}

// runtimeExecutor runs one task on a real device via the task runtime. It is
// the scheduler's production Executor.
type runtimeExecutor struct {
	pool    *db.Pool
	blobs   blob.Store
	reg     *vision.Registry
	graph   *runtime.Graph
	adbPath string
}

func (e *runtimeExecutor) Execute(ctx context.Context, accountID int64, taskName string) error {
	fn, ok := tasks.Get(taskName)
	if !ok {
		return fmt.Errorf("unknown task %q", taskName)
	}
	target, err := e.pool.CaptureTargetByAccount(ctx, accountID)
	if err != nil {
		return err
	}
	tr, err := transport.NewADBTransport(ctx, transport.ADBOptions{
		ADBPath: e.adbPath,
		Serial:  target.Serial,
		Package: target.Package,
	})
	if err != nil {
		return fmt.Errorf("opening transport for device %s: %w", target.Serial, err)
	}
	defer tr.Close()

	rt, err := runtime.New(runtime.Options{
		Transport: tr,
		Registry:  e.reg,
		Graph:     e.graph,
		Kill:      runtime.NewDBKillSwitch(e.pool, accountID),
		Capture:   capture.New(e.pool, e.blobs, nil),
		AccountID: accountID,
		Rand:      rand.New(rand.NewSource(time.Now().UnixNano())),
	})
	if err != nil {
		return err
	}
	_, err = runtime.Run(ctx, e.pool, rt, taskName, fn)
	return err
}

func runScheduler(ctx context.Context, cfg config.Config, args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	manifest := fs.String("templates", "templates/manifest.yaml", "template manifest path")
	tick := fs.Duration("tick", 60*time.Second, "base interval between scheduler ticks")
	if err := fs.Parse(args); err != nil {
		return err
	}

	// Load and validate the registry + graph before any DB work: a manifest
	// missing the graph's screens must fail here, loudly.
	reg, err := vision.LoadRegistry(*manifest)
	if err != nil {
		return err
	}
	graph := runtime.DefaultGraph()

	// Only drive devices attached to this host. An account registered
	// against a device that is not attached here is another agent's job.
	serials, err := transport.ListDevices(ctx, cfg.ADBPath)
	if err != nil {
		return err
	}
	if len(serials) == 0 {
		return fmt.Errorf("no devices attached; start an emulator or connect a handset")
	}

	pool, err := db.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	blobs, err := blob.New(ctx, cfg.Blob)
	if err != nil {
		return err
	}

	// Fail loudly at startup if any enabled task row names a task the
	// registry does not know — the same discipline as graph validation.
	snap, err := pool.SchedulerSnapshot(ctx, serials)
	if err != nil {
		return err
	}
	for _, t := range snap.Tasks {
		if !t.Enabled {
			continue
		}
		if _, ok := tasks.Get(t.Name); !ok {
			return fmt.Errorf("tasks table enables %q but no such task is registered", t.Name)
		}
	}

	exec := &runtimeExecutor{pool: pool, blobs: blobs, reg: reg, graph: graph, adbPath: cfg.ADBPath}
	loop, err := scheduler.New(scheduler.Options{
		Store:    scheduler.NewDBStore(pool),
		Executor: exec,
		Serials:  serials,
		Tick:     *tick,
	})
	if err != nil {
		return err
	}

	slog.Info("scheduler starting", "devices", serials, "tick", tick.String(), "location", loop.Location().String())
	return loop.Run(ctx)
}
