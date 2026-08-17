-- +goose Up

-- roster_capture and vs_capture are the M4 collection routes. Both are
-- alliance_data-only (they walk screens a farm/scout/main account has no
-- reason to visit) and run once a day: the roster and the weekly ranking
-- both change slowly enough that a tighter cadence would only burn device
-- time and increase ban-surface for no fresher a number.
INSERT INTO tasks (name, cadence_seconds, enabled_for_roles, days_of_week) VALUES
    ('roster_capture', 86400, '{alliance_data}', '{0,1,2,3,4,5,6}'),
    ('vs_capture',     86400, '{alliance_data}', '{0,1,2,3,4,5,6}');

-- +goose Down
DELETE FROM tasks WHERE name IN ('roster_capture', 'vs_capture');
