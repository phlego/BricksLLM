package postgresql

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/bricks-cloud/bricksllm/internal/apikey"
	"github.com/bricks-cloud/bricksllm/internal/event"
	"github.com/bricks-cloud/bricksllm/internal/key"

	"github.com/lib/pq"
	_ "github.com/lib/pq"
)

type Store struct {
	db *sql.DB
	wt time.Duration
	rt time.Duration
}

func NewStore(connStr string, wt time.Duration, rt time.Duration) (*Store, error) {
	db, err := sql.Open("postgres", connStr)
	if err != nil {
		return nil, err
	}

	return &Store{
		db: db,
		wt: wt,
		rt: rt,
	}, nil
}

func (s *Store) CreateApiKeysTable() error {
	createTableQuery := `
	CREATE TABLE IF NOT EXISTS apikeys (
		key_id VARCHAR(255) PRIMARY KEY,
		updated_at BIGINT NOT NULL,
		key VARCHAR(255) NOT NULL,
		key_name VARCHAR(255) NOT NULL,
		provider VARCHAR(255)
	)`

	ctxTimeout, cancel := context.WithTimeout(context.Background(), s.wt)
	defer cancel()
	_, err := s.db.ExecContext(ctxTimeout, createTableQuery)
	if err != nil {
		return err
	}

	return nil
}

func (s *Store) CreateKeysTable() error {
	createTableQuery := `
	CREATE TABLE IF NOT EXISTS keys (
		name VARCHAR(255) NOT NULL,
		created_at BIGINT NOT NULL,
		updated_at BIGINT NOT NULL,
		tags VARCHAR(255)[],
		revoked BOOLEAN NOT NULL,
		key_id VARCHAR(255) PRIMARY KEY,
		key VARCHAR(255) NOT NULL,
		revoked_reason VARCHAR(255),
		cost_limit_in_usd FLOAT8,
		cost_limit_in_usd_over_time FLOAT8,
		cost_limit_in_usd_unit VARCHAR(255),
		rate_limit_over_time INT,
		rate_limit_unit VARCHAR(255),
		ttl VARCHAR(255)
	)`

	ctxTimeout, cancel := context.WithTimeout(context.Background(), s.wt)
	defer cancel()
	_, err := s.db.ExecContext(ctxTimeout, createTableQuery)
	if err != nil {
		return err
	}

	return nil
}

func (s *Store) CreateEventsTable() error {
	createTableQuery := `
	CREATE TABLE IF NOT EXISTS events (
		event_id VARCHAR(255) PRIMARY KEY,
		created_at BIGINT NOT NULL,
		tags VARCHAR(255)[],
		key_id VARCHAR(255),
		cost_in_usd FLOAT8,
		provider VARCHAR(255),
		model VARCHAR(255),
		status_code INT,
		prompt_token_count INT,
		completion_token_count INT,
		latency_in_ms INT
	)`

	ctxTimeout, cancel := context.WithTimeout(context.Background(), s.wt)
	defer cancel()
	_, err := s.db.ExecContext(ctxTimeout, createTableQuery)
	if err != nil {
		return err
	}

	return nil
}

func (s *Store) DropApiKeysTable() error {
	dropTableQuery := `DROP TABLE apikeys`
	ctxTimeout, cancel := context.WithTimeout(context.Background(), s.wt)
	defer cancel()

	_, err := s.db.ExecContext(ctxTimeout, dropTableQuery)
	if err != nil {
		return err
	}

	return nil
}

func (s *Store) DropEventsTable() error {
	dropTableQuery := `DROP TABLE events`
	ctxTimeout, cancel := context.WithTimeout(context.Background(), s.wt)
	defer cancel()

	_, err := s.db.ExecContext(ctxTimeout, dropTableQuery)
	if err != nil {
		return err
	}

	return nil
}

func (s *Store) DropKeysTable() error {
	dropTableQuery := `DROP TABLE keys`
	ctxTimeout, cancel := context.WithTimeout(context.Background(), s.wt)
	defer cancel()

	_, err := s.db.ExecContext(ctxTimeout, dropTableQuery)
	if err != nil {
		return err
	}

	return nil
}

