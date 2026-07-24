-- +goose Up

-- Scheduler config: one row per schedulable task. name is the primary key
-- because it is already the registry key in internal/tasks; a row naming a
-- task the registry does not know is a config error the agent fails on at
-- startup. days_of_week holds Go time.Weekday numbers (0=Sun … 6=Sat); the
-- default is every day, a subset gates the task to those weekdays.
CREATE TABLE tasks (
    name              text        PRIMARY KEY,
    cadence_seconds   integer     NOT NULL CHECK (cadence_seconds > 0),
    enabled_for_roles text[]      NOT NULL DEFAULT '{}',
    days_of_week      smallint[]  NOT NULL DEFAULT '{0,1,2,3,4,5,6}',
    enabled           boolean     NOT NULL DEFAULT true,
    created_at        timestamptz NOT NULL DEFAULT now()
);

INSERT INTO tasks (name, cadence_seconds, enabled_for_roles, days_of_week) VALUES
    ('help_all',     180,   '{main,farm,scout,alliance_data}', '{0,1,2,3,4,5,6}'),
    ('daily_gather', 14400, '{main,farm,scout,alliance_data}', '{0,1,2,3,4,5,6}'),
    ('tech_donate',  7200,  '{main,farm,scout,alliance_data}', '{0,1,2,3,4,5,6}'),
    ('mail_collect', 86400, '{main,farm,scout,alliance_data}', '{0,1,2,3,4,5,6}'),
    ('radar_quick',  10800, '{main,farm,scout,alliance_data}', '{0,1,2,3,4,5,6}'),
    ('radar_claim',  86400, '{main,farm,scout,alliance_data}', '{1,3,5,6}');

-- +goose Down
DROP TABLE tasks;
