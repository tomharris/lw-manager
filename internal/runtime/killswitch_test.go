package runtime

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/tomharris/lw-manager/internal/db"
)

type scriptedKillStore struct {
	state db.KillState
	err   error
	calls int
}

func (s *scriptedKillStore) KillState(ctx context.Context, accountID int64) (db.KillState, error) {
	s.calls++
	return s.state, s.err
}

func TestKillSwitchStates(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name  string
		state db.KillState
		want  error
	}{
		{"running", db.KillState{AccountEnabled: true}, nil},
		{"paused", db.KillState{PauseAll: true, Reason: "event", AccountEnabled: true}, ErrPaused},
		{"disabled", db.KillState{AccountEnabled: false}, ErrAccountDisabled},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ks := NewDBKillSwitch(&scriptedKillStore{state: tc.state}, 42)
			err := ks.Check(ctx)
			if !errors.Is(err, tc.want) {
				t.Fatalf("Check: got %v, want %v", err, tc.want)
			}
		})
	}
}

func TestKillSwitchCachesWithinTTL(t *testing.T) {
	st := &scriptedKillStore{state: db.KillState{AccountEnabled: true}}
	ks := NewDBKillSwitch(st, 42)
	ks.ttl = time.Hour // never expires within the test
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		if err := ks.Check(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if st.calls != 1 {
		t.Fatalf("store calls: got %d, want 1 (cached)", st.calls)
	}

	ks.ttl = 0 // every check refetches
	if err := ks.Check(ctx); err != nil {
		t.Fatal(err)
	}
	if st.calls != 2 {
		t.Fatalf("store calls after expiry: got %d, want 2", st.calls)
	}
}

func TestKillSwitchFailsClosed(t *testing.T) {
	st := &scriptedKillStore{err: errors.New("db down")}
	ks := NewDBKillSwitch(st, 42)
	if err := ks.Check(context.Background()); err == nil {
		t.Fatal("Check with failing store: got nil, want error (fail closed)")
	}
}