func (s *Store) InsertEvent(e *event.Event) error {
	query := `
		INSERT INTO events (event_id, created_at, tags, key_id, cost_in_usd, provider, model, status_code, prompt_token_count, completion_token_count, latency_in_ms)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
	`

	values := []any{
		e.Id,
		e.CreatedAt,
		sliceToSqlStringArray(e.Tags),
		e.KeyId,
		e.CostInUsd,
		e.Provider,
		e.Model,
		e.Status,
		e.PromptTokenCount,
		e.CompletionTokenCount,
		e.LatencyInMs,
	}

	ctx, cancel := context.WithTimeout(context.Background(), s.wt)
	defer cancel()
	if _, err := s.db.ExecContext(ctx, query, values...); err != nil {
		return err
	}

	return nil
}

func (s *Store) GetLatencyPercentiles(start, end int64, tags, keyIds []string) ([]float64, error) {
	eventSelectionBlock := `
	WITH events_table AS
		(
			SELECT * FROM events 
	`

	conditionBlock := fmt.Sprintf("WHERE created_at >= %d AND created_at <= %d", start, end)
	if len(tags) != 0 {
		conditionBlock += fmt.Sprintf("AND tags @> '%s' ", sliceToSqlStringArray(tags))
	}

	if len(keyIds) != 0 {
		conditionBlock += fmt.Sprintf("AND key_id = ANY('%s')", sliceToSqlStringArray(keyIds))
	}

	if len(tags) != 0 || len(keyIds) != 0 {
		eventSelectionBlock += conditionBlock
	}

	eventSelectionBlock += ")"

	query :=
		`
		SELECT    COALESCE(percentile_cont(0.5) WITHIN GROUP (ORDER BY events_table.latency_in_ms), 0) as median_latency, COALESCE(percentile_cont(0.99) WITHIN GROUP (ORDER BY events_table.latency_in_ms), 0) as top_latency
		FROM      events_table
		`

	query = eventSelectionBlock + query

	ctx, cancel := context.WithTimeout(context.Background(), s.rt)
	defer cancel()

	rows, err := s.db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	data := []float64{}
	for rows.Next() {
		var median float64
		var top float64

		if err := rows.Scan(
			&median,
			&top,
		); err != nil {
			return nil, err
		}

		data = []float64{
			median,
			top,
		}
		break
	}

	return data, nil
}

func (s *Store) GetEventDataPoints(start, end, increment int64, tags, keyIds []string) ([]*event.DataPoint, error) {
	query := fmt.Sprintf(
		`
		,time_series_table AS
		(
			SELECT generate_series(%d, %d, %d) series
		)
		SELECT    series AS time_stamp, COALESCE(COUNT(events_table.event_id),0) AS num_of_requests, COALESCE(SUM(events_table.cost_in_usd),0) AS cost_in_usd, COALESCE(SUM(events_table.latency_in_ms),0) AS latency_in_ms, COALESCE(SUM(events_table.prompt_token_count),0) AS prompt_token_count, COALESCE(SUM(events_table.completion_token_count),0) AS completion_token_count, COALESCE(SUM(CASE WHEN status_code = 200 THEN 1 END),0) AS success_count
		FROM      time_series_table
		LEFT JOIN events_table
		ON        events_table.created_at >= time_series_table.series 
		AND       events_table.created_at < time_series_table.series + %d
		GROUP BY  time_series_table.series
		ORDER BY  time_series_table.series;
		`,
		start, end, increment, increment,
	)

	eventSelectionBlock := `
	WITH events_table AS
		(
			SELECT * FROM events 
	`

	conditionBlock := "WHERE "
	if len(tags) != 0 {
		conditionBlock += fmt.Sprintf("tags @> '%s' ", sliceToSqlStringArray(tags))
	}

	if len(tags) != 0 && len(keyIds) != 0 {
		conditionBlock += "AND "
	}

	if len(keyIds) != 0 {
		conditionBlock += fmt.Sprintf("key_id = ANY('%s')", sliceToSqlStringArray(keyIds))
	}

	if len(tags) != 0 || len(keyIds) != 0 {
		eventSelectionBlock += conditionBlock
	}

	eventSelectionBlock += ")"

	query = eventSelectionBlock + query

	ctx, cancel := context.WithTimeout(context.Background(), s.rt)
	defer cancel()

	rows, err := s.db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	data := []*event.DataPoint{}
	for rows.Next() {
		var e event.DataPoint
		if err := rows.Scan(
			&e.TimeStamp,
			&e.NumberOfRequests,
			&e.CostInUsd,
			&e.LatencyInMs,
			&e.PromptTokenCount,
			&e.CompletionTokenCount,
			&e.SuccessCouunt,
		); err != nil {
			return nil, err
		}

		data = append(data, &e)
	}

	return data, nil
}

