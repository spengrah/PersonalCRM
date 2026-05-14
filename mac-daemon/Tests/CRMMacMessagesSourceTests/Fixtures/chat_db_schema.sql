-- Minimum chat.db schema needed by the ChatDBReader.
--
-- Verified against the macOS Messages.app schema (private, not public
-- API). Only the columns the reader touches are listed; the real chat.db
-- has many more (e.g., reply_thread_id, balloon_bundle_id) we don't read.
--
-- Tests load this script into an in-memory SQLite database and INSERT
-- test-specific rows on top. No binary fixture is committed.

CREATE TABLE handle (
    ROWID INTEGER PRIMARY KEY AUTOINCREMENT,
    id TEXT NOT NULL,
    service TEXT,
    country TEXT,
    uncanonicalized_id TEXT
);

CREATE TABLE chat (
    ROWID INTEGER PRIMARY KEY AUTOINCREMENT,
    guid TEXT NOT NULL UNIQUE,
    style INTEGER,
    chat_identifier TEXT,
    display_name TEXT
);

CREATE TABLE chat_handle_join (
    chat_id INTEGER NOT NULL,
    handle_id INTEGER NOT NULL
);

CREATE TABLE message (
    ROWID INTEGER PRIMARY KEY AUTOINCREMENT,
    guid TEXT NOT NULL UNIQUE,
    text TEXT,
    handle_id INTEGER,
    date INTEGER NOT NULL,
    date_read INTEGER,
    date_delivered INTEGER,
    is_from_me INTEGER NOT NULL DEFAULT 0,
    is_read INTEGER NOT NULL DEFAULT 0,
    cache_has_attachments INTEGER NOT NULL DEFAULT 0,
    associated_message_guid TEXT
);

CREATE TABLE chat_message_join (
    chat_id INTEGER NOT NULL,
    message_id INTEGER NOT NULL
);

CREATE TABLE attachment (
    ROWID INTEGER PRIMARY KEY AUTOINCREMENT,
    guid TEXT NOT NULL UNIQUE,
    uti TEXT,
    mime_type TEXT,
    transfer_name TEXT,
    total_bytes INTEGER
);

CREATE TABLE message_attachment_join (
    ROWID INTEGER PRIMARY KEY AUTOINCREMENT,
    message_id INTEGER NOT NULL,
    attachment_id INTEGER NOT NULL
);
