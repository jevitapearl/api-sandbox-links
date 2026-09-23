-- 0001_init.down.sql — reverse of 0001_init.up.sql. Drop in dependency order.

DROP TABLE IF EXISTS resource_snapshots;
DROP TABLE IF EXISTS file_edits;
DROP TABLE IF EXISTS deployments;
DROP TABLE IF EXISTS sandbox_databases;
DROP TABLE IF EXISTS sandbox_env_vars;
DROP TABLE IF EXISTS sandbox_lifetime_warnings;
DROP TABLE IF EXISTS sandboxes;
DROP TABLE IF EXISTS repositories;
DROP TABLE IF EXISTS users;