func (s *Store) GetKeysByTag(tag string) ([]*key.ResponseKey, error) {
	ctxTimeout, cancel := context.WithTimeout(context.Background(), s.rt)
	defer cancel()

	rows, err := s.db.QueryContext(ctxTimeout, "SELECT * FROM keys WHERE $1 = ANY(tags)", tag)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	keys := []*key.ResponseKey{}
	for rows.Next() {
		var k key.ResponseKey
		if err := rows.Scan(
			&k.Name,
			&k.CreatedAt,
			&k.UpdatedAt,
			pq.Array(&k.Tags),
			&k.Revoked,
			&k.KeyId,
			&k.Key,
			&k.RevokedReason,
			&k.CostLimitInUsd,
			&k.CostLimitInUsdOverTime,
			&k.CostLimitInUsdUnit,
			&k.RateLimitOverTime,
			&k.RateLimitUnit,
			&k.Ttl,
		); err != nil {
			return nil, err
		}
		keys = append(keys, &k)
	}

	return keys, nil
}

func (s *Store) GetKey(keyId string) (*key.ResponseKey, error) {
	ctxTimeout, cancel := context.WithTimeout(context.Background(), s.rt)
	defer cancel()

	rows, err := s.db.QueryContext(ctxTimeout, "SELECT * FROM keys WHERE key_id = $1", keyId)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	keys := []*key.ResponseKey{}
	for rows.Next() {
		var k key.ResponseKey
		if err := rows.Scan(
			&k.Name,
			&k.CreatedAt,
			&k.UpdatedAt,
			pq.Array(&k.Tags),
			&k.Revoked,
			&k.KeyId,
			&k.Key,
			&k.RevokedReason,
			&k.CostLimitInUsd,
			&k.CostLimitInUsdOverTime,
			&k.CostLimitInUsdUnit,
			&k.RateLimitOverTime,
			&k.RateLimitUnit,
			&k.Ttl,
		); err != nil {
			return nil, err
		}
		keys = append(keys, &k)
	}

	if len(keys) == 0 {
		return nil, nil
	}

	return keys[0], nil
}

func (s *Store) GetAllApiKeys() ([]*apikey.ResponseApiKey, error) {
	ctxTimeout, cancel := context.WithTimeout(context.Background(), s.rt)
	defer cancel()

	rows, err := s.db.QueryContext(ctxTimeout, "SELECT * FROM apikeys")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	keys := []*apikey.ResponseApiKey{}
	for rows.Next() {
		var k apikey.ResponseApiKey
		if err := rows.Scan(
			&k.Id,
			&k.UpdatedAt,
			&k.Key,
			&k.KeyName,
			&k.Provider,
		); err != nil {
			return nil, err
		}
		keys = append(keys, &k)
	}

	return keys, nil
}

func (s *Store) GetAllKeys() ([]*key.ResponseKey, error) {
	ctxTimeout, cancel := context.WithTimeout(context.Background(), s.rt)
	defer cancel()

	rows, err := s.db.QueryContext(ctxTimeout, "SELECT * FROM keys")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	keys := []*key.ResponseKey{}
	for rows.Next() {
		var k key.ResponseKey
		if err := rows.Scan(
			&k.Name,
			&k.CreatedAt,
			&k.UpdatedAt,
			pq.Array(&k.Tags),
			&k.Revoked,
			&k.KeyId,
			&k.Key,
			&k.RevokedReason,
			&k.CostLimitInUsd,
			&k.CostLimitInUsdOverTime,
			&k.CostLimitInUsdUnit,
			&k.RateLimitOverTime,
			&k.RateLimitUnit,
			&k.Ttl,
		); err != nil {
			return nil, err
		}
		keys = append(keys, &k)
	}

	return keys, nil
}

func (s *Store) GetUpdatedApiKeys(interval time.Duration) ([]*apikey.ResponseApiKey, error) {
	ctxTimeout, cancel := context.WithTimeout(context.Background(), s.rt)
	defer cancel()

	rows, err := s.db.QueryContext(ctxTimeout, "SELECT * FROM apikeys WHERE updated_at >= $1", time.Now().Unix()-int64(interval.Seconds()))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	keys := []*apikey.ResponseApiKey{}
	for rows.Next() {
		var k apikey.ResponseApiKey
		if err := rows.Scan(
			&k.Id,
			&k.UpdatedAt,
			&k.Key,
			&k.KeyName,
			&k.Provider,
		); err != nil {
			return nil, err
		}
		keys = append(keys, &k)
	}

	return keys, nil
}

