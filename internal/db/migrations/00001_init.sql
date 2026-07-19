-- +goose Up

-- A device is one adb target: an emulator instance or a physical handset.
-- `transport` names the Transport implementation that drives it, so a future
-- non-adb device (a legacy desktop target, say) needs no schema change.
CREATE TABLE devices (
    id             bigserial PRIMARY KEY,
    serial         text        NOT NULL UNIQUE,
    transport      text        NOT NULL DEFAULT 'adb',
    resolution_w   integer     NOT NULL,
    resolution_h   integer     NOT NULL,
    status         text        NOT NULL DEFAULT 'unknown',
    last_heartbeat timestamptz,
    created_at     timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT devices_resolution_positive CHECK (resolution_w > 0 AND resolution_h > 0),
    CONSTRAINT devices_status_valid CHECK (status IN ('unknown', 'online', 'offline', 'error'))
);

-- One installed copy of the game on a device. Cloning (Island/Shelter or the
-- device's dual-app feature) lets one device host two accounts, distinguished
-- by clone_id.
CREATE TABLE app_instances (
    id         bigserial PRIMARY KEY,
    device_id  bigint      NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
    package    text        NOT NULL DEFAULT 'com.fun.lastwar.gp',
    clone_id   integer     NOT NULL DEFAULT 0,
    created_at timestamptz NOT NULL DEFAULT now(),

    UNIQUE (device_id, package, clone_id)
);

-- A game account, pinned 1:1 to an app instance. `role` decides which tasks
-- the scheduler will consider; 'alliance_data' is the role that runs the
-- analytics capture routes.
CREATE TABLE accounts (
    id              bigserial PRIMARY KEY,
    app_instance_id bigint      NOT NULL UNIQUE REFERENCES app_instances(id) ON DELETE CASCADE,
    game_uid        text,
    nickname        text        NOT NULL,
    server          integer,
    role            text        NOT NULL DEFAULT 'farm',
    -- No FK yet: the alliances table arrives with the analytics milestone.
    alliance_id     bigint,
    enabled         boolean     NOT NULL DEFAULT true,
    created_at      timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT accounts_role_valid CHECK (role IN ('main', 'farm', 'scout', 'alliance_data'))
);

-- Every captured frame. object_key is content-addressed by sha256, so
-- identical frames share one blob while still recording distinct observations.
-- screen_id stays NULL until the vision milestone can recognize screens.
CREATE TABLE screenshots (
    id          bigserial PRIMARY KEY,
    account_id  bigint      NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    captured_at timestamptz NOT NULL DEFAULT now(),
    screen_id   text,
    object_key  text        NOT NULL,
    sha256      text        NOT NULL,

    CONSTRAINT screenshots_sha256_hex CHECK (sha256 ~ '^[0-9a-f]{64}$')
);

CREATE INDEX screenshots_account_captured_idx ON screenshots (account_id, captured_at DESC);
CREATE INDEX screenshots_sha256_idx ON screenshots (sha256);

-- +goose Down
DROP TABLE screenshots;
DROP TABLE accounts;
DROP TABLE app_instances;
DROP TABLE devices;
