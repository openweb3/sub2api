-- TokenHive Bee installation instances and their shared upstream platforms.

CREATE TABLE IF NOT EXISTS bee (
    id                       BIGSERIAL PRIMARY KEY,
    user_id                  BIGINT NOT NULL REFERENCES users(id),
    device_id                UUID NOT NULL,
    name                     VARCHAR(100) NOT NULL,
    status                   VARCHAR(20) NOT NULL,
    credential_hash          VARCHAR NOT NULL,
    credential_created_at    TIMESTAMPTZ NOT NULL,
    app_version              VARCHAR(50),
    last_connected_at        TIMESTAMPTZ,
    last_disconnected_at     TIMESTAMPTZ,
    last_seen_at             TIMESTAMPTZ,
    created_at               TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at               TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    deleted_at               TIMESTAMPTZ,
    CONSTRAINT bee_device_id_unique UNIQUE (device_id)
);

CREATE INDEX IF NOT EXISTS idx_bee_user_id
    ON bee (user_id);

CREATE INDEX IF NOT EXISTS idx_bee_status
    ON bee (status);

CREATE INDEX IF NOT EXISTS idx_bee_deleted_at
    ON bee (deleted_at);

CREATE TABLE IF NOT EXISTS bee_platform (
    id                       BIGSERIAL PRIMARY KEY,
    bee_id                   BIGINT NOT NULL REFERENCES bee(id),
    platform                 VARCHAR(50) NOT NULL,
    upstream_account_key     VARCHAR(64) NOT NULL,
    identity_version         SMALLINT NOT NULL DEFAULT 1,
    subscription_tier        VARCHAR(50),
    concurrency              INTEGER NOT NULL,
    quota_snapshot           JSONB NOT NULL DEFAULT '{}'::JSONB,
    quota_updated_at         TIMESTAMPTZ,
    last_task_at             TIMESTAMPTZ,
    status                   VARCHAR(20) NOT NULL,
    extra                    JSONB NOT NULL DEFAULT '{}'::JSONB,
    created_at               TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at               TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    deleted_at               TIMESTAMPTZ,
    CONSTRAINT bee_platform_platform_check
        CHECK (platform IN ('openai', 'anthropic', 'gemini', 'grok')),
    CONSTRAINT bee_platform_concurrency_check
        CHECK (concurrency > 0)
);

CREATE INDEX IF NOT EXISTS idx_bee_platform_bee_id
    ON bee_platform (bee_id);

CREATE INDEX IF NOT EXISTS idx_bee_platform_platform
    ON bee_platform (platform);

CREATE INDEX IF NOT EXISTS idx_bee_platform_status
    ON bee_platform (status);

CREATE INDEX IF NOT EXISTS idx_bee_platform_deleted_at
    ON bee_platform (deleted_at);

CREATE UNIQUE INDEX IF NOT EXISTS uq_bee_platform_active_bee_platform
    ON bee_platform (bee_id, platform)
    WHERE deleted_at IS NULL;

CREATE UNIQUE INDEX IF NOT EXISTS uq_bee_platform_active_account
    ON bee_platform (platform, upstream_account_key)
    WHERE deleted_at IS NULL;
