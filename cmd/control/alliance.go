package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"time"

	"github.com/tomharris/lw-manager/internal/config"
	"github.com/tomharris/lw-manager/internal/db"
)

// allianceStore is the surface runAlliance needs. Unlike runIngest's
// ingester interface, production satisfies this directly with *db.Pool —
// both methods already live there, so there is no ingestService-style
// adapter fanning calls out to two different concrete types.
type allianceStore interface {
	UpsertAlliance(ctx context.Context, a db.Alliance) (int64, error)
	CurrentAlliance(ctx context.Context) (db.Alliance, error)
	AllianceByTagName(ctx context.Context, tag, name string) (db.Alliance, error)
}

// runAllianceCmd wires the real database and runs the alliance subcommand.
// It is the only piece of this file that touches Postgres — everything else
// is exercised in cmd/control/alliance_test.go against a fake, with no
// Docker required, matching runIngestCmd's split in cmd/control/ingest.go.
func runAllianceCmd(ctx context.Context, cfg config.Config, args []string) error {
	pool, err := db.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	if code := runAlliance(os.Stdout, os.Stderr, args, pool); code != 0 {
		return fmt.Errorf("control alliance: failed")
	}
	return nil
}

// runAlliance dispatches to the set/show subcommands. Results go to out
// (stdout in production) and everything else — usage errors, failures — to
// errOut (stderr), so a caller can pipe a set/show result without log lines
// mixed in, matching runIngest's split.
func runAlliance(out, errOut io.Writer, args []string, store allianceStore) int {
	if len(args) == 0 {
		fmt.Fprintln(errOut, "usage: control alliance <set|show> [flags]")
		return 1
	}

	switch args[0] {
	case "set":
		return runAllianceSet(out, errOut, args[1:], store)
	case "show":
		return runAllianceShow(out, errOut, args[1:], store)
	default:
		fmt.Fprintf(errOut, "control alliance: unknown subcommand %q\n", args[0])
		return 1
	}
}

// runAllianceSet declares the bot's one tracked alliance identity.
// UpsertAlliance is idempotent on (tag, name), so re-running this with the
// same values refreshes observed_at rather than minting a second row — that
// idempotency is the property that makes it safe to run again after an
// ingest failure, rather than a special case this command has to implement
// itself.
//
// Before upserting, it looks the row up by AllianceByTagName — the exact
// (tag, name) pair UpsertAlliance's own ON CONFLICT target uses — to decide
// two things: whether to report "created" or "refreshed", and, more than
// cosmetics, what member_count to pass through. UpsertAlliance's ON
// CONFLICT overwrites member_count with whatever it is given, and this
// command has no count of its own to offer: identity is declared here, but
// quantities are measured, by ingest's own SetAllianceMemberCount call from
// the alliance frame's "Members: 97/100" line (see this task's brief for
// the full reasoning). Passing zero on a refresh would silently erase that
// measurement every time an operator re-ran `set` to confirm the tag and
// name are still right. So: a match carries the existing count forward;
// anything else (a fresh deployment, or a genuinely different identity)
// starts at zero, because there is nothing yet to carry.
//
// This must look up by (tag, name), not by CurrentAlliance's "most recently
// observed" row: an earlier version compared against CurrentAlliance, which
// only agrees with AllianceByTagName's answer as long as at most one
// alliance has ever been recorded. Switching away and back — `set A`, then
// `set B`, then `set A` again — made CurrentAlliance report B while A's own
// row (with A's already-observed member_count) sat unread, so the second
// `set A` announced action=created and zeroed a count that was never lost
// in the database, only in the lookup.
//
// The read and the write below are not atomic: nothing stops a concurrent
// SetAllianceMemberCount from landing between the AllianceByTagName read
// and the UpsertAlliance write, in which case that write's own count would
// be the one that gets carried forward next time, not this one's. Left
// unguarded deliberately — this is a one-off manual command an operator
// runs interactively, not a path any scheduler or task loop calls, so the
// window is both narrow and low-stakes. Do not copy this pattern into
// scheduled or concurrent code without adding the locking this command
// skips.
func runAllianceSet(out, errOut io.Writer, args []string, store allianceStore) int {
	fs := flag.NewFlagSet("alliance set", flag.ContinueOnError)
	fs.SetOutput(errOut)
	tag := fs.String("tag", "", "alliance tag (required)")
	name := fs.String("name", "", "alliance name (required)")
	server := fs.String("server", "", "server id (optional)")
	fs.Usage = func() {
		fmt.Fprintln(errOut, "usage: control alliance set --tag <tag> --name <name> [--server <id>]")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 1
	}
	if *tag == "" {
		fmt.Fprintln(errOut, "control alliance set: --tag is required")
		fs.Usage()
		return 1
	}
	if *name == "" {
		fmt.Fprintln(errOut, "control alliance set: --name is required")
		fs.Usage()
		return 1
	}

	logger := slog.New(slog.NewTextHandler(errOut, nil))
	ctx := context.Background()

	existing, err := store.AllianceByTagName(ctx, *tag, *name)
	if err != nil && !errors.Is(err, db.ErrNotFound) {
		logger.ErrorContext(ctx, "control alliance set: checking for an existing alliance failed", "tag", *tag, "name", *name, "error", err)
		return 1
	}

	// action reflects what AllianceByTagName actually found, not an
	// inference from some other query — see the doc comment above for why
	// that distinction is the whole fix.
	action := "created"
	memberCount := 0
	if err == nil {
		action = "refreshed"
		memberCount = existing.MemberCount
	}

	id, err := store.UpsertAlliance(ctx, db.Alliance{Tag: *tag, Name: *name, Server: *server, MemberCount: memberCount})
	if err != nil {
		logger.ErrorContext(ctx, "control alliance set: upsert failed", "tag", *tag, "name", *name, "error", err)
		return 1
	}

	fmt.Fprintf(out, "id=%d tag=%s name=%q server=%q action=%s\n", id, *tag, *name, *server, action)
	return 0
}

// runAllianceShow prints the alliance control ingest resolves via
// CurrentAllianceID, so an operator who just hit "resolving current
// alliance: ... not found" has somewhere to look and something to run.
func runAllianceShow(out, errOut io.Writer, args []string, store allianceStore) int {
	fs := flag.NewFlagSet("alliance show", flag.ContinueOnError)
	fs.SetOutput(errOut)
	fs.Usage = func() {
		fmt.Fprintln(errOut, "usage: control alliance show")
	}
	if err := fs.Parse(args); err != nil {
		return 1
	}

	logger := slog.New(slog.NewTextHandler(errOut, nil))
	ctx := context.Background()

	a, err := store.CurrentAlliance(ctx)
	if errors.Is(err, db.ErrNotFound) {
		fmt.Fprintln(errOut, "control alliance show: no alliance recorded yet — run `control alliance set --tag <tag> --name <name>` first")
		return 1
	}
	if err != nil {
		logger.ErrorContext(ctx, "control alliance show: failed", "error", err)
		return 1
	}

	fmt.Fprintf(out, "id=%d tag=%s name=%q server=%q member_count=%d observed_at=%s\n",
		a.ID, a.Tag, a.Name, a.Server, a.MemberCount, a.ObservedAt.Format(time.RFC3339))
	return 0
}
