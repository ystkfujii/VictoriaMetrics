package logsql

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/VictoriaMetrics/VictoriaMetrics/app/vlstorage"
	"github.com/VictoriaMetrics/VictoriaMetrics/lib/bytesutil"
	"github.com/VictoriaMetrics/VictoriaMetrics/lib/httpserver"
	"github.com/VictoriaMetrics/VictoriaMetrics/lib/httputils"
	"github.com/VictoriaMetrics/VictoriaMetrics/lib/logstorage"
	"github.com/VictoriaMetrics/VictoriaMetrics/lib/promutils"
)

// ProcessHitsRequest handles /select/logsql/hits request.
//
// See https://docs.victoriametrics.com/victorialogs/querying/#querying-hits-stats
func ProcessHitsRequest(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	q, tenantIDs, err := parseCommonArgs(r)
	if err != nil {
		httpserver.Errorf(w, r, "%s", err)
		return
	}

	// Obtain step
	stepStr := r.FormValue("step")
	if stepStr == "" {
		stepStr = "1d"
	}
	step, err := promutils.ParseDuration(stepStr)
	if err != nil {
		httpserver.Errorf(w, r, "cannot parse 'step' arg: %s", err)
		return
	}
	if step <= 0 {
		httpserver.Errorf(w, r, "'step' must be bigger than zero")
	}

	// Obtain offset
	offsetStr := r.FormValue("offset")
	if offsetStr == "" {
		offsetStr = "0s"
	}
	offset, err := promutils.ParseDuration(offsetStr)
	if err != nil {
		httpserver.Errorf(w, r, "cannot parse 'offset' arg: %s", err)
		return
	}

	// Obtain field entries
	fields := r.Form["field"]

	// Prepare the query
	q.AddCountByTimePipe(int64(step), int64(offset), fields)
	q.Optimize()

	var mLock sync.Mutex
	m := make(map[string]*hitsSeries)
	writeBlock := func(_ uint, timestamps []int64, columns []logstorage.BlockColumn) {
		if len(columns) == 0 || len(columns[0].Values) == 0 {
			return
		}

		timestampValues := columns[0].Values
		hitsValues := columns[len(columns)-1].Values
		columns = columns[1 : len(columns)-1]

		bb := blockResultPool.Get()
		for i := range timestamps {
			timestampStr := strings.Clone(timestampValues[i])
			hitsStr := strings.Clone(hitsValues[i])

			bb.Reset()
			WriteFieldsForHits(bb, columns, i)

			mLock.Lock()
			hs, ok := m[string(bb.B)]
			if !ok {
				k := string(bb.B)
				hs = &hitsSeries{}
				m[k] = hs
			}
			hs.timestamps = append(hs.timestamps, timestampStr)
			hs.values = append(hs.values, hitsStr)
			mLock.Unlock()
		}
		blockResultPool.Put(bb)
	}

	// Execute the query
	if err := vlstorage.RunQuery(ctx, tenantIDs, q, writeBlock); err != nil {
		httpserver.Errorf(w, r, "cannot execute query [%s]: %s", q, err)
		return
	}

	// Write response
	w.Header().Set("Content-Type", "application/json")
	WriteHitsSeries(w, m)
}

type hitsSeries struct {
	timestamps []string
	values     []string
}

func (hs *hitsSeries) sort() {
	sort.Sort(hs)
}

func (hs *hitsSeries) Len() int {
	return len(hs.timestamps)
}

func (hs *hitsSeries) Swap(i, j int) {
	hs.timestamps[i], hs.timestamps[j] = hs.timestamps[j], hs.timestamps[i]
	hs.values[i], hs.values[j] = hs.values[j], hs.values[i]
}

func (hs *hitsSeries) Less(i, j int) bool {
	return hs.timestamps[i] < hs.timestamps[j]
}

