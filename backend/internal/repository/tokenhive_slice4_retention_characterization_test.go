package repository

import (
	"context"
	"database/sql"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	dbusagecleanuptask "github.com/Wei-Shaw/sub2api/ent/usagecleanuptask"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

var tokenHiveSlice4UsageLogColumns = []string{
	"user_id", "api_key_id", "account_id", "request_id", "model", "requested_model", "upstream_model",
	"upstream_response_model", "upstream_model_mismatch", "group_id", "subscription_id", "input_tokens", "output_tokens", "cache_creation_tokens", "cache_read_tokens",
	"cache_creation_5m_tokens", "cache_creation_1h_tokens", "image_output_tokens", "image_output_cost",
	"image_input_tokens", "image_input_cost", "input_cost", "output_cost", "cache_creation_cost", "cache_read_cost",
	"total_cost", "actual_cost", "rate_multiplier", "account_rate_multiplier", "billing_type", "request_type",
	"stream", "openai_ws_mode", "duration_ms", "first_token_ms", "user_agent", "ip_address", "image_count",
	"image_size", "image_input_size", "image_output_size", "image_size_source", "image_size_breakdown", "video_count",
	"video_resolution", "video_duration_seconds", "service_tier", "reasoning_effort", "requested_reasoning_effort", "inbound_endpoint",
	"upstream_endpoint", "cache_ttl_overridden", "long_context_billing_applied", "channel_id", "model_mapping_chain",
	"billing_tier", "billing_mode", "account_stats_cost", "session_id", "native_compaction_v2", "created_at",
}

func tokenHiveSlice4InsertColumns(t *testing.T, query string) []string {
	t.Helper()
	startMarker := "INSERT INTO usage_logs ("
	start := strings.Index(query, startMarker)
	require.NotEqual(t, -1, start)
	remaining := query[start+len(startMarker):]
	end := strings.Index(remaining, ")")
	require.NotEqual(t, -1, end)
	rawColumns := strings.Split(remaining[:end], ",")
	columns := make([]string, 0, len(rawColumns))
	for _, raw := range rawColumns {
		columns = append(columns, strings.TrimSpace(raw))
	}
	return columns
}

func TestTokenHiveSlice4UsageInsertWhitelistConflictAndNullability(t *testing.T) {
	empty := ""
	log := &service.UsageLog{
		UserID:         11,
		APIKeyID:       22,
		AccountID:      33,
		RequestID:      "   ",
		Model:          "",
		RequestedModel: "",
		UpstreamModel:  &empty,
		CreatedAt:      time.Date(2026, 8, 22, 1, 2, 3, 0, time.UTC),
	}
	prepared := prepareUsageLogInsert(log)
	require.Len(t, prepared.args, 61)

	key := usageLogBatchKey(log.RequestID, log.APIKeyID)
	query, args := buildUsageLogBatchInsertQuery(
		[]string{key},
		map[string]usageLogInsertPrepared{key: prepared},
	)
	require.Equal(t, tokenHiveSlice4UsageLogColumns, tokenHiveSlice4InsertColumns(t, query))
	require.Contains(t, query, "ON CONFLICT (request_id, api_key_id) DO NOTHING")
	require.Len(t, args, 62, "batch input prepends one synthetic input_idx to the 61 whitelisted values")

	require.Nil(t, prepared.args[3], "blank request_id becomes SQL NULL")
	require.Equal(t, "", prepared.args[4], "non-null model preserves an explicit empty string")
	require.Equal(t, sql.NullString{}, prepared.args[5], "nullable requested_model maps empty to SQL NULL")
	require.Equal(t, sql.NullString{}, prepared.args[6], "nullable upstream_model maps empty to SQL NULL")
	require.Equal(t, sql.NullString{}, prepared.args[7], "nullable upstream_response_model maps empty to SQL NULL")
	require.Equal(t, sql.NullBool{}, prepared.args[8], "nullable upstream_model_mismatch maps nil to SQL NULL")
	require.Equal(t, int16(service.RequestTypeSync), prepared.args[30], "legacy zero values normalize to sync at the pinned request_type index")

	bestEffortQuery, bestEffortArgs := buildUsageLogBestEffortInsertQuery([]usageLogInsertPrepared{prepared})
	require.Equal(t, tokenHiveSlice4UsageLogColumns, tokenHiveSlice4InsertColumns(t, bestEffortQuery))
	require.Contains(t, bestEffortQuery, "ON CONFLICT (request_id, api_key_id) DO NOTHING")
	require.Len(t, bestEffortArgs, 61)
}

func TestTokenHiveSlice4CleanupTaskStateIsUpdatedByID(t *testing.T) {
	repo, client := newUsageCleanupEntRepo(t)
	ctx := context.Background()
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	older := &service.UsageCleanupTask{
		Status:    service.UsageCleanupStatusRunning,
		Filters:   service.UsageCleanupFilters{StartTime: start, EndTime: start.Add(time.Hour)},
		CreatedBy: 1,
	}
	newer := &service.UsageCleanupTask{
		Status:    service.UsageCleanupStatusRunning,
		Filters:   service.UsageCleanupFilters{StartTime: start.Add(time.Hour), EndTime: start.Add(2 * time.Hour)},
		CreatedBy: 1,
	}
	require.NoError(t, repo.CreateTask(ctx, older))
	require.NoError(t, repo.CreateTask(ctx, newer))
	require.Less(t, older.ID, newer.ID)

	require.NoError(t, repo.UpdateTaskProgress(ctx, older.ID, 9))
	require.NoError(t, repo.MarkTaskSucceeded(ctx, older.ID, 17))

	oldRow, err := client.UsageCleanupTask.Get(ctx, older.ID)
	require.NoError(t, err)
	require.Equal(t, service.UsageCleanupStatusSucceeded, oldRow.Status)
	require.Equal(t, int64(17), oldRow.DeletedRows)
	require.NotNil(t, oldRow.FinishedAt)

	newRow, err := client.UsageCleanupTask.Get(ctx, newer.ID)
	require.NoError(t, err)
	require.Equal(t, service.UsageCleanupStatusRunning, newRow.Status)
	require.Zero(t, newRow.DeletedRows)

	require.NoError(t, repo.MarkTaskFailed(ctx, newer.ID, 3, "fixture failure"))
	newRow, err = client.UsageCleanupTask.Query().Where(dbusagecleanuptask.IDEQ(newer.ID)).Only(ctx)
	require.NoError(t, err)
	require.Equal(t, service.UsageCleanupStatusFailed, newRow.Status)
	require.Equal(t, int64(3), newRow.DeletedRows)
	require.Equal(t, "fixture failure", *newRow.ErrorMessage)
}

func TestTokenHiveSlice4CleanupBatchUsesClosedRangeAndStableOrder(t *testing.T) {
	setUsageCleanupRollupTestTimezone(t)
	db, mock := newSQLMock(t)
	repo := &usageCleanupRepository{sql: db}
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(time.Hour)
	firstDeletedAt := start.Add(10 * time.Minute)

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT id FROM usage_group_rollup_state.*FOR UPDATE`).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(1))
	mock.ExpectQuery("WITH target AS \\(.*created_at >= \\$1 AND created_at <= \\$2.*ORDER BY created_at ASC, id ASC.*LIMIT \\$3.*DELETE FROM usage_logs").
		WithArgs(start, end, 2).
		WillReturnRows(sqlmock.NewRows([]string{"created_at"}).AddRow(firstDeletedAt).AddRow(firstDeletedAt.Add(time.Minute)))
	mock.ExpectExec(`UPDATE usage_group_rollup_state`).
		WithArgs(firstDeletedAt, "Asia/Shanghai").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	deleted, err := repo.DeleteUsageLogsBatch(ctxBackground(), service.UsageCleanupFilters{StartTime: start, EndTime: end}, 2)
	require.NoError(t, err)
	require.Equal(t, int64(2), deleted)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestTokenHiveSlice4RetentionPartitionAndRowBoundaries(t *testing.T) {
	cutoff := time.Date(2026, 2, 15, 12, 0, 0, 0, time.FixedZone("fixture", 8*60*60))

	t.Run("partitioned drops only months before the cutoff month", func(t *testing.T) {
		setUsageCleanupRollupTestTimezone(t)
		db, mock := newSQLMock(t)
		repo := &dashboardAggregationRepository{sql: db}
		mock.ExpectQuery("SELECT c.relname").WillReturnRows(sqlmock.NewRows([]string{"relname"}).
			AddRow("usage_logs_202512").
			AddRow("usage_logs_202601").
			AddRow("usage_logs_202602").
			AddRow("usage_logs_invalid").
			AddRow("other_202501"))
		for _, month := range []time.Time{
			time.Date(2025, 12, 1, 0, 0, 0, 0, time.UTC),
			time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		} {
			mock.ExpectBegin()
			mock.ExpectQuery(`SELECT id FROM usage_group_rollup_state.*FOR UPDATE`).
				WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(1))
			mock.ExpectExec(`UPDATE usage_group_rollup_state`).
				WithArgs(month, "Asia/Shanghai").
				WillReturnResult(sqlmock.NewResult(0, 1))
			mock.ExpectExec(regexp.QuoteMeta(`DROP TABLE IF EXISTS "usage_logs_` + month.Format("200601") + `"`)).
				WillReturnResult(sqlmock.NewResult(0, 0))
			mock.ExpectCommit()
		}

		require.NoError(t, repo.dropUsageLogsPartitions(context.Background(), cutoff))
		require.NoError(t, mock.ExpectationsWereMet())
	})

	t.Run("unpartitioned removes rows strictly before the UTC cutoff", func(t *testing.T) {
		setUsageCleanupRollupTestTimezone(t)
		db, mock := newSQLMock(t)
		repo := &dashboardAggregationRepository{sql: db}
		firstDeletedAt := cutoff.UTC().Add(-time.Hour)
		rows := sqlmock.NewRows([]string{"created_at"})
		for i := 0; i < 7; i++ {
			rows.AddRow(firstDeletedAt.Add(time.Duration(i) * time.Minute))
		}
		mock.ExpectBegin()
		mock.ExpectQuery(`SELECT id FROM usage_group_rollup_state.*FOR UPDATE`).
			WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(1))
		mock.ExpectQuery("WHERE created_at < \\$1").
			WithArgs(cutoff.UTC(), usageLogsCleanupBatchSize).
			WillReturnRows(rows)
		mock.ExpectExec(`UPDATE usage_group_rollup_state`).
			WithArgs(firstDeletedAt, "Asia/Shanghai").
			WillReturnResult(sqlmock.NewResult(0, 1))
		mock.ExpectCommit()

		require.NoError(t, repo.cleanupUsageLogsBatches(context.Background(), cutoff))
		require.NoError(t, mock.ExpectationsWereMet())
	})
}

func ctxBackground() context.Context { return context.Background() }
