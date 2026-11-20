// Package query computes the dashboard aggregates from the requests table.
// Every function reads raw rows and prices them at query time.
package query

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	defaultRange = 7 * 24 * time.Hour
	maxRange     = 400 * 24 * time.Hour
	// maxTZOffsetMinutes covers every real UTC offset (UTC-12 to UTC+14).
	maxTZOffsetMinutes = 14 * 60
)

// Shared SQL expressions. The reported model wins over the requested one,
// matching how pricing picks the model.
const (
	modelExpr  = "CASE WHEN response_model <> '' THEN response_model ELSE model END"
	tierExpr   = "CASE WHEN response_service_tier <> '' THEN response_service_tier ELSE effective_service_tier END"
	clientExpr = "CASE WHEN client_name = '' THEN 'unknown' WHEN client_version = '' THEN client_name ELSE client_name || '/' || client_version END"
	// effortExpr is the reasoning effort CPA sent upstream. "unset" means the
	// request named none and the model's default applied.
	effortExpr = "CASE WHEN reasoning_effort = '' THEN 'unset' ELSE reasoning_effort END"
	// callExpr identifies one upstream call: CPA's usage reporter stamps all
	// records of a call, split billing included, with the same trace and start
	// time. A WebSocket connection shares one trace across turns, but each
	// turn gets its own reporter and start time. Rows without a trace fall
	// back to their own request ID.
	callExpr = "(CASE WHEN trace_id <> '' THEN trace_id ELSE request_id END) || ':' || requested_at"
)

// Filter selects the rows every query works on. From is inclusive, To exclusive.
type Filter struct {
	From, To time.Time
	Provider string
	// Model matches the reported model, or the requested one when none was reported.
	Model      string
	Client     string
	Account    string
	APIKeyHash string
	// TZOffsetMinutes is the viewer's offset east of UTC. Day and hour buckets follow it.
	TZOffsetMinutes int
}

// ParamError is a caller error naming the offending query parameter.
type ParamError struct {
	Param  string
	Reason string
}

func (e *ParamError) Error() string {
	return fmt.Sprintf("invalid parameter %q: %s", e.Param, e.Reason)
}

// ParseFilter reads from, to (unix ms), provider, model, client, account, api_key and
// tz (minutes east of UTC). The range defaults to the 7 days before now.
func ParseFilter(q url.Values, now time.Time) (Filter, error) {
	f := Filter{
		Provider:   q.Get("provider"),
		Model:      q.Get("model"),
		Client:     q.Get("client"),
		Account:    q.Get("account"),
		APIKeyHash: q.Get("api_key"),
		To:         now,
	}
	if raw := q.Get("to"); raw != "" {
		to, err := parseMillis("to", raw)
		if err != nil {
			return Filter{}, err
		}
		f.To = to
	}
	f.From = f.To.Add(-defaultRange)
	if raw := q.Get("from"); raw != "" {
		from, err := parseMillis("from", raw)
		if err != nil {
			return Filter{}, err
		}
		f.From = from
	}
	if !f.From.Before(f.To) {
		return Filter{}, &ParamError{Param: "from", Reason: "must be before to"}
	}
	if f.To.Sub(f.From) > maxRange {
		return Filter{}, &ParamError{Param: "from", Reason: "range must not exceed 400 days"}
	}
	if raw := q.Get("tz"); raw != "" {
		tz, err := strconv.Atoi(raw)
		if err != nil || tz < -maxTZOffsetMinutes || tz > maxTZOffsetMinutes {
			return Filter{}, &ParamError{Param: "tz", Reason: "must be minutes east of UTC, between -840 and 840"}
		}
		f.TZOffsetMinutes = tz
	}
	return f, nil
}

func parseMillis(param, raw string) (time.Time, error) {
	ms, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || ms < 0 {
		return time.Time{}, &ParamError{Param: param, Reason: "must be a unix time in milliseconds"}
	}
	return time.UnixMilli(ms), nil
}

// where returns the SQL condition and arguments for the filter. Without the
// lower bound it selects all history up to To, which change markers need.
func (f Filter) where(withFrom bool) (string, []any) {
	conds := []string{"requested_at < ?"}
	args := []any{f.To.UnixMilli()}
	if withFrom {
		conds = append(conds, "requested_at >= ?")
		args = append(args, f.From.UnixMilli())
	}
	add := func(expr, value string) {
		if value != "" {
			conds = append(conds, expr+" = ?")
			args = append(args, value)
		}
	}
	add("provider", f.Provider)
	add("("+modelExpr+")", f.Model)
	add("client_name", f.Client)
	add("auth_index", f.Account)
	add("api_key_hash", f.APIKeyHash)
	return strings.Join(conds, " AND "), args
}

func (f Filter) offsetMillis() int64 {
	return int64(f.TZOffsetMinutes) * int64(time.Minute/time.Millisecond)
}

// bucketStart returns the unix ms start of the local bucket holding at.
func (f Filter) bucketStart(at, size int64) int64 {
	local := at + f.offsetMillis()
	start := local - local%size
	if local%size < 0 {
		start -= size
	}
	return start - f.offsetMillis()
}

// bucketSQL is the SQL form of bucketStart for a non-negative requested_at.
func (f Filter) bucketSQL(size int64) string {
	off := f.offsetMillis()
	return fmt.Sprintf("(((requested_at + %d) / %d) * %d - %d)", off, size, size, off)
}

const (
	hourMillis = int64(time.Hour / time.Millisecond)
	dayMillis  = 24 * hourMillis
)