// ProcessFieldNamesRequest handles /select/logsql/field_names request.
//
// See https://docs.victoriametrics.com/victorialogs/querying/#querying-field-names
func ProcessFieldNamesRequest(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	q, tenantIDs, err := parseCommonArgs(r)
	if err != nil {
		httpserver.Errorf(w, r, "%s", err)
		return
	}

	// Obtain field names for the given query
	q.Optimize()
	fieldNames, err := vlstorage.GetFieldNames(ctx, tenantIDs, q)
	if err != nil {
		httpserver.Errorf(w, r, "cannot obtain field names: %s", err)
		return
	}

	// Write results
	w.Header().Set("Content-Type", "application/json")
	WriteValuesWithHitsJSON(w, fieldNames)
}

// ProcessFieldValuesRequest handles /select/logsql/field_values request.
//
// See https://docs.victoriametrics.com/victorialogs/querying/#querying-field-values
func ProcessFieldValuesRequest(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	q, tenantIDs, err := parseCommonArgs(r)
	if err != nil {
		httpserver.Errorf(w, r, "%s", err)
		return
	}

	// Parse fieldName query arg
	fieldName := r.FormValue("field")
	if fieldName == "" {
		httpserver.Errorf(w, r, "missing 'field' query arg")
		return
	}

	// Parse limit query arg
	limit, err := httputils.GetInt(r, "limit")
	if err != nil {
		httpserver.Errorf(w, r, "%s", err)
		return
	}
	if limit < 0 {
		limit = 0
	}

	// Obtain unique values for the given field
	q.Optimize()
	values, err := vlstorage.GetFieldValues(ctx, tenantIDs, q, fieldName, uint64(limit))
	if err != nil {
		httpserver.Errorf(w, r, "cannot obtain values for field %q: %s", fieldName, err)
		return
	}

	// Write results
	w.Header().Set("Content-Type", "application/json")
	WriteValuesWithHitsJSON(w, values)
}

// ProcessStreamFieldNamesRequest processes /select/logsql/stream_field_names request.
//
// See https://docs.victoriametrics.com/victorialogs/querying/#querying-stream-field-names
func ProcessStreamFieldNamesRequest(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	q, tenantIDs, err := parseCommonArgs(r)
	if err != nil {
		httpserver.Errorf(w, r, "%s", err)
		return
	}

	// Obtain stream field names for the given query
	q.Optimize()
	names, err := vlstorage.GetStreamFieldNames(ctx, tenantIDs, q)
	if err != nil {
		httpserver.Errorf(w, r, "cannot obtain stream field names: %s", err)
	}

	// Write results
	w.Header().Set("Content-Type", "application/json")
	WriteValuesWithHitsJSON(w, names)
}

// ProcessStreamFieldValuesRequest processes /select/logsql/stream_field_values request.
//
// See https://docs.victoriametrics.com/victorialogs/querying/#querying-stream-field-values
func ProcessStreamFieldValuesRequest(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	q, tenantIDs, err := parseCommonArgs(r)
	if err != nil {
		httpserver.Errorf(w, r, "%s", err)
		return
	}

	// Parse fieldName query arg
	fieldName := r.FormValue("field")
	if fieldName == "" {
		httpserver.Errorf(w, r, "missing 'field' query arg")
		return
	}

	// Parse limit query arg
	limit, err := httputils.GetInt(r, "limit")
	if err != nil {
		httpserver.Errorf(w, r, "%s", err)
		return
	}
	if limit < 0 {
		limit = 0
	}

	// Obtain stream field values for the given query and the given fieldName
	q.Optimize()
	values, err := vlstorage.GetStreamFieldValues(ctx, tenantIDs, q, fieldName, uint64(limit))
	if err != nil {
		httpserver.Errorf(w, r, "cannot obtain stream field values: %s", err)
	}

	// Write results
	w.Header().Set("Content-Type", "application/json")
	WriteValuesWithHitsJSON(w, values)
}