func (s *Store) GetUpdatedKeys(interval time.Duration) ([]*key.ResponseKey, error) {
	ctxTimeout, cancel := context.WithTimeout(context.Background(), s.rt)
	defer cancel()

	rows, err := s.db.QueryContext(ctxTimeout, "SELECT * FROM keys WHERE updated_at >= $1", time.Now().Unix()-int64(interval.Seconds()))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	keys := []*key.ResponseKey{}
	for rows.Next() {
		var k key.ResponseKey
		if err := rows.Scan(
			&k.Name,
			&k.CreatedAt,
			&k.UpdatedAt,
			pq.Array(&k.Tags),
			&k.Revoked,
			&k.KeyId,
			&k.Key,
			&k.RevokedReason,
			&k.CostLimitInUsd,
			&k.CostLimitInUsdOverTime,
			&k.CostLimitInUsdUnit,
			&k.RateLimitOverTime,
			&k.RateLimitUnit,
			&k.Ttl,
		); err != nil {
			return nil, err
		}
		keys = append(keys, &k)
	}

	return keys, nil
}

func (s *Store) UpdateKey(id string, uk *key.UpdateKey) (*key.ResponseKey, error) {
	fields := []string{}
	counter := 2
	values := []any{
		id,
	}

	if len(uk.Name) != 0 {
		values = append(values, uk.Name)
		fields = append(fields, fmt.Sprintf("name = $%d", counter))
		counter++
	}

	if uk.UpdatedAt != 0 {
		values = append(values, uk.UpdatedAt)
		fields = append(fields, fmt.Sprintf("updated_at = $%d", counter))
		counter++
	}

	if len(uk.Tags) != 0 {
		values = append(values, sliceToSqlStringArray(uk.Tags))
		fields = append(fields, fmt.Sprintf("tags = $%d", counter))
		counter++
	}

	if uk.Revoked != nil {
		values = append(values, uk.Revoked)
		fields = append(fields, fmt.Sprintf("revoked = $%d", counter))
		counter++
	}

	if len(uk.RevokedReason) != 0 {
		values = append(values, uk.RevokedReason)
		fields = append(fields, fmt.Sprintf("revoked_reason = $%d", counter))
		counter++
	}

	if uk.CostLimitInUsd != 0 {
		values = append(values, uk.CostLimitInUsd)
		fields = append(fields, fmt.Sprintf("cost_limit_in_usd = $%d", counter))
		counter++
	}

	if uk.CostLimitInUsdOverTime != 0 {
		values = append(values, uk.CostLimitInUsdOverTime)
		fields = append(fields, fmt.Sprintf("cost_limit_in_usd_over_time = $%d", counter))
		counter++
	}

	if len(uk.CostLimitInUsdUnit) != 0 {
		values = append(values, uk.CostLimitInUsdUnit)
		fields = append(fields, fmt.Sprintf("cost_limit_in_usd_unit = $%d", counter))
		counter++
	}

	if uk.RateLimitOverTime != 0 {
		values = append(values, uk.RateLimitOverTime)
		fields = append(fields, fmt.Sprintf("rate_limit_over_time = $%d", counter))
		counter++
	}

	if len(uk.RateLimitUnit) != 0 {
		values = append(values, uk.RateLimitUnit)
		fields = append(fields, fmt.Sprintf("rate_limit_unit = $%d", counter))
		counter++
	}

	if len(uk.Ttl) != 0 {
		values = append(values, uk.Ttl)
		fields = append(fields, fmt.Sprintf("ttl = $%d", counter))
		counter++
	}

	query := fmt.Sprintf("UPDATE keys SET %s WHERE key_id = $1 RETURNING *;", strings.Join(fields, ","))

	ctxTimeout, cancel := context.WithTimeout(context.Background(), s.wt)
	defer cancel()

	var k key.ResponseKey
	if err := s.db.QueryRowContext(ctxTimeout, query, values...).Scan(
		&k.Name,
		&k.CreatedAt,
		&k.UpdatedAt,
		pq.Array(&k.Tags),
		&k.Revoked,
		&k.KeyId,
		&k.Key,
		&k.RevokedReason,
		&k.CostLimitInUsd,
		&k.CostLimitInUsdOverTime,
		&k.CostLimitInUsdUnit,
		&k.RateLimitOverTime,
		&k.RateLimitUnit,
		&k.Ttl,
	); err != nil {
		return nil, err
	}

	return &k, nil
}

