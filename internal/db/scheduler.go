package db

import (
	"context"
	"fmt"
	"time"
)

// ScheduledAccount is one account the scheduler may drive.
type ScheduledAccount struct {
	ID      int64
	Role    string
	Enabled bool
}

// ScheduledTask is one row of the tasks config table.
type ScheduledTask struct {
	Name           string
	CadenceSeconds int
	Roles          []string
	DaysOfWeek     []int16 // Go time.Weekday numbers
	Enabled        bool
}

// TaskRunState summarises task_runs for one (account, task) pair.
type TaskRunState struct {
	AccountID           int64
	TaskName            string
	LastRun             *time.Time
	ConsecutiveFailures int
}

// SchedulerSnapshot is everything the scheduler needs for one planning tick.
type SchedulerSnapshot struct {
	Paused   bool
	Accounts []ScheduledAccount
	Tasks    []ScheduledTask
	Runs     []TaskRunState
}

// SchedulerSnapshot reads the pause flag, the accounts on the given device
// serials, the task config, and each (account, task) pair's run state.
// Scoping by serials is how one agent drives only the devices attached to
// its own host: an account on a device not attached here is not its business.
func (p *Pool) SchedulerSnapshot(ctx context.Context, serials []string) (SchedulerSnapshot, error) {
	var snap SchedulerSnapshot

	if err := p.QueryRow(ctx, `SELECT pause_all FROM flags`).Scan(&snap.Paused); err != nil {
		return SchedulerSnapshot{}, fmt.Errorf("db: reading pause flag: %w", err)
	}

	rows, err := p.Query(ctx, `
		SELECT a.id, a.role, a.enabled
		FROM accounts a
		JOIN app_instances ai ON ai.id = a.app_instance_id
		JOIN devices d        ON d.id  = ai.device_id
		WHERE d.serial = ANY($1)`, serials)
	if err != nil {
		return SchedulerSnapshot{}, fmt.Errorf("db: reading scheduled accounts: %w", err)
	}
	for rows.Next() {
		var a ScheduledAccount
		if err := rows.Scan(&a.ID, &a.Role, &a.Enabled); err != nil {
			rows.Close()
			return SchedulerSnapshot{}, fmt.Errorf("db: scanning scheduled account: %w", err)
		}
		snap.Accounts = append(snap.Accounts, a)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return SchedulerSnapshot{}, fmt.Errorf("db: iterating scheduled accounts: %w", err)
	}

	trows, err := p.Query(ctx, `
		SELECT name, cadence_seconds, enabled_for_roles, days_of_week, enabled
		FROM tasks`)
	if err != nil {
		return SchedulerSnapshot{}, fmt.Errorf("db: reading tasks: %w", err)
	}
	for trows.Next() {
		var t ScheduledTask
		if err := trows.Scan(&t.Name, &t.CadenceSeconds, &t.Roles, &t.DaysOfWeek, &t.Enabled); err != nil {
			trows.Close()
			return SchedulerSnapshot{}, fmt.Errorf("db: scanning task: %w", err)
		}
		snap.Tasks = append(snap.Tasks, t)
	}
	trows.Close()
	if err := trows.Err(); err != nil {
		return SchedulerSnapshot{}, fmt.Errorf("db: iterating tasks: %w", err)
	}

	// Per (account, task): last_run is the most recent attempt that actually
	// ran (succeeded or failed — paused/running did no work and must not
	// push the cadence out). consec_failures counts failed runs since the
	// last success, so a pause neither counts nor resets the streak.
	rrows, err := p.Query(ctx, `
		WITH scoped AS (
			SELECT a.id AS account_id
			FROM accounts a
			JOIN app_instances ai ON ai.id = a.app_instance_id
			JOIN devices d        ON d.id  = ai.device_id
			WHERE d.serial = ANY($1)
		),
		agg AS (
			SELECT tr.account_id, tr.task_name,
			       max(tr.started_at) FILTER (WHERE tr.status IN ('succeeded','failed')) AS last_run,
			       max(tr.started_at) FILTER (WHERE tr.status = 'succeeded')             AS last_success
			FROM task_runs tr
			JOIN scoped s ON s.account_id = tr.account_id
			GROUP BY tr.account_id, tr.task_name
		)
		SELECT agg.account_id, agg.task_name, agg.last_run,
		       (SELECT count(*) FROM task_runs f
		        WHERE f.account_id = agg.account_id
		          AND f.task_name  = agg.task_name
		          AND f.status = 'failed'
		          AND f.started_at > COALESCE(agg.last_success, '-infinity')) AS consec_failures
		FROM agg`, serials)
	if err != nil {
		return SchedulerSnapshot{}, fmt.Errorf("db: reading run state: %w", err)
	}
	for rrows.Next() {
		var rs TaskRunState
		if err := rrows.Scan(&rs.AccountID, &rs.TaskName, &rs.LastRun, &rs.ConsecutiveFailures); err != nil {
			rrows.Close()
			return SchedulerSnapshot{}, fmt.Errorf("db: scanning run state: %w", err)
		}
		snap.Runs = append(snap.Runs, rs)
	}
	rrows.Close()
	if err := rrows.Err(); err != nil {
		return SchedulerSnapshot{}, fmt.Errorf("db: iterating run state: %w", err)
	}

	return snap, nil
}
