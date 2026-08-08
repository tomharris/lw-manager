package tasks

import (
	"context"

	"github.com/tomharris/lw-manager/internal/runtime"
	"github.com/tomharris/lw-manager/internal/vision"
)

// mailboxes are the three mail screens a task acts from. Mail is a tree,
// not a tab bar: the index at `mail` drills into one screen per mailbox,
// each carrying its own claim-all button.
var mailboxes = []string{
	vision.ScreenMailAlliance,
	vision.ScreenMailEvent,
	vision.ScreenMailSystem,
}

func init() { Register("mail_collect", mailCollect) }

// mail_collect claims each mailbox in turn.
//
// Claim All here is unlike every other control in this milestone: it is
// always rendered and always enabled, and simply no-ops when there is
// nothing to claim. So tapIfPresent is deliberately not used — a missing
// Claim All is a broken anchor or a changed UI, not an empty mailbox, and
// treating the two the same would hide the first behind the second.
//
// What is optional is the celebration, which only plays when there actually
// were rewards. dismissRewards handles both cases without branching, because
// the banner it taps exists only when there is something to dismiss.
func mailCollect(ctx context.Context, rt *runtime.Ctx) error {
	for _, box := range mailboxes {
		if err := rt.NavigateTo(ctx, box); err != nil {
			return err
		}
		if err := rt.Tap(ctx, box, "claim_all_button"); err != nil {
			return err
		}
		if err := dismissRewards(ctx, rt, box); err != nil {
			return err
		}
	}
	return nil
}