func (s *Store) UpsertApiKey(rk *apikey.RequestApiKey) error {
	if len(rk.Provider) == 0 {
		return errors.New("provider is empty")
	}

	if len(rk.KeyName) == 0 {
		return errors.New("key name is empty")
	}

	ctxTimeout, cancel := context.WithTimeout(context.Background(), s.wt)
	defer cancel()
	duplicated, err := s.db.QueryContext(ctxTimeout, "SELECT * FROM apikeys WHERE $1 = provider AND $2 = key_name", rk.Key, rk.KeyName)
	if err != nil {
		return err
	}
	defer duplicated.Close()

	i := 0
	for duplicated.Next() {
		i++
	}

	if i > 0 {
		values := []any{
			rk.Provider,
			rk.KeyName,
			rk.Key,
		}

		query := "UPDATE apikeys SET key = $3 WHERE $1 = provider AND $2 = key_name"
		if _, err := s.db.ExecContext(ctxTimeout, query, values...); err != nil {
			return err
		}

		return nil
	}

	query := `
		INSERT INTO apikeys (key_id, updated_at, key, key_name, provider)
		VALUES ($1, $2, $3, $4, $5)
	`

	values := []any{
		rk.Id,
		rk.UpdatedAt,
		rk.Key,
		rk.KeyName,
		rk.Provider,
	}

	ctxTimeout, cancel = context.WithTimeout(context.Background(), s.wt)
	defer cancel()
	if _, err := s.db.ExecContext(ctxTimeout, query, values...); err != nil {
		return err
	}

	return nil
}

func (s *Store) CreateKey(rk *key.RequestKey) (*key.ResponseKey, error) {
	ctxTimeout, cancel := context.WithTimeout(context.Background(), s.wt)
	defer cancel()
	duplicated, err := s.db.QueryContext(ctxTimeout, "SELECT * FROM keys WHERE $1 = key", rk.Key)
	if err != nil {
		return nil, err
	}
	defer duplicated.Close()

	i := 0
	for duplicated.Next() {
		i++
	}

	if i > 0 {
		return nil, NewDuplicationError("key can not be duplicated")
	}

	query := `
		INSERT INTO keys (name, created_at, updated_at, tags, revoked, key_id, key, revoked_reason, cost_limit_in_usd, cost_limit_in_usd_over_time, cost_limit_in_usd_unit, rate_limit_over_time, rate_limit_unit, ttl)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)
		RETURNING *;
	`

	values := []any{
		rk.Name,
		rk.CreatedAt,
		rk.UpdatedAt,
		sliceToSqlStringArray(rk.Tags),
		false,
		rk.KeyId,
		rk.Key,
		"",
		rk.CostLimitInUsd,
		rk.CostLimitInUsdOverTime,
		rk.CostLimitInUsdUnit,
		rk.RateLimitOverTime,
		rk.RateLimitUnit,
		rk.Ttl,
	}

	ctxTimeout, cancel = context.WithTimeout(context.Background(), s.wt)
	defer cancel()

	var k key.ResponseKey
	if err := s.db.QueryRowContext(ctxTimeout, query, values...).Scan(
		&k.Name,
		&k.CreatedAt,
		&k.UpdatedAt,
		pq.Array(&k.Tags),
		&k.Revoked,
		&k.KeyId,
		&k.Key,
		&k.RevokedReason,
		&k.CostLimitInUsd,
		&k.CostLimitInUsdOverTime,
		&k.CostLimitInUsdUnit,
		&k.RateLimitOverTime,
		&k.RateLimitUnit,
		&k.Ttl,
	); err != nil {
		return nil, err
	}

	return &k, nil
}

func (s *Store) DeleteKey(id string) error {
	ctxTimeout, cancel := context.WithTimeout(context.Background(), s.wt)
	defer cancel()

	_, err := s.db.ExecContext(ctxTimeout, "DELETE FROM keys WHERE key_id = $1", id)
	return err
}

func sliceToSqlStringArray(slice []string) string {
	return "{" + strings.Join(slice, ",") + "}"
}
