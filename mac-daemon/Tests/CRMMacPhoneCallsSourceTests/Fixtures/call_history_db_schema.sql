-- Minimum CallHistoryDB schema needed by the CallHistoryDBReader.
--
-- Verified against the macOS CallHistoryDB schema (private, not public
-- API). Only the columns the reader touches are listed; the real
-- CallHistoryDB has many more (e.g., ZNAME, ZLOCATION) we don't read.
--
-- Tests load this script into an in-memory SQLite database and INSERT
-- test-specific rows on top. No binary fixture is committed.

CREATE TABLE ZCALLRECORD (
    Z_PK INTEGER PRIMARY KEY AUTOINCREMENT,
    ZUNIQUE_ID TEXT NOT NULL,
    ZDATE REAL NOT NULL,
    ZADDRESS TEXT,
    ZORIGINATED INTEGER,
    ZANSWERED INTEGER,
    ZDURATION REAL,
    ZSERVICE_PROVIDER TEXT,
    ZCALLTYPE INTEGER,
    ZHASMESSAGE INTEGER
);
