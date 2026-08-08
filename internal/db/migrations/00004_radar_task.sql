-- +goose Up

-- radar_quick and radar_claim assumed executing and claiming were
-- independent tasks. They are not: only one of the two buttons is on screen
-- at a time, and a pending Claim All blocks the next Quick Execute. Banking
-- claims for VS scoring days would have blocked every execution in between,
-- while radar_quick logged a success roughly eight times a day for finding
-- no button to press.
--
-- One task now claims anything banked, executes once, and claims again,
-- scheduled only on VS scoring days so targets accumulate untouched in
-- between.
DELETE FROM tasks WHERE name IN ('radar_quick', 'radar_claim');

INSERT INTO tasks (name, cadence_seconds, enabled_for_roles, days_of_week) VALUES
    ('radar', 10800, '{main,farm,scout,alliance_data}', '{1,3,5,6}');

-- +goose Down

DELETE FROM tasks WHERE name = 'radar';

INSERT INTO tasks (name, cadence_seconds, enabled_for_roles, days_of_week) VALUES
    ('radar_quick', 10800, '{main,farm,scout,alliance_data}', '{0,1,2,3,4,5,6}'),
    ('radar_claim', 86400, '{main,farm,scout,alliance_data}', '{1,3,5,6}');
