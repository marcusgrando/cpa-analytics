-- Each Claude and Codex response reports its account's subscription limit
-- usage in headers. A row keeps that snapshot, in percent of the limit, and
-- the reset time in Unix milliseconds. NULL means the response did not
-- report the window. auth_index matches the credential list in CPA's
-- management API.
ALTER TABLE requests ADD COLUMN auth_index TEXT NOT NULL DEFAULT '';
ALTER TABLE requests ADD COLUMN limit_session_pct REAL;
ALTER TABLE requests ADD COLUMN limit_session_reset INTEGER;
ALTER TABLE requests ADD COLUMN limit_weekly_pct REAL;
ALTER TABLE requests ADD COLUMN limit_weekly_reset INTEGER;
