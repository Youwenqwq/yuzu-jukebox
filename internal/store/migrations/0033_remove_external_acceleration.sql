-- +goose Up
-- Drop children before their parents with foreign-key enforcement enabled.
-- SQLite removes each table's indexes and triggers together with the table.
DROP TABLE IF EXISTS acceleration_inventory_objects;
DROP TABLE IF EXISTS acceleration_inventory_snapshots;
DROP TABLE IF EXISTS acceleration_inventory_scans;
DROP TABLE IF EXISTS acceleration_storage_reservations;
DROP TABLE IF EXISTS acceleration_deletion_jobs;
DROP TABLE IF EXISTS acceleration_storage_status;
DROP TABLE IF EXISTS acceleration_objects;
DROP TABLE IF EXISTS distribution_candidates;
DROP TABLE IF EXISTS distribution_leases;
DROP TABLE IF EXISTS distribution_requests;
DROP TABLE IF EXISTS distribution_publishers;
DROP TABLE IF EXISTS distribution_attempts;
DROP TABLE IF EXISTS distribution_metric_buckets;
DROP TABLE IF EXISTS distribution_metrics;
DROP TABLE IF EXISTS accelerations;

-- Remove only the retired role, retaining other assignments and session data.
-- Invalid legacy JSON is left untouched rather than preventing startup.
UPDATE users
SET roles_json = (
    SELECT json_group_array(value) FROM json_each(users.roles_json)
    WHERE value IS NOT 'sys_admin'
)
WHERE CASE WHEN json_valid(roles_json) THEN
    json_type(roles_json) = 'array'
    AND EXISTS (SELECT 1 FROM json_each(roles_json) WHERE value = 'sys_admin')
ELSE 0 END;

UPDATE sessions
SET identity_json = json_set(identity_json, '$.roles', (
    SELECT json_group_array(value) FROM json_each(sessions.identity_json, '$.roles')
    WHERE value IS NOT 'sys_admin'
))
WHERE CASE WHEN json_valid(identity_json) THEN
    json_type(identity_json, '$.roles') = 'array'
    AND EXISTS (SELECT 1 FROM json_each(identity_json, '$.roles') WHERE value = 'sys_admin')
ELSE 0 END;

UPDATE rooms
SET trusted_roles_json = (
    SELECT json_group_array(value) FROM json_each(rooms.trusted_roles_json)
    WHERE value IS NOT 'sys_admin'
)
WHERE CASE WHEN json_valid(trusted_roles_json) THEN
    json_type(trusted_roles_json) = 'array'
    AND EXISTS (SELECT 1 FROM json_each(trusted_roles_json) WHERE value = 'sys_admin')
ELSE 0 END;
