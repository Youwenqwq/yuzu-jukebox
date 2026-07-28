-- +goose Up
CREATE TABLE external_binding_codes (
    code_hash                BLOB PRIMARY KEY CHECK (length(code_hash) = 32),
    principal_id             TEXT NOT NULL UNIQUE,
    created_at               INTEGER NOT NULL,
    expires_at               INTEGER NOT NULL,
    consumed_at              INTEGER,
    consumed_integration_id  TEXT,
    consumed_adapter_id      TEXT,
    consumed_scope_type      TEXT,
    consumed_scope_id        TEXT,
    consumed_subject_id      TEXT,
    FOREIGN KEY (principal_id) REFERENCES users(id) ON DELETE CASCADE,
    FOREIGN KEY (consumed_integration_id) REFERENCES integrations(id) ON DELETE CASCADE,
    CHECK (
        (consumed_at IS NULL
            AND consumed_integration_id IS NULL
            AND consumed_adapter_id IS NULL
            AND consumed_scope_type IS NULL
            AND consumed_scope_id IS NULL
            AND consumed_subject_id IS NULL)
        OR
        (consumed_at IS NOT NULL
            AND consumed_integration_id IS NOT NULL
            AND consumed_adapter_id IS NOT NULL
            AND consumed_scope_type IS NOT NULL
            AND consumed_scope_id IS NOT NULL
            AND consumed_subject_id IS NOT NULL)
    )
);
CREATE INDEX idx_external_binding_codes_expires
    ON external_binding_codes(expires_at);
