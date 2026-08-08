package tasks

import (
	"context"
	"errors"
	"image"
	"testing"

	"github.com/tomharris/lw-manager/internal/runtime"
	"github.com/tomharris/lw-manager/internal/runtime/runtimetest"
	"github.com/tomharris/lw-manager/internal/vision"
)

// mailboxScript stages one mailbox visit from wherever the previous one left
// off: the hop up to the mail index, the hop down into the box, and then the
// frames the box itself is looked at on — the NavigateTo landing check, the
// claim tap, the celebration dismissal, and the CurrentScreen the *next*
// visit opens with.
//
// Writing it out per mailbox rather than as a flat list of names is what makes
// the count auditable. Each Ctx primitive consumes exactly one frame, so a
// script that is one frame short does not fail where it is wrong: it fails
// later, as ErrWrongScreen, on whichever call happens to inherit the shift.
func mailboxScript(box string) []image.Image {
	mail := runtimetest.Frame(vision.ScreenMail)
	return append(
		[]image.Image{mail, mail}, // WaitFor(mail) after Back, then the tap verify
		rep(runtimetest.Frame(box), 5)...,
	)
}

// Three mailboxes, each entered, claimed, and left.
func TestMailCollectVisitsAllThreeMailboxes(t *testing.T) {
	base := runtimetest.Frame(vision.ScreenBase)
	frames := []image.Image{base, base} // CurrentScreen, then the base->mail tap verify
	frames = append(frames, mailboxScript(vision.ScreenMailAlliance)...)
	frames = append(frames, mailboxScript(vision.ScreenMailEvent)...)
	frames = append(frames, mailboxScript(vision.ScreenMailSystem)...)

	rt, tr := ctxFor(t, frames...)
	fn, ok := Get("mail_collect")
	if !ok {
		t.Fatal("mail_collect not registered")
	}
	if err := fn(context.Background(), rt); err != nil {
		t.Fatalf("mail_collect: %v", err)
	}
	// Three claim taps and three celebration dismissals, plus the four taps
	// that walk base -> mail and down into each box.
	if taps := countTaps(tr); taps != 10 {
		t.Errorf("got %d taps, want 10 (4 navigation, 3 claims, 3 dismissals)", taps)
	}
}

// Claim All is always enabled, so it never legitimately goes missing.
// Treating its absence as "nothing to claim" would make a broken anchor
// indistinguishable from an empty mailbox.
func TestMailCollectFailsWhenClaimAllIsMissing(t *testing.T) {
	f := runtimetest.FrameWithout(vision.ScreenMailAlliance, "claim_all_button")
	rt, _ := ctxFor(t, rep(f, 6)...)
	fn, _ := Get("mail_collect")
	err := fn(context.Background(), rt)
	if !errors.Is(err, runtime.ErrAnchorNotFound) {
		t.Fatalf("got %v, want ErrAnchorNotFound — an always-enabled control going missing is a fault", err)
	}
}

// A mailbox with nothing in it raises no celebration, so there is no banner
// to tap. The claim still happens: Claim All no-ops rather than vanishing.
func TestMailCollectClaimsWithoutDismissingWhenNothingWasWon(t *testing.T) {
	base := runtimetest.Frame(vision.ScreenBase)
	mail := runtimetest.Frame(vision.ScreenMail)
	quiet := func(box string) []image.Image {
		return append([]image.Image{mail, mail},
			rep(runtimetest.FrameWithout(box, "rewards_banner"), 5)...)
	}
	frames := []image.Image{base, base}
	frames = append(frames, quiet(vision.ScreenMailAlliance)...)
	frames = append(frames, quiet(vision.ScreenMailEvent)...)
	frames = append(frames, quiet(vision.ScreenMailSystem)...)

	rt, tr := ctxFor(t, frames...)
	fn, _ := Get("mail_collect")
	if err := fn(context.Background(), rt); err != nil {
		t.Fatalf("an empty mailbox must be success: %v", err)
	}
	if taps := countTaps(tr); taps != 7 {
		t.Errorf("got %d taps, want 7 (4 navigation, 3 claims, no dismissals)", taps)
	}
}
