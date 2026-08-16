-- +goose Up

-- vs_capture reaches the weekly ranking through the Alliance Duel screen
-- (base -> VS -> Alliance Duel -> Ranking -> Weekly Rank), and that screen is
-- not there on Sunday: the duel does not run, so there is nothing to navigate
-- to and no ranking to read. Migration 00006 registered the task for all seven
-- days, which schedules it into a screen the game will not show, once a week,
-- indefinitely.
--
-- The route fails safely rather than blindly -- no anchor matches, and
-- invariant #3 makes that a refusal to act rather than a wrong tap -- but it
-- still burns a device slot and books a consecutive failure against the
-- account, which is what the scheduler's backoff reads to decide the task is
-- broken. A weekly guaranteed failure is indistinguishable to that backoff
-- from a route that has genuinely regressed.
--
-- days_of_week holds Go time.Weekday numbers, so Sunday is 0 and this leaves
-- Monday through Saturday. This is the same shape as the radar task, which has
-- carried a VS-day subset ({1,3,5,6}) since migration 00004: that task was
-- gated because radar targets only score on those days, and the vs_ranking
-- route depends on the same event without having been gated with it.
--
-- roster_capture is deliberately untouched. It walks Alliance -> Members,
-- which is present every day, and narrowing it would cost a day of roster
-- history to fix a problem it does not have.
UPDATE tasks SET days_of_week = '{1,2,3,4,5,6}' WHERE name = 'vs_capture';

-- +goose Down
UPDATE tasks SET days_of_week = '{0,1,2,3,4,5,6}' WHERE name = 'vs_capture';