// ProcessStreamsRequest processes /select/logsql/streams request.
//
// See https://docs.victoriametrics.com/victorialogs/querying/#querying-streams
func ProcessStreamsRequest(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	q, tenantIDs, err := parseCommonArgs(r)
	if err != nil {
		httpserver.Errorf(w, r, "%s", err)
		return
	}

	// Parse limit query arg
	limit, err := httputils.GetInt(r, "limit")
	if err != nil {
		httpserver.Errorf(w, r, "%s", err)
		return
	}
	if limit < 0 {
		limit = 0
	}

	// Obtain streams for the given query
	q.Optimize()
	streams, err := vlstorage.GetStreams(ctx, tenantIDs, q, uint64(limit))
	if err != nil {
		httpserver.Errorf(w, r, "cannot obtain streams: %s", err)
	}

	// Write results
	w.Header().Set("Content-Type", "application/json")
	WriteValuesWithHitsJSON(w, streams)
}

// ProcessQueryRequest handles /select/logsql/query request.
//
// See https://docs.victoriametrics.com/victorialogs/querying/#http-api
func ProcessQueryRequest(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	q, tenantIDs, err := parseCommonArgs(r)
	if err != nil {
		httpserver.Errorf(w, r, "%s", err)
		return
	}

	// Parse limit query arg
	limit, err := httputils.GetInt(r, "limit")
	if err != nil {
		httpserver.Errorf(w, r, "%s", err)
		return
	}
	if limit > 0 {
		q.AddPipeLimit(uint64(limit))
	}

	bw := getBufferedWriter(w)

	writeBlock := func(_ uint, timestamps []int64, columns []logstorage.BlockColumn) {
		if len(columns) == 0 || len(columns[0].Values) == 0 {
			return
		}

		bb := blockResultPool.Get()
		for i := range timestamps {
			WriteJSONRow(bb, columns, i)
		}
		bw.WriteIgnoreErrors(bb.B)
		blockResultPool.Put(bb)
	}

	w.Header().Set("Content-Type", "application/stream+json")
	q.Optimize()
	err = vlstorage.RunQuery(ctx, tenantIDs, q, writeBlock)

	bw.FlushIgnoreErrors()
	putBufferedWriter(bw)

	if err != nil {
		httpserver.Errorf(w, r, "cannot execute query [%s]: %s", q, err)
	}
}

var blockResultPool bytesutil.ByteBufferPool

func parseCommonArgs(r *http.Request) (*logstorage.Query, []logstorage.TenantID, error) {
	// Extract tenantID
	tenantID, err := logstorage.GetTenantIDFromRequest(r)
	if err != nil {
		return nil, nil, fmt.Errorf("cannot obtain tenanID: %w", err)
	}
	tenantIDs := []logstorage.TenantID{tenantID}

	// Parse query
	qStr := r.FormValue("query")
	q, err := logstorage.ParseQuery(qStr)
	if err != nil {
		return nil, nil, fmt.Errorf("cannot parse query [%s]: %s", qStr, err)
	}

	// Parse optional start and end args
	start, okStart, err := getTimeNsec(r, "start")
	if err != nil {
		return nil, nil, err
	}
	end, okEnd, err := getTimeNsec(r, "end")
	if err != nil {
		return nil, nil, err
	}
	if okStart || okEnd {
		if !okStart {
			start = math.MinInt64
		}
		if !okEnd {
			end = math.MaxInt64
		}
		q.AddTimeFilter(start, end)
	}

	return q, tenantIDs, nil
}

func getTimeNsec(r *http.Request, argName string) (int64, bool, error) {
	s := r.FormValue(argName)
	if s == "" {
		return 0, false, nil
	}
	currentTimestamp := float64(time.Now().UnixNano()) / 1e9
	secs, err := promutils.ParseTimeAt(s, currentTimestamp)
	if err != nil {
		return 0, false, fmt.Errorf("cannot parse %s=%s: %w", argName, s, err)
	}
	return int64(secs * 1e9), true, nil
}